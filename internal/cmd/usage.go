package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

// usageRow is the usage row a headless session's result reads to, false when
// the result carried no usage object.
func usageRow(id, stage, logPath string, res runlog.Result) (ledger.Row, bool) {
	if res.Usage == nil {
		return ledger.Row{}, false
	}
	u, ctx := *res.Usage, res.ContextMax
	return ledger.Row{
		ID: id, Stage: stage, Log: filepath.Base(logPath), Usage: &u, Model: res.Model,
		SessionID: res.SessionID, CostUSD: res.Cost, DurationMS: res.DurationMS, Turns: res.NumTurns,
		Source: ledger.SourceHeadless, ContextMax: &ctx, RateLimit: res.RateLimit,
	}, true
}

// recordUsage appends the usage row for a headless launch that has exited,
// under the drover session id its stage row carries. Whatever the exit: a
// session that failed or was interrupted spent its tokens all the same, and
// the result event, when the runner wrote one, says how many.
//
// A row that will not write is reported and fails nothing. The launch it
// describes already happened, and the command's own outcome is about that.
func (s *session) recordUsage(id, stage, session string, oc runner.Outcome) {
	if !oc.Logged {
		return
	}
	row, ok := usageRow(id, stage, oc.LogPath, oc.Result)
	if !ok {
		return
	}
	row.Session = session
	if err := s.record(row); err != nil {
		fmt.Fprintf(errOut, "  ! %s: the usage row would not write: %v\n", id, err)
	}
}

// usageTally is what one --backfill or --sync pass did, for the line it
// prints.
type usageTally struct {
	// Read is logs for --backfill and transcript files for --sync, and Bytes
	// the transcript bytes --sync read.
	Read  int
	Bytes int64
	Wrote int
	// Skipped counts what was read and written nowhere, by reason.
	Skipped map[string]int
	// Unparsed is transcript lines that would not decode, and Pending the
	// session days --sync holds until the day is over.
	Unparsed int
	Pending  int
}

func newTally() usageTally { return usageTally{Skipped: map[string]int{}} }

// Why a log or a session day got no row.
const (
	skipName      = "not a run log name"
	skipPrefix    = "no [paths] log_prefix"
	skipUnread    = "would not read"
	skipNoResult  = "no result event"
	skipNoUsage   = "no usage object"
	skipNoSession = "no session id"
	skipRecorded  = "already recorded"
	skipHeadless  = "a run log names the session"
)

func (t usageTally) write(w io.Writer, what string, dry bool) {
	verb := "wrote"
	if dry {
		verb = "would write"
	}
	line := fmt.Sprintf("%s: read %d, %s %d", what, t.Read, verb, t.Wrote)
	if t.Bytes > 0 {
		line = fmt.Sprintf("%s: read %d (%d bytes), %s %d", what, t.Read, t.Bytes, verb, t.Wrote)
	}
	reasons := make([]string, 0, len(t.Skipped))
	for why := range t.Skipped {
		reasons = append(reasons, why)
	}
	sort.Strings(reasons)
	skipped := 0
	parts := make([]string, 0, len(reasons))
	for _, why := range reasons {
		skipped += t.Skipped[why]
		parts = append(parts, fmt.Sprintf("%d %s", t.Skipped[why], why))
	}
	line += fmt.Sprintf(", skipped %d", skipped)
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	if t.Unparsed > 0 {
		line += fmt.Sprintf("; %d lines would not parse", t.Unparsed)
	}
	if t.Pending > 0 {
		line += fmt.Sprintf("; %d session days held until the day ends", t.Pending)
	}
	fmt.Fprintln(w, line)
}

// backfill records a usage row for every log in the log directory whose
// result carries usage and whose session no usage row names yet.
func (s *session) backfill(dry bool) (usageTally, error) {
	t, _, err := s.backfillLogs(dry)
	return t, err
}

