package cmd

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// ItemOrigin is where an item came from, read from its origin row. Text is the
// phrase the status table prints; the fields beside it are the row's own, so a
// consumer never parses the phrase back.
type ItemOrigin struct {
	// Unrecorded is an item minted before drover wrote origin rows. Every other
	// field is then empty and Text is "unrecorded".
	Unrecorded bool   `json:"unrecorded"`
	Text       string `json:"text"`
	Cmd        string `json:"cmd,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Session    string `json:"session,omitempty"`
	FoundIn    string `json:"found_in,omitempty"`
	Issue      int    `json:"issue,omitempty"`
	SplitFrom  string `json:"split_from,omitempty"`
	// Drive, Stage and Of are the stage row the origin's session resolves to:
	// which drive launched it, at which stage, for which item.
	Drive string `json:"drive,omitempty"`
	Stage string `json:"stage,omitempty"`
	Of    string `json:"of,omitempty"`
}

// ItemTime is when an item was created. At is RFC 3339, or a bare date where
// the row it came from carries no ts. Source names that row: origin, the
// earliest ledger row naming the item (which bounds the creation from above,
// so the table prints it with ≤), or git, the commit show falls back to.
type ItemTime struct {
	Unrecorded bool   `json:"unrecorded"`
	At         string `json:"at,omitempty"`
	Source     string `json:"source,omitempty"`
	when       time.Time
	clock      bool
}

// ItemEdit is the latest write to an item's body. Source is body (the latest
// row carrying a digest that still matches the file), origin (no digest row
// yet, so the creation was the last write drover saw), hand (the file no longer
// matches the latest digest, and At is its mtime) or mtime (no row at all).
type ItemEdit struct {
	Unrecorded bool   `json:"unrecorded"`
	At         string `json:"at,omitempty"`
	Source     string `json:"source"`
	Cmd        string `json:"cmd,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Drive      string `json:"drive,omitempty"`
	ByHand     bool   `json:"by_hand,omitempty"`
	when       time.Time
	clock      bool
}

const (
	sourceOrigin = "origin"
	sourceLedger = "ledger"
	sourceGit    = "git"
	sourceBody   = "body"
	sourceHand   = "hand"
	sourceMtime  = "mtime"
)

// provenance reads where it came from and who last wrote it, through the
// ledger's Provenance helper. first is the earliest row naming the item, the
// fallback creation time for an item with no origin row.
func (s *session) provenance(id, path string, first *ledger.Row) (*ItemOrigin, *ItemTime, *ItemEdit) {
	raw, _ := os.ReadFile(path) // #nosec G304 -- the path came from the backlog
	var mtime time.Time
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime()
	}
	p := s.ledger.Provenance(id, bodySum(raw), digestBytes(raw))

	origin := &ItemOrigin{Unrecorded: true, Text: "unrecorded"}
	created := &ItemTime{Unrecorded: true}
	if p.Recorded() {
		o := p.Origin.Origin
		origin = &ItemOrigin{Cmd: o.Cmd, Actor: o.Actor, Agent: o.Agent, Session: o.Session,
			FoundIn: o.FoundIn, Issue: o.Issue, SplitFrom: o.SplitFrom}
		if l := p.Launch; l != nil {
			origin.Drive, origin.Stage, origin.Of = l.Drive, l.Stage, l.ID
		}
		origin.Text = origin.phrase()
		created = &ItemTime{Source: sourceOrigin}
		created.set(p.Origin.TS, p.Origin.Date)
	} else if first != nil {
		created.Source = sourceLedger
		created.set(first.TS, first.Date)
	}

	edited := &ItemEdit{Unrecorded: true, Source: sourceMtime}
	switch b := p.LastBody; {
	case b != nil && !p.Matches:
		edited = &ItemEdit{Source: sourceHand, ByHand: true}
	case b != nil:
		edited = &ItemEdit{Source: sourceBody, Unrecorded: b.Actor == "", Cmd: b.Cmd, Actor: b.Actor, Agent: b.Agent, Drive: b.Drive}
		edited.set(b.TS, b.Date)
	case p.Recorded():
		o := p.Origin.Origin
		edited = &ItemEdit{Source: sourceOrigin, Cmd: o.Cmd, Actor: o.Actor, Agent: o.Agent, Drive: origin.Drive}
		edited.when, edited.At, edited.clock = created.when, created.At, created.clock
	}
	if (edited.Source == sourceHand || edited.Source == sourceMtime) && !mtime.IsZero() {
		edited.when, edited.At, edited.clock = mtime, mtime.Format(time.RFC3339), true
	}
	return origin, created, edited
}

