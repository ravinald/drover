package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/spf13/cobra"
)

// driveEventNames is every event a drive emits, which is what watch --until
// may name.
var driveEventNames = []string{
	evStarted, evQueued, evDispatched, evSkipped, evPreflight, evStage, evCheck, evParked, evFiled,
	evNeedsOperator, evMerged, evPaused, evResumed, evReexecWaiting, evConfigChanged, evUsageWarning, evThreadOpened, evExit,
}

// driveEventsPath is where a drive keeps its events, one object per line,
// whether or not it streams them under --events. Per-user state rather than
// the project: a process that did not launch the drive has to find it, and
// nothing the project tracks may name one machine's drive.
func driveEventsPath(project, id string) (string, error) {
	dir, err := driveFilesDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".jsonl"), nil
}

// driveFilesDir holds a project's drive event files and detached drive logs.
func driveFilesDir(project string) (string, error) {
	dir, err := stateDir("drives")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, project), nil
}

// followed is one drive attach or watch reads: its id, its event file and the
// lease of the checkout it runs in, which says whether it still runs.
type followed struct {
	id     string
	events string
	lease  string
	// root is the project root, which a stage event's log is relative to.
	root string
	// tick, when set, runs once per pass over the event file, after the lines
	// it gained are handed out and before the wait for more: attach --session
	// reads the session logs it follows there.
	tick func()
}

// errDriveGone is a drive that stopped running with no exit event in its file:
// killed, or its event file stopped taking writes.
var errDriveGone = errors.New("ended without an exit event")

// followPoll is how often attach and watch look for a line the drive appended.
var followPoll = 200 * time.Millisecond

// resolveFollowed names the drive args points at: the id given, else the drive
// running in this checkout, else the project's latest. An id that neither the
// ledger nor the lease names is no drive at all.
func resolveFollowed(args []string) (followed, error) {
	wd, err := os.Getwd()
	if err != nil {
		return followed{}, err
	}
	cfg, err := config.Load(wd)
	if err != nil {
		return followed{}, err
	}
	lp, err := (&session{cfg: cfg}).leasePath()
	if err != nil {
		return followed{}, err
	}
	led, err := ledger.Load(cfg.Abs(cfg.Paths.Ledger))
	if err != nil {
		return followed{}, err
	}
	id := ""
	if len(args) == 1 {
		id = strings.ToUpper(args[0])
	}
	l, busy, _ := lease.Read(lp)
	running := busy && l.Command == "drive" && l.Drive != ""
	ds := led.Drives()
	switch {
	case id != "":
	case running:
		id = l.Drive
	case len(ds) > 0:
		id = ds[0].ID
	default:
		return followed{}, errors.New("no drive has run in this project, so there is none to follow")
	}
	if (!running || l.Drive != id) && !slices.ContainsFunc(ds, func(d ledger.Drive) bool { return d.ID == id }) {
		return followed{}, fmt.Errorf("no drive %s in this project; drover drives lists them", id)
	}
	path, err := driveEventsPath(cfg.Project, id)
	if err != nil {
		return followed{}, err
	}
	return followed{id: id, events: path, lease: lp, root: cfg.Root}, nil
}

// running reports whether the checkout's lease still names this drive. A drive
// holding the lease that has not named its id yet counts, since it may be this
// one about to.
func (f followed) running() bool {
	l, busy, err := lease.Read(f.lease)
	return err == nil && busy && l.Command == "drive" && (l.Drive == f.id || l.Drive == "")
}

// follow hands fn each event in the drive's file from the start, with its line
// number from 1 (the seq the drive stamped on it) and the offset just past the
// line, and waits for more until fn returns true or the
// exit event is read. Liveness is read before the file each time, so a line the
// drive wrote just before it let the lease go is still read.
func (f followed) follow(ctx context.Context, fn func(line []byte, n int, end int64, e DriveEvent) bool) error {
	var off, consumed int64
	n := 0
	var buf []byte
	seen := false
	for {
		alive := f.running()
		chunk, exists, err := readFrom(f.events, off)
		if err != nil {
			return err
		}
		seen = seen || exists
		off += int64(len(chunk))
		buf = append(buf, chunk...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := buf[:i]
			buf = buf[i+1:]
			consumed += int64(i) + 1
			n++
			var e DriveEvent
			if json.Unmarshal(line, &e) != nil || e.Event == "" {
				continue
			}
			if fn(line, n, consumed, e) || e.Event == evExit {
				return nil
			}
		}
		if f.tick != nil {
			f.tick()
		}
		if !alive {
			if !seen {
				return fmt.Errorf("drive %s is not running and has no event file at %s: it ran before drives kept one, or the file would not write", f.id, f.events)
			}
			return fmt.Errorf("drive %s %w at %s: it was killed, or its event file stopped taking writes", f.id, errDriveGone, f.events)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(followPoll):
		}
	}
}

