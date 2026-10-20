package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// shelfProject writes a project whose only variable is whether it configures a
// shelf, so anything that differs between two of them is caused by the key.
func shelfProject(t *testing.T, bodies, shelved map[string]string) string {
	t.Helper()
	key := ""
	if shelved != nil {
		key = "shelved"
	}
	return shelfProjectAt(t, key, bodies, shelved)
}

// shelfProjectAt is shelfProject with the shelf key written independently of
// the directory, which is the only way to build the two states where they
// disagree: a key naming nothing, and a key naming the bodies directory.
func shelfProjectAt(t *testing.T, shelfKey string, bodies, shelved map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	conf := schemaLine + `project = "fixture"

[paths]
backlog = "work"
ledger = "ledger.jsonl"
`
	if shelfKey != "" {
		conf += fmt.Sprintf("shelf  = %q\n", shelfKey)
	}
	conf += `reports = "reports"

[ids]
namespaces = { B = "bug", F = "feature" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "severity"]
shape = {}

[verify]
default = "true"

[intake]
stop_marker = "STOP: requirements not yet written"

# The rest of what drover init writes. Declared so these fixtures assert what
# they are about: a config omitting one is STALE CONFIG's finding, and it would
# fire in every case below that counts findings. shape is empty on purpose —
# shaping a key the lists above do not declare is UNDECLARED KEY's finding. enabled
# is true because a scratch directory is not a work tree and these tests run
# with branch automation on, which is what an unset key already meant here.

[git]
enabled = true
`
	write := func(sub string, files map[string]string) {
		if files == nil {
			return
		}
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, sub, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	write("work", bodies)
	write("shelved", shelved)
	return dir
}

func body(title, deps string) string {
	return fmt.Sprintf("---\ntitle: %s\ndeps: %s\n---\n\nRequirements:\n\n1. Do it.\n", title, deps)
}

func openIn(t *testing.T, dir string) *session {
	t.Helper()
	t.Chdir(dir)
	s, err := open()
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return s
}

func findings(rep CheckReport, kind string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// Seven shelved F bodies and none live. An
// allocator reading only the bodies directory hands out F1 again, and the
// duplicate check in backlog.Load never sees it, since that map is built while
// reading one directory.
func TestNextIDAllocatesAboveTheShelf(t *testing.T) {
	shelved := map[string]string{}
	for i := 1; i <= 7; i++ {
		shelved[fmt.Sprintf("F%02d-retired-%d.md", i, i)] = "no frontmatter here at all\n"
	}
	s := openIn(t, shelfProject(t, map[string]string{"B01-live.md": body("A bug", "")}, shelved))

	if got := nextID(t, s, "F"); got != "F8" {
		t.Errorf("nextID(F) = %s, want F8", got)
	}
	// The other namespace is unaffected: a shelf is per-id, not per-project.
	if got := nextID(t, s, "B"); got != "B2" {
		t.Errorf("nextID(B) = %s, want B2", got)
	}
}

// A live body above the shelf still wins. The shelf raises the floor; it does
// not become the answer.
func TestNextIDPrefersWhicheverIsHigher(t *testing.T) {
	s := openIn(t, shelfProject(t,
		map[string]string{"F09-live.md": body("Live", "")},
		map[string]string{"F03-retired.md": ""}))

	if got := nextID(t, s, "F"); got != "F10" {
		t.Errorf("nextID(F) = %s, want F10", got)
	}
}

// One id, two bodies. Reviving the shelved one would take over the live item's
// identity, silently: same branch name, same log names, same report path.
func TestCheckReportsAnIDInBothDirectories(t *testing.T) {
	s := openIn(t, shelfProject(t,
		map[string]string{"F01-live.md": body("Live", "")},
		map[string]string{"F01-retired.md": ""}))

	got := findings(s.checkReport(), "SHELF CLASH")
	if len(got) != 1 {
		t.Fatalf("SHELF CLASH findings = %d, want 1", len(got))
	}
	for _, want := range []string{"work/F01-live.md", "shelved/F01-retired.md"} {
		if !strings.Contains(got[0].Message, want) {
			t.Errorf("finding does not name %s: %q", want, got[0].Message)
		}
	}
	if got[0].ID != "F1" {
		t.Errorf("finding id = %q, want F1", got[0].ID)
	}
}

// A half-revived body — copied back but not yet given frontmatter — is the
// worst version of the clash, not an exempt one. It lands in backlog.Broken
// rather than Items, so a check keyed off parsed items never sees it.
func TestCheckSeesAClashWithAnUnreadableBody(t *testing.T) {
	s := openIn(t, shelfProject(t,
		map[string]string{"F01-revived.md": "no frontmatter yet\n"},
		map[string]string{"F01-retired.md": ""}))

	rep := s.checkReport()
	got := findings(rep, "SHELF CLASH")
	if len(got) != 1 {
		t.Fatalf("SHELF CLASH findings = %d, want 1: %+v", len(got), rep.Findings)
	}
	if !strings.Contains(got[0].Message, "work/F01-revived.md") {
		t.Errorf("finding does not name the unreadable body: %q", got[0].Message)
	}
	// UNREADABLE still fires. The clash is an additional fact about the same
	// file, not a replacement for it.
	if len(findings(rep, "UNREADABLE")) != 1 {
		t.Errorf("UNREADABLE was swallowed: %+v", rep.Findings)
	}
}

// The record is not gone, the body moved. A deleted body stays ORPHAN ROW,
// because that is the completion recorded against nothing.
func TestOrphanRowSeparatesShelvedFromDeleted(t *testing.T) {
	dir := shelfProject(t,
		map[string]string{"B01-live.md": body("Live", "")},
		map[string]string{"B09-retired.md": ""})
	rows := `{"id":"B9","merged":true,"implementer":"upstream","date":"2026-01-01"}
{"id":"B12","merged":true,"implementer":"upstream","date":"2026-01-02"}
`
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}

	got := findings(openIn(t, dir).checkReport(), "ORPHAN ROW")
	if len(got) != 1 {
		t.Fatalf("ORPHAN ROW findings = %d, want 1 (B12 only): %+v", len(got), got)
	}
	if got[0].ID != "B12" {
		t.Errorf("ORPHAN ROW names %q, want B12", got[0].ID)
	}
}

// A shelved dep blocks its dependent as hard as a missing one, so the finding
// stays. What changes is the fix it points at.
func TestDanglingSaysShelvedRatherThanMissing(t *testing.T) {
	s := openIn(t, shelfProject(t,
		map[string]string{
			"B01-needs-shelved.md": body("Needs a retired dep", "F1"),
			"B02-needs-nothing.md": body("Needs a dep nobody wrote", "F5"),
		},
		map[string]string{"F01-retired.md": ""}))

	got := findings(s.checkReport(), "MISSING DEP")
	if len(got) != 2 {
		t.Fatalf("MISSING DEP findings = %d, want 2: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "is shelved") || got[0].Detail == "" {
		t.Errorf("B1's dep is shelved, reported as %q / %q", got[0].Message, got[0].Detail)
	}
	if !strings.Contains(got[1].Message, "has no body") {
		t.Errorf("B2's dep was never written, reported as %q", got[1].Message)
	}
}

// A shelved body is never opened, so nothing in it can produce a finding. This
// one would fail every rule the backlog enforces: no fence, an undeclared key,
// and a stop marker that would refuse a run.
func TestShelvedBodiesAreNeverParsed(t *testing.T) {
	s := openIn(t, shelfProject(t,
		map[string]string{"B01-live.md": body("Live", "")},
		map[string]string{
			"F01-broken.md": "---\nmade_up_key: 1\n---\nSTOP: requirements not yet written\n",
			"README.md":     "# What this directory is for\n",
		}))

	if rep := s.checkReport(); !rep.OK {
		t.Errorf("shelf produced findings: %+v", rep.Findings)
	}
}

// Requirement 7 is the whole design rule made falsifiable: the shelf is read
// for ids, never for content, so configuring one changes nothing a reader sees.
func TestShelfChangesNothingRendered(t *testing.T) {
	// gh is off PATH, so the register degrades the same way in both halves
	// rather than answering from whatever this host happens to be logged into.
	stubPATH(t)

	bodies := map[string]string{
		"01-planned.md": body("A planned item", ""),
		"B01-a-bug.md":  body("A bug", "1"),
		"F02-live.md":   body("A live feature", ""),
	}
	shelved := map[string]string{
		"F01-retired.md":  "STOP: requirements not yet written\n",
		"F03-retired.md":  "---\nnope\n",
		"B04-upstream.md": "",
	}

	plain := shelfProject(t, bodies, nil)
	shelf := shelfProject(t, bodies, shelved)

	bare := render(t, plain)
	if got := render(t, shelf); got != bare {
		t.Errorf("a configured shelf changed rendered output:\n--- without ---\n%s\n--- with ---\n%s", bare, got)
	}

	without, with := openIn(t, plain), openIn(t, shelf)
	if n := len(with.shelf.IDs()); n != 3 {
		t.Fatalf("fixture shelf holds %d ids, want 3", n)
	}
	if a, b := tally(without.rows()), tally(with.rows()); a != b {
		t.Errorf("counts differ: %+v vs %+v", a, b)
	}
	a, b := without.checkReport(), with.checkReport()
	// Config names the file each report was validated against, and the two
	// fixtures are two directories. Comparing it would assert the temp dirs
	// match, which is not what a shelf could ever change.
	a.Config, b.Config = nil, nil
	if !reflect.DeepEqual(a, b) {
		t.Errorf("check report differs:\n%+v\n%+v", a, b)
	}
}

// render is every surface a shelf must not reach: the table, one item of each
// kind, and the counts check prints.
func render(t *testing.T, dir string) string {
	t.Helper()
	t.Chdir(dir)
	return capture(t, func() {
		for _, args := range [][]string{{"status"}, {"show", "1"}, {"show", "B1"}, {"show", "F2"}, {"check"}} {
			if err := execute(t, args...); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
		}
	})
}

// capture swaps os.Stdout for a pipe, which is where these commands print.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	return captureStream(t, &os.Stdout, fn)
}

// captureErr is the same for the other stream: a progress line a command sends
// to stderr so --json stays parseable is invisible to capture.
func captureErr(t *testing.T, fn func()) string {
	t.Helper()
	return captureStream(t, &os.Stderr, fn)
}

// captureStream reads after the write end closes, which is safe only because
// the output is far below the pipe buffer; anything larger would deadlock.
func captureStream(t *testing.T, stream **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := *stream
	*stream = w
	fn()
	*stream = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A shelf drover cannot open reads as no shelf at all, and every behavior the
// key prevents comes back with nothing saying why. An unset key stays silent,
// or upgrading would hand every project a finding.
func TestCheckReportsAShelfPathThatDoesNotExist(t *testing.T) {
	bodies := map[string]string{"B01-live.md": body("Live", "")}

	rep := openIn(t, shelfProjectAt(t, "retired", bodies, nil)).checkReport()
	got := findings(rep, "NO SHELF")
	if len(got) != 1 {
		t.Fatalf("NO SHELF findings = %d, want 1: %+v", len(got), got)
	}
	// A directory with no stat is the first case the identity check meets, and
	// an error there would trade this finding for a different one.
	if dir := findings(rep, "SHELF IS BACKLOG"); len(dir) != 0 {
		t.Errorf("a shelf that does not exist read as the bodies directory: %+v", dir)
	}
	if got[0].Path != "retired" || !strings.Contains(got[0].Message, "retired") {
		t.Errorf("finding does not name the path: %q / %q", got[0].Message, got[0].Path)
	}

	if rep := openIn(t, shelfProject(t, bodies, nil)).checkReport(); !rep.OK {
		t.Errorf("an unset shelf produced findings: %+v", rep.Findings)
	}
}

// One directory cannot be both. Every live body would read as shelved, which
// suppresses the orphan and dep checks the shelf only ever softens for retired
// work — and the clash it does raise names one file as two.
func TestCheckReportsAShelfPointedAtBodies(t *testing.T) {
	s := openIn(t, shelfProjectAt(t, "work", map[string]string{
		"B01-live.md":   body("Live", ""),
		"F07-broken.md": "no frontmatter here at all\n",
	}, nil))

	rep := s.checkReport()
	got := findings(rep, "SHELF IS BACKLOG")
	if len(got) != 1 {
		t.Fatalf("SHELF IS BACKLOG findings = %d, want 1: %+v", len(got), rep.Findings)
	}
	if !strings.Contains(got[0].Message, "work") || got[0].Detail == "" {
		t.Errorf("finding does not name the collision: %q / %q", got[0].Message, got[0].Detail)
	}
	if clash := findings(rep, "SHELF CLASH"); len(clash) != 0 {
		t.Errorf("SHELF CLASH named a file as two: %+v", clash)
	}
}

// A symlinked shelf is one directory under two names, and the string
// comparison this replaced saw two. It reproduces on every platform, which the
// case variant below does not.
func TestCheckSeesASymlinkedShelfAsOneDirectory(t *testing.T) {
	dir := shelfProjectAt(t, "link", map[string]string{"B01-live.md": body("Live", "")}, nil)
	if err := os.Symlink(filepath.Join(dir, "work"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	rep := openIn(t, dir).checkReport()
	if got := findings(rep, "SHELF IS BACKLOG"); len(got) != 1 {
		t.Fatalf("SHELF IS BACKLOG findings = %d, want 1: %+v", len(got), rep.Findings)
	}
	if clash := findings(rep, "SHELF CLASH"); len(clash) != 0 {
		t.Errorf("SHELF CLASH named one file as two: %+v", clash)
	}
}

// It reproduces only where the filesystem folds case,
// which is the macOS default and not CI's, so this probes for the property
// rather than asserting a comparison that passes on Linux for the wrong reason.
func TestCheckSeesACaseVariantShelfAsOneDirectory(t *testing.T) {
	dir := shelfProjectAt(t, "Work", map[string]string{"B01-live.md": body("Live", "")}, nil)
	if !foldsCase(t, dir) {
		t.Skip("case-sensitive filesystem: Work and work are two directories here")
	}

	rep := openIn(t, dir).checkReport()
	got := findings(rep, "SHELF IS BACKLOG")
	if len(got) != 1 {
		t.Fatalf("SHELF IS BACKLOG findings = %d, want 1: %+v", len(got), rep.Findings)
	}
	// Both spellings, because "both name work" describes a config nobody wrote.
	for _, want := range []string{"Work", "work"} {
		if !strings.Contains(got[0].Message, want) {
			t.Errorf("finding does not name %s: %q", want, got[0].Message)
		}
	}
	if clash := findings(rep, "SHELF CLASH"); len(clash) != 0 {
		t.Errorf("SHELF CLASH named one file as two: %+v", clash)
	}
}

// foldsCase reports whether this filesystem resolves two spellings of one
// directory to the same thing, asked of the tree under test rather than of the
// platform, since a case-sensitive volume can be mounted on either.
func foldsCase(t *testing.T, dir string) bool {
	t.Helper()
	lower, err := os.Stat(filepath.Join(dir, "work"))
	if err != nil {
		t.Fatal(err)
	}
	upper, err := os.Stat(filepath.Join(dir, "Work"))
	if err != nil {
		return false
	}
	return os.SameFile(lower, upper)
}