// backfillLogs is backfill, and also returns every session id a log names
// whether or not it wrote a row for it: a killed session's log has no result
// and gets no row, but its transcript is still not an interactive session's.
func (s *session) backfillLogs(dry bool) (usageTally, map[string]bool, error) {
	t := newTally()
	logged := map[string]bool{}
	dir := s.cfg.LogDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return t, logged, nil
	}
	if err != nil {
		return t, logged, fmt.Errorf("read the log directory %s: %w", dir, err)
	}
	path := s.cfg.Abs(s.cfg.Paths.Ledger)
	seen, logs := map[string]bool{}, map[string]bool{}
	for _, r := range s.ledger.Usage {
		seen[r.SessionID] = true
		if r.Log != "" {
			logs[r.Log] = true
		}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		t.Read++
		// A log a usage row already names is not reopened: one is one session,
		// and the row already says which.
		if logs[e.Name()] {
			t.Skipped[skipRecorded]++
			continue
		}
		row, why := s.backfillRow(dir, e.Name(), logged)
		if why == "" && seen[row.SessionID] {
			why = skipRecorded
		}
		if why != "" {
			t.Skipped[why]++
			continue
		}
		seen[row.SessionID] = true
		if dry {
			t.Wrote++
			continue
		}
		wrote, err := ledger.AppendUnless(path, row, func(l *ledger.Ledger) bool { return l.HasSession("", row.SessionID) })
		if err != nil {
			return t, logged, err
		}
		if !wrote {
			t.Skipped[skipRecorded]++
			continue
		}
		s.ledger.Usage = append(s.ledger.Usage, row)
		t.Wrote++
	}
	return t, logged, nil
}

// backfillRow reads one log into the row it would write, or names why it
// writes none, adding the session id the log names to logged. The name is
// read as runlog.Name wrote it: the prefix, the base a stage chose, and the
// writer's local clock.
func (s *session) backfillRow(dir, name string, logged map[string]bool) (ledger.Row, string) {
	base, at, ok := runlog.ParseName(name, time.Local)
	if !ok {
		return ledger.Row{}, skipName
	}
	if p := s.cfg.Paths.LogPrefix; p != "" {
		rest, found := strings.CutPrefix(base, p)
		if !found {
			return ledger.Row{}, skipPrefix
		}
		base = rest
	}
	stage := ledger.StageImplement
	if rest, found := strings.CutSuffix(base, "-review"); found {
		stage, base = ledger.StageReview, rest
	} else if rest, found := strings.CutSuffix(base, "-draft"); found {
		stage, base = ledger.StageDraft, rest
	}
	rd, err := runlog.ReadResult(filepath.Join(dir, name))
	if err == nil {
		if rd.SessionID != "" {
			logged[rd.SessionID] = true
		}
		if rd.Result.SessionID != "" {
			logged[rd.Result.SessionID] = true
		}
	}
	switch {
	case err != nil:
		return ledger.Row{}, skipUnread
	case !rd.Found:
		return ledger.Row{}, skipNoResult
	}
	row, ok := usageRow(s.stemItem(base), stage, name, rd.Result)
	switch {
	case !ok:
		return ledger.Row{}, skipNoUsage
	case row.SessionID == "":
		return ledger.Row{}, skipNoSession
	}
	row.TS, row.Date = at.Format(time.RFC3339), at.Format("2006-01-02")
	return row, ""
}

// stemItem is the bare id of the item a log base names, "" when its stem names
// no item this project has, on the backlog or the shelf.
func (s *session) stemItem(base string) string {
	stem, _, _ := strings.Cut(base, "-")
	id, err := backlog.ParseStem(stem, s.cfg.Prefixes())
	if err != nil {
		return ""
	}
	switch state, _ := s.resolve(id.Bare()); state {
	case idAbsent, idUnresolved:
		return ""
	}
	return id.Bare()
}

// usageSyncName is the state file --sync keeps beside the ledger.
const usageSyncName = "usage-sync.json"

// usageSync is what --sync carries from one pass to the next.
//
// Offsets are why it exists: a transcript is append-only and one reached 285
// MB, so each pass reads only what landed since the last. Pending is the
// price of that. A row covers one session on one local day, and a day still
// in progress is not done being written, so its messages wait here rather than
// in a row the rest of the day could never join: the ledger is append-only and
// a usage row is never replaced.
type usageSync struct {
	Files   map[string]int64       `json:"files"`
	Started map[string]time.Time   `json:"started,omitempty"`
	Pending map[string]*sessionDay `json:"pending,omitempty"`
}