// readFrom is path's bytes past off. A file not there yet is empty: a drive
// opens it with its id, and attach may get there first.
func readFrom(path string, off int64) ([]byte, bool, error) {
	f, err := os.Open(path) // #nosec G304 -- under the per-user state directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, true, err
	}
	b, err := io.ReadAll(f)
	return b, true, err
}

// before reports whether an event stamped ts was emitted before since. An
// unreadable stamp is never skipped.
func before(ts string, since time.Time) bool {
	at, err := time.Parse(time.RFC3339, ts)
	return err == nil && at.Before(since)
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since takes an event's ts, RFC 3339 (2026-10-18T09:00:00Z); got %q", s)
	}
	return at, nil
}

// eventLineTo is one event as attach prints it without --json: the stamp, the
// event, the item, then whichever fields it carries. tint colors it in the
// palette a foreground drive's stream uses, padded before it is colored so the
// columns hold.
func eventLineTo(e DriveEvent, tint bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s", sgr(e.TS, pal.Muted, tint), sgr(fmt.Sprintf("%-14s", e.Event), pal.event(e.Event), tint))
	if e.ID != "" {
		b.WriteString(" " + sgr(e.ID, pal.ID, tint))
	}
	add := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, " %s=%s", k, v)
		}
	}
	num := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	add("queue", strings.Join(e.Queue, ","))
	add("jobs", num(e.Jobs))
	add("deadline", e.Deadline)
	add("stage", e.Stage)
	add("log", e.Log)
	add("park", e.Park)
	add("detail", e.Detail)
	add("action", e.Action)
	add("policy", e.Policy)
	add("pr", num(e.PR))
	add("slot", num(e.Slot))
	add("position", num(e.Position))
	add("check", e.Check)
	add("class", e.Class)
	add("head", e.Head)
	if e.Code != nil {
		add("code", strconv.Itoa(*e.Code))
	}
	if e.Summary != nil {
		add("ended", e.Summary.Ended)
		add("merged", strconv.Itoa(e.Summary.Merged))
		add("parked", strconv.Itoa(e.Summary.Parked))
		add("skipped", strconv.Itoa(e.Summary.Skipped))
		add("held", strconv.Itoa(e.Summary.Held))
	}
	add("changed", strings.Join(e.Changed, ","))
	add("tables", strings.Join(e.Tables, ","))
	add("window", e.Window)
	if e.Remaining != nil {
		add("remaining", strconv.Itoa(*e.Remaining)+"%")
	}
	add("resets_at", e.ResetsAt)
	add("thread", e.Thread)
	add("url", e.URL)
	add("seq", num(e.Seq))
	for _, kv := range [][2]string{{"reason", e.Reason}, {"run", e.Run}, {"evidence", e.Evidence}, {"error", e.Error}} {
		if kv[1] != "" {
			add(kv[0], strconv.Quote(kv[1]))
		}
	}
	return b.String()
}

var (
	attachSince   string
	attachJSON    bool
	attachSession bool
	attachItem    string
)

// SessionActivity is one entry attach --session --json prints for a session
// log line. It is keyed activity rather than event, so a reader filtering the
// stream on event sees the drive's events and nothing else.
type SessionActivity struct {
	// Activity is runlog.ActivityTool or runlog.ActivityText.
	Activity string `json:"activity"`
	// TS is when attach read the line, not when the session wrote it: codex
	// and agy stamp no line, and one clock across runners keeps the entries
	// of items running at once in the order they were read.
	TS    string `json:"ts"`
	Drive string `json:"drive"`
	ID    string `json:"id"`
	Stage string `json:"stage"`
	Tool  string `json:"tool,omitempty"`
	Text  string `json:"text"`
}

