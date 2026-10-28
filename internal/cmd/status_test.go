package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/sev"
	"github.com/spf13/pflag"
)

func backlogRows() []Row {
	return []Row{
		{ID: "1", State: stateDone, Level: "verified", Planned: true},
		{ID: "2", State: stateDone, Planned: true},
		{ID: "3", State: stateReady, Planned: true},
		{ID: "B1", State: stateDone, Level: "reviewed"},
		{ID: "B2", State: stateRan},
	}
}

// Hiding merged rows is a view. A footer that shrank with the list would
// restate a finished project as an empty one, which is the failure the split
// between tally and printTable exists to avoid.
func TestTallyIgnoresTheOpenFilter(t *testing.T) {
	all := tally(backlogRows())
	filtered := tally(openRows(backlogRows()))

	if all.plannedTotal != 3 || all.plannedDone != 2 {
		t.Fatalf("planned = %d of %d, want 2 of 3", all.plannedDone, all.plannedTotal)
	}
	if all.intakeTotal != 2 || all.intakeDone != 1 {
		t.Fatalf("intake = %d of %d, want 1 of 2", all.intakeDone, all.intakeTotal)
	}
	if all.unaudited != 1 {
		t.Errorf("unaudited = %d, want 1 (item 2 merged with no level)", all.unaudited)
	}
	// The point of the split: filtering must be unable to reach the arithmetic.
	// Comparing the two tallies proves only that they differ, so the assertion
	// that matters is that printTable is handed the unfiltered slice.
	if filtered.plannedDone != 0 || filtered.plannedTotal != 1 {
		t.Errorf("filtered tally = %d of %d, want 0 of 1 — the fixture must actually drop rows",
			filtered.plannedDone, filtered.plannedTotal)
	}
}

func TestOpenRowsDropsOnlyMerged(t *testing.T) {
	got := openRows(backlogRows())
	if len(got) != 2 {
		t.Fatalf("kept %d rows, want 2", len(got))
	}
	for _, r := range got {
		if r.State == stateDone {
			t.Errorf("%s is done and should have been hidden", r.ID)
		}
	}
}