// sessionDay is one session's messages on one local day. Messages is keyed by
// message id, because Claude Code writes one transcript line per content block
// and each repeats its message's usage.
type sessionDay struct {
	Session    string                  `json:"session"`
	Date       string                  `json:"date"`
	First      time.Time               `json:"first"`
	Last       time.Time               `json:"last"`
	Model      string                  `json:"model,omitempty"`
	ContextMax int64                   `json:"context_max,omitempty"`
	Messages   map[string]runlog.Usage `json:"messages,omitempty"`
}

func loadUsageSync(path string) (*usageSync, error) {
	st := &usageSync{}
	b, err := os.ReadFile(path) // #nosec G304 -- beside the configured ledger
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, st); err != nil {
			return nil, fmt.Errorf("%s will not parse (%v): delete it and the next --sync rereads every transcript, writing no row the ledger already holds", path, err)
		}
	}
	if st.Files == nil {
		st.Files = map[string]int64{}
	}
	if st.Started == nil {
		st.Started = map[string]time.Time{}
	}
	if st.Pending == nil {
		st.Pending = map[string]*sessionDay{}
	}
	return st, nil
}

func (st *usageSync) save(path string) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// claudeProjects is where Claude Code keeps its transcripts.
func claudeProjects() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to find Claude Code's transcripts under: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// transcriptSlug is the directory name Claude Code files a working directory's
// transcripts under: the absolute path with every character but a letter or a
// digit replaced by a dash, which covers its `/` and `.`.
func transcriptSlug(path string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, path)
}

// transcriptDirs is the project root's transcript directory and every one a
// worktree drive made for this project left, whether or not the tree is still
// there: a removed worktree's sessions were spent all the same.
func (s *session) transcriptDirs() ([]string, error) {
	base, err := claudeProjects()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", base, err)
	}
	trees, err := s.projectTrees()
	if err != nil {
		return nil, err
	}
	roots, prefixes := transcriptSlugs(s.cfg.Root), transcriptSlugs(trees)
	var out []string
	for _, e := range entries {
		if e.IsDir() && matchesTranscriptDir(e.Name(), roots, prefixes) {
			out = append(out, filepath.Join(base, e.Name()))
		}
	}
	return out, nil
}

// transcriptSlugs is the slug of path as given and, when it differs, the slug
// of path with its symlinks resolved, which is the one Claude Code names the
// directory after: on macOS anything under /tmp or /var is really under
// /private. A path that does not resolve contributes its literal slug alone.
func transcriptSlugs(path string) []string {
	slugs := []string{transcriptSlug(path)}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && transcriptSlug(resolved) != slugs[0] {
		slugs = append(slugs, transcriptSlug(resolved))
	}
	return slugs
}

func matchesTranscriptDir(name string, roots, trees []string) bool {
	if slices.Contains(roots, name) {
		return true
	}
	for _, t := range trees {
		if strings.HasPrefix(name, t+"-") {
			return true
		}
	}
	return false
}

