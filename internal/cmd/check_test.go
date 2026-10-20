package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Both counts agree in number, including the backlog of one that every project
// starts as.
func TestCheckLineAgreesWithItsCounts(t *testing.T) {
	for _, c := range []struct {
		bodies, rows int
		want         string
	}{
		{1, 0, "ok — 1 body parses, 0 ledger rows, no orphans or stale paths"},
		{14, 3, "ok — 14 bodies parse, 3 ledger rows, no orphans or stale paths"},
		{1, 1, "ok — 1 body parses, 1 ledger row, no orphans or stale paths"},
		{0, 0, "ok — 0 bodies parse, 0 ledger rows, no orphans or stale paths"},
	} {
		if got := checkLine(c.bodies, c.rows); got != c.want {
			t.Errorf("checkLine(%d, %d) = %q, want %q", c.bodies, c.rows, got, c.want)
		}
	}
}

// A body on disk that will not parse is not a body nobody wrote. Sending the
// reader to write the item produces a second file under the same id.
func TestOrphanRowSeparatesUnreadableFromAbsent(t *testing.T) {
	dir := shelfProject(t, map[string]string{
		"B01-live.md":    body("Live", ""),
		"B09-revived.md": "no frontmatter here at all\n",
	}, nil)
	rows := `{"id":"B9","merged":true,"implementer":"upstream","date":"2026-01-01"}
{"id":"B12","merged":true,"implementer":"upstream","date":"2026-01-02"}
`
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}

	got := findings(openIn(t, dir).checkReport(), "ORPHAN ROW")
	if len(got) != 2 {
		t.Fatalf("ORPHAN ROW findings = %d, want 2: %+v", len(got), got)
	}
	byID := map[string]Finding{}
	for _, f := range got {
		byID[f.ID] = f
	}
	broken, absent := byID["B9"], byID["B12"]
	if !strings.Contains(broken.Message, "will not parse") {
		t.Errorf("B9's body is on disk, reported as %q", broken.Message)
	}
	if broken.Path != "work/B09-revived.md" || !strings.Contains(broken.Detail, "frontmatter") {
		t.Errorf("B9 does not name the file to repair: %q / %q", broken.Path, broken.Detail)
	}
	if !strings.Contains(absent.Message, "has no body") || absent.Path != "" {
		t.Errorf("B12 was never written, reported as %q / %q", absent.Message, absent.Path)
	}
}

// Three ways a dep can fail to resolve, three fixes. All stay findings.
func TestDanglingSeparatesUnreadableShelvedAndAbsent(t *testing.T) {
	s := openIn(t, shelfProject(t,
		map[string]string{
			"B01-needs-broken.md":  body("Needs an unreadable dep", "F1"),
			"B02-needs-nothing.md": body("Needs a dep nobody wrote", "F5"),
			"B03-needs-shelved.md": body("Needs a retired dep", "F9"),
			"F01-broken.md":        "no frontmatter here at all\n",
		},
		map[string]string{"F09-retired.md": ""}))

	got := findings(s.checkReport(), "MISSING DEP")
	if len(got) != 3 {
		t.Fatalf("MISSING DEP findings = %d, want 3: %+v", len(got), got)
	}
	for i, want := range []struct{ phrase, path string }{
		{"will not parse", "work/F01-broken.md"},
		{"has no body", ""},
		{"is shelved", "shelved/F09-retired.md"},
	} {
		if !strings.Contains(got[i].Message, want.phrase) {
			t.Errorf("finding %d = %q, want %q", i, got[i].Message, want.phrase)
		}
		if got[i].Path != want.path {
			t.Errorf("finding %d path = %q, want %q", i, got[i].Path, want.path)
		}
	}
}

// An unreadable body spends its id exactly as a parsed one does: the id was
// authored in the filename, and SHELF CLASH cannot catch the reissue because
// both files land in the same directory.
func TestNextIDAllocatesAboveAnUnreadableBody(t *testing.T) {
	s := openIn(t, shelfProject(t, map[string]string{
		"B01-live.md":   body("A bug", ""),
		"F07-broken.md": "no frontmatter here at all\n",
	}, nil))

	if got := nextID(t, s, "F"); got != "F8" {
		t.Errorf("nextID(F) = %s, want F8", got)
	}
	if got := nextID(t, s, "B"); got != "B2" {
		t.Errorf("nextID(B) = %s, want B2", got)
	}
}

