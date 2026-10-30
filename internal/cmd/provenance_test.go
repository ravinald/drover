package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

// originOf is the origin row the ledger at path holds for id, failing the test
// when there is none.
func originOf(t *testing.T, path, id string) ledger.Row {
	t.Helper()
	l, err := ledger.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := l.Provenance(id)
	if !p.Recorded() {
		t.Fatalf("%s has no origin row in %+v", id, l.Rows)
	}
	return *p.Origin
}

// A command typed at the operator's own terminal is the operator's, and an
// agent driving that terminal is named beside the actor rather than in place of
// it: actor answers whom drover launched, agent who typed.
func TestNewRecordsTheOperatorAndTheAgentDrivingTheTerminal(t *testing.T) {
	t.Cleanup(func() { newSev, newFoundIn = "", "" })
	dir := newProject(t)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")

	capture(t, func() {
		if err := execute(t, "new", "bug", "typed by a person"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	o := originOf(t, ledgerPath, "B1")
	if o.Origin.Cmd != "new" || o.Origin.Actor != runner.ActorOperator || o.Origin.Agent != "" || o.Origin.Session != "" {
		t.Errorf("a person's new recorded %+v, want cmd new, actor operator, no agent and no session", *o.Origin)
	}
	if o.TS == "" || o.Date == "" {
		t.Errorf("the origin row carries ts %q and date %q, want both", o.TS, o.Date)
	}

	t.Setenv(envClaudeCode, "1")
	capture(t, func() {
		if err := execute(t, "new", "bug", "typed by claude"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	o = originOf(t, ledgerPath, "B2")
	if o.Origin.Actor != runner.ActorOperator || o.Origin.Agent != "claude" {
		t.Errorf("new under CLAUDECODE=1 recorded actor %q agent %q, want operator and claude", o.Origin.Actor, o.Origin.Agent)
	}
}

// A finding filed from inside a review session resolves to that session's
// stage row, and its agent comes from the runner drover recorded there rather
// than from whatever the session's environment claims.
func TestNewFromAReviewSessionResolvesToItsDrive(t *testing.T) {
	t.Cleanup(func() { newSev, newFoundIn = "", "" })
	dir := newProject(t)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	write(t, filepath.Join(dir, "work", "B01-reviewed.md"), "---\ntitle: reviewed\ndeps:\nkind: bug\n---\n\n1. Fix it.\n")
	write(t, ledgerPath, `{"id":"B1","stage":"review","runner":"codex","session":"5e55","drive":"D3","date":"2026-10-27"}`+"\n")
	// A finding is filed only against merged work, so the review this session
	// ran is of an item that has since merged.
	markMerged(t, dir, "B1")
	t.Setenv(runner.EnvActor, runner.ActorAgent)
	t.Setenv(runner.EnvSession, "5e55")
	t.Setenv(envClaudeCode, "1")

	newFoundIn = "B1"
	capture(t, func() {
		if err := execute(t, "new", "bug", "found in review"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	o := originOf(t, ledgerPath, "B2")
	if o.Origin.Actor != runner.ActorAgent || o.Origin.Agent != "codex" || o.Origin.Session != "5e55" || o.Origin.FoundIn != "B1" {
		t.Errorf("recorded %+v, want actor agent, agent codex from the stage row, session 5e55, found_in B1", *o.Origin)
	}

	l, err := ledger.Load(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p := l.Provenance("B2")
	if p.Launch == nil {
		t.Fatalf("the origin's session resolved to no stage row: %+v", p)
	}
	if p.Launch.Drive != "D3" || p.Launch.Stage != ledger.StageReview || p.Launch.ID != "B1" {
		t.Errorf("resolved to drive %q stage %q item %q, want D3, review, B1", p.Launch.Drive, p.Launch.Stage, p.Launch.ID)
	}
}

func TestIntakeRecordsTheIssueItCameFrom(t *testing.T) {
	dir := intakeProject(t, map[int]string{44: `{"number":44,"title":"ranked bug","body":"b","state":"OPEN",` +
		`"labels":[{"name":"bug"},{"name":"severity:low"}]}`})
	var err error
	capture(t, func() { err = execute(t, "intake", "44") })
	if err != nil {
		t.Fatalf("intake 44: %v", err)
	}
	o := originOf(t, filepath.Join(dir, "ledger.jsonl"), "B1")
	if o.Origin.Cmd != "intake" || o.Origin.Issue != 44 || o.Origin.Actor != runner.ActorOperator {
		t.Errorf("intake recorded %+v, want cmd intake, issue 44, actor operator", *o.Origin)
	}
}

func TestSplitRecordsTheItemItSplit(t *testing.T) {
	dir := draftProject(t, "")
	proposal := filepath.Join(dir, "proposal.md")
	write(t, proposal, "Requirements:\n\n1. Publish by rename.\n")
	t.Cleanup(func() { splitFile, splitInto = "", "" })
	splitInto = "bug"
	capture(t, func() {
		if err := execute(t, "split", "1", "atomic local publication", "-f", proposal); err != nil {
			t.Fatal(err)
		}
	})
	o := originOf(t, filepath.Join(dir, "ledger.jsonl"), "B2")
	if o.Origin.Cmd != "split" || o.Origin.SplitFrom != "1" {
		t.Errorf("split recorded %+v, want cmd split, split_from 1", *o.Origin)
	}
}

// The latest body row says who drover last saw write the body, and a file that
// no longer digests to it was changed by someone drover did not run. An item
// minted before origins were recorded reads as unrecorded, not as a failure.
func TestAcceptThenAHandEditReadsBackAsUnrecorded(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	path := filepath.Join(dir, "work", "B01-scaffold.md")
	t.Chdir(dir)
	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})

	provenance := func() ledger.Provenance {
		t.Helper()
		l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path) // #nosec G304 -- the fixture's own item
		if err != nil {
			t.Fatal(err)
		}
		return l.Provenance("B1", bodySum(b), digestBytes(b))
	}

	p := provenance()
	if p.LastBody == nil || p.LastBody.Cmd != "accept" || p.LastBody.Actor != runner.ActorOperator || p.LastBody.TS == "" {
		t.Fatalf("the latest body row is %+v, want accept by operator with a ts", p.LastBody)
	}
	if !p.Matches {
		t.Error("the body accept just wrote reads as edited")
	}
	if p.Recorded() || p.Launch != nil {
		t.Errorf("an item written before any origin row reads as recorded: %+v", p)
	}

	write(t, path, read(t, path)+"\nA line nobody recorded.\n")
	p = provenance()
	if p.LastBody == nil || p.LastBody.Cmd != "accept" {
		t.Fatalf("the hand edit moved the latest body row: %+v", p.LastBody)
	}
	if p.Matches {
		t.Error("a hand edit reads as matching the digest accept recorded")
	}
}

// touch certifies the operator's edit, so its row names the operator.
func TestTouchRecordsTheOperator(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	path := filepath.Join(dir, "work", "B01-scaffold.md")
	t.Chdir(dir)
	capture(t, func() {
		if err := acceptCmd.RunE(acceptCmd, []string{"B1"}); err != nil {
			t.Fatal(err)
		}
	})
	write(t, path, read(t, path)+"\nA line the operator wrote.\n")
	t.Setenv(envCodexThread, "019a")
	capture(t, func() {
		if err := execute(t, "touch", "B1", "--reason", "fixed a typo"); err != nil {
			t.Fatal(err)
		}
	})
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	last := l.Provenance("B1").LastBody
	if last == nil || last.Cmd != "touch" || last.Actor != runner.ActorOperator || last.Agent != "codex" {
		t.Errorf("touch recorded %+v, want cmd touch by operator, agent codex", last)
	}
}

// rawRows decodes every line of the ledger at path into its keys as written,
// which is the only way to see a key the encoder omitted: a Row decoded from a
// line without "agent" and one with "agent":"" are the same value.
func rawRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(read(t, path)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		rows = append(rows, m)
	}
	return rows
}

// assertAttributed fails unless raw names who wrote its digest under every
// key a body row owes: an operator with no agent still writes "agent":"".
func assertAttributed(t *testing.T, raw map[string]any, cmd, actor string) {
	t.Helper()
	for _, k := range []string{"body", "cmd", "actor", "agent", "ts"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("body row %v has no %q key", raw, k)
		}
	}
	if raw["cmd"] != cmd || raw["actor"] != actor {
		t.Errorf("body row %v names cmd %v actor %v, want %s and %s", raw, raw["cmd"], raw["actor"], cmd, actor)
	}
}

// A body row is refused at the ledger itself when it cannot say who wrote the
// digest, whichever path built it, and leaves the file as it was.
func TestABodyRowWithoutAttributionIsRefused(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	s := openIn(t, dir)
	for _, by := range []ledger.Author{{Cmd: "accept"}, {Actor: runner.ActorOperator}, {}} {
		if err := s.record(ledger.BodyRow("B1", "aaaaaaaaaaaa", by)); err == nil {
			t.Errorf("a body row by %+v was recorded", by)
		}
		if err := ledger.Append(ledgerPath, ledger.Row{ID: "B1", Body: "aaaaaaaaaaaa", Cmd: by.Cmd, Actor: by.Actor}); err == nil {
			t.Errorf("ledger.Append wrote a body row by %+v", by)
		}
	}
	if b, err := os.ReadFile(ledgerPath); err == nil && len(b) > 0 {
		t.Errorf("a refused body row reached the ledger: %s", b)
	}

	if err := s.record(ledger.BodyRow("B1", "bbbbbbbbbbbb", s.author("accept"))); err != nil {
		t.Fatal(err)
	}
	rows := rawRows(t, ledgerPath)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %v", rows)
	}
	assertAttributed(t, rows[0], "accept", runner.ActorOperator)
	if rows[0]["agent"] != "" {
		t.Errorf("an operator with no agent recorded agent %v, want empty", rows[0]["agent"])
	}
}

// The review stage row carries the body as the session found it, so it owes
// the same attribution as a body row, and rows without a digest gain no key.
func TestTheReviewStageRowIsAttributed(t *testing.T) {
	dir := markerProject(t, map[string]string{"B01-scaffold.md": scaffold})
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	s := openIn(t, dir)
	it, err := s.item("B1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.launched(it, "review", ledger.StageReview, "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.launched(it, "run", ledger.StageImplement, "claude"); err != nil {
		t.Fatal(err)
	}
	rows := rawRows(t, ledgerPath)
	if len(rows) != 2 {
		t.Fatalf("want two stage rows, got %v", rows)
	}
	assertAttributed(t, rows[0], "review", runner.ActorOperator)
	for _, k := range []string{"body", "cmd", "actor", "agent"} {
		if _, ok := rows[1][k]; ok {
			t.Errorf("the implement stage row %v carries %q", rows[1], k)
		}
	}
}

// provenanceProject is a backlog holding one item per ORIGIN form status
// renders, every time fixed in the current year so the golden holds from one
// January to the next, and every body's mtime pinned to one minute.
func provenanceProject(t *testing.T) {
	t.Helper()
	zone := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = zone })
	resetStatusFlags(t)
	y := time.Now().Year()
	at := func(day, hour int) string {
		return time.Date(y, time.March, day, hour, 21, 0, 0, time.UTC).Format(time.RFC3339)
	}
	item := func(title string) string {
		return "---\ntitle: " + title + "\ndeps:\n---\n\n1. Do " + title + ".\n"
	}
	bodies := map[string]string{}
	names := []string{"B01-one.md", "B02-two.md", "B03-three.md", "B04-four.md", "B05-five.md",
		"B06-six.md", "B07-seven.md", "B08-eight.md", "B09-nine.md", "F01-long.md", "F02-merged.md"}
	for i, n := range names {
		bodies[n] = item(fmt.Sprintf("item %d", i+1))
	}
	dir := markerProject(t, bodies)
	t.Chdir(dir)
	sum := func(name string) string {
		return bodySum([]byte(bodies[name]))
	}
	origin := func(id, ts string, o ledger.Origin) string {
		b, err := json.Marshal(ledger.Row{ID: id, TS: ts, Date: ts[:10], Origin: &o})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	body := func(id, ts, digest, cmd, actor, drive string) string {
		b, err := json.Marshal(ledger.Row{ID: id, TS: ts, Date: ts[:10], Body: digest, Cmd: cmd, Actor: actor, Drive: drive})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	old := time.Date(2020, 1, 2, 8, 0, 0, 0, time.UTC).Format(time.RFC3339)
	rows := []string{
		origin("B1", at(1, 9), ledger.Origin{Cmd: "new", Actor: "operator"}),
		origin("B2", at(2, 9), ledger.Origin{Cmd: "new", Actor: "operator", Agent: "claude"}),
		body("B2", at(4, 10), sum("B02-two.md"), "accept", "operator", ""),
		origin("B3", old, ledger.Origin{Cmd: "intake", Actor: "operator", Issue: 44}),
		origin("B4", at(3, 9), ledger.Origin{Cmd: "split", Actor: "operator", SplitFrom: "E84"}),
		`{"id":"B104","stage":"review","runner":"codex","session":"5e55","drive":"D20","ts":"` + at(4, 8) + `","date":"` + at(4, 8)[:10] + `"}`,
		origin("B5", at(4, 9), ledger.Origin{Cmd: "new", Actor: "agent", Agent: "codex", Session: "5e55", FoundIn: "B104"}),
		body("B5", at(5, 9), sum("B05-five.md"), "request-changes", "operator", "D20"),
		origin("B6", at(6, 9), ledger.Origin{Cmd: "new", Actor: "supervisor"}),
		origin("B7", at(7, 9), ledger.Origin{Cmd: "new", Actor: "agent", Session: "gone", FoundIn: "B104"}),
		`{"id":"B8","stage":"implement","runner":"claude","ts":"` + at(8, 9) + `","date":"` + at(8, 9)[:10] + `"}`,
		origin("B9", at(9, 9), ledger.Origin{Cmd: "new", Actor: "operator"}),
		body("B9", at(9, 10), sum("B09-nine.md"), "accept", "operator", ""),
		origin("F1", at(10, 9), ledger.Origin{Cmd: "split", Actor: "operator", Agent: "claude", SplitFrom: "B1234567890123456789"}),
		origin("F2", at(11, 9), ledger.Origin{Cmd: "new", Actor: "operator"}),
		`{"id":"F2","merged":true,"date":"` + at(12, 9)[:10] + `"}`,
	}
	write(t, filepath.Join(dir, "ledger.jsonl"), strings.Join(rows, "\n")+"\n")
	write(t, filepath.Join(dir, "work", "B09-nine.md"), bodies["B09-nine.md"]+"\nA line nobody recorded.\n")
	pinned := time.Date(y, 3, 20, 11, 5, 0, 0, time.UTC)
	for _, n := range names {
		if err := os.Chtimes(filepath.Join(dir, "work", n), pinned, pinned); err != nil {
			t.Fatal(err)
		}
	}
}

func statusTable(t *testing.T, args ...string) string {
	t.Helper()
	resetStatusFlags(t)
	return capture(t, func() {
		if err := execute(t, append([]string{"status", "--no-tracker"}, args...)...); err != nil {
			t.Fatalf("status %v: %v", args, err)
		}
	})
}

// Requirements 1, 2, 3 and 8: every ORIGIN form, an unrecorded item, a hand
// edit, and the merged row's line marked for the rtk filter, against a golden.
// DROVER_UPDATE_GOLDEN=1 rewrites it.
func TestStatusSecondLineGolden(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	provenanceProject(t)
	got := statusTable(t, "--all")
	path := filepath.Join(goldenDir(t), "status-two-line.golden")
	if os.Getenv("DROVER_UPDATE_GOLDEN") != "" {
		write(t, path, got)
	}
	if want := read(t, path); got != want {
		t.Errorf("status no longer renders status-two-line.golden:\n--- got\n%s\n--- want\n%s", got, want)
	}
	for _, want := range []string{
		"operator new ", "claude new ", "operator intake #44 ", "operator split of E84 ",
		"D20 review of B104 ", "supervisor new ", "agent new ← B104 ", "unrecorded ",
		"by hand", "request-changes · D20", "accept · operator", "new · operator", "≤Mar 08 09:21",
		"2020-01-02", "claude split of B1234567890…",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the table lacks %q:\n%s", want, got)
		}
	}
	if !regexp.MustCompile(`(?m)^ {6}operator new {18}Mar 09 09:21 ~Mar 20 11:05 by hand$`).MatchString(got) {
		t.Errorf("the hand-edited B9 does not read by hand at its mtime:\n%s", got)
	}
}

// Requirement 8: CREATED starts at one column on every second line, whatever
// the ORIGIN text before it, and so does EDITED. Requirement 1: every second
// line, a merged row's included, opens on six spaces and so repeats no state;
// only a merged row's ends in the tab the rtk filter drops it by.
func TestSecondLineFieldsSitAtFixedColumns(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	provenanceProject(t)
	cell := regexp.MustCompile(`^([A-Z][a-z]{2} \d{2} \d{2}:\d{2}|\d{4}-\d{2}-\d{2}  |-           )$`)
	created := secondIndent + originWidth + 2
	edited := created + timeWidth + 2
	lines := strings.Split(statusTable(t, "--all"), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "[") || i+1 == len(lines) {
			continue
		}
		next := lines[i+1]
		if !strings.HasPrefix(next, strings.Repeat(" ", secondIndent)) || next[secondIndent] == ' ' {
			t.Errorf("the line under %q does not open on exactly six spaces: %q", line, next)
		}
		if merged := strings.HasPrefix(line, "[x]"); merged != strings.HasSuffix(next, "\t") {
			t.Errorf("row %q (merged %v) has a second line ending %q", line, merged, next[len(next)-1:])
		}
	}
	n := 0
	for _, line := range lines {
		r := []rune(line)
		if len(r) < edited+timeWidth || !strings.HasPrefix(line, strings.Repeat(" ", secondIndent)) {
			continue
		}
		n++
		for _, at := range []int{created, edited} {
			if mark := r[at-1]; mark != ' ' && mark != '≤' && mark != '~' {
				t.Errorf("the column before %d is %q, want a space or a qualifier: %q", at, mark, line)
			}
			if c := string(r[at : at+timeWidth]); !cell.MatchString(c) {
				t.Errorf("column %d holds %q, not a time cell: %q", at, c, line)
			}
		}
	}
	if n != 11 {
		t.Errorf("checked %d second lines, want one per item (11)", n)
	}
}

// Requirement 4: --short is the table before the second line existed, and the
// colored second line is dim from end to end.
func TestStatusShortDropsTheSecondLine(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	provenanceProject(t)
	long, short := statusTable(t), statusTable(t, "--short")
	var kept []string
	for _, line := range strings.Split(long, "\n") {
		if !strings.HasPrefix(line, "      ") {
			kept = append(kept, line)
		}
	}
	if strings.Join(kept, "\n") != short {
		t.Errorf("--short is not the table less its second lines:\n--- short\n%s\n--- long, filtered\n%s", short, strings.Join(kept, "\n"))
	}
	forceColor(t, colorAlways)
	colored := statusTable(t)
	if !strings.Contains(colored, "\x1b[2m      operator new ") {
		t.Errorf("the second line is not dim:\n%q", colored)
	}
}

// Requirement 5: --since keeps what was created or edited inside the span,
// beside --sev, and the footer counts what it hid.
func TestStatusSinceHidesAnOlderItem(t *testing.T) {
	zone := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = zone })
	item := "---\ntitle: %s\ndeps:\n---\n\n1. Do it.\n"
	dir := markerProject(t, map[string]string{
		"B01-fresh.md": fmt.Sprintf(item, "fresh"),
		"B02-stale.md": fmt.Sprintf(item, "stale"),
	})
	t.Chdir(dir)
	now := time.Now()
	row := func(id string, ago time.Duration) string {
		ts := now.Add(-ago).Format(time.RFC3339)
		return fmt.Sprintf(`{"id":%q,"ts":%q,"date":%q,"origin":{"cmd":"new","actor":"operator","agent":"","session":"","found_in":"","issue":0,"split_from":""}}`, id, ts, ts[:10])
	}
	write(t, filepath.Join(dir, "ledger.jsonl"), row("B1", time.Hour)+"\n"+row("B2", 72*time.Hour)+"\n")
	got := statusTable(t, "--since", "1d")
	if !strings.Contains(got, "fresh") || strings.Contains(got, "stale") {
		t.Errorf("--since 1d kept the wrong items:\n%s", got)
	}
	if !strings.Contains(got, "1 row hidden by --since") {
		t.Errorf("the footer does not count the hidden item:\n%s", got)
	}
	if got := statusTable(t, "--since", "1d", "--sev", "H"); !strings.Contains(got, "no items at that severity") {
		t.Errorf("--since beside --sev H kept an unranked item:\n%s", got)
	}
	resetStatusFlags(t)
	out := capture(t, func() {
		if err := execute(t, "status", "--no-tracker", "--json", "--since", "1d"); err != nil {
			t.Fatal(err)
		}
	})
	var rep struct {
		Items []struct {
			ID     string `json:"id"`
			Origin struct {
				Unrecorded bool   `json:"unrecorded"`
				Text       string `json:"text"`
			} `json:"origin"`
			Created struct {
				Unrecorded bool   `json:"unrecorded"`
				Source     string `json:"source"`
			} `json:"created"`
			Edited struct {
				Unrecorded bool   `json:"unrecorded"`
				Source     string `json:"source"`
			} `json:"edited"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Items) != 1 || rep.Items[0].ID != "B1" || rep.Items[0].Created.Source != "origin" || rep.Items[0].Edited.Source != "origin" ||
		rep.Items[0].Origin.Unrecorded || rep.Items[0].Origin.Text != "operator new" {
		t.Errorf("--json --since 1d carries %+v, want B1 alone with recorded origin, created and edited", rep.Items)
	}
	resetStatusFlags(t)
	if err := execute(t, "status", "--since", "yesterday-ish"); err == nil || !strings.Contains(err.Error(), "3d, 12h") {
		t.Errorf("an unreadable --since returned %v, want the forms it takes", err)
	}
}

// Requirement 6: show reads created from the origin row and carries the same
// objects as status --json; an item with none reads as unrecorded.
func TestShowCarriesTheProvenanceObjects(t *testing.T) {
	provenanceProject(t)
	show := func(id string) Show {
		resetStatusFlags(t)
		out := capture(t, func() {
			if err := execute(t, "show", id, "--json", "--no-tracker"); err != nil {
				t.Fatal(err)
			}
		})
		var sh Show
		if err := json.Unmarshal([]byte(out), &sh); err != nil {
			t.Fatal(err)
		}
		return sh
	}
	sh := show("B5")
	if sh.Origin == nil || sh.Origin.Text != "D20 review of B104" || sh.Created == nil || sh.Created.Source != "origin" ||
		sh.Edited == nil || sh.Edited.Cmd != "request-changes" || sh.Edited.Drive != "D20" {
		t.Errorf("show B5 carries origin %+v created %+v edited %+v", sh.Origin, sh.Created, sh.Edited)
	}
	// The fixture is no git work tree, so B8 has neither an origin row nor a
	// commit: created is the bare unknown object and created_unknown says why.
	if sh := show("B8"); sh.Origin == nil || !sh.Origin.Unrecorded || sh.Created == nil || *sh.Created != (ItemTime{Unrecorded: true}) ||
		sh.CreatedUnknown == "" {
		t.Errorf("show B8 carries origin %+v created %+v (%q), want unrecorded, {unrecorded:true} and a reason", sh.Origin, sh.Created, sh.CreatedUnknown)
	}
	resetStatusFlags(t)
	raw := capture(t, func() {
		if err := execute(t, "show", "B8", "--json", "--no-tracker"); err != nil {
			t.Fatal(err)
		}
	})
	var wire map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["created"]) != `{"unrecorded":true}` && !regexp.MustCompile(`^\{\s*"unrecorded": true\s*\}$`).Match(wire["created"]) {
		t.Errorf("show B8 --json carries created %s, want the {\"unrecorded\": true} docs/automation.md documents", wire["created"])
	}
	resetStatusFlags(t)
	out := capture(t, func() {
		if err := execute(t, "show", "B2", "--no-tracker"); err != nil {
			t.Fatal(err)
		}
	})
	y := time.Now().Year()
	for _, want := range []string{
		fmt.Sprintf("created    %d-03-02  origin claude new", y),
		"edited accept · operator",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show B2 lacks %q:\n%s", want, out)
		}
	}
}

// Requirements 1, 4 and 5 on a body that will not parse: the ledger and the
// file's mtime answer without it, so the row keeps its parse error and still
// carries a second line, all three JSON objects, and the --since cut.
func TestAnUnreadableRowCarriesProvenance(t *testing.T) {
	zone := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = zone })
	dir := markerProject(t, map[string]string{
		"B01-good.md":   "---\ntitle: good\ndeps:\n---\n\n1. Do it.\n",
		"B02-broken.md": "---\ntitle: broken\ndeps:\nunknown: bad\n---\n\n1. Do it.\n",
	})
	t.Chdir(dir)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "work", "B02-broken.md"), old, old); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().Add(-time.Hour).Format(time.RFC3339)
	write(t, filepath.Join(dir, "ledger.jsonl"),
		`{"id":"B1","ts":"`+fresh+`","origin":{"cmd":"new","actor":"operator"}}`+"\n"+
			`{"id":"B2","ts":"2020-01-01T00:00:00Z","origin":{"cmd":"new","actor":"operator"}}`+"\n")

	got := statusTable(t)
	if !regexp.MustCompile(`(?m)^\[!\]   B02 .*unknown frontmatter key "unknown".*\n {6}operator new {16}  2020-01-01    2020-01-01   new · operator$`).MatchString(got) {
		t.Errorf("the unreadable B02 lost its parse error or its second line:\n%s", got)
	}

	type objects struct {
		ID      string      `json:"id"`
		State   string      `json:"state"`
		Error   string      `json:"error"`
		Origin  *ItemOrigin `json:"origin"`
		Created *ItemTime   `json:"created"`
		Edited  *ItemEdit   `json:"edited"`
	}
	statusJSON := func(args ...string) []objects {
		t.Helper()
		resetStatusFlags(t)
		out := capture(t, func() {
			if err := execute(t, append([]string{"status", "--no-tracker", "--json"}, args...)...); err != nil {
				t.Fatal(err)
			}
		})
		var rep struct {
			Items []objects `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		return rep.Items
	}
	var broken *objects
	items := statusJSON()
	for i := range items {
		if items[i].ID == "B02" {
			broken = &items[i]
		}
	}
	switch {
	case broken == nil:
		t.Fatalf("status --json lost B02: %+v", items)
	case broken.State != stateBroken || !strings.Contains(broken.Error, "unknown frontmatter key"):
		t.Errorf("B02 lost its parse error: %+v", *broken)
	case broken.Origin == nil || broken.Origin.Unrecorded || broken.Origin.Text != "operator new":
		t.Errorf("B02 carries origin %+v, want the recorded operator new", broken.Origin)
	case broken.Created == nil || broken.Created.Unrecorded || broken.Created.Source != sourceOrigin || broken.Created.At != "2020-01-01T00:00:00Z":
		t.Errorf("B02 carries created %+v, want the origin row's time", broken.Created)
	case broken.Edited == nil || broken.Edited.Source != sourceOrigin:
		t.Errorf("B02 carries edited %+v, want the origin row", broken.Edited)
	}

	if items := statusJSON("--since", "1d"); len(items) != 1 || items[0].ID != "B1" {
		t.Errorf("--json --since 1d kept %+v, want B1 alone", items)
	}
	if items := statusJSON("--since", "2099-01-01"); len(items) != 0 {
		t.Errorf("--json --since 2099-01-01 kept %+v, want nothing", items)
	}
	got = statusTable(t, "--since", "1d")
	if strings.Contains(got, "B02") || !strings.Contains(got, "1 row hidden by --since") {
		t.Errorf("--since 1d kept the old unreadable row or did not count it:\n%s", got)
	}
}