// phrase is the ORIGIN cell: who, then what. A launched session that resolves
// to its stage row is named by that row, since "agent" says nothing a reader
// can act on; an interactive agent typing at the operator's terminal is named
// in place of the operator, because that is who chose to file it.
func (o ItemOrigin) phrase() string {
	var text string
	switch {
	case o.Of != "":
		text = strings.TrimSpace(o.Drive+" "+o.Stage) + " of " + o.Of
	default:
		who := o.Actor
		if who == runner.ActorOperator && o.Agent != "" {
			who = o.Agent
		}
		text = who + " " + o.Cmd
		switch {
		case o.Cmd == "intake" && o.Issue > 0:
			text += " #" + strconv.Itoa(o.Issue)
		case o.Cmd == "split" && o.SplitFrom != "":
			text += " of " + o.SplitFrom
		}
	}
	if o.FoundIn != "" && o.FoundIn != o.Of {
		text += " ← " + o.FoundIn
	}
	return text
}

// editor is the EDITOR cell. A drive's in-process write is named by the drive,
// which is who an operator can ask about it.
func (e ItemEdit) editor() string {
	switch {
	case e.ByHand:
		return "by hand"
	case e.Unrecorded:
		return "unrecorded"
	}
	who := e.Actor
	switch {
	case e.Drive != "":
		who = e.Drive
	case who == runner.ActorOperator && e.Agent != "":
		who = e.Agent
	}
	return e.Cmd + " · " + who
}

// set reads a row's ts, falling back to its date. A row with neither leaves
// the time unset, which reads as unknown rather than as the zero year.
func (t *ItemTime) set(ts, date string) {
	t.when, t.clock = parseStamp(ts, date)
	t.At = stampText(ts, date, t.when)
}

func (e *ItemEdit) set(ts, date string) {
	e.when, e.clock = parseStamp(ts, date)
	e.At = stampText(ts, date, e.when)
}

func parseStamp(ts, date string) (time.Time, bool) {
	if at, err := time.Parse(time.RFC3339, ts); err == nil {
		return at, true
	}
	if at, err := time.ParseInLocation(dateOnly, date, time.Local); err == nil {
		return at, false
	}
	return time.Time{}, false
}

func stampText(ts, date string, at time.Time) string {
	switch {
	case at.IsZero():
		return ""
	case ts != "":
		return ts
	}
	return date
}

// cellTime is a time as the second line prints it, in local time: month, day
// and clock this year, the date alone for any other, so a row from last
// October never reads as this one.
func cellTime(at time.Time, clock bool, now time.Time) string {
	if at.IsZero() {
		return "-"
	}
	at = at.In(time.Local)
	switch {
	case at.Year() != now.In(time.Local).Year():
		return at.Format(dateOnly)
	case clock:
		return at.Format("Jan 02 15:04")
	}
	return at.Format("Jan 02")
}

// The second line's fixed widths. Each time cell is preceded by one column for
// its qualifier, ≤ or ~, so a qualified time and a plain one start at the same
// column.
const (
	originWidth = 28
	timeWidth   = 12
	// secondIndent puts the second line under the id column: the glyph, a
	// space, the current marker and a space.
	secondIndent = 6
)