// The failure result agrees for the same reason the success one does, and a
// single finding is the common case — check stops at the first thing wrong.
func TestFindingsLineAgreesWithItsCount(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{
		{1, "1 finding"},
		{2, "2 findings"},
		{7, "7 findings"},
	} {
		if got := findingsLine(c.n); got != c.want {
			t.Errorf("findingsLine(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// shapeProject writes one fixture whose only variable is the [frontmatter]
// shape table, so anything that differs between two of them is caused by the
// table and nothing else. refs carries the idlist rather than deps, because a
// malformed dep is MISSING DEP before it is anything else and the fixture would
// then be proving two things at once.
// emptyShape declares the table for a fixture that supplies none, so a case
// about shapes is not also a case about STALE CONFIG. A fixture passing its own
// [frontmatter.shape] gets nothing here: the same path twice is a parse error.
func emptyShape(shapes string) string {
	if shapes == "" {
		return "shape = {}"
	}
	return ""
}

func shapeProject(t *testing.T, shapes string) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"
reports = "reports"

[ids]
namespaces = { B = "bug", F = "feature" }

[frontmatter]
required = ["title", "deps"]
optional = ["issue", "severity", "refs"]
` + emptyShape(shapes) + `

[verify]
default = "true"

# The rest of what drover init writes. Declared so these fixtures assert what
# they are about: a config omitting one is STALE CONFIG's finding, and it would
# fire in every case below that counts findings. shape is empty on purpose —
# shaping a key the lists above do not declare is UNDECLARED KEY's finding. enabled
# is true because a scratch directory is not a work tree and these tests run
# with branch automation on, which is what an unset key already meant here.

[intake]
stop_marker = "STOP: requirements not yet written"

[git]
enabled = true
` + shapes
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		"B01-passes.md": shaped("Every value fits", "46", "M", "B1 F2"),
		"B02-fails.md":  shaped("Every value misses", "banana", "Z", "B0004 nonsense"),
		"B03-empty.md":  shaped("Every value absent", "", "", "   "),
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, "work", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func shaped(title, issue, severity, refs string) string {
	return fmt.Sprintf("---\ntitle: %s\ndeps: \nissue: %s\nseverity: %s\nrefs: %s\n---\n\nRequirements:\n\n1. Do it.\n",
		title, issue, severity, refs)
}

const shapeTable = `
[frontmatter.shape]
issue    = "int"
severity = "enum:C,H,M,L"
refs     = "idlist"
`

// One passing and one failing value per shape, and the empty value that must
// pass all three: [frontmatter] required is what separates presence from
// content, and a shape that also demanded a value would move that line.
func TestBadValueNamesTheItemKeyValueAndShape(t *testing.T) {
	got := findings(openIn(t, shapeProject(t, shapeTable)).checkReport(), "BAD VALUE")
	if len(got) != 3 {
		t.Fatalf("BAD VALUE findings = %d, want 3: %+v", len(got), got)
	}
	for i, want := range []struct{ message, detail string }{
		{`B2 declares issue = "banana", which is not int`, "whole number"},
		{`B2 declares refs = "B0004 nonsense", which is not idlist`, "zero-padded"},
		{`B2 declares severity = "Z", which is not enum:C,H,M,L`, "not one of C, H, M, L"},
	} {
		if got[i].Message != want.message {
			t.Errorf("finding %d = %q, want %q", i, got[i].Message, want.message)
		}
		if !strings.Contains(got[i].Detail, want.detail) {
			t.Errorf("finding %d detail = %q, want %q", i, got[i].Detail, want.detail)
		}
		if got[i].ID != "B2" || got[i].Path != "work/B02-fails.md" {
			t.Errorf("finding %d does not name the body: %q / %q", i, got[i].ID, got[i].Path)
		}
	}
}

// The additive promise, and nothing else proves it: the same three bodies under
// the same config less the table are a project with nothing wrong with it. A
// value that fails is a finding against the project, so the item stays in the
// backlog either way and BAD VALUE is the only difference between the two runs.
func TestNoShapeTableIsNoFindings(t *testing.T) {
	bare := openIn(t, shapeProject(t, "")).checkReport()
	if len(bare.Findings) != 0 {
		t.Fatalf("a project declaring no shape reports %d findings: %+v", len(bare.Findings), bare.Findings)
	}
	shaped := openIn(t, shapeProject(t, shapeTable)).checkReport()
	if shaped.Bodies != bare.Bodies {
		t.Errorf("shaped backlog holds %d bodies, bare one holds %d", shaped.Bodies, bare.Bodies)
	}
	for _, f := range shaped.Findings {
		if f.Kind != "BAD VALUE" {
			t.Errorf("the table added a %s finding: %s", f.Kind, f.Message)
		}
	}
}

// A shape keyed on a key the schema does not declare fails the way [severity]
// naming one does: nothing can carry the key, so nothing is ever checked.
func TestShapeOnAnUndeclaredKeyIsAFinding(t *testing.T) {
	got := findings(openIn(t, shapeProject(t, "\n[frontmatter.shape]\nissu = \"int\"\n")).checkReport(), "UNDECLARED KEY")
	if len(got) != 1 {
		t.Fatalf("UNDECLARED KEY findings = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, `"issu"`) || !strings.Contains(got[0].Message, "[frontmatter]") {
		t.Errorf("finding does not name the key or the table: %q", got[0].Message)
	}
	if got[0].Detail == "" {
		t.Error("finding carries no fix")
	}
}

// Merged is the only state that silences NO REQUIREMENTS. B1 and B2 both lack a
// list and differ only in the ledger row, so a fix that skipped by anything
// else (the title, the file, the run count) fails one of the two.
func TestNoChecklistSkipsMergedItemsOnly(t *testing.T) {
	dir := registerProject(t, noProject)
	prose := func(title string) string {
		return fmt.Sprintf("---\ntitle: %s\ndeps: \n---\n\nThe evidence below is the contract.\n", title)
	}
	for name, content := range map[string]string{
		"B01-merged.md": prose("Merged, prose only"),
		"B02-open.md":   prose("Open, prose only"),
		"B03-listed.md": body("Open, numbered", ""),
	} {
		if err := os.WriteFile(filepath.Join(dir, "work", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	row := `{"id":"B1","merged":true,"implementer":"upstream","date":"2026-01-01"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}

	got := findings(openIn(t, dir).checkReport(), "NO REQUIREMENTS")
	if len(got) != 1 || got[0].ID != "B2" {
		t.Fatalf("NO REQUIREMENTS findings = %+v, want exactly one, for B2", got)
	}
}

// dormancyProject is registerProject plus the two items every register fixture
// below routes to. B1 merges and B2 does not, so what separates two rows is the
// state of the owner its label names and nothing else.
func dormancyProject(t *testing.T, conf string, merged ...string) string {
	t.Helper()
	dir := registerProject(t, conf)
	for name, title := range map[string]string{
		"B01-merged.md": "Merged owner",
		"B02-open.md":   "Open owner",
	} {
		if err := os.WriteFile(filepath.Join(dir, "work", name), []byte(body(title, "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rows := ""
	for _, id := range merged {
		rows += fmt.Sprintf("{\"id\":%q,\"merged\":true,\"implementer\":\"upstream\",\"date\":\"2026-01-01\"}\n", id)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fileRegister writes the append-only source the file backend reads, at the
// path applyDefaults gives it.
func fileRegister(t *testing.T, dir string, lines ...string) {
	t.Helper()
	path := filepath.Join(dir, ".drover", "register.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The register stanzas a check fixture picks between.
const (
	ghProject   = "[github]\nrepo = \"owner/name\"\n"
	fileProject = "[register]\nbackend = \"file\"\n"
	noProject   = "[register]\nbackend = \"none\"\n"
)

// This repository's own register is empty, so `drover check` here exits 0 with
// and without the finding. These cells are the only thing that will ever
// exercise it: every one of them is a state a project reaches on an ordinary
// day, and three would report a project clean off a register nobody read.
func TestCheckDormantSeparatesEveryRegisterState(t *testing.T) {
	for _, c := range []struct {
		name    string
		conf    string
		page    string   // the gh page; empty leaves no gh on PATH at all
		rows    []string // the file backend's source
		merged  []string
		dormant int
		read    bool
		state   string
		note    string
		// bad marks the cell that also fails BAD REGISTER, which is a finding
		// and so changes the exit code the operator sees.
		bad bool
	}{
		{
			name: "github, readable, an orphaned row", conf: ghProject, page: reachability,
			merged: []string{"B1"}, dormant: 1, read: true, state: registerRead,
		},
		{
			name: "github, readable, nothing orphaned", conf: ghProject, page: reachability,
			read: true, state: registerRead,
		},
		{
			name: "github, unreachable", conf: ghProject,
			merged: []string{"B1"}, read: false, state: registerUnreachable, note: "gh is not on PATH",
		},
		{
			// The count is a floor, so the finding it produces is one too, and
			// the note is what says so.
			name: "github, truncated, an orphaned row", conf: ghProject + "page_limit = 1\n", page: reachability,
			merged: []string{"B1"}, dormant: 1, read: true, state: registerTruncated, note: "1-issue page limit",
		},
		{
			// The cell that would report a project clean: no finding, off a
			// read that stopped after one issue.
			name: "github, truncated, nothing orphaned", conf: ghProject + "page_limit = 1\n", page: reachability,
			read: true, state: registerTruncated, note: "1-issue page limit",
		},
		{
			name: "file, readable, an orphaned row", conf: fileProject,
			rows: []string{
				`{"number":1,"title":"owned by a merged item","owners":["B1"],"state":"open","date":"2026-01-01"}`,
				`{"number":2,"title":"owned by an open item","owners":["B2"],"state":"open","date":"2026-01-01"}`,
			},
			merged: []string{"B1"}, dormant: 1, read: true, state: registerRead,
		},
		{
			name: "file, readable, nothing orphaned", conf: fileProject,
			rows: []string{
				`{"number":2,"title":"owned by an open item","owners":["B2"],"state":"open","date":"2026-01-01"}`,
				`{"number":3,"title":"nobody owns this","state":"open","date":"2026-01-01"}`,
			},
			merged: []string{"B1"}, read: true, state: registerRead,
		},
		{
			name: "file, unreachable", conf: fileProject,
			rows: []string{
				`{"number":1,"title":"fine","owners":["B1"],"state":"open","date":"2026-01-01"}`,
				`{"number":2,`,
			},
			merged: []string{"B1"}, read: false, state: registerUnreachable,
			note: "BAD REGISTER above says why", bad: true,
		},
		{
			// Unpaged, so the page limit says nothing about it: a file holding
			// exactly page_limit rows is a whole read.
			name: "file, a whole read at the page limit", conf: fileProject + "\n[github]\npage_limit = 1\n",
			rows: []string{
				`{"number":1,"title":"owned by a merged item","owners":["B1"],"state":"open","date":"2026-01-01"}`,
			},
			merged: []string{"B1"}, dormant: 1, read: true, state: registerRead,
		},
		{
			// Declared absent: no finding, and nothing said about it either.
			// Anything else fails CI for every project that never wanted one.
			name: "none, silent", conf: noProject,
			merged: []string{"B1"}, read: true, state: registerOff,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := dormancyProject(t, c.conf, c.merged...)
			bin := stubPATH(t)
			if c.page != "" {
				stubGHIssues(t, bin, c.page)
			}
			if c.rows != nil {
				fileRegister(t, dir, c.rows...)
			}

			rep := openIn(t, dir).checkReport()
			got := findings(rep, "ORPHANED FOLLOW-UP")
			if len(got) != min(c.dormant, 1) {
				t.Fatalf("ORPHANED FOLLOW-UP findings = %d, want %d: %+v", len(got), min(c.dormant, 1), got)
			}
			if c.dormant > 0 {
				if !strings.Contains(got[0].Message, fmt.Sprintf("%d open follow-up", c.dormant)) {
					t.Errorf("finding does not carry the count: %q", got[0].Message)
				}
				if !strings.Contains(got[0].Detail, "deps:") {
					t.Errorf("finding does not name what discharges it: %q", got[0].Detail)
				}
			}
			if rep.RegisterState != c.state {
				t.Errorf("register_state = %q, want %q", rep.RegisterState, c.state)
			}
			// register_read stays exactly this derivation, so a consumer
			// that reads it rather than register_state gets the same answer.
			if rep.RegisterRead != (rep.RegisterState != registerUnreachable) {
				t.Errorf("register_read = %v under state %q; it must stay state != %q",
					rep.RegisterRead, rep.RegisterState, registerUnreachable)
			}
			if rep.RegisterRead != c.read {
				t.Errorf("register_read = %v, want %v — a zero and an unanswered question are not one answer",
					rep.RegisterRead, c.read)
			}
			if c.note == "" && rep.RegisterNote != "" {
				t.Errorf("a register that answered whole still wrote a note: %q", rep.RegisterNote)
			}
			if c.note != "" && !strings.Contains(rep.RegisterNote, c.note) {
				t.Errorf("note = %q, want it to name %q", rep.RegisterNote, c.note)
			}

			// Everything above reads the report; this reads what an operator is
			// told. The note reaching the struct and not the terminal is the
			// failure that leaves a floor looking like a clean bill.
			var err error
			out := capture(t, func() { err = execute(t, "check") })
			if got := Code(err); got != map[bool]int{true: ExitFindings, false: 0}[c.dormant > 0 || c.bad] {
				t.Fatalf("check exited %d (%v):\n%s", got, err, out)
			}
			if c.note != "" && !strings.Contains(out, c.note) {
				t.Errorf("check wrote:\n%s\nwant it to name %q", out, c.note)
			}
			if c.note == "" && strings.Contains(out, "note:") {
				t.Errorf("check wrote a note about a register that answered whole:\n%s", out)
			}
			if !c.read && strings.Contains(out, "0 orphaned") {
				t.Errorf("check reported a count off a register it never read:\n%s", out)
			}
		})
	}
}

// BAD REGISTER can easily say everything twice: the reader's error in the
// finding's detail and again in the note below it, the register path in the
// headline and again inside the error, and a kind wider than its column
// leaves the wrapped line under the wrong one.
//
// Asserted by counting rather than by matching a rendered string: the point is
// that each fact appears once, and a substring check passes on two copies.
func TestBadRegisterSaysEachThingOnce(t *testing.T) {
	dir := dormancyProject(t, fileProject, "B1")
	stubPATH(t)
	fileRegister(t, dir, `{"number":2,`)
	openIn(t, dir)

	var err error
	out := capture(t, func() { err = execute(t, "check") })
	if Code(err) != ExitFindings {
		t.Fatalf("check exited %d (%v):\n%s", Code(err), err, out)
	}

	if n := strings.Count(out, "is not a register row"); n != 1 {
		t.Errorf("the reader's error appears %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "register.jsonl"); n != 1 {
		t.Errorf("the register path appears %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, dir) {
		t.Errorf("the finding names an absolute path; the reader edits a relative one:\n%s", out)
	}

	// The column, read off the two lines rather than off the format string: the
	// detail has to start under the message, whatever findingWidth is.
	var headline, detail string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "BAD REGISTER"):
			headline = line
		case strings.HasPrefix(line, " "):
			detail = line
		}
	}
	if headline == "" || detail == "" {
		t.Fatalf("check printed no BAD REGISTER finding with a detail line:\n%s", out)
	}
	rest := headline[len("BAD REGISTER"):]
	message := len("BAD REGISTER") + len(rest) - len(strings.TrimLeft(rest, " "))
	if indent := len(detail) - len(strings.TrimLeft(detail, " ")); indent != message {
		t.Errorf("the detail starts at column %d and the message at %d:\n%s", indent, message, out)
	}
}

// The silent cell, asserted on the whole output rather than on the absence of
// one substring: a project declaring no register has failed nothing, and the
// word for that is nothing at all.
func TestCheckSaysNothingAboutARegisterTheProjectDeclaredItHasNot(t *testing.T) {
	dir := dormancyProject(t, noProject, "B1")
	stubPATH(t)
	openIn(t, dir)

	var err error
	out := capture(t, func() { err = execute(t, "check") })
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	for _, unwanted := range []string{"ORPHANED FOLLOW-UP", "register", "orphaned"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("check mentioned %q on a project with no register:\n%s", unwanted, out)
		}
	}
}

// A register that resolved and holds nothing, and one drover asked the wrong
// question about, are the same count. Every cell here is either a state an
// ordinary project is in or a config nobody meant to write, and the count alone
// separates none of them: each backend consults a second fact instead.
func TestCheckSeparatesAnUnresolvedRegisterFromAnEmptyOne(t *testing.T) {
	for _, c := range []struct {
		name   string
		conf   string
		page   string   // the gh page; empty leaves no gh on PATH at all
		labels string   // the repository's label set; empty is a call that fails
		rows   []string // the file backend's source; nil writes no file
		parent bool     // create the directory the register path points into
		kind   string   // the finding this cell must produce; empty is silence
		names  []string // what that finding has to name
		note   string
		state  string
	}{
		{
			// Where every tracker project starts: the label is there, nobody
			// has filed against it yet.
			name: "github, empty under a label the repository defines", conf: ghProject,
			page: noRegister, labels: `[{"name":"follow-up"}]`, state: registerRead,
		},
		{
			name: "github, empty under a label nobody created", conf: ghProject,
			page: noRegister, labels: `[{"name":"bug"}]`,
			kind:  "NO LABEL",
			names: []string{"follow_up_label", `"follow-up"`, "owner/name"},
			state: registerUnresolved,
		},
		{
			// The label set is never asked for a register that answered with
			// rows: it resolved by demonstration, and the call is a round trip.
			name: "github, rows, so the label set is never asked", conf: ghProject,
			page: reachability, labels: "", state: registerRead,
		},
		{
			// Degraded, and it degrades the way ORPHANED FOLLOW-UP does: a note naming what
			// could not be asked, no finding, and the same exit code.
			name: "github, the label set cannot be read", conf: ghProject,
			page: noRegister, labels: "",
			note: "the label set was not read", state: registerRead,
		},
		{
			name: "file, no file and no directory to hold one", conf: fileProject,
			kind:  "NO REGISTER",
			names: []string{"[paths] register", ".drover"},
			state: registerUnresolved,
		},
		{
			// Where every file project starts: the directory is there, no row
			// has been appended yet.
			name: "file, no file beside a directory that exists", conf: fileProject,
			parent: true, state: registerRead,
		},
		{
			name: "file, a register with a row in it", conf: fileProject, parent: true,
			rows:  []string{`{"number":1,"title":"a filed follow-up","state":"open","date":"2026-01-01"}`},
			state: registerRead,
		},
		{
			// A project that declared no register has no path and no label, so
			// there is nothing here it could have got wrong. Not "read" either:
			// nothing was read, and a wrapper deciding whether to trust a zero
			// wants that separated from a register that answered whole.
			name: "none, silent", conf: noProject, state: registerOff,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := registerProject(t, c.conf)
			bin := stubPATH(t)
			if c.page != "" {
				stubGHRegister(t, bin, c.page, c.labels)
			}
			if c.parent {
				if err := os.MkdirAll(filepath.Join(dir, ".drover"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if c.rows != nil {
				fileRegister(t, dir, c.rows...)
			}

			rep := openIn(t, dir).checkReport()
			want := 0
			if c.kind != "" {
				want = 1
			}
			if n := len(rep.Findings); n != want {
				t.Fatalf("findings = %d, want %d: %+v", n, want, rep.Findings)
			}
			if rep.RegisterState != c.state {
				t.Errorf("register_state = %q, want %q — a resolved empty register and an unresolvable one are not one state",
					rep.RegisterState, c.state)
			}
			if c.note == "" && rep.RegisterNote != "" {
				t.Errorf("a register that answered whole still wrote a note: %q", rep.RegisterNote)
			}
			if c.note != "" && !strings.Contains(rep.RegisterNote, c.note) {
				t.Errorf("note = %q, want it to name %q", rep.RegisterNote, c.note)
			}

			var err error
			out := capture(t, func() { err = execute(t, "check") })
			if code := Code(err); code != map[bool]int{true: ExitFindings, false: 0}[want == 1] {
				t.Fatalf("check exited %d (%v):\n%s", code, err, out)
			}
			if want == 0 {
				return
			}
			got := findings(rep, c.kind)
			if len(got) != 1 {
				t.Fatalf("%s findings = %d, want 1: %+v", c.kind, len(got), rep.Findings)
			}
			for _, name := range c.names {
				if !strings.Contains(got[0].Message, name) {
					t.Errorf("finding does not name %s: %q", name, got[0].Message)
				}
			}
			if got[0].Detail == "" {
				t.Error("finding carries no next step")
			}
			// The one claim drover cannot make here, and the reason the finding
			// exists at all: it never read the register this config names.
			for _, line := range []string{got[0].Message, got[0].Detail} {
				if strings.Contains(line, "empty") {
					t.Errorf("finding calls the register empty, which is what it could not check: %q", line)
				}
			}
			if !strings.Contains(out, c.kind) {
				t.Errorf("check wrote:\n%s\nwant the %s finding", out, c.kind)
			}
		})
	}
}

// One issue in both sets. The register reads a labelled row, the backlog reads
// an ordinary item, and neither side can see the other: intake drops the label
// as it promotes, so what reaches this is a removal that did not happen.
func TestCheckReportsAnIssueThatIsAlsoABacklogItem(t *testing.T) {
	for _, c := range []struct {
		name  string
		issue string
		file  bool
		want  int
	}{
		{name: "the item names an open register row", issue: "101", want: 1},
		{name: "the item names an issue no row carries", issue: "999"},
		{
			// A file register numbers its own rows, so a number matching an
			// item's issue: key there names no tracker issue and is chance.
			name: "the file backend numbers its own rows", issue: "101", file: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			conf := ghProject
			if c.file {
				conf = fileProject
			}
			dir := registerProject(t, conf)
			item := "---\ntitle: Taken in\ndeps:\nissue: " + c.issue + "\n---\n\nRequirements:\n\n1. Do it.\n"
			if err := os.WriteFile(filepath.Join(dir, "work", "B01-taken-in.md"), []byte(item), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := stubPATH(t)
			if c.file {
				fileRegister(t, dir, `{"number":101,"title":"a filed follow-up","state":"open","date":"2026-01-01"}`)
			} else {
				stubGHIssues(t, bin, reachability)
			}

			rep := openIn(t, dir).checkReport()
			got := findings(rep, "DUPLICATE ISSUE")
			if len(got) != c.want {
				t.Fatalf("DUPLICATE ISSUE findings = %d, want %d: %+v", len(got), c.want, rep.Findings)
			}
			if c.want == 0 {
				return
			}
			for _, name := range []string{"#101", "B1"} {
				if !strings.Contains(got[0].Message, name) {
					t.Errorf("finding does not name %s: %q", name, got[0].Message)
				}
			}
			if !strings.Contains(got[0].Detail, `"follow-up"`) {
				t.Errorf("finding does not name the label to drop: %q", got[0].Detail)
			}
			if got[0].ID != "B1" {
				t.Errorf("finding id = %q, want the item that claims the issue", got[0].ID)
			}

			var err error
			out := capture(t, func() { err = execute(t, "check") })
			if code := Code(err); code != ExitFindings {
				t.Fatalf("check exited %d (%v):\n%s", code, err, out)
			}
			if !strings.Contains(out, "DUPLICATE ISSUE") {
				t.Errorf("check wrote:\n%s\nwant the DUPLICATE ISSUE finding", out)
			}
		})
	}
}

// registerFile writes the file backend's source at the path applyDefaults gives
// it, at a chosen mode. fileRegister above always writes a readable one, and the
// mode is the axis this matrix turns.
func registerFile(t *testing.T, dir, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, ".drover", "register.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

// Every state the register path can be in, against every backend. The cells
// that are not findings carry the point: absence and emptiness are where a
// project starts, so a finding there would fail CI for every one of them on the
// day it declared the backend. The github and none columns are silent whatever
// is on disk, because neither ever opens the path.
func TestCheckReportsOnlyAFileRegisterItCannotRead(t *testing.T) {
	const goodRow = `{"number":1,"title":"a filed follow-up","state":"open","date":"2026-01-01"}` + "\n"

	for _, file := range []struct {
		name  string
		write bool
		body  string
		mode  os.FileMode
		// detail is the substring register.Read's own error must carry through
		// to the finding. Empty means the state is not a finding at all.
		detail string
		// asRoot marks the cell root defeats: a mode-000 file opens fine there.
		asRoot bool
	}{
		{name: "absent"},
		{name: "empty", write: true, mode: 0o600},
		{name: "readable", write: true, body: goodRow, mode: 0o600},
		{
			name: "unreadable by permission", write: true, body: goodRow, mode: 0o000,
			detail: "permission denied", asRoot: true,
		},
		{
			name: "malformed line", write: true, body: "{\"number\":2,\n", mode: 0o600,
			detail: "register.jsonl:1 is not a register row",
		},
	} {
		for _, backend := range []struct {
			name string
			conf string
			gh   bool
		}{
			{name: "file", conf: fileProject},
			{name: "github", conf: ghProject, gh: true},
			{name: "none", conf: noProject},
		} {
			t.Run(file.name+", backend "+backend.name, func(t *testing.T) {
				if file.asRoot && os.Geteuid() == 0 {
					t.Skip("root opens a mode-000 file, so this cell cannot reach the finding")
				}
				dir := registerProject(t, backend.conf)
				bin := stubPATH(t)
				if backend.gh {
					stubGHIssues(t, bin, noRegister)
				}
				// The parent exists in every cell: this matrix turns the file's
				// readability, and a path pointing into a directory that is not
				// there is NO REGISTER, a different finding with a different fix.
				if err := os.MkdirAll(filepath.Join(dir, ".drover"), 0o750); err != nil {
					t.Fatal(err)
				}
				if file.write {
					registerFile(t, dir, file.body, file.mode)
				}

				want := 0
				if backend.name == "file" && file.detail != "" {
					want = 1
				}
				rep := openIn(t, dir).checkReport()
				got := findings(rep, "BAD REGISTER")
				if len(got) != want {
					t.Fatalf("BAD REGISTER findings = %d, want %d: %+v", len(got), want, got)
				}
				// The register and the ledger are different files with different
				// fixes, so a wrapper keying on the token has to tell them apart.
				if n := len(findings(rep, "BAD LEDGER")); n != 0 {
					t.Errorf("a register fault was reported against the ledger: %d BAD LEDGER findings", n)
				}

				var err error
				out := capture(t, func() { err = execute(t, "check") })
				if code := Code(err); code != map[bool]int{true: ExitFindings, false: 0}[want == 1] {
					t.Fatalf("check exited %d (%v):\n%s", code, err, out)
				}
				if want == 0 {
					if strings.Contains(out, "BAD REGISTER") {
						t.Errorf("check reported a register it had nothing to say about:\n%s", out)
					}
					return
				}

				// The message is register.Read's own error: it names the file,
				// the line and the cause in one sentence, and the detail is
				// free to carry the fix the way every other finding's does.
				if !strings.Contains(got[0].Message, file.detail) {
					t.Errorf("finding does not carry what register.Read said: %q", got[0].Message)
				}
				if got[0].Path != filepath.Join(".drover", "register.jsonl") {
					t.Errorf("finding path = %q, want the configured register path", got[0].Path)
				}
				if !strings.Contains(out, "BAD REGISTER") || !strings.Contains(out, file.detail) {
					t.Errorf("check wrote:\n%s\nwant the finding and %q", out, file.detail)
				}
			})
		}
	}
}

// The clause that carries the finding's meaning ("the row reads as work no run
// carries while B1 carries it") is false the moment B1 merges. Two items
// claiming one row each, differing only in what the ledger says about them.
func TestRowClashSkipsAnItemTheLedgerRecordsAsMerged(t *testing.T) {
	dir := registerProject(t, ghProject)
	for name, issue := range map[string]string{
		"B01-merged.md": "101",
		"B02-open.md":   "102",
	} {
		item := "---\ntitle: Taken in\ndeps:\nissue: " + issue + "\n---\n\nRequirements:\n\n1. Do it.\n"
		if err := os.WriteFile(filepath.Join(dir, "work", name), []byte(item), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"),
		[]byte(`{"id":"B1","merged":true,"implementer":"upstream","date":"2026-01-01"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubGHIssues(t, stubPATH(t), reachability)

	got := findings(openIn(t, dir).checkReport(), "DUPLICATE ISSUE")
	if len(got) != 1 {
		t.Fatalf("DUPLICATE ISSUE findings = %d, want the open item's alone: %+v", len(got), got)
	}
	if got[0].ID != "B2" || !strings.Contains(got[0].Message, "#102") {
		t.Errorf("DUPLICATE ISSUE fired on %q (%q), want B2 and #102", got[0].ID, got[0].Message)
	}
	// The half of the sentence the filter exists for: whatever the detail names
	// has to be carrying the row at the moment the reader reads it.
	if !strings.Contains(got[0].Detail, "while B2 carries it") {
		t.Errorf("finding does not claim the open item carries it: %q", got[0].Detail)
	}
}

// A missing register is not a finding on absence alone: a misspelled filename
// beside a real directory reads as an empty register. register.Append creates
// what it writes to, so rows at a path drover
// was never asked to read separate the two without costing a first-day project
// a finding.
func TestNoRegisterSeparatesAMisspelledPathFromAProjectThatHasFiledNothing(t *testing.T) {
	const row = `{"number":1,"title":"a filed follow-up","state":"open","date":"2026-01-01"}` + "\n"
	for _, c := range []struct {
		name string
		conf string
		// stray is written into the register's own directory under a name the
		// config does not point at.
		stray   string
		content string
		// want is the filename the finding must name; empty means silence.
		want string
	}{
		{name: "nothing has ever been filed", conf: fileProject},
		{
			name: "a ledger beside it is not a register", conf: fileProject,
			stray: "ledger.jsonl", content: `{"id":"B1","merged":true,"date":"2026-01-01"}` + "\n",
		},
		{
			name: "prose beside it is not a register", conf: fileProject,
			stray: "CHANGELOG_2026_01_01.md", content: "# 2026-01-01\n\nA session.\n",
		},
		{
			name: "the rows are in a file the key does not name", conf: fileProject,
			stray: "regsiter.jsonl", content: row, want: "regsiter.jsonl",
		},
		// Neither other backend opens the path, so the same tree is silent
		// under both.
		{name: "github never opens the path", conf: ghProject, stray: "regsiter.jsonl", content: row},
		{name: "none never opens the path", conf: noProject, stray: "regsiter.jsonl", content: row},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := registerProject(t, c.conf)
			bin := stubPATH(t)
			if c.conf == ghProject {
				stubGHIssues(t, bin, noRegister)
			}
			// The directory exists in every cell: a path pointing into one that
			// is not there is the other cause of this finding.
			if err := os.MkdirAll(filepath.Join(dir, ".drover"), 0o750); err != nil {
				t.Fatal(err)
			}
			if c.stray != "" {
				if err := os.WriteFile(filepath.Join(dir, ".drover", c.stray), []byte(c.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			rep := openIn(t, dir).checkReport()
			got := findings(rep, "NO REGISTER")
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("NO REGISTER findings = %d where nothing proves the path wrong: %+v", len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("NO REGISTER findings = %d, want 1: %+v", len(got), rep.Findings)
			}
			// Naming the file the rows are in is the difference between telling
			// a reader the key is wrong and telling them what it should say.
			if !strings.Contains(got[0].Message, c.want) || !strings.Contains(got[0].Detail, c.want) {
				t.Errorf("finding does not name %s:\n%q\n%q", c.want, got[0].Message, got[0].Detail)
			}
			if rep.RegisterState != registerUnresolved {
				t.Errorf("register_state = %q, want %q", rep.RegisterState, registerUnresolved)
			}
		})
	}
}

// [run] runner_key is [severity] key one table over: a config naming a
// frontmatter key nothing declares, so no item can carry it. It is quieter than
// severity's, because run falls back to default_runner and reports nothing about
// the pin it ignored.
func TestRunnerKeyOnAnUndeclaredKeyIsAFinding(t *testing.T) {
	got := findings(openIn(t, shapeProject(t, "\n[run]\nrunner_key = \"runner\"\n")).checkReport(), "UNDECLARED KEY")

	if len(got) != 1 {
		t.Fatalf("UNDECLARED KEY findings = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, `"runner"`) || !strings.Contains(got[0].Message, "[run]") {
		t.Errorf("finding does not name the key or the table: %q", got[0].Message)
	}
	if got[0].Detail == "" {
		t.Error("finding carries no fix")
	}
}

func TestRunnerKeyOnADeclaredKeyIsSilent(t *testing.T) {
	if got := findings(openIn(t, shapeProject(t, "\n[run]\nrunner_key = \"issue\"\n")).checkReport(), "UNDECLARED KEY"); len(got) != 0 {
		t.Errorf("UNDECLARED KEY findings = %d, want 0: %+v", len(got), got)
	}
}

// The case the gate exists for. runner_key defaults to "agent" for every
// project, and this fixture declares no agent — so an ungated check would report
// every project that never pinned a runner, which is most of them.
func TestAnUnsetRunnerKeyIsSilentEvenWhenItsDefaultIsUndeclared(t *testing.T) {
	s := openIn(t, shapeProject(t, ""))
	if s.cfg.Known(s.cfg.Run.RunnerKey) {
		t.Fatalf("the fixture declares %q, so this test no longer covers an undeclared default", s.cfg.Run.RunnerKey)
	}
	if got := findings(s.checkReport(), "UNDECLARED KEY"); len(got) != 0 {
		t.Errorf("UNDECLARED KEY findings = %d, want 0 for a project that never set runner_key: %+v", len(got), got)
	}
}

// runnerProject is a project whose only variable is its [runners] table, with
// a stub for each named binary put on PATH: the check is gated on the binary
// being there, so a fixture asserting against whatever the developer installed
// would pass or fail by accident.
func runnerProject(t *testing.T, table string, onPath ...string) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug" }

[verify]
default = "true"

` + table
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range onPath {
		stub(t, filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
	}
	t.Setenv("PATH", bin)
	return dir
}

// A config that fails only at launch, unattended, is the shape that cost two
// drives of this project: the reviewer started, consumed a flag as its prompt,
// ignored the implement prompt and left a log nobody could read.
func TestBadRunnerCatchesAnArgvTheBinaryCannotUse(t *testing.T) {
	dir := runnerProject(t,
		`[runners]`+"\n"+`agy = { headless = ["agy", "-p", "--output-format", "stream-json"] }`+"\n",
		"agy")

	got := findings(openIn(t, dir).checkReport(), "BAD RUNNER")
	if len(got) != 1 {
		t.Fatalf("BAD RUNNER findings = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "[runners] agy headless") {
		t.Errorf("the finding does not name the entry: %q", got[0].Message)
	}
	if !strings.Contains(got[0].Detail, "agy --output-format stream-json -p") {
		t.Errorf("the finding does not carry the argv that works: %q", got[0].Detail)
	}
}

func TestBadRunnerIsSilentOnAnArgvThatRuns(t *testing.T) {
	dir := runnerProject(t,
		`[runners]`+"\n"+`agy = { headless = ["agy", "--output-format", "stream-json", "-p"] }`+"\n",
		"agy")

	if got := findings(openIn(t, dir).checkReport(), "BAD RUNNER"); len(got) != 0 {
		t.Errorf("a working argv reported %+v", got)
	}
}

// CI on a host that never installed a runner has nothing to judge, and failing
// there would fail every project that declares a runner its build image lacks.
func TestBadRunnerIsSilentWhenTheBinaryIsAbsent(t *testing.T) {
	dir := runnerProject(t,
		`[runners]`+"\n"+`agy = { headless = ["agy", "-p", "--output-format", "stream-json"] }`+"\n")

	if got := findings(openIn(t, dir).checkReport(), "BAD RUNNER"); len(got) != 0 {
		t.Errorf("a binary absent from PATH reported %+v", got)
	}
}

// templateProject is a project whose [prompts] implement is text, written to
// brief.tmpl.
func templateProject(t *testing.T, text string) string {
	t.Helper()
	dir := runnerProject(t, "[prompts]\nimplement = \"prompt.tmpl\"\n")
	write(t, filepath.Join(dir, "prompt.tmpl"), text)
	return dir
}

// declaredProject is templateProject declaring the frontmatter key phase and,
// with gate set, the verify integration. table lands straight after
// runnerProject's [verify] header, so a leading key joins that table.
func declaredProject(t *testing.T, gate bool, text string) string {
	t.Helper()
	table := "[frontmatter]\noptional = [\"phase\"]\n\n[prompts]\nimplement = \"prompt.tmpl\"\n"
	if gate {
		table = "integration = \"true\"\n\n" + table
	}
	dir := runnerProject(t, table)
	write(t, filepath.Join(dir, "prompt.tmpl"), text)
	return dir
}

// run renders a declared key for the item carrying it, so check must too,
// and missingkey=error must still catch the key nobody declared.
func TestCheckRendersDeclaredMapKeys(t *testing.T) {
	cases := []struct {
		name string
		gate bool
		text string
		want int
	}{
		{"declared meta key", false, "{{ .Meta.phase }}\n", 0},
		{"declared verify command", true, "{{ .VerifyCommands.integration }}\n", 0},
		{"undeclared meta key", false, "{{ .Meta.phse }}\n", 1},
		{"undeclared verify command", false, "{{ .VerifyCommands.integration }}\n", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findings(openIn(t, declaredProject(t, c.gate, c.text)).checkReport(), "BAD TEMPLATE")
			if len(got) != c.want {
				t.Errorf("BAD TEMPLATE findings = %d, want %d: %+v", len(got), c.want, got)
			}
		})
	}
}

func TestCheckSaysAMissingTemplateCouldNotBeRead(t *testing.T) {
	s := openIn(t, runnerProject(t, "[prompts]\nimplement = \"absent.tmpl\"\n"))
	got := findings(s.checkReport(), "BAD TEMPLATE")
	if len(got) != 1 {
		t.Fatalf("BAD TEMPLATE findings = %d, want 1: %+v", len(got), got)
	}
	if got[0].Message != "absent.tmpl could not be read" {
		t.Errorf("message = %q, want the read failure", got[0].Message)
	}
}

// A template naming a field the data does not carry has to fail check, or
// the break surfaces only at `drover run --headless`.
func TestCheckRendersAStalePromptTemplate(t *testing.T) {
	dir := templateProject(t, "{{ .Title }}\n{{ range .Issues }}{{ .Title }}{{ end }}\n")
	openIn(t, dir)

	var err error
	out := capture(t, func() { err = execute(t, "check") })
	if Code(err) != ExitFindings {
		t.Fatalf("check exited %d (%v), want %d:\n%s", Code(err), err, ExitFindings, out)
	}
	if n := strings.Count(out, "BAD TEMPLATE"); n != 1 {
		t.Errorf("BAD TEMPLATE appears %d times, want 1:\n%s", n, out)
	}

	prev := checkJSON
	t.Cleanup(func() { checkJSON = prev })
	out = capture(t, func() { err = execute(t, "check", "--json") })
	if Code(err) != ExitFindings {
		t.Fatalf("check --json exited %d (%v):\n%s", Code(err), err, out)
	}
	var rep CheckReport
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("parse %q: %v", out, jerr)
	}
	got := findings(rep, "BAD TEMPLATE")
	if len(got) != 1 {
		t.Fatalf("BAD TEMPLATE findings = %d, want 1: %+v", len(got), rep.Findings)
	}
	if got[0].Path != "prompt.tmpl" {
		t.Errorf("path = %q, want prompt.tmpl", got[0].Path)
	}
	if !strings.Contains(got[0].Detail, "Issues") {
		t.Errorf("detail does not carry Render's error: %q", got[0].Detail)
	}
}

// A zero brief has no follow-ups to range over, so a stale field reached only inside
// the range is never looked up. The sample has one.
func TestCheckRendersAFieldReachedOnlyInsideARange(t *testing.T) {
	s := openIn(t, templateProject(t, "{{ .Title }}\n{{ range .FollowUps }}{{ .Owner }}{{ end }}\n"))
	if got := findings(s.checkReport(), "BAD TEMPLATE"); len(got) != 1 {
		t.Errorf("BAD TEMPLATE findings = %d, want 1: %+v", len(got), got)
	}
}

func TestCheckIsSilentOnATemplateThatRenders(t *testing.T) {
	s := openIn(t, templateProject(t, "{{ .Title }}\n{{ range .FollowUps }}{{ .Title }}{{ end }}\n"))
	if got := findings(s.checkReport(), "BAD TEMPLATE"); len(got) != 0 {
		t.Errorf("BAD TEMPLATE findings = %d, want 0: %+v", len(got), got)
	}
}

// Every key is read, not only the implement one, and an unset key is not.
func TestCheckRendersTheDraftAndReviewTemplates(t *testing.T) {
	dir := runnerProject(t, "[prompts]\ndraft = \"draft.tmpl\"\nreview = \"review.tmpl\"\n")
	write(t, filepath.Join(dir, "draft.tmpl"), "{{ .Issues }}\n")
	write(t, filepath.Join(dir, "review.tmpl"), "{{ .Issues }}\n")
	got := findings(openIn(t, dir).checkReport(), "BAD TEMPLATE")
	paths := make([]string, 0, len(got))
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "draft.tmpl,review.tmpl" {
		t.Errorf("BAD TEMPLATE paths = %v, want [draft.tmpl review.tmpl]", paths)
	}
}

// `deps: [F1]` must not read as a missing F1 while F01 sits in the backlog.
func TestAListFormDepsLineIsBadDepsNotDangling(t *testing.T) {
	s := fixture(t, "")
	write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"), "---\ntitle: Second\ndeps: [1]\n---\n\n1. Two.\n")
	rep := openIn(t, s.cfg.Root).checkReport()

	bad := findings(rep, "BAD DEPS")
	if len(bad) != 1 || !strings.Contains(bad[0].Detail, "`deps: 1`") || !strings.Contains(bad[0].Detail, "work/02-second.md:3") {
		t.Errorf("BAD DEPS = %+v, want one naming `deps: 1` at work/02-second.md:3", bad)
	}
	if got := findings(rep, "MISSING DEP"); len(got) != 0 {
		t.Errorf("a dep on a body that exists is MISSING DEP: %+v", got)
	}
}

func TestAMultiIDListFormDepsLineIsOneBadDepsAndDanglesOnlyTheMissingID(t *testing.T) {
	for _, tc := range []struct {
		deps, fixed string
		dangling    []string
	}{
		{`["1", "2"]`, "deps: 1 2", nil},
		{`[1,2]`, "deps: 1 2", nil},
		{`[1, 2]`, "deps: 1 2", nil},
		{`["1","2"]`, "deps: 1 2", nil},
		{`[1, 9]`, "deps: 1 9", []string{"9"}},
		{`[ 1 , 2 ]`, "deps: 1 2", nil},
		{`[ 1 , 9 ]`, "deps: 1 9", []string{"9"}},
	} {
		t.Run(tc.deps, func(t *testing.T) {
			s := fixture(t, "")
			write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Two.\n")
			write(t, filepath.Join(s.cfg.Root, "work", "03-third.md"), "---\ntitle: Third\ndeps: "+tc.deps+"\n---\n\n1. Three.\n")
			rep := openIn(t, s.cfg.Root).checkReport()

			bad := findings(rep, "BAD DEPS")
			if len(bad) != 1 || !strings.Contains(bad[0].Detail, "`"+tc.fixed+"`") {
				t.Errorf("BAD DEPS = %+v, want one naming `%s`", bad, tc.fixed)
			}
			dang := findings(rep, "MISSING DEP")
			if len(dang) != len(tc.dangling) {
				t.Fatalf("MISSING DEP = %+v, want %d", dang, len(tc.dangling))
			}
			for i, id := range tc.dangling {
				if want := fmt.Sprintf("depends on %q, which has no body", id); !strings.Contains(dang[i].Message, want) {
					t.Errorf("MISSING DEP = %q, want %q", dang[i].Message, want)
				}
			}
		})
	}
}

// check and depsLint read a deps value through one tokenizer, so every value
// is malformed to both or to neither, and a malformed one gets the same
// correction from each. A comma is malformed with or without list marks.
func TestCheckAndDepsLintAgreeOnTheCorrectedDepsLine(t *testing.T) {
	quoted := regexp.MustCompile("`(deps:[^`]*)`")
	for _, tc := range []struct {
		deps, fixed string
		dangling    []string
	}{
		{`1,2`, "deps: 1 2", nil},
		{`1, 2`, "deps: 1 2", nil},
		{`1 , 2`, "deps: 1 2", nil},
		{`[1,2]`, "deps: 1 2", nil},
		{`[ 1, 2 ]`, "deps: 1 2", nil},
		{`[ 1 , 2 ]`, "deps: 1 2", nil},
		{`["1", "2"]`, "deps: 1 2", nil},
		{`[9]`, "deps: 9", []string{"9"}},
		{`1 2`, "", nil},
		{`1`, "", nil},
		{``, "", nil},
	} {
		t.Run(tc.deps, func(t *testing.T) {
			s := fixture(t, "")
			write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"), "---\ntitle: Second\ndeps:\n---\n\n1. Two.\n")
			write(t, filepath.Join(s.cfg.Root, "work", "03-third.md"), "---\ntitle: Third\ndeps: "+tc.deps+"\n---\n\n1. Three.\n")
			s = openIn(t, s.cfg.Root)
			it, err := s.item("3")
			if err != nil {
				t.Fatal(err)
			}
			lintErr := s.depsLint(it)
			rep := s.checkReport()
			bad, dang := findings(rep, "BAD DEPS"), findings(rep, "MISSING DEP")

			if len(dang) != len(tc.dangling) {
				t.Errorf("MISSING DEP = %+v, want %d", dang, len(tc.dangling))
			}
			for i, id := range tc.dangling {
				if i < len(dang) && !strings.Contains(dang[i].Message, fmt.Sprintf("depends on %q, which has no body", id)) {
					t.Errorf("MISSING DEP = %q, want one naming %q", dang[i].Message, id)
				}
			}
			if tc.fixed == "" {
				if lintErr != nil || len(bad) != 0 {
					t.Errorf("depsLint = %v, BAD DEPS = %+v, want neither", lintErr, bad)
				}
				return
			}
			if lintErr == nil {
				t.Fatal("depsLint passed a line it should correct")
			}
			m := quoted.FindAllStringSubmatch(lintErr.Error(), -1)
			if lint := m[len(m)-1][1]; lint != tc.fixed {
				t.Errorf("depsLint corrects to %q, want %q", lint, tc.fixed)
			}
			if len(bad) != 1 {
				t.Fatalf("BAD DEPS = %+v, want one", bad)
			}
			if m := quoted.FindStringSubmatch(bad[0].Detail); m == nil || m[1] != tc.fixed {
				t.Errorf("check corrects to %q, depsLint to %q", bad[0].Detail, tc.fixed)
			}
		})
	}
}

func TestAListFormDepOnNothingStaysDanglingUnderItsStrippedID(t *testing.T) {
	s := fixture(t, "")
	write(t, filepath.Join(s.cfg.Root, "work", "02-second.md"), "---\ntitle: Second\ndeps: [9]\n---\n\n1. Two.\n")
	got := findings(openIn(t, s.cfg.Root).checkReport(), "MISSING DEP")
	if len(got) != 1 || !strings.Contains(got[0].Message, `depends on "9", which has no body`) {
		t.Errorf("MISSING DEP = %+v, want one naming 9", got)
	}
}
