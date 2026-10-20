package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/ledger"
)

// closesProject is a project declaring the key finish acts on, with one item
// naming three issues.
func closesProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	conf := `schema_version = 2
project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"

[ids]
namespaces = { B = "bug", F = "feature" }

[frontmatter]
required = ["title", "deps"]
optional = ["closes"]

[frontmatter.shape]
closes = "intlist"

[github]
repo = "owner/name"

[verify]
default = "true"
`
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\ntitle: Refusals reach no audit row\ndeps:\ncloses: 47 100 103\n---\n\n" +
		"Requirements:\n\n1. Write the row.\n"
	if err := os.WriteFile(filepath.Join(dir, "work", "B01-refusals.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readLedger(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl")) // #nosec G304 -- a fixture this test wrote
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

// The number has to appear in the item's own key. drover writes each of these
// to a tracker other people read, so a typo caught here costs nothing and the
// same typo caught nowhere lands as a comment on a stranger's report.
func TestResolveRefusesAnIssueTheItemDoesNotLink(t *testing.T) {
	dir := closesProject(t)
	t.Chdir(dir)

	err := execute(t, "resolve", "B1", "999", "fixed", "--code-by", "agent")
	if err == nil {
		t.Fatal("resolve accepted an issue the item does not claim")
	}
	if code := Code(err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	// The fixture's register is off, so the hint reports what it could not read
	// rather than asserting the issue is unrouted, and the refusal offers the flag
	// that absorbs it instead of only the separate command.
	for _, want := range []string{"#999", "closes", "#47", "went unread", "--link"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if got := readLedger(t, dir); got != "" {
		t.Errorf("a refused resolve wrote a row: %s", got)
	}
}

func TestResolveRefusesWhatItCannotRecord(t *testing.T) {
	dir := closesProject(t)
	t.Chdir(dir)

	for _, c := range []struct {
		name string
		args []string
	}{
		{"unknown disposition", []string{"resolve", "B1", "47", `satisfied`, "--code-by", "agent"}},
		{"no authorship", []string{"resolve", "B1", "47", "fixed"}},
		{"unknown authorship", []string{"resolve", "B1", "47", "fixed", "--code-by", "author"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			resolveCodeBy = ""
			err := execute(t, c.args...)
			if err == nil {
				t.Fatal("accepted")
			}
			if code := Code(err); code != 3 {
				t.Errorf("exit %d, want 3", code)
			}
			if c.name == "unknown disposition" {
				for _, want := range []string{"fixed", "partial", "unrelated"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not name %q: %v", want, err)
					}
				}
			}
		})
	}
	if got := readLedger(t, dir); got != "" {
		t.Errorf("a refused resolve wrote a row: %s", got)
	}
}

// Correctable in either direction until finish applies it, which is the audit
// ladder's rule and the reason the file is append-only rather than edited.
func TestResolveRecordsAndCorrects(t *testing.T) {
	dir := closesProject(t)
	t.Chdir(dir)

	resolveCodeBy = ""
	if err := execute(t, "resolve", "B1", "47", "fixed", "--code-by", "agent", "traced to the handler"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	resolveCodeBy = ""
	if err := execute(t, "resolve", "B1", "47", "partial", "--code-by", "agent", "the migration is B2"); err != nil {
		t.Fatalf("correcting resolve: %v", err)
	}

	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	res, ok := l.Resolution("B1", 47)
	if !ok {
		t.Fatal("nothing recorded for #47")
	}
	if res.Disposition != ledger.DispositionPartial || res.Note != "the migration is B2" {
		t.Errorf("newest row did not win: %+v", res)
	}
	if len(l.Rows) != 2 {
		t.Errorf("%d rows, want 2 — a correction appends rather than replaces", len(l.Rows))
	}
	// The other two are still open questions, and nothing may quietly answer them.
	if left := undisposed(l, "B1", []int{47, 100, 103}); len(left) != 2 {
		t.Errorf("undisposed: %v, want #100 and #103", left)
	}
}

// Once finish has written to the tracker the record is public, and the ledger
// is not where it gets corrected.
func TestResolveRefusesAfterFinishApplied(t *testing.T) {
	dir := closesProject(t)
	t.Chdir(dir)

	path := filepath.Join(dir, "ledger.jsonl")
	if err := ledger.Append(path, ledger.Row{ID: "B1", Issue: 47,
		Applied: ledger.AppliedClosed, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}

	resolveCodeBy = ""
	err := execute(t, "resolve", "B1", "47", "partial", "--code-by", "agent")
	if err == nil {
		t.Fatal("resolve rewrote a disposition the tracker already carries")
	}
	if code := Code(err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if !strings.Contains(err.Error(), ledger.AppliedClosed) {
		t.Errorf("refusal does not say what the tracker already has: %v", err)
	}
}

// The refusal's middle line is the one a reviewer acts on, and saying the same
// thing about every number (that a disposition lands on a stranger's report) is
// wrong for an issue labeled for the item under review. That is the common case:
// a review pass that fixes a follow-up routed to its own item meets it every
// time. Each branch is asserted to say what it is rather
// than merely to differ from the others, since four sentences nobody reads are no
// better than one.
func TestLinkRouteSeparatesWhoseReportItIs(t *testing.T) {
	tests := []struct {
		name   string
		owners []string
		known  bool
		deps   []string
		want   string
	}{
		{
			name:   "owned by the item under review",
			owners: []string{"E27"},
			known:  true,
			want:   "uncontested",
		},
		{
			name:   "owned by a dep the item declares",
			owners: []string{"B12"},
			known:  true,
			deps:   []string{"B12", "F3"},
			want:   "inherited",
		},
		{
			name:   "owned by an item this one has nothing to do with",
			owners: []string{"B12"},
			known:  true,
			deps:   []string{"F3"},
			want:   "reaches a stranger",
		},
		{
			name:   "carries no owner link at all",
			owners: nil,
			known:  true,
			want:   "Nothing names an owner",
		},
		{
			// Unreachable, or a project whose register is not GitHub. The refusal
			// stands either way, so this reports what it does not know rather than
			// asserting the issue is unrouted.
			name:   "never read",
			owners: nil,
			known:  false,
			want:   "went unread",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := linkRoute(436, tt.owners, tt.known, "E27", tt.deps)
			if !strings.Contains(got, tt.want) {
				t.Errorf("linkRoute = %q, want it to say %q", got, tt.want)
			}
			if !strings.Contains(got, "#436") {
				t.Errorf("linkRoute = %q, want the number in it", got)
			}
		})
	}

	// The warning has to be absent where it is false, not merely outweighed by the
	// rest of the sentence: a reviewer who sees "stranger" stops reading.
	own := linkRoute(436, []string{"E27"}, true, "E27", nil)
	if strings.Contains(own, "stranger") {
		t.Errorf("an issue owned by the item still warns about a stranger's report: %q", own)
	}
}

// Settled warns and writes. Refusing would turn agreement into a lock, and the
// supersede rule is the only way an early wrong answer gets corrected.
func TestResolveWarnsOverASettledAnswerAndStillRecordsIt(t *testing.T) {
	for _, c := range []struct {
		name, third, note string
		want              []string
	}{
		{"the same answer again", "fixed", "still true", []string{"settled fixed", "changes nothing"}},
		{"a reopening with a note", "partial", "the migration moved to B2", []string{"settled fixed", "moves it to partial"}},
		{"a reopening with no note", "partial", "", []string{"with no note", "Say what changed"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Cleanup(func() { resolveCodeBy, resolveLink = "", false })
			dir := closesProject(t)
			t.Chdir(dir)
			for i := 0; i < 2; i++ {
				resolveCodeBy = ""
				if err := execute(t, "resolve", "B1", "47", "fixed", "--code-by", "agent", "first"); err != nil {
					t.Fatalf("seed %d: %v", i, err)
				}
			}
			resolveCodeBy = ""
			args := []string{"resolve", "B1", "47", c.third, "--code-by", "agent"}
			if c.note != "" {
				args = append(args, c.note)
			}
			var out string
			err := func() error {
				var e error
				out = capture(t, func() { e = execute(t, args...) })
				return e
			}()
			if err != nil {
				t.Fatalf("refused a settled answer instead of warning: %v", err)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("output does not say %q:\n%s", want, out)
				}
			}
			// The row is written either way: append-only, and the most recent
			// still wins.
			if n := strings.Count(readLedger(t, dir), `"issue":47`); n != 3 {
				t.Errorf("%d rows for #47, want 3", n)
			}
		})
	}
}

// The Applied refusal is a different class and must not have softened into a
// warning beside the new one.
func TestResolveStillRefusesAnIssueAlreadyOnTheTracker(t *testing.T) {
	t.Cleanup(func() { resolveCodeBy, resolveLink = "", false })
	dir := closesProject(t)
	t.Chdir(dir)
	resolveCodeBy = ""
	if err := execute(t, "resolve", "B1", "47", "fixed", "--code-by", "agent", "first"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "B1", Issue: 47, Applied: ledger.AppliedClosed}); err != nil {
		t.Fatal(err)
	}
	resolveCodeBy = ""
	err := execute(t, "resolve", "B1", "47", "partial", "--code-by", "agent", "second")
	if err == nil {
		t.Fatal("accepted a row over an applied disposition")
	}
	if code := Code(err); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
}