// syncUsage backfills, then records the interactive sessions the transcripts
// hold, one row per session per local day before now's. dry reads the same
// bytes and writes neither a row nor the state file.
//
// The backfill is not optional. Claude Code writes a transcript for every
// headless session too, and a session no headless row names yet would be
// recorded here as interactive with no item, a row the append-only ledger never
// takes back and that backfill's dedupe then keeps out for good. A backfill
// that fails records nothing interactive for the same reason.
func (s *session) syncUsage(dry bool, now time.Time) (backfilled, t usageTally, err error) {
	t = newTally()
	backfilled, logged, err := s.backfillLogs(dry)
	if err != nil {
		return backfilled, t, fmt.Errorf("backfill before the sync: %w", err)
	}
	ledgerPath := s.cfg.Abs(s.cfg.Paths.Ledger)
	statePath := filepath.Join(filepath.Dir(ledgerPath), usageSyncName)
	st, err := loadUsageSync(statePath)
	if err != nil {
		return backfilled, t, err
	}
	dirs, err := s.transcriptDirs()
	if err != nil {
		return backfilled, t, err
	}
	changed := false
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return err
			}
			t.Read++
			info, err := d.Info()
			if err != nil {
				return err
			}
			from := st.Files[path]
			if info.Size() < from {
				// Shorter than what was consumed is a file rewritten in place,
				// and rereading it is safe: a day already recorded is skipped.
				from = 0
			}
			if info.Size() == from {
				return nil
			}
			n, to, bad, err := st.read(path, from)
			t.Bytes += n
			t.Unparsed += bad
			if err != nil {
				return err
			}
			if to != st.Files[path] {
				st.Files[path], changed = to, true
			}
			return nil
		})
		if err != nil {
			return backfilled, t, fmt.Errorf("read transcripts under %s: %w", dir, err)
		}
	}

	today := now.In(time.Local).Format("2006-01-02")
	keys := make([]string, 0, len(st.Pending))
	for k := range st.Pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		day := st.Pending[k]
		if day.Date >= today {
			t.Pending++
			continue
		}
		changed = true
		delete(st.Pending, k)
		if len(day.Messages) == 0 {
			continue
		}
		row := st.row(day)
		var why string
		skip := func(l *ledger.Ledger) bool {
			switch {
			case logged[day.Session] || l.HasSession(ledger.SourceHeadless, day.Session):
				why = skipHeadless
			case l.HasSessionDay(day.Session, day.Date):
				why = skipRecorded
			}
			return why != ""
		}
		if dry {
			if !skip(s.ledger) {
				t.Wrote++
			}
		} else {
			wrote, err := ledger.AppendUnless(ledgerPath, row, skip)
			if err != nil {
				return backfilled, t, err
			}
			if wrote {
				s.ledger.Usage = append(s.ledger.Usage, row)
				t.Wrote++
			}
		}
		if why != "" {
			t.Skipped[why]++
		}
	}
	if dry || !changed {
		return backfilled, t, nil
	}
	return backfilled, t, st.save(statePath)
}

// read folds the whole lines of one transcript from offset from into the
// pending days, and reports the bytes it read, the offset it consumed to and
// how many lines would not decode. A final line with no newline is one Claude
// Code is still writing, so it is read and left for the next pass.
func (st *usageSync) read(path string, from int64) (n, to int64, bad int, err error) {
	f, err := os.Open(path) // #nosec G304 -- a transcript under the Claude projects directory
	if err != nil {
		return 0, from, 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, from, 0, err
	}
	to = from
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		n += int64(len(line))
		if rerr == io.EOF {
			return n, to, bad, nil
		}
		if rerr != nil {
			return n, to, bad, rerr
		}
		to += int64(len(line))
		if !st.fold(line) {
			bad++
		}
	}
}