func TestPlural(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{0, "rows"}, {1, "row"}, {2, "rows"}} {
		if got := plural(c.n, "row", "rows"); got != c.want {
			t.Errorf("plural(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func openIndex(issues ...gh.Issue) gh.Index { return gh.Index{Issues: issues, OK: true} }

// The branch that prompted this: hand-made, owned by no item, and the only
// thing tying it to the work is a number nobody had looked up.
func TestDescribeBranchNamesTheIssueAndItsOwner(t *testing.T) {
	ix := openIndex(gh.Issue{Number: 300, Title: "the check is red on main", Owners: []string{"F2"}})
	got := describeBranch("follow-up/300-make-the-local-check-green", ix, plainRef)
	want := "branch follow-up/300-make-the-local-check-green  #300 the check is red on main  (F2)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestDescribeBranchSaysUnroutedRatherThanGuessing(t *testing.T) {
	ix := openIndex(gh.Issue{Number: 79, Title: "a blocked edge can never be re-requested"})
	if got := describeBranch("follow-up/79-blocked-edge", ix, plainRef); !strings.Contains(got, "(unowned)") {
		t.Errorf("got %q, want it to mark the issue unowned", got)
	}
}

// A number in a branch is not a citation. Reporting one the register never
// confirmed would invent an owner for work that has none.
func TestIssueForBranchReportsNothingWithoutConfirmation(t *testing.T) {
	ix := openIndex(gh.Issue{Number: 300, Title: "the check is red"})
	for _, branch := range []string{
		"release-2024-cutover",           // a year, not an issue
		"follow-up/301-off-by-one",       // no such issue
		"main",                           // no digits at all
		"follow-up/300-and-301-together", // 301 is unknown, 300 is not — still one hit
	} {
		_, ok := issueForBranch(branch, ix)
		if branch == "follow-up/300-and-301-together" {
			if !ok {
				t.Errorf("%q: one confirmed number is a match", branch)
			}
			continue
		}
		if ok {
			t.Errorf("%q resolved to an issue it has no evidence for", branch)
		}
	}
}

// Two confirmed numbers name no single owner, and picking the first would be a
// coin toss dressed as an answer.
func TestIssueForBranchRefusesAmbiguity(t *testing.T) {
	ix := openIndex(gh.Issue{Number: 12, Title: "a"}, gh.Issue{Number: 34, Title: "b"})
	if _, ok := issueForBranch("fix-12-and-34", ix); ok {
		t.Error("two matching issues must resolve to neither")
	}
}

// An unreachable register must not turn into "no item owns it, and no issue
// either" — that reads as a clean answer to a question nobody could ask.
func TestIssueForBranchStaysSilentWhenTheRegisterIsUnreadable(t *testing.T) {
	if _, ok := issueForBranch("follow-up/300-x", gh.Index{OK: false}); ok {
		t.Error("an unreadable register cannot confirm anything")
	}
}

// sevRowsFixture mixes every case the filter has to survive: the four declared
// ranks, a value drover does not recognise, an item that declares nothing, and
// a broken body that has no frontmatter to rank at all.
func sevRowsFixture() []Row {
	return []Row{
		{ID: "1", State: stateReady, Severity: "C", Planned: true},
		{ID: "2", State: stateReady, Severity: "Medium", Planned: true},
		{ID: "3", State: stateDone, Severity: "H", Planned: true},
		{ID: "4", State: stateReady, Severity: "L", Planned: true},
		{ID: "5", State: stateReady, Severity: "Sev1", Planned: true},
		{ID: "6", State: stateReady, Planned: true},
		{ID: "7", State: stateBroken, Planned: true},
		{ID: "B1", State: stateReady, Severity: "C"},
		{ID: "B2", State: stateReady, Severity: "M"},
	}
}

func withSev(t *testing.T, spec ...string) {
	t.Helper()
	want, err := parseSevSpec(spec)
	if err != nil {
		t.Fatalf("parseSevSpec(%v): %v", spec, err)
	}
	sevWanted = want
	t.Cleanup(func() { sevWanted = nil })
}

// withAll sets the scope flag for one test and restores it, the way withSev
// does for --sev. Package state, so a leak reverses the default for every test
// that runs after it.
func withAll(t *testing.T, all bool) {
	t.Helper()
	statusAll = all
	t.Cleanup(func() { statusAll = false })
}

func TestParseSevSpecAcceptsBothSpellingsAndNone(t *testing.T) {
	got, err := parseSevSpec([]string{"c", "High", "none"})
	if err != nil {
		t.Fatalf("parseSevSpec: %v", err)
	}
	for _, r := range []sev.Rank{sev.Critical, sev.High, sev.Unranked} {
		if !got[r] {
			t.Errorf("rank %v missing from the set", r)
		}
	}
	if got[sev.Medium] {
		t.Error("Medium was not requested")
	}
	// Absent flag means no filter at all, which is not the same as an empty set
	// that would match nothing.
	if set, err := parseSevSpec(nil); err != nil || set != nil {
		t.Errorf("parseSevSpec(nil) = %v, %v; want nil, nil", set, err)
	}
}

// Silently matching nothing would read as "no items at that severity", which is
// a wrong answer rather than a degraded one.
func TestParseSevSpecRejectsWhatItCannotRank(t *testing.T) {
	_, err := parseSevSpec([]string{"C", "P0"})
	if err == nil {
		t.Fatal("parseSevSpec accepted P0")
	}
	for _, want := range []string{"P0", "C", "none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// The vocabulary belongs in the refusal, not only in --help: a wrapper that
// exits 3 hands its operator this line and nothing else. Driven through the
// real command so what a shell prints is what gets asserted, and built from
// sev.Letters so a rank added to the ladder cannot leave the message behind.
func TestStatusSevRefusalNamesWhatItWouldHaveTaken(t *testing.T) {
	t.Cleanup(func() { statusSev, sevWanted = nil, nil })
	newProject(t)
	err := refuse(t, "status", "--sev", "P0")
	if err == nil {
		t.Fatal("status accepted a rank nothing ranks")
	}
	if got := Code(err); got != ExitRefused {
		t.Errorf("exit %d, want %d", got, ExitRefused)
	}
	// none is on this list and off follow-up open's: the filter needs a token for
	// the rows carrying no rank, and a row cannot be filed at the absence of one.
	vocab := strings.Join(sev.Letters(), ", ") + ", none"
	if !strings.Contains(err.Error(), vocab) {
		t.Errorf("error %q does not name the accepted values (%s)", err, vocab)
	}
	if !strings.Contains(err.Error(), `"P0"`) {
		t.Errorf("error %q does not name the value it refused", err)
	}
}

func TestKeepNarrowsToTheRanksAsked(t *testing.T) {
	withSev(t, "C")
	got := ids(sevRows(sevRowsFixture()))
	// 7 is broken: it has no readable frontmatter to rank, and hiding the row
	// that says the backlog is damaged is how a filter starts lying.
	want := []string{"1", "7", "B1"}
	assertIDs(t, got, want)
}

func TestKeepMatchesUnrankedOnlyWhenAsked(t *testing.T) {
	withSev(t, "none")
	assertIDs(t, ids(sevRows(sevRowsFixture())), []string{"5", "6", "7"})
}

func TestNoFilterKeepsEverything(t *testing.T) {
	sevWanted = nil
	if got := sevRows(sevRowsFixture()); len(got) != 9 {
		t.Errorf("kept %d rows with no filter, want 9", len(got))
	}
}

// The same doctrine the merged-row scope follows: a filter is a view, and a
// footer that shrank with it would restate the project as smaller than it is.
func TestSevFilteringCannotReachTheArithmetic(t *testing.T) {
	all := tally(sevRowsFixture())
	withSev(t, "C")
	filtered := tally(sevRows(sevRowsFixture()))

	if all.plannedTotal != 7 || all.intakeTotal != 2 {
		t.Fatalf("unfiltered tally = %d planned, %d intake; want 7 and 2", all.plannedTotal, all.intakeTotal)
	}
	if filtered.plannedTotal == all.plannedTotal {
		t.Fatal("the fixture must actually drop rows, or this proves nothing")
	}
	// printTable is handed the unfiltered slice; keep() skips as it prints.
	if got := tally(sevRowsFixture()); got != all {
		t.Error("tally is not a pure function of the rows it is given")
	}
}

// Worst first, but within the planned and intake partitions: the table prints
// an "-- intake --" header at the first unplanned row, so a global sort would
// file planned items under it.
func TestBySeverityKeepsThePartitionAndTheIDOrder(t *testing.T) {
	assertIDs(t, ids(bySeverity(sevRowsFixture())),
		[]string{"1", "3", "2", "4", "5", "6", "7", "B1", "B2"})
}

func ids(rows []Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// table is the golden text a reader gets, minus the footer line under test.
// The three cases differ only in that line and in the gutter, so writing the
// common part once is what makes the difference the subject.
func table(marked bool) string {
	mark := " "
	if marked {
		mark = "▸"
	}
	return "fixture status  (ledger: ledger.jsonl)\n" +
		"\n" +
		"[>]   1           First                    READY\n" +
		"[>]   2           Second                   READY\n" +
		"\n" +
		"-- intake --\n" +
		"[>] " + mark + " B1    bug   Alone                    READY\n" +
		"\n" +
		"0 of 2 planned complete  ·  0 of 1 intake done\n"
}

// The footer is the line that always answers "where am I", and it has three
// forms. Two were rendered by nothing but hand-running the command; the third
// answered "(no item owns it)" for a branch two items owned.
//
// Color off is the form that survives a pipe and NO_COLOR, and it is the form
// this pins: the marker and the footer are the signal, and the color only
// makes them faster to find.
func TestPrintTableMarksHereAndNamesTheBranch(t *testing.T) {
	s := sharedBranchRepo(t)
	// Without gh the register cannot answer, so the footer counts no follow-ups
	// and describeBranch confirms no issue number — the state the goldens below
	// are written for, rather than whatever the host's tracker says today.
	stubPATH(t)
	t.Setenv("NO_COLOR", "1")
	// The title column is sized to the terminal, so the golden would otherwise
	// be a fact about the window the test ran in.
	t.Setenv("COLUMNS", "40")

	for _, c := range []struct {
		name   string
		branch string
		footer string
		marked bool
	}{
		{
			name:   "one item owns the branch",
			branch: "b01-alone",
			marked: true,
			footer: "▸ here: B1  Alone  (branch b01-alone)",
		},
		{
			name:   "no item owns the branch",
			branch: "main",
			footer: "▸ here: branch main  (no item owns it)",
		},
		{
			name:   "several items own the branch",
			branch: "shared",
			footer: "▸ here: branch shared  (1, 2 — name the id)",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			onBranch(t, s, c.branch)
			var buf bytes.Buffer
			s.printTable(&buf, s.rows(), nil, time.Now())
			want := table(c.marked) + "\n" + c.footer + "\n"
			if got := buf.String(); got != want {
				t.Errorf("printTable wrote:\n%s\nwant:\n%s", got, want)
			}
			if strings.Contains(buf.String(), "\x1b") {
				t.Error("colour is off and the output still carries escape bytes")
			}
		})
	}
}

// The marker answers "which item am I on", so it is set only when there is one
// answer. Marking a row on a branch two items render to would pick a winner
// finish has already refused to pick, and --json would carry it as fact.
func TestRowsMarkCurrentOnlyWhenOneItemOwnsTheBranch(t *testing.T) {
	s := sharedBranchRepo(t)
	for _, c := range []struct {
		branch string
		want   string
	}{
		{branch: "b01-alone", want: "B1"},
		{branch: "main", want: ""},
		{branch: "shared", want: ""},
	} {
		t.Run(c.branch, func(t *testing.T) {
			onBranch(t, s, c.branch)
			var marked []string
			for _, r := range s.rows() {
				if r.Current {
					marked = append(marked, r.ID)
				}
			}
			if got := strings.Join(marked, ", "); got != c.want {
				t.Errorf(`rows carrying "current": true = %q, want %q`, got, c.want)
			}
		})
	}
}

// This repo's own summary line: one open row is not a rounding error, it is the
// state a project sits in for most of its life.
func TestRegisterPartsAgreeWithTheirCounts(t *testing.T) {
	issues := func(n int) gh.Index {
		ix := gh.Index{OK: true}
		for i := 0; i < n; i++ {
			ix.Issues = append(ix.Issues, gh.Issue{Number: i + 1})
		}
		return ix
	}
	for _, c := range []struct {
		rows int
		want []string
	}{
		{1, []string{"1 open follow-up (drover follow-ups)"}},
		{6, []string{"6 open follow-ups (drover follow-ups)"}},
	} {
		got := registerParts(issues(c.rows), 300, registerTally{Open: c.rows, Live: c.rows})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("registerParts(%d rows) = %q, want %q", c.rows, got, c.want)
		}
	}
}

// The footer's register clause is the one place the reachability vocabulary was
// missing, so a register nothing reaches read as the ordinary backlog of known
// gaps. Both sides of the threshold are asserted from one table: a widened line
// nobody can get back to the narrow one is the same failure in reverse.
func TestRegisterPartsNameLiveRowsOnlyWhenTheyAreScarce(t *testing.T) {
	for _, c := range []struct {
		name string
		t    registerTally
		want string
	}{
		{"every row live", registerTally{Open: 6, Live: 6}, "6 open follow-ups (drover follow-ups)"},
		{"orphaned minority", registerTally{Open: 6, Live: 4, Orphaned: 2}, "6 open follow-ups (drover follow-ups)"},
		{"unowned minority", registerTally{Open: 6, Live: 4, Unowned: 2}, "6 open follow-ups (drover follow-ups)"},
		{"both, still a minority", registerTally{Open: 6, Live: 4, Orphaned: 1, Unowned: 1}, "6 open follow-ups (drover follow-ups)"},
		{"orphaned majority", registerTally{Open: 6, Live: 2, Orphaned: 4}, "6 open follow-ups · 2 live (drover follow-ups)"},
		{"unowned majority", registerTally{Open: 6, Live: 2, Unowned: 4}, "6 open follow-ups · 2 live (drover follow-ups)"},
		{"both outnumber live", registerTally{Open: 6, Live: 2, Orphaned: 2, Unowned: 2}, "6 open follow-ups · 2 live (drover follow-ups)"},
		{"orphaned only", registerTally{Open: 6, Orphaned: 6}, "6 open follow-ups · 0 live (drover follow-ups)"},
		{"unowned only", registerTally{Open: 6, Unowned: 6}, "6 open follow-ups · 0 live (drover follow-ups)"},
		{"webapp", registerTally{Open: 81, Orphaned: 71, Unowned: 10}, "81 open follow-ups · 0 live (drover follow-ups)"},
		// A tie leaves live in the majority of nothing, and the reader is owed
		// the state that is not the one they assume.
		{"tie", registerTally{Open: 4, Live: 2, Orphaned: 2}, "4 open follow-ups (drover follow-ups)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := registerParts(gh.Index{OK: true}, 300, c.t)
			if !reflect.DeepEqual(got, []string{c.want}) {
				t.Errorf("registerParts(%+v) = %q, want [%q]", c.t, got, c.want)
			}
		})
	}
	// An empty register says nothing about reachability: there is nothing to
	// reach, and "0 live" would read as a project that lost its register.
	if got := registerParts(gh.Index{OK: true}, 300, registerTally{}); got != nil {
		t.Errorf("an empty register produced %q", got)
	}
}

// The footer splits progress from what waits on somebody, and a scaffold counts
// on whichever side of accept its requirements put it.
func TestCountPartsAgreeWithTheirCounts(t *testing.T) {
	for _, c := range []struct {
		c                 counts
		progress, waiting []string
	}{
		{counts{toDraft: 1}, nil, []string{"1 to draft"}},
		{counts{toAccept: 4}, nil, []string{"4 to accept"}},
		{
			counts{plannedTotal: 1, plannedDone: 1, intakeTotal: 16, intakeDone: 16,
				toDraft: 1, toAccept: 2, ran: 1, unaudited: 1, broken: 1},
			[]string{"1 of 1 planned complete", "16 of 16 intake done"},
			[]string{
				"1 to draft",
				"2 to accept",
				"1 awaiting PR",
				"1 merged unaudited (drover unaudited)",
				"1 unreadable",
			},
		},
	} {
		progress, waiting := countParts(c.c)
		if !reflect.DeepEqual(progress, c.progress) || !reflect.DeepEqual(waiting, c.waiting) {
			t.Errorf("countParts(%+v) = %q / %q, want %q / %q", c.c, progress, waiting, c.progress, c.waiting)
		}
	}
	// Every counter at zero is a project with nothing to report, not six zeroes.
	if progress, waiting := countParts(counts{}); progress != nil || waiting != nil {
		t.Errorf("an empty tally produced %q / %q", progress, waiting)
	}
}

// A reviewer recorded and rendered nowhere is the same false negative one layer
// up: the row would say a person looked and every view would still read as
// nobody did. The pre-authorship rows keep their "?" alongside it.
func TestNoteNamesTheRecordedReviewer(t *testing.T) {
	for _, c := range []struct {
		name string
		row  Row
		want string
	}{
		{name: "nobody looked", row: Row{State: stateDone}, want: "done · UNAUDITED"},
		{
			name: "an independent pass with no name is what drover always recorded",
			row:  Row{State: stateDone, Level: "reviewed", By: "agent", Independent: true},
			want: "done · reviewed",
		},
		{
			name: "a human reviewer",
			row:  Row{State: stateDone, Level: "reviewed", By: "agent", Independent: true, Auditor: "sam"},
			want: "done · reviewed (sam)",
		},
		{
			name: "the author, named",
			row:  Row{State: stateDone, Level: "claimed", By: "agent", Auditor: "claude"},
			want: "done · claimed (author, claude)",
		},
		{
			name: "authorship predates the field, and the reviewer does not",
			row:  Row{State: stateDone, Level: "reviewed", Auditor: "sam"},
			want: "done · reviewed (no authorship, sam)",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.row.note(); got != c.want {
				t.Errorf("note() = %q, want %q", got, c.want)
			}
		})
	}
}

// scopeProject is one merged item and one open one, which is the smallest
// backlog where the default scope is visible at all: with no merged row, every
// scope renders identically and a test asserting the default proves nothing.
func scopeProject(t *testing.T) *session {
	t.Helper()
	dir := shelfProject(t, map[string]string{
		"B01-merged.md": body("Merged", ""),
		"B02-open.md":   body("Open", ""),
	}, nil)
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"),
		[]byte(`{"id":"B1","merged":true,"date":"2026-01-01"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolate(t)
	return openIn(t, dir)
}

// The scope flags decide which rows print, and the bug they guard against is a
// boolean pointing the wrong way: that renders a full table where the default
// should be short, or an empty one on a finished project, and neither errors.
// Driven through printTable rather than through openRows, because openRows
// answers "which rows are open" identically whichever way the default points.
func TestDefaultScopeHidesMergedAndAllShowsThem(t *testing.T) {
	stubPATH(t)
	t.Setenv("NO_COLOR", "1")
	t.Setenv("COLUMNS", "80")
	s := scopeProject(t)

	for _, c := range []struct {
		name       string
		all        bool
		wantMerged bool
		wantFooter string
	}{
		{name: "default hides the merged row", wantFooter: "1 done row hidden"},
		{name: "--all shows it", all: true, wantMerged: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			withAll(t, c.all)
			var buf bytes.Buffer
			s.printTable(&buf, s.rows(), nil, time.Now())
			got := buf.String()
			if strings.Contains(got, "Merged") != c.wantMerged {
				t.Errorf("merged row present = %t, want %t:\n%s",
					!c.wantMerged, c.wantMerged, got)
			}
			if !strings.Contains(got, "Open") {
				t.Errorf("the open row is missing from every scope:\n%s", got)
			}
			// The count is the other half: a footer that shrank with the list
			// would restate a finished project as an empty one.
			if c.wantFooter != "" && !strings.Contains(got, c.wantFooter) {
				t.Errorf("footer does not say %q:\n%s", c.wantFooter, got)
			}
			if c.all && strings.Contains(got, "hidden") {
				t.Errorf("--all hid something:\n%s", got)
			}
		})
	}
}

// --open is retired. It stays registered so a wrapper still passing
// it is told what replaced it, rather than handed cobra's unknown-flag error.
func TestOpenIsRemoved(t *testing.T) {
	stubPATH(t)
	resetStatusFlags(t)
	t.Chdir(scopeProject(t).cfg.Root)
	err := execute(t, "status", "--open")
	if got := Code(err); got != ExitRefused {
		t.Fatalf("status --open exited %d (%v), want %d", got, err, ExitRefused)
	}
	want := "--open is not a flag: open items are the default, so drop it; --all is the whole census"
	if err.Error() != want {
		t.Errorf("refusal = %q, want %q", err, want)
	}
	if flags := statusCmd.LocalFlags().FlagUsages(); strings.Contains(flags, "--open") {
		t.Errorf("status --help still lists --open:\n%s", flags)
	}
}

// A scaffold whose list is written waits on accept, not on an author, and the
// note has to send the operator to the step that is actually next.
func TestScaffoldNoteTellsDraftedFromEmpty(t *testing.T) {
	for _, c := range []struct {
		name string
		row  Row
		want string
	}{
		{"empty", Row{ID: "B51", State: stateScaffold}, "scaffold · needs requirements"},
		{"one", Row{ID: "B51", State: stateScaffold, Requirements: 1}, "drafted · 1 req · drover accept B51"},
		{"several", Row{ID: "E47", State: stateScaffold, Requirements: 7}, "drafted · 7 reqs · drover accept E47"},
		{"capped implemented", Row{State: stateRan, Runs: 8, Capped: true}, "implemented · pass 8 · at-limit · validate + PR"},
		{"capped ready", Row{State: stateReady, Capped: true}, "READY · at-limit"},
	} {
		if got := c.row.note(); got != c.want {
			t.Errorf("%s: note() = %q, want %q", c.name, got, c.want)
		}
	}
}

// The scaffold template's bare "1." and "2." are placeholders, not requirements.
func TestWrittenRequirementsSkipsPlaceholders(t *testing.T) {
	for _, c := range []struct {
		body string
		want int
	}{
		{"Requirements:\n\n1.\n2.\n", 0},
		{"Requirements:\n\n1. The form keeps what it can show.\n2.\n", 1},
		{"1. one\n2. two\n3. three\n", 3},
	} {
		if got := writtenRequirements(backlog.Item{Body: c.body}); got != c.want {
			t.Errorf("writtenRequirements(%q) = %d, want %d", c.body, got, c.want)
		}
	}
}

// [!] is for a body drover cannot read. A scaffold needs a person, not a fix.
func TestScaffoldGlyphIsNotBroken(t *testing.T) {
	if glyph(stateScaffold) == glyph(stateBroken) {
		t.Errorf("scaffold and broken share %q", glyph(stateBroken))
	}
}

// COLUMNS is a measurement, so it lifts the cap that bounds the fallback; with
// neither it nor a terminal, the fallback and its cap hold.
func TestTitleWidthCapsOnlyTheFallback(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	if got, want := titleWidthBeside(10), 200-fixedCells-10; got != want {
		t.Errorf("COLUMNS=200: width %d, want %d", got, want)
	}
	t.Setenv("COLUMNS", "")
	if got := titleWidthBeside(10); got != maxTitle {
		t.Errorf("no COLUMNS, no terminal: width %d, want the cap %d", got, maxTitle)
	}
}

func plainRef(n int) string { return "#" + strconv.Itoa(n) }

// prProject is a git project of three open items whose branches the default
// template renders, plus a gh on PATH that logs every call and answers the pull
// request list with script. A script of "" is a gh that fails every call.
func prProject(t *testing.T, script string) (s *session, calls string) {
	t.Helper()
	answer := "echo 'stub gh: HTTP 401: Bad credentials; run gh auth login' >&2; exit 4"
	if script != "" {
		t.Setenv("DROVER_TEST_GH_PRS", script)
		answer = "printf '%s' \"$DROVER_TEST_GH_PRS\""
	}
	return prProjectAnswering(t, answer)
}

// prProjectAnswering is prProject with the pull request list answered by a
// line of shell.
func prProjectAnswering(t *testing.T, answer string) (s *session, calls string) {
	t.Helper()
	dir := shelfProject(t, map[string]string{
		"01-first.md":  body("First", ""),
		"02-second.md": body("Second", ""),
		"B01-alone.md": body("Alone", ""),
	}, nil)
	isolate(t)
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	bin := stubPATH(t)
	calls = filepath.Join(t.TempDir(), "calls")
	t.Setenv("DROVER_TEST_GH_LOG", calls)
	stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho \"$*\" >> \"$DROVER_TEST_GH_LOG\"\n"+
		"case \"$1 $2\" in\n"+
		"'pr list') "+answer+" ;;\n"+
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n"+
		"esac\n")
	t.Setenv("NO_COLOR", "1")
	t.Setenv("COLUMNS", "120")
	return openIn(t, dir), calls
}

// prReads counts the pull request lists among the calls the stub logged.
func prReads(t *testing.T, log string) int {
	t.Helper()
	n := 0
	for _, line := range ghCalls(t, log) {
		if strings.HasPrefix(line, "pr list") {
			n++
		}
	}
	return n
}

// resetStatusFlags clears the flag state another test's execute left on the
// shared commands, now and again afterwards: cobra never resets Changed.
func resetStatusFlags(t *testing.T) {
	reset := func() {
		statusJSON, statusAll, statusOpen, statusBySev, statusNoTracker = false, false, false, false, false
		showJSON, showNoTracker = false, false
		statusSev = nil
		statusTemplate, statusOut, statusForce, statusRender, statusTZ = "", "", false, false, ""
		statusHTML, statusPrintTemplate, statusPrintCSS = false, "", ""
		statusWindow, statusAllProjects = defaultWindow, false
		statusShort, statusSince, sinceCut = false, "", time.Time{}
		for _, fs := range []*pflag.FlagSet{statusCmd.Flags(), showCmd.Flags()} {
			fs.VisitAll(func(f *pflag.Flag) { f.Changed = false })
		}
	}
	reset()
	t.Cleanup(reset)
}

// One read serves the whole table. A query per item is a round trip per row,
// which is what the branch match against one page exists to avoid.
func TestStatusReadsPullRequestsOnceForEveryItem(t *testing.T) {
	_, calls := prProject(t, `[`+
		`{"number":11,"state":"OPEN","isDraft":false,"url":"https://forge.example/o/r/pull/11","headRefName":"first"},`+
		`{"number":12,"state":"OPEN","isDraft":true,"url":"https://forge.example/o/r/pull/12","headRefName":"second"},`+
		`{"number":13,"state":"CLOSED","isDraft":false,"url":"https://forge.example/o/r/pull/13","headRefName":"b01-alone"},`+
		`{"number":14,"state":"OPEN","isDraft":false,"url":"https://forge.example/x/r/pull/14","headRefName":"b01-alone","isCrossRepository":true}]`)
	resetStatusFlags(t)

	var err error
	out := capture(t, func() { err = execute(t, "status") })
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if n := prReads(t, calls); n != 1 {
		t.Errorf("status made %d pull request reads over three items, want 1", n)
	}
	for _, want := range []string{"#11 open", "#12 draft", "#13 closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("status wrote:\n%s\nwant %q in the PR column", out, want)
		}
	}
	if strings.Contains(out, "#14") {
		t.Errorf("a fork's branch sharing the item's name is not the item's pull request:\n%s", out)
	}

	out = capture(t, func() { err = execute(t, "status", "--json") })
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var report StatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, out)
	}
	if got := report.Items[0].PR; got == nil || got.Number != 11 || got.State != "open" || got.Error != "" {
		t.Errorf("items[0].pr = %+v, want #11 open with no error", got)
	}
	if n := prReads(t, calls); n != 2 {
		t.Errorf("status --json made %d more pull request reads, want 1", n-1)
	}
}

// A merged item whose number open-pr recorded answers from the ledger, so a
// table of them asks the forge nothing.
func TestStatusAnswersAMergedItemFromTheLedger(t *testing.T) {
	s, calls := prProject(t, "")
	resetStatusFlags(t)
	for _, row := range []ledger.Row{
		{ID: "1", Branch: "first", PR: 236},
		{ID: "1", Merged: new(true)},
	} {
		if err := s.record(row); err != nil {
			t.Fatal(err)
		}
	}
	out := capture(t, func() {
		if err := execute(t, "status", "--all", "--sev", "none"); err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	if !strings.Contains(out, "#236 merged") {
		t.Errorf("status wrote:\n%s\nwant the recorded #236 merged", out)
	}
	if n := prReads(t, calls); n != 1 {
		t.Errorf("made %d reads, want the one the open rows need", n)
	}
}

// A forge that cannot answer is an unknown state with its reason, never a
// blank that reads as no pull request, and never a failed status.
func TestStatusReportsAFailedPullRequestReadAsUnknown(t *testing.T) {
	_, _ = prProject(t, "")
	resetStatusFlags(t)

	var err error
	out := capture(t, func() { err = execute(t, "status") })
	if err != nil {
		t.Fatalf("status exited non-zero on a failed forge read: %v", err)
	}
	if !strings.Contains(out, "unknown (not authenticated)") {
		t.Errorf("status wrote:\n%s\nwant the PR column to say unknown and why", out)
	}
	if !strings.Contains(out, "First") || !strings.Contains(out, "READY") {
		t.Errorf("status dropped the other columns:\n%s", out)
	}

	out = capture(t, func() { err = execute(t, "status", "--json") })
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var report StatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, out)
	}
	for _, r := range report.Items {
		if r.PR == nil || r.PR.State != "unknown" || !strings.Contains(r.PR.Error, "not authenticated") {
			t.Errorf("%s: pr = %+v, want state unknown with the reason in error", r.ID, r.PR)
		}
	}
}

// status <id> is show <id>, flags included.
func TestStatusWithAnIDPrintsWhatShowPrints(t *testing.T) {
	_, _ = prProject(t, `[{"number":13,"state":"MERGED","isDraft":false,"url":"https://forge.example/o/r/pull/13","headRefName":"b01-alone","statusCheckRollup":[]}]`)
	resetStatusFlags(t)

	for _, flags := range [][]string{nil, {"--json"}} {
		run := func(cmd string) string {
			var err error
			out := capture(t, func() { err = execute(t, append([]string{cmd, "B1"}, flags...)...) })
			if err != nil {
				t.Fatalf("%s B1 %v: %v", cmd, flags, err)
			}
			statusJSON, showJSON = false, false
			return out
		}
		show, status := run("show"), run("status")
		if status != show {
			t.Errorf("status B1 %v wrote:\n%s\nshow B1 %v wrote:\n%s", flags, status, flags, show)
		}
		if !strings.Contains(show, "#13") && !strings.Contains(show, `"number": 13`) {
			t.Errorf("show B1 %v names no pull request:\n%s", flags, show)
		}
	}
	if err := execute(t, "status", "B1", "detail"); err == nil {
		t.Error("status B1 detail: a second positional argument must stay a usage error")
	}
}

// A checkout whose remotes name no GitHub host has no pull requests, which is
// not a read that failed: the table is the one a forge listing none prints,
// byte for byte, and --json and show carry no pr at all. gh tells that user to
// run gh auth login, so not authenticated is the misleading answer to rule out.
func TestStatusOnACheckoutWithNoGitHubRemoteHasNoPRColumn(t *testing.T) {
	noHost := "echo 'none of the git remotes configured for this repository point to a known GitHub host. To tell gh about a new GitHub host, please use `gh auth login`' >&2; exit 1"
	table := func(answer string, args ...string) string {
		_, _ = prProjectAnswering(t, answer)
		resetStatusFlags(t)
		var err error
		out := capture(t, func() { err = execute(t, args...) })
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}
	none := table("printf '[]'", "status")
	got := table(noHost, "status")
	if got != none {
		t.Errorf("no GitHub remote wrote:\n%s\nwant what a forge listing none writes:\n%s", got, none)
	}
	if strings.Contains(got, "not authenticated") || strings.Contains(got, "unknown") {
		t.Errorf("status called a checkout with no GitHub remote a failed read:\n%s", got)
	}

	var report StatusReport
	if err := json.Unmarshal([]byte(table(noHost, "status", "--json")), &report); err != nil {
		t.Fatal(err)
	}
	for _, r := range report.Items {
		if r.PR != nil {
			t.Errorf("%s: pr = %+v, want absent", r.ID, r.PR)
		}
	}
	if out := table(noHost, "show", "B1"); strings.Contains(out, "  pr ") {
		t.Errorf("show B1 printed a pr line for a checkout with no pull requests:\n%s", out)
	}
	if out := table(noHost, "show", "B1", "--json"); strings.Contains(out, `"pr"`) {
		t.Errorf("show B1 --json carries a pr:\n%s", out)
	}
}

// A drive renders offline so a ledger row never waits on the forge, and offline
// alone builds no link. The renderer asks gh for the repository once and every
// render after it links each pull request from that answer.
func TestADrivesRenderLinksPullRequestsAskingTheForgeOnce(t *testing.T) {
	s, _ := prProject(t, "[]")
	if err := s.record(ledger.Row{ID: "1", Branch: "first", PR: 236}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("DROVER_TEST_GH_LOG", calls)
	stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho \"$*\" >> \"$DROVER_TEST_GH_LOG\"\n"+
		"case \"$1 $2\" in\n"+
		"'repo view') printf '{\"url\":\"https://github.com/owner/name/\"}' ;;\n"+
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n"+
		"esac\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	write(t, filepath.Join(s.cfg.Root, "links.tmpl"), "{{range .items}}{{with .pr}}{{pr .number}}{{end}}{{end}}")

	r := &driveRenderer{targets: []config.Render{{Template: "links.tmpl", Path: "links.out"}}}
	for range 2 {
		r.render()
		raw, err := os.ReadFile(filepath.Join(s.cfg.Root, "links.out")) // #nosec G304 -- the fixture's own render
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(raw), "https://github.com/owner/name/pull/236"; got != want {
			t.Errorf("render = %q, want %q", got, want)
		}
	}
	if got := ghCalls(t, calls); len(got) != 1 || !strings.HasPrefix(got[0], "repo view") {
		t.Errorf("two renders made gh calls %q, want one repo view", got)
	}
}

// --no-tracker asks the forge nothing, and building a link is a forge read
// when no URL is in hand. Links are forced on, since a captured stream never
// takes them and the call this guards against would go unseen; the run without
// the flag proves the forcing reached the path.
func TestNoTrackerBuildsNoLinkThroughGh(t *testing.T) {
	s, calls := prProject(t, "[]")
	for _, row := range []ledger.Row{{ID: "1", Branch: "first", PR: 236}, {ID: "1", Merged: new(true)}} {
		if err := s.record(row); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	restore := onTerminal
	onTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { onTerminal = restore })

	for _, args := range [][]string{{"status", "--all", "--no-tracker"}, {"show", "1", "--no-tracker"}} {
		resetStatusFlags(t)
		_ = os.Remove(calls)
		var err error
		out := capture(t, func() { err = execute(t, args...) })
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out, "236") {
			t.Fatalf("%v printed no recorded number to link:\n%s", args, out)
		}
		if got := ghCalls(t, calls); len(got) > 0 {
			t.Errorf("%v made gh calls: %q", args, got)
		}
	}

	resetStatusFlags(t)
	_ = os.Remove(calls)
	capture(t, func() { _ = execute(t, "show", "1") })
	if got := ghCalls(t, calls); !slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "repo view") }) {
		t.Errorf("show 1 with links on made %q, want a repo view: the forced terminal did not reach forgeRef", got)
	}
}