// sessionLog is the file a stage event's log names, which displayPath wrote
// relative to the project root or under ~/.
func sessionLog(root, log string) string {
	if rest, ok := strings.CutPrefix(log, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(log) {
		return log
	}
	return filepath.Join(root, log)
}

// sessionTail is one session log attach follows, by offset as follow reads the
// event file. broken is a log that would not read, reported once and then
// left alone.
type sessionTail struct {
	id, stage, path string
	off             int64
	buf             []byte
	broken          bool
}

// attacher is attach's state across the event file and the session logs it
// opened from it.
type attacher struct {
	f        followed
	out, err io.Writer
	json     bool
	session  bool
	item     string
	since    time.Time
	// replayTo is the event file's size when attach started. A stage event
	// within it launched a session that may have finished long ago, so its log
	// waits in pending until the replay is read: an item with a later event
	// finished it, and the rest are live only if the drive still runs.
	replayTo int64
	read     int64
	live     bool
	tails    map[string]*sessionTail
	pending  map[string]*sessionTail
}

func newAttacher(f followed, out, errw io.Writer) *attacher {
	a := &attacher{
		f: f, out: out, err: errw,
		json: attachJSON, session: attachSession, item: attachItem,
		tails: map[string]*sessionTail{}, pending: map[string]*sessionTail{},
	}
	if info, err := os.Stat(f.events); err == nil {
		a.replayTo = info.Size()
	}
	return a
}

// event prints one drive event, after closing whatever session of the same
// item it ends: the next event of an item is the end of its session's
// activity, so that log is read once more to its last line and dropped.
func (a *attacher) event(line []byte, end int64, e DriveEvent) {
	a.read = end
	if a.item != "" && !strings.EqualFold(e.ID, a.item) && e.Event != evExit {
		return
	}
	switch {
	case e.Event == evExit:
		for _, id := range slices.Sorted(maps.Keys(a.tails)) {
			a.stop(id)
		}
		clear(a.pending)
	case e.ID != "":
		a.stop(e.ID)
	}
	if !a.since.IsZero() && before(e.TS, a.since) {
		return
	}
	if a.json {
		fmt.Fprintf(a.out, "%s\n", line)
	} else {
		fmt.Fprintln(a.out, eventLineTo(e, colorTo(a.out)))
	}
	if a.session && e.Event == evStage && e.Log != "" && e.ID != "" {
		t := &sessionTail{id: e.ID, stage: e.Stage, path: sessionLog(a.f.root, e.Log)}
		if end <= a.replayTo {
			a.pending[e.ID] = t
		} else {
			a.tails[e.ID] = t
		}
	}
}

func (a *attacher) stop(id string) {
	delete(a.pending, id)
	if t := a.tails[id]; t != nil {
		a.poll(t)
		delete(a.tails, id)
	}
}

// tick reads every session log attach follows, once the replay is behind it.
func (a *attacher) tick() {
	if !a.live && a.read >= a.replayTo {
		a.live = true
		if a.f.running() {
			maps.Copy(a.tails, a.pending)
		}
		clear(a.pending)
	}
	for _, id := range slices.Sorted(maps.Keys(a.tails)) {
		a.poll(a.tails[id])
	}
}

// poll prints the activity in every whole line the log gained. A log not
// there yet is waited for; the drive names it before the runner opens it.
func (a *attacher) poll(t *sessionTail) {
	if t.broken {
		return
	}
	chunk, _, err := readFrom(t.path, t.off)
	if err != nil {
		t.broken = true
		fmt.Fprintf(a.err, "· %s: cannot read the %s session's log %s: %v; following the drive's events without it\n", t.id, t.stage, t.path, err)
		return
	}
	t.off += int64(len(chunk))
	t.buf = append(t.buf, chunk...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			return
		}
		line := t.buf[:i]
		t.buf = t.buf[i+1:]
		for _, act := range runlog.Activities(line) {
			a.activity(t, act)
		}
	}
}

func (a *attacher) activity(t *sessionTail, act runlog.Activity) {
	if a.json {
		b, err := json.Marshal(SessionActivity{
			Activity: act.Kind, TS: time.Now().UTC().Format(time.RFC3339), Drive: a.f.id,
			ID: t.id, Stage: t.stage, Tool: act.Tool, Text: act.Summary,
		})
		if err == nil {
			fmt.Fprintf(a.out, "%s\n", b)
		}
		return
	}
	mark := "·"
	if act.Kind == runlog.ActivityTool {
		mark = ">"
	}
	tint := colorTo(a.out)
	fmt.Fprintf(a.out, "%s%s %s %s\n", activityIndent, sgr(t.id, pal.ID, tint), sgr(mark, pal.Detail, tint), act.Summary)
}