// fold adds one transcript line to its session's day, false for a line that
// does not decode. Only user and assistant lines carry a message; every other
// kind is Claude Code's bookkeeping and folds to nothing.
func (st *usageSync) fold(line []byte) bool {
	if len(strings.TrimSpace(string(line))) == 0 {
		return true
	}
	var ln struct {
		Type      string          `json:"type"`
		SessionID string          `json:"sessionId"`
		UUID      string          `json:"uuid"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &ln) != nil {
		return false
	}
	if ln.Type != "user" && ln.Type != "assistant" {
		return true
	}
	at, err := time.Parse(time.RFC3339Nano, ln.Timestamp)
	if err != nil || ln.SessionID == "" {
		return false
	}
	date := at.In(time.Local).Format("2006-01-02")
	key := ln.SessionID + "|" + date
	day, ok := st.Pending[key]
	if !ok {
		day = &sessionDay{Session: ln.SessionID, Date: date, First: at, Last: at, Messages: map[string]runlog.Usage{}}
		st.Pending[key] = day
	}
	if at.Before(day.First) {
		day.First = at
	}
	if at.After(day.Last) {
		day.Last = at
	}
	if first, ok := st.Started[ln.SessionID]; !ok || at.Before(first) {
		st.Started[ln.SessionID] = at
	}
	if ln.Type != "assistant" {
		return true
	}
	var msg struct {
		ID    string           `json:"id"`
		Model string           `json:"model"`
		Usage *runlog.APIUsage `json:"usage"`
	}
	if json.Unmarshal(ln.Message, &msg) != nil {
		return false
	}
	if msg.Usage == nil {
		return true
	}
	id := msg.ID
	if id == "" {
		id = ln.UUID
	}
	if day.Messages == nil {
		day.Messages = map[string]runlog.Usage{}
	}
	day.Messages[id] = msg.Usage.Usage()
	day.ContextMax = max(day.ContextMax, msg.Usage.Context())
	if day.Model == "" && msg.Model != "" && msg.Model != "<synthetic>" {
		day.Model = msg.Model
	}
	return true
}

// row is the usage row one finished session day reads to.
func (st *usageSync) row(day *sessionDay) ledger.Row {
	var u runlog.Usage
	for _, m := range day.Messages {
		u.Add(m)
	}
	ctx := day.ContextMax
	row := ledger.Row{
		Usage: &u, Model: day.Model, SessionID: day.Session, Source: ledger.SourceInteractive,
		ContextMax: &ctx, Date: day.Date,
		FirstTS: day.First.In(time.Local).Format(time.RFC3339),
		TS:      day.Last.In(time.Local).Format(time.RFC3339),
	}
	if started, ok := st.Started[day.Session]; ok {
		row.Started = started.In(time.Local).Format(time.RFC3339)
	}
	return row
}

// driveSync is drive's own --sync. It never stops the drive: usage is a record
// about the work, and a transcript drover cannot read is no reason to leave
// the queue undriven.
func (s *session) driveSync(when string) {
	b, t, err := s.syncUsage(false, time.Now())
	if b.Wrote > 0 {
		b.write(errOut, "· usage backfill "+when, false)
	}
	if err != nil {
		fmt.Fprintf(errOut, "· usage sync %s: %v; the drive continues\n", when, err)
		return
	}
	if t.Wrote > 0 || t.Unparsed > 0 {
		t.write(errOut, "· usage sync "+when, false)
	}
}

var (
	usageBackfill bool
	usageSyncFlag bool
	usageDry      bool
)

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Record each session's token usage in the ledger, from run logs and Claude Code's transcripts.",
	Long: `Every headless session drover launches already records its usage when it
exits. This command records the rest.

--backfill reads every log in the log directory and appends one usage row per
log whose result event carries a usage object and whose session id no usage row
names yet. The filename says which item and stage the log was for, and when.

--sync reads Claude Code's transcripts for the project root and for every
worktree drive made for this project, and appends one interactive usage row per
session per local day. A day is recorded once it is over, so today's sessions
wait for the first --sync after midnight. --sync always backfills first, because
Claude Code writes a transcript for every headless session too, and a session
any run log names is skipped rather than recorded as interactive. Each
transcript is read from where the last --sync stopped, kept in
` + usageSyncName + ` beside the ledger. drive runs the same step, backfill and
sync, before it reads its queue and after its end row.

Run either twice and the second writes nothing. The counts are tokens as the API
reported them, not the subscription's quota, and cost_usd is list price.`,
	Example: `  drover usage --backfill --dry-run   # what a backfill would write
  drover usage --backfill             # one row per log not yet recorded
  drover usage --sync                 # backfill, then interactive sessions per day`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		if !usageBackfill && !usageSyncFlag {
			return c.Help()
		}
		s, err := open()
		if err != nil {
			return err
		}
		if !usageSyncFlag {
			t, err := s.backfill(usageDry)
			if err != nil {
				return err
			}
			t.write(os.Stdout, "backfill: logs", usageDry)
			return nil
		}
		b, t, err := s.syncUsage(usageDry, time.Now())
		b.write(os.Stdout, "backfill: logs", usageDry)
		if err != nil {
			return err
		}
		t.write(os.Stdout, "sync: transcripts", usageDry)
		return nil
	},
}

func init() {
	usageCmd.Flags().BoolVar(&usageBackfill, "backfill", false, "record usage from every run log not yet in the ledger")
	usageCmd.Flags().BoolVar(&usageSyncFlag, "sync", false, "backfill, then record interactive sessions from Claude Code's transcripts")
	usageCmd.Flags().BoolVar(&usageDry, "dry-run", false, "print the counts and write nothing")
	root.AddCommand(usageCmd)
}
