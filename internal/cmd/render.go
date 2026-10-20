package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/dashboard"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
)

// Queued is a pending item's place in a running drive's queue.
type Queued struct {
	PID      int `json:"pid"`
	Position int `json:"position"`
}

// etaSamples is how many timed runs of a stage an estimate needs. Below it a
// median is one or two runs, and a number printed from that reads as a forecast
// it is not.
const etaSamples = 5

// driveStages are the stages a drive runs per item, in order. The estimate
// covers these and nothing after them: the gate, the pull request and the merge
// write no row that opens a stage, so their time is not measured.
var driveStages = []string{ledger.StageImplement, ledger.StageReview}

// attachTimes sets each row's stage times from the ledger.
func (s *session) attachTimes(rows []Row) {
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	for i := range rows {
		if rows[i].State == stateBroken {
			continue
		}
		t := s.ledger.Times(rows[i].ID)
		rows[i].Started, rows[i].Finished = stamp(t.Started), stamp(t.Finished)
		if !t.StageStarted.IsZero() {
			rows[i].Stage, rows[i].StageStarted = t.Stage, stamp(t.StageStarted)
		}
	}
}

// attachDrive marks the items a running drive has queued and estimates when
// its running and queued items will finish their review. rows is the whole
// backlog, before any filter, so positions and offsets count every item ahead.
func (s *session) attachDrive(rows []Row, l lease.Lease, now time.Time) {
	if l.Command != "drive" || l.PID == 0 {
		return
	}
	byID := make(map[string]*Row, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	for n, id := range l.Queue {
		if r := byID[id]; r != nil && r.State != stateDone {
			r.Queued = &Queued{PID: l.PID, Position: n + 1}
		}
	}

	med := map[string]time.Duration{}
	durs := s.ledger.StageDurations()
	var whole time.Duration
	for _, st := range driveStages {
		if len(durs[st]) < etaSamples {
			return
		}
		med[st] = median(durs[st])
		whole += med[st]
	}
	leased, _ := time.Parse(time.RFC3339, l.Started)
	var free []time.Time
	for _, id := range l.Running {
		left := whole
		r := byID[id]
		if r != nil && r.Stage != "" {
			if at, err := time.Parse(time.RFC3339, r.StageStarted); err == nil && !at.Before(leased) {
				left = remaining(med, r.Stage, now.Sub(at))
			}
		}
		end := now.Add(left)
		free = append(free, end)
		if r != nil {
			r.ETA = end.Format(time.RFC3339)
		}
	}
	for len(free) < max(l.Jobs, 1) {
		free = append(free, now)
	}
	for _, id := range l.Queue {
		slices.SortFunc(free, func(a, b time.Time) int { return a.Compare(b) })
		free[0] = free[0].Add(whole)
		if r := byID[id]; r != nil && r.State != stateDone {
			r.ETA = free[0].Format(time.RFC3339)
		}
	}
}

// remaining is what is left of a run in stage, elapsed into it: the rest of
// that stage's median, never below zero, and every stage after it.
func remaining(med map[string]time.Duration, stage string, elapsed time.Duration) time.Duration {
	var left time.Duration
	at := slices.Index(driveStages, stage)
	for i, st := range driveStages {
		switch {
		case i == at:
			left += max(med[st]-elapsed, 0)
		case i > at:
			left += med[st]
		}
	}
	return left
}

func median(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// statusReport is the object --json emits and a template reads. scoped applies
// the operator's --all, --sev and --by-sev; a configured render ignores them
// and carries every item, since a file kept current over a drive has to show
// the items it finishes.
func (s *session) statusReport(scoped, tracker bool, now time.Time) StatusReport {
	rows := s.rows()
	s.attachTimes(rows)
	if scoped && statusBySev {
		rows = bySeverity(rows)
	}
	var report StatusReport
	l, busy, err := s.leaseHolder()
	switch {
	case err != nil:
		report.LeaseError = err.Error()
	case busy && l.PID == 0:
		report.LeaseError = leaseUnnamed
	case busy:
		report.Lease = &l
		s.attachDrive(rows, l, now)
	}
	if ds := s.ledger.Drives(); len(ds) > 0 {
		drivesRunning(ds, l, busy && err == nil)
		report.Drives = ds
	}
	report.Trends = trends(s.ledger.Rows, now, s.location(), s.window)
	// Asked under the gate a tracker read answers to, so --no-tracker and a
	// drive's render leave the forge alone. A drive's render judges the
	// register as the drive last read it; with no rows read, the error says
	// so, because an empty list would otherwise read as nothing to triage.
	index := gh.Index{OK: true}
	switch last, read := activeDrive.Load().lastRegister(); {
	case (tracker && !s.offline) || s.cfg.RegisterBackend() != config.BackendGitHub:
		if index = s.register(); !index.OK {
			report.FindingsError = index.Reason().Error()
		}
	case read:
		index = last
	case activeDrive.Load() != nil:
		report.FindingsError = "the drive has not read the follow-up list yet, so no follow-up is listed"
	default:
		report.FindingsError = "the follow-up list was not read (--no-tracker), so no follow-up is listed"
	}
	report.FindingsNeedingTriage = s.findingsNeedingTriage(index, s.heldQueue())
	localize(rows, &report, s.location())
	report.all = rows
	out := rows
	if scoped {
		out = sevRows(out)
		if !statusAll {
			out = openRows(out)
		}
	}
	_ = s.attachPRs(out, func(Row) bool { return true }, tracker)
	report.Items = out
	if report.Items == nil {
		report.Items = []Row{}
	}
	return report
}

// localize rewrites every time in the report into loc, offset kept. The ledger
// stores each row in its writer's zone, so a report read as written shows
// -07:00 beside Z for two rows a minute apart. A value that does not parse is
// left as it is rather than dropped.
func localize(rows []Row, report *StatusReport, loc *time.Location) {
	in := func(v *string) {
		if t, err := time.Parse(time.RFC3339, *v); err == nil {
			*v = t.In(loc).Format(time.RFC3339)
		}
	}
	for i := range rows {
		in(&rows[i].Started)
		in(&rows[i].Finished)
		in(&rows[i].StageStarted)
		in(&rows[i].ETA)
	}
	if report.Lease != nil {
		in(&report.Lease.Started)
	}
	for i := range report.Drives {
		in(&report.Drives[i].Started)
		in(&report.Drives[i].Ended)
	}
}

// tableTime is an RFC 3339 time as the table prints it, in loc and named by the
// zone's abbreviation. A value that does not parse prints as written.
func tableTime(v string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return v
	}
	return t.In(loc).Format("2006-01-02 15:04:05 MST")
}

// renderTemplate executes a template over the report. The data is the report
// as --json encodes it, decoded back into maps, so a template names the same
// fields a JSON consumer does (.items, .id, .stage_started) and never the Go
// ones; now, zone, project, version, refresh, index and attention are added beside
// them.
//
// ref is a file path or a builtin: name. The template is named by it, which is
// what text/template puts in every parse and execution error beside the line
// number. builtin:html executes through html/template, so a title holding
// markup arrives as text.
func (s *session) renderTemplate(ref string, report StatusReport, now time.Time) ([]byte, error) {
	raw, err := json.Marshal(struct {
		StatusReport
		Attention []Attention `json:"attention"`
	}{report, s.attention(report.all)})
	if err != nil {
		return nil, err
	}
	loc := s.location()
	return s.execTemplate(ref, raw, func(data map[string]any) {
		data["zone"] = loc.String()
		if loc == time.Local {
			data["zone"], _ = now.In(loc).Zone()
		}
		data["project"] = s.cfg.Project
		data["version"] = version
		data["refresh"] = s.cfg.Status.RefreshSeconds()
		data["index"] = s.cfg.Status.Index
	}, now)
}

// execTemplate executes the template ref over raw, a JSON object decoded back
// into maps, with now added to it and whatever extend adds beside.
func (s *session) execTemplate(ref string, raw []byte, extend func(map[string]any), now time.Time) ([]byte, error) {
	src, isHTML, err := templateSource(ref)
	if err != nil {
		return nil, err
	}
	loc := s.location()
	now = now.In(loc)
	funcs := s.templateFuncs(now, loc)
	var exec func(io.Writer, any) error
	if isHTML {
		tmpl, err := htmltemplate.New(ref).Funcs(funcs).Parse(src)
		if err != nil {
			return nil, err
		}
		exec = tmpl.Execute
	} else {
		tmpl, err := template.New(ref).Funcs(funcs).Parse(src)
		if err != nil {
			return nil, err
		}
		exec = tmpl.Execute
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return nil, err
	}
	data["now"] = now.Format(time.RFC3339)
	extend(data)
	var out bytes.Buffer
	if err := exec(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// templateSource reads the template ref names, and whether it renders HTML.
func templateSource(ref string) (string, bool, error) {
	if name, ok := dashboard.Builtin(ref); ok {
		src, err := dashboard.Source(name)
		return src, dashboard.HTML(name), err
	}
	src, err := os.ReadFile(ref) // #nosec G304 -- the operator named the template
	if err != nil {
		return "", false, fmt.Errorf("read template: %w", err)
	}
	return string(src), false, nil
}

// templateRef resolves a [status] render template against the project root,
// leaving a builtin: name as it is.
func (s *session) templateRef(t string) string {
	if _, ok := dashboard.Builtin(t); ok {
		return t
	}
	return s.cfg.Abs(t)
}

// Attention is one thing in a status view waiting on a person, and the command
// that answers it.
type Attention struct {
	// Kind is parked, needs_operator, scaffold, unaudited or follow-up.
	Kind string `json:"kind"`
	// ID is the item, or the register issue's number on a follow-up, and absent on
	// an entry saying a source could not be read.
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason"`
	// Since is when the wait began, absent where nothing records it: a scaffold
	// has no ledger row, and the register keeps no date drover reads.
	Since   string `json:"since,omitempty"`
	Command string `json:"command"`
}

// attention is every entry waiting on a person: each row's park, scaffold or
// missing audit in backlog order, then each register row nothing scheduled
// will carry. It reads the ledger and the backlog status read, and the register
// unaudited and follow-ups read; a register that could not be read is an entry saying so,
// because an empty list would claim nobody filed anything.
func (s *session) attention(rows []Row) []Attention {
	out := []Attention{}
	loc := s.location()
	for _, r := range rows {
		switch {
		case r.Park != "":
			a := Attention{Kind: "parked", ID: r.ID, Reason: join(": ", r.Park, r.Detail)}
			if pk, ok := s.ledger.Parked(r.ID); ok {
				a.Since = pk.TS
				if t, err := time.Parse(time.RFC3339, pk.TS); err == nil {
					a.Since = t.In(loc).Format(time.RFC3339)
				}
			}
			key := parkAction(DriveItem{Park: r.Park, Detail: r.Detail})
			if p, _ := s.cfg.Policy(key); key != "" && p == polOperator {
				a.Kind = "needs_operator"
			}
			a.Command = "drover drive " + r.ID
			if op := action(key).operator; op != "" {
				a.Command = strings.ReplaceAll(op, "{id}", r.ID)
			}
			out = append(out, a)
		case r.State == stateScaffold && r.Requirements > 0:
			out = append(out, Attention{Kind: "scaffold", ID: r.ID,
				Reason:  fmt.Sprintf("%d %s drafted, not accepted", r.Requirements, plural(r.Requirements, "requirement", "requirements")),
				Command: strings.ReplaceAll(action("accept").operator, "{id}", r.ID)})
		case r.State == stateScaffold:
			out = append(out, Attention{Kind: "scaffold", ID: r.ID, Reason: "requirements unwritten",
				Command: strings.ReplaceAll(action("write_scaffold_requirements").operator, "{id}", r.ID)})
		case r.State == stateDone && r.Level == "":
			out = append(out, Attention{Kind: "unaudited", ID: r.ID, Reason: "merged, and nobody has audited it",
				Since: r.Finished, Command: "drover audit " + r.ID + ` verified --code-by agent "<what you re-checked>"`})
		}
	}
	index := gh.Index{Err: errors.New("the forge was not asked (--no-tracker, or a drive's render)")}
	if !s.offline || s.cfg.RegisterBackend() != config.BackendGitHub {
		index = s.register()
	}
	if !index.OK {
		return append(out, Attention{Kind: "follow-up", Reason: fmt.Sprintf("the follow-up list could not be read: %v", index.Reason()),
			Command: "drover follow-ups"})
	}
	for _, is := range index.Issues {
		n := strconv.Itoa(is.Number)
		switch {
		case len(is.Owners) == 0:
			out = append(out, Attention{Kind: "follow-up", ID: n, Reason: is.Title + " (unowned: no item owns it)",
				Command: "give #" + n + " an owner (a sub-issue of that item's issue, or an \"" + gh.OwnerLine("<id>") + "\" line), or close it"})
		case s.orphaned(is.Owners):
			out = append(out, Attention{Kind: "follow-up", ID: n,
				Reason:  is.Title + " (orphaned: " + strings.Join(is.Owners, " ") + " merged)",
				Command: "move #" + n + " to an open owner, or close it"})
		}
	}
	return out
}

// markupEscaper keeps text from opening a tag or an entity in Markdown, which
// renders a quote as typed: html's &#39; reaches an Obsidian reader literally.
var markupEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// templateFuncs is what a status template can compute. Each takes its time and
// id arguments as any, because the data is decoded JSON: a field the report
// omits arrives as nil, and every function answers that with "" rather than
// failing the render.
func (s *session) templateFuncs(now time.Time, loc *time.Location) template.FuncMap {
	return template.FuncMap{
		"class":   s.class,
		"groups":  s.groups,
		"done":    doneNewest,
		"held":    heldAt,
		"pausing": pausingCount,
		"ago": func(v any) string {
			t, ok := rfc3339(v)
			if !ok {
				return ""
			}
			if d := now.Sub(t); d >= 0 {
				return span(d) + " ago"
			}
			return "in " + span(t.Sub(now))
		},
		"dur": func(from, to any) string {
			a, ok1 := rfc3339(from)
			b, ok2 := rfc3339(to)
			if !ok1 || !ok2 {
				return ""
			}
			d := b.Sub(a).Abs()
			h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
			switch {
			case h >= 24:
				return fmt.Sprintf("%dd %dh", h/24, h%24)
			case h > 0:
				return fmt.Sprintf("%dh %dm", h, m)
			}
			return fmt.Sprintf("%dm", m)
		},
		"time": func(layout string, v any) string {
			t, ok := rfc3339(v)
			if !ok {
				return ""
			}
			return t.In(loc).Format(layout)
		},
		"anchor": func(v any) string {
			return strings.Map(func(r rune) rune {
				switch {
				case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
					return r
				case r >= 'A' && r <= 'Z':
					return r + ('a' - 'A')
				}
				return '-'
			}, str(v))
		},
		"pr":    func(v any) string { return s.forgeLink("pull", v) },
		"issue": func(v any) string { return s.forgeLink("issues", v) },
		"md": func(v any) string {
			return strings.NewReplacer(`\`, `\\`, "|", `\|`, "\r\n", " ", "\n", " ", "\r", " ").Replace(str(v))
		},
		"html":  func(v any) string { return template.HTMLEscapeString(str(v)) },
		"esc":   func(v any) string { return markupEscaper.Replace(str(v)) },
		"spark": spark,
		"secs": func(v any) string {
			f, ok := number(v)
			if !ok {
				return ""
			}
			return clock(time.Duration(f * float64(time.Second)))
		},
		"pct": func(v any) string {
			f, ok := number(v)
			if !ok {
				return ""
			}
			return strconv.FormatFloat(f*100, 'f', 0, 64) + "%"
		},
		"num": func(v any) string {
			f, ok := number(v)
			if !ok {
				return ""
			}
			return strconv.FormatFloat(f, 'f', -1, 64)
		},
	}
}

// number is a template argument as a float, false for an absent field or
// anything that is not a number.
func number(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(str(v), 64)
	return f, err == nil
}

// clock is a duration in its two largest units: 42s, 14m 5s, 2h 5m, 3d 2h.
func clock(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

// Sparkline geometry, in the SVG's own units. The page sizes the element; the
// viewBox keeps the line's proportions whatever it is drawn at.
const (
	sparkWidth  = 120.0
	sparkHeight = 24.0
	sparkPad    = 2.0
)

// spark draws one metric across the trend days as an inline SVG sparkline:
// days is .trends.days, metric a TrendSet key and field the value inside it
// (value, median, p90, n). A day the metric is null on is a gap in the line,
// never a point at zero, and a run of one day is a dot. The markup is built
// from numbers alone, so nothing a project wrote reaches it, and it needs no
// script and fetches nothing.
func spark(days any, metric, field string) htmltemplate.HTML {
	list, _ := days.([]any)
	vals := make([]*float64, len(list))
	top := 0.0
	for i, d := range list {
		m, _ := d.(map[string]any)
		inner, _ := m[metric].(map[string]any)
		if f, ok := number(inner[field]); ok {
			vals[i] = &f
			top = max(top, f)
		}
	}
	step := 0.0
	if len(vals) > 1 {
		step = (sparkWidth - 2*sparkPad) / float64(len(vals)-1)
	}
	at := func(i int) (float64, float64) {
		y := sparkHeight - sparkPad
		if top > 0 {
			y -= *vals[i] / top * (sparkHeight - 2*sparkPad)
		}
		return sparkPad + float64(i)*step, y
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %g %g" width="%g" height="%g" role="img" aria-label="%s %s by day">`,
		sparkWidth, sparkHeight, sparkWidth, sparkHeight, template.HTMLEscapeString(metric), template.HTMLEscapeString(field))
	for i := 0; i < len(vals); {
		if vals[i] == nil {
			i++
			continue
		}
		j := i
		for j < len(vals) && vals[j] != nil {
			j++
		}
		if j-i == 1 {
			x, y := at(i)
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="1.5" />`, x, y)
		} else {
			pts := make([]string, 0, j-i)
			for k := i; k < j; k++ {
				x, y := at(k)
				pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
			}
			fmt.Fprintf(&b, `<polyline points="%s" />`, strings.Join(pts, " "))
		}
		i = j
	}
	b.WriteString(`</svg>`)
	// #nosec G203 -- every value in the markup is a number or an escaped key
	return htmltemplate.HTML(b.String())
}

// heldAt is the stage a paused drive holds id before, or "" when the lease
// holds nothing for it. A paused drive keeps a held item in its running list,
// because the item still belongs to the drive, but no session is working on
// it: rendering it as running reports work that is not happening.
func heldAt(holder, id any) string {
	l, ok := holder.(map[string]any)
	if !ok {
		return ""
	}
	for _, h := range objects(l["holds"]) {
		if str(h["id"]) == str(id) {
			return str(h["at"])
		}
	}
	return ""
}

// runningWord is what an item the lease runs is doing: running, pausing while
// a paused drive lets it finish the stage it is inside, or paused once the
// drive holds it at a stage boundary. The lease writes a hold only at that
// boundary, so a paused drive with an unheld running item has not gone quiet.
func runningWord(holder, id any) string {
	l, _ := holder.(map[string]any)
	switch {
	case heldAt(holder, id) != "":
		return "paused"
	case l["paused"] == true:
		return "pausing"
	default:
		return "running"
	}
}

// pausingCount is how many running items a paused drive is still waiting on,
// 0 when the drive is not paused or every running item already holds.
func pausingCount(holder any) int {
	l, ok := holder.(map[string]any)
	if !ok || l["paused"] != true {
		return 0
	}
	n := 0
	for _, id := range objectsOrStrings(l["running"]) {
		if heldAt(holder, id) == "" {
			n++
		}
	}
	return n
}

func objectsOrStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, str(x))
	}
	return out
}

// class names the state, park reason or audit level v in a fixed vocabulary a
// stylesheet or a callout can key on. A value status never reports as a state
// or level is a park reason, the one open vocabulary of the three, and the
// supervise catalog decides whether that park waits on an operator. An absent
// value is "", so a template never invents a class for a field the item lacks.
func (s *session) class(v any) string {
	switch k := str(v); k {
	case "":
		return ""
	case stateReady:
		return "ready"
	case stateRan:
		return "running"
	case "pausing":
		return "pausing"
	case "paused":
		return "paused"
	case stateBlocked:
		return "blocked"
	case stateScaffold:
		return "scaffold"
	case stateDone, ledger.LevelClaimed, ledger.LevelReviewed, ledger.LevelVerified:
		return "done"
	case stateBroken:
		return "broken"
	case "unaudited", "UNAUDITED":
		return "unaudited"
	case "needs_operator":
		return "needs-operator"
	default:
		if p, _ := s.cfg.Policy(parkActions[k]); parkActions[k] != "" && p == polOperator {
			return "needs-operator"
		}
		return "parked"
	}
}

// stateOrder is the order groups lists an item's states in: what waits on a
// person first, then what is moving, then what cannot move yet.
var stateOrder = []string{"needs-operator", "parked", "broken", "running", "pausing", "paused", stateRan, stateReady, stateBlocked, stateScaffold}

// groups is the open items, by the drive that last ran them and then by state,
// for a template that cannot build a nested list itself. Drives come in the
// order drives lists them, newest first, and the items no listed drive ran come
// last under an empty drive. Each state group carries the word to print and
// the class to style it with, which differ: ran is classed running. An item the
// lease names is running whatever its backlog state, so this list and the
// lease's own agree. A parked item is grouped by its park's class, so it sits
// with the other parks rather than among the ready items it nominally is.
func (s *session) groups(items, drives, holder any) []map[string]any {
	var order []string
	driveLabels := map[string]string{}
	for _, d := range objects(drives) {
		id := str(d["id"])
		if _, seen := driveLabels[id]; !seen {
			order = append(order, id)
		}
		driveLabels[id] = str(d["label"])
	}
	order = append(order, "")
	var running []string
	if l, ok := holder.(map[string]any); ok {
		list, _ := l["running"].([]any)
		for _, id := range list {
			running = append(running, str(id))
		}
	}
	classOf := map[string]string{"running": "running", "pausing": "pausing", "paused": "paused"}
	byDrive := map[string]map[string][]any{}
	for _, it := range objects(items) {
		if str(it["state"]) == stateDone {
			continue
		}
		ran := str(it["drive"])
		if _, ok := driveLabels[ran]; !ok {
			ran = ""
		}
		var word string
		switch {
		case slices.Contains(running, str(it["id"])):
			word = runningWord(holder, it["id"])
		case str(it["park"]) != "":
			word = s.class(it["park"])
			classOf[word] = word
		default:
			word = str(it["state"])
			classOf[word] = s.class(word)
		}
		if byDrive[ran] == nil {
			byDrive[ran] = map[string][]any{}
		}
		byDrive[ran][word] = append(byDrive[ran][word], it)
	}
	out := []map[string]any{}
	for _, d := range order {
		words := byDrive[d]
		if len(words) == 0 {
			continue
		}
		var rest []string
		for w := range words {
			if !slices.Contains(stateOrder, w) {
				rest = append(rest, w)
			}
		}
		slices.Sort(rest)
		var states []map[string]any
		for _, w := range append(slices.Clone(stateOrder), rest...) {
			if len(words[w]) > 0 {
				states = append(states, map[string]any{"state": w, "class": classOf[w], "items": words[w]})
			}
		}
		out = append(out, map[string]any{"drive": d, "label": driveLabels[d], "states": states})
	}
	return out
}

// doneNewest is the merged items, the most recently finished first. An item
// with no recorded finish sorts last, in the order it came.
func doneNewest(items any) []any {
	type row struct {
		obj map[string]any
		at  time.Time
	}
	var rows []row
	for _, it := range objects(items) {
		if str(it["state"]) != stateDone {
			continue
		}
		at, _ := rfc3339(it["finished"])
		rows = append(rows, row{it, at})
	}
	slices.SortStableFunc(rows, func(a, b row) int { return b.at.Compare(a.at) })
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r.obj
	}
	return out
}

// objects is a decoded JSON array's objects, skipping anything else in it.
func objects(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// forgeLink is the web address of pull request or issue n, or "" when the
// forge is unknown. A command told to ask the forge nothing builds a link only
// from an address it already holds, and an issue in a register that is not the
// forge's has no page there.
func (s *session) forgeLink(kind string, v any) string {
	n, err := strconv.Atoi(strings.TrimPrefix(str(v), "#"))
	if err != nil || n <= 0 {
		return ""
	}
	if kind == "issues" && s.cfg.RegisterBackend() != config.BackendGitHub {
		return ""
	}
	base := ""
	switch {
	case s.forgeURL != nil:
		base = *s.forgeURL
	case !s.offline:
		base = s.repoURL()
	}
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%d", base, kind, n)
}

// span is a duration in its largest whole unit: 14m, 2h, 3d.
func span(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

func rfc3339(v any) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, str(v))
	return t, err == nil
}

// str is a template argument as text, "" for an absent field. A JSON number
// arrives as json.Number, which prints as the number.
func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// expandHome resolves a leading ~/ against $HOME, the way a shell would have
// had the path not been quoted.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home := os.Getenv("HOME"); home != "" {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// writeOutput replaces path with data through a temp file in the same
// directory and a rename, so a reader of path sees the old file or the new one
// and never half of either. A file already there is refused unless force says
// replacing it is the point.
func writeOutput(path string, data []byte, force bool) error {
	if !force {
		if _, err := os.Lstat(path); err == nil {
			return Refusal{fmt.Errorf("%s already exists: pass --force to replace it", path)}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// renderAll renders every [status] render pair once, and returns one error per
// pair, nil where it rendered. A failure stops nothing: the next pair renders
// regardless.
func (s *session) renderAll(targets []config.Render, tracker bool) []error {
	now := time.Now()
	report := s.statusReport(false, tracker, now)
	errs := make([]error, len(targets))
	for i, t := range targets {
		out, err := s.renderTemplate(s.templateRef(t.Template), report, now)
		if err == nil {
			err = writeOutput(s.cfg.Abs(t.Path), out, true)
		}
		if err != nil {
			errs[i] = fmt.Errorf("render %s: %w", t.Path, err)
		}
	}
	return errs
}

// driveRenderer keeps a drive's [status] render files current. One per drive,
// called from every goroutine that appends a row, so renders are serialized
// and a slow one delays the next rather than racing it to the rename.
type driveRenderer struct {
	mu      sync.Mutex
	targets []config.Render
	warned  bool
}

// activeRender is the running drive's renderer, nil everywhere else. Package
// state for the reason held is: the rows a drive appends are written by forked
// sessions and by finish's own RunE, and every one of them goes through
// appended.
var activeRender atomic.Pointer[driveRenderer]

// appended is called after every ledger row this process writes. Outside a
// drive with [status] render configured it does nothing.
func appended() {
	if r := activeRender.Load(); r != nil {
		r.render()
	}
}

// render reads the project afresh and renders every pair. A failure is
// reported once per drive and goes no further: a status file is a view of the
// drive, and a view that could stop what it is showing has its priorities
// backwards.
func (r *driveRenderer) render() {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := open()
	var errs []error
	if err != nil {
		errs = []error{fmt.Errorf("status render: %w", err)}
	} else {
		s.offline = true
		errs = s.renderAll(r.targets, false)
	}
	if err := errors.Join(errs...); err != nil && !r.warned {
		r.warned = true
		fmt.Fprintf(os.Stderr, "· %v; the drive goes on, and further render failures this drive are not reported\n", err)
	}
}