// activityIndent puts a session's activity under the item column of
// eventLineTo: a 20-rune stamp, two spaces, a 14-rune event name and a space.
var activityIndent = strings.Repeat(" ", 37)

var attachCmd = &cobra.Command{
	Use:   "attach [drive id]",
	Short: "Print a drive's events from the start and follow them until it exits.",
	Long: `Reads the drive's event file, which every drive writes whether or not it ran
under --events or --detach, prints it from the first line, then follows it
until the drive's exit event and exits 0. With no id it follows the drive
running in this checkout, else the project's latest.

An interrupt stops following and leaves the drive running: attach never
signals the drive or writes anything it reads. A drive that stops running with
no exit event in its file (killed, or its event file stopped taking writes)
exits 1, and so does an id this project has no drive under.

--session prints, under each stage event that launched a session, what that
session is doing as its log grows: each tool it calls and each line of text it
writes, one per line, prefixed with the item's id. It follows a log until the
item's next event. Replaying a drive already under way, it reads the logs of
sessions still running and of every session launched after; a session that
finished before attach started keeps its stage line alone. A log that will not
read is reported once on stderr and attach goes on with the events.

--item limits the output to one item's events and the exit. An id the drive
has not reached prints nothing until it does, since it may be enqueued later.

Every event the drive emits prints, usage_warning among them: a [usage]
notify level crossed, with the window, the percent of it left and its reset.
A usage hold prints as paused and needs_operator with action=usage_admission,
and the resume at the reset as resumed with the same action.

--since skips events stamped before it. --json prints each line as the drive
wrote it, the objects documented under drive --events; without it each event
is one readable line. Under --json --session each activity is an object keyed
activity rather than event, its ts the time attach read it.`,
	Example: `  drover attach                     # the drive running here, or the latest
  drover attach D3 --json           # its events as JSON, one per line
  drover attach --since 2026-10-18T09:00:00Z
  drover attach D8 --session --item F14   # what F14's session is doing

  2026-10-18T09:12:03Z  dispatched     4
  2026-10-18T09:12:03Z  stage          4 stage=implement log=~/.cache/drover/webapp/4-retry.20261018-021203.123456789.jsonl
  2026-10-18T09:40:51Z  parked         4 park=silent detail=unknown reason="the review recorded nothing"
  2026-10-18T09:40:52Z  exit           code=0

  under --session, the session's activity beneath its stage line:
  2026-10-24T22:40:11Z  stage          F14 stage=implement log=~/.cache/drover/webapp/F14-ntfy.20261024-154011.123456789.jsonl seq=7
                                       F14 > Read internal/cmd/events.go
                                       F14 > Bash make build && make check
                                       F14 · The verify command passes; committing.

  under --json, each line as the drive wrote it:
  {"event":"exit","ts":"2026-10-18T09:40:52Z","drive":"D3","code":0}
  and under --json --session, each activity beside them:
  {"activity":"tool","ts":"2026-10-24T22:41:02Z","drive":"D8","id":"F14","stage":"implement","tool":"Bash","text":"Bash make build && make check"}`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		since, err := parseSince(attachSince)
		if err != nil {
			return err
		}
		f, err := resolveFollowed(args)
		if err != nil {
			return err
		}
		a := newAttacher(f, os.Stdout, errOut)
		a.since = since
		f.tick = a.tick
		err = f.follow(c.Context(), func(line []byte, _ int, end int64, e DriveEvent) bool {
			a.event(line, end, e)
			return false
		})
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(errOut, "· stopped following drive %s; it is still running\n", f.id)
			return nil
		}
		return err
	},
}

func init() {
	attachCmd.Flags().StringVar(&attachSince, "since", "", "skip events stamped before this RFC 3339 time")
	attachCmd.Flags().BoolVar(&attachJSON, "json", false, "print each event as the JSON line the drive wrote")
	attachCmd.Flags().BoolVar(&attachSession, "session", false, "print what each running session is doing beneath its stage event")
	attachCmd.Flags().StringVar(&attachItem, "item", "", "print only this item's events, and the exit")
	root.AddCommand(attachCmd)
}