// secondLine is a row's provenance as the table prints it under the row, plain.
// A merged row's line ends in a tab: rtk filters a line at a time, so this line
// has to say on its own that it belongs to a merged row for the drover-status
// filter to drop it with that row, and the glyph above already says merged to a
// reader, so the mark is one nobody sees.
func (r Row) secondLine(now time.Time, editorWidth int, branch string) string {
	indent := strings.Repeat(" ", secondIndent)
	merged := ""
	if r.State == stateDone {
		merged = "\t"
	}
	created, edited := "", ""
	var cAt, eAt time.Time
	var cClock, eClock bool
	cMark, eMark := " ", " "
	if c := r.Created; c != nil {
		cAt, cClock = c.when, c.clock
		if c.Source == sourceLedger && !cAt.IsZero() {
			cMark = "≤"
		}
	}
	if e := r.Edited; e != nil {
		eAt, eClock = e.when, e.clock
		if e.ByHand || (e.Source == sourceMtime && !eAt.IsZero()) {
			eMark = "~"
		}
		edited = e.editor()
	}
	created = cellTime(cAt, cClock, now)
	origin := ""
	if r.Origin != nil {
		origin = r.Origin.Text
	}
	if branch != "" {
		edited = padRunes(edited, editorWidth) + "  " + branch
	}
	return fmt.Sprintf("%s%-*s %s%-*s %s%-*s %s%s", indent,
		originWidth, trunc(origin, originWidth),
		cMark, timeWidth, created,
		eMark, timeWidth, cellTime(eAt, eClock, now),
		edited, merged)
}

// since is when the row was last created or edited, the later of the two.
func (r Row) since() time.Time {
	var t time.Time
	if r.Created != nil {
		t = r.Created.when
	}
	if r.Edited != nil && r.Edited.when.After(t) {
		t = r.Edited.when
	}
	return t
}

// attachProvenance sets each row's origin, created and edited objects. An
// unreadable row gets them too, read under the id its filename names: the
// ledger and the file's mtime answer without parsing the body.
func (s *session) attachProvenance(rows []Row) {
	first := map[string]*ledger.Row{}
	firstAt := map[string]time.Time{}
	for i := range s.ledger.Rows {
		r := &s.ledger.Rows[i]
		if r.ID == "" {
			continue
		}
		at, _ := parseStamp(r.TS, r.Date)
		if at.IsZero() {
			continue
		}
		if prev, ok := firstAt[r.ID]; !ok || at.Before(prev) {
			first[r.ID], firstAt[r.ID] = r, at
		}
	}
	items := make(map[string]backlog.Item, len(s.backlog.Items))
	for _, it := range s.backlog.Items {
		items[it.ID.Bare()] = it
	}
	for i := range rows {
		id, path := rows[i].ID, rows[i].path
		if rows[i].State == stateBroken {
			if parsed, ok := (backlog.Broken{Path: path}).ID(s.cfg.Prefixes()); ok {
				id = parsed.Bare()
			}
		} else if it, ok := items[id]; ok {
			path = it.Path
		} else {
			continue
		}
		rows[i].Origin, rows[i].Created, rows[i].Edited = s.provenance(id, path, first[id])
	}
}

var sinceSpan = regexp.MustCompile(`^([0-9]+)d$`)

// parseSinceFlag reads --since: a span back from now (3d, 12h, 90m) or a
// date, local midnight, or an RFC 3339 time.
func parseSinceFlag(v string, now time.Time) (time.Time, error) {
	if m := sinceSpan.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.AddDate(0, 0, -n), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if at, err := time.ParseInLocation(dateOnly, v, time.Local); err == nil {
		return at, nil
	}
	if at, err := time.Parse(time.RFC3339, v); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: give a span back from now (3d, 12h, 90m), a date (2026-10-20) or an RFC 3339 time", v)
}
