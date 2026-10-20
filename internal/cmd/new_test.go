package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newProject is a project with no tracker at all, which is the case `new`
// exists for: before it, every path that created an item went through intake,
// and intake refuses here.
func newProject(t *testing.T) string {
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
optional = ["kind", "severity"]
shape = {}

[verify]
default = "true"

[intake]
stop_marker = "STOP: requirements not yet written"

[register]
backend = "none"

# The rest of what drover init writes. Declared so these fixtures assert what
# they are about: a config omitting one is STALE CONFIG's finding, and it would
# fire in every case below that counts findings. shape is empty on purpose —
# shaping a key the lists above do not declare is UNDECLARED KEY's finding. enabled
# is true because a scratch directory is not a work tree and these tests run
# with branch automation on, which is what an unset key already meant here.

[git]
enabled = true
`
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return dir
}

// The scaffold has to be inert on arrival. An item that runs with two empty
// numbered lines produces a checklist nobody can falsify, which is the whole
// reason the marker exists.
func TestNewScaffoldsAnItemRunRefusesUntilAccepted(t *testing.T) {
	dir := newProject(t)
	// runDry is a package-level flag cobra does not reset between Execute calls,
	// so a test that dry-runs leaves every later one dry — and a dry run writes
	// no ledger row.
	t.Cleanup(func() { newSev, runDry = "", false })

	capture(t, func() {
		if err := execute(t, "new", "bug", "the resolver drops a trailing dot"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	path := filepath.Join(dir, "work", "B01-the-resolver-drops-a-trailing-dot.md")
	body, err := os.ReadFile(path) // #nosec G304 -- a path this test just named
	if err != nil {
		t.Fatalf("new wrote no body at %s: %v", path, err)
	}
	for _, want := range []string{
		"title: the resolver drops a trailing dot",
		"kind: bug",
		"severity: M",
		"STOP: requirements not yet written",
		"Requirements:",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("scaffold is missing %q:\n%s", want, body)
		}
	}
	// The marker written is the configured one, not a literal of new's own: a
	// scaffold whose marker run cannot see launches with an empty list.
	if err := execute(t, "run", "B1", "--dry-run"); Code(err) != ExitRefused {
		t.Errorf("run --dry-run exited %d (%v), want %d — the scaffold is not inert",
			Code(err), err, ExitRefused)
	}
	capture(t, func() {
		if err := execute(t, "accept", "B1"); err != nil {
			t.Fatalf("accept: %v", err)
		}
	})
	capture(t, func() {
		if err := execute(t, "run", "B1", "--dry-run"); err != nil {
			t.Errorf("run --dry-run after accept: %v", err)
		}
	})
}

// Allocation reads the directory rather than a counter, so two items in a row
// cannot collide and the second does not have to be told what the first took.
func TestNewAllocatesTheNextIDInItsNamespace(t *testing.T) {
	dir := newProject(t)
	t.Cleanup(func() { newSev = "" })
	for _, c := range []struct{ kind, title, want string }{
		{"bug", "first bug", "B01-first-bug.md"},
		{"bug", "second bug", "B02-second-bug.md"},
		{"feature", "a feature", "F01-a-feature.md"},
	} {
		capture(t, func() {
			if err := execute(t, "new", c.kind, c.title); err != nil {
				t.Fatalf("new %s: %v", c.title, err)
			}
		})
		if _, err := os.Stat(filepath.Join(dir, "work", c.want)); err != nil {
			t.Errorf("new %s %q did not write %s", c.kind, c.title, c.want)
		}
	}
}

func TestNewRefusesWhatItCannotResolve(t *testing.T) {
	newProject(t)
	t.Cleanup(func() { newSev = "" })
	for _, c := range []struct {
		name string
		args []string
		sev  string
		want string
	}{
		{"unknown kind", []string{"new", "chore", "a title"}, "", `unknown kind "chore"`},
		{"unranked severity", []string{"new", "bug", "a title"}, "hihg", `unknown severity "hihg"`},
		{"title with no slug", []string{"new", "bug", "!!!"}, "", "yields no filename component"},
	} {
		t.Run(c.name, func(t *testing.T) {
			newSev = c.sev
			err := execute(t, c.args...)
			if Code(err) != ExitRefused {
				t.Fatalf("exited %d (%v), want %d", Code(err), err, ExitRefused)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not name the cause %q", err, c.want)
			}
		})
	}
}

// The marker is written verbatim from the config. A project that declares none
// would otherwise get a scaffold carrying a literal only new knows, so run
// would launch it with an empty numbered list.
func TestNewRefusesWithoutAStopMarker(t *testing.T) {
	dir := newProject(t)
	conf, err := os.ReadFile(filepath.Join(dir, ".drover.toml")) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.Replace(string(conf), "[intake]\nstop_marker = \"STOP: requirements not yet written\"\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, ".drover.toml"), []byte(stripped), 0o600); err != nil {
		t.Fatal(err)
	}
	err = execute(t, "new", "bug", "a title")
	if Code(err) != ExitRefused {
		t.Fatalf("exited %d (%v), want %d", Code(err), err, ExitRefused)
	}
	if !strings.Contains(err.Error(), "stop_marker") {
		t.Errorf("refusal %q does not name the key to set", err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "work", "*.md")); len(files) != 0 {
		t.Errorf("the refusal still wrote %v", files)
	}
}

// The cap's own refusal tells a reviewer to file the finding and name the id in
// their report. A report reaches the operator and nobody else, so every link
// between a filed finding and the item it came out of is hand work: a deps line
// edited by hand and each issue relabeled. --found-in puts it where whoever
// opens the item will read it.
func TestNewNamesTheItemAFindingCameFrom(t *testing.T) {
	t.Cleanup(func() { newSev, newFoundIn = "", "" })
	dir := newProject(t)
	write(t, filepath.Join(dir, "work", "B01-finish-refuses-after-done.md"),
		"---\ntitle: finish refuses after done\ndeps:\nkind: bug\n---\n\n1. Fix it.\n")

	newFoundIn = "B1"
	out := capture(t, func() {
		if err := execute(t, "new", "bug", "the walk over-excludes on a nullable column"); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	if !strings.Contains(out, "found while working B1") {
		t.Errorf("the output does not say where it came from:\n%s", out)
	}

	bodies, _ := filepath.Glob(filepath.Join(dir, "work", "B*-the-walk-over-excludes*.md"))
	if len(bodies) != 1 {
		t.Fatalf("scaffolded %d bodies, want 1", len(bodies))
	}
	b, err := os.ReadFile(bodies[0]) // #nosec G304 -- written by this test
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "Found while working B1,") {
		t.Errorf("the body does not name the item it came from:\n%s", body)
	}
	// Provenance, not a dependency: reading it as one would block work that could
	// ship today.
	if strings.Contains(body, "deps: B1") {
		t.Error("--found-in wrote a dependency rather than provenance")
	}
	// The scaffold is still a scaffold.
	if !strings.Contains(body, "Requirements:") {
		t.Errorf("the numbered list went missing:\n%s", body)
	}
}

// An id nothing owns is a typo, and a scaffold naming an item that does not exist
// is worse than one naming none.
func TestNewRefusesAFoundInThatIsNotAnItem(t *testing.T) {
	t.Cleanup(func() { newSev, newFoundIn = "", "" })
	newProject(t)

	newFoundIn = "Z99"
	var err error
	capture(t, func() { err = execute(t, "new", "bug", "a title") })
	if err == nil {
		t.Fatal("new accepted a --found-in naming no item")
	}
}

// newFiled scaffolds one item with --found-in and -f, returning its body. The
// flags are package globals cobra does not reset, so they are cleared here
// rather than leaking -f into the next test's plain `new`.
func newFiled(t *testing.T, dir, title, foundIn, description string) (string, string) {
	t.Helper()
	t.Cleanup(func() { newSev, newFoundIn, newFile = "", "", "" })
	newFoundIn, newFile = foundIn, ""
	if description != "" {
		newFile = filepath.Join(t.TempDir(), "finding.md")
		write(t, newFile, description)
	}
	capture(t, func() {
		if err := execute(t, "new", "bug", title); err != nil {
			t.Fatalf("new: %v", err)
		}
	})
	newFoundIn, newFile = "", ""
	bodies, _ := filepath.Glob(filepath.Join(dir, "work", "B*-"+slugify(title)+".md"))
	if len(bodies) != 1 {
		t.Fatalf("scaffolded %d bodies for %q, want 1", len(bodies), title)
	}
	b, err := os.ReadFile(bodies[0]) // #nosec G304 -- written by this test
	if err != nil {
		t.Fatal(err)
	}
	return bodies[0], string(b)
}

// The description goes where the next reader looks first: under the marker,
// under the line naming where it was found, and above the placeholder that a
// human replaces. The marker stays, so the description makes nothing runnable.
func TestNewWritesTheDescriptionBelowProvenanceAndAboveTheRole(t *testing.T) {
	dir := newProject(t)
	t.Cleanup(func() { runDry = false })
	write(t, filepath.Join(dir, "work", "B01-vet-the-allow-list.md"),
		"---\ntitle: vet the allow-list\ndeps:\nkind: bug\n---\n\n1. Fix it.\n")

	desc := "\ninternal/vet/cache.go:88 never lists proxy-cached objects.\n\n    $ drover vet --narrow\n    0 re-vetted\n\n"
	_, body := newFiled(t, dir, "re-vet skips proxy-cached objects", "B1", desc)

	marker := strings.Index(body, "STOP: requirements not yet written")
	prov := strings.Index(body, "Found while working B1, vet the allow-list.")
	text := strings.Index(body, "internal/vet/cache.go:88 never lists proxy-cached objects.\n\n    $ drover vet --narrow\n    0 re-vetted\n\nAct as a <role>.")
	role := strings.Index(body, "Act as a <role>.")
	if marker < 0 || prov < 0 || text < 0 || role < 0 || marker >= prov || prov >= text || text >= role {
		t.Fatalf("want marker < provenance < description < role, got %d %d %d %d:\n%s", marker, prov, text, role, body)
	}

	if err := execute(t, "run", "B2", "--dry-run"); Code(err) != ExitRefused {
		t.Errorf("run --dry-run exited %d (%v), want %d: the description made the scaffold runnable",
			Code(err), err, ExitRefused)
	}

	// A finding that opens with the reproducing command keeps it a code block:
	// the first line's indent is the block, not whitespace to trim.
	_, body = newFiled(t, dir, "found one", "B1", "\n    $ drover vet\n    0 re-vetted\n\ncache.go:88 wrong\n")
	if want := "Found while working B1, vet the allow-list.\n\n    $ drover vet\n    0 re-vetted\n\ncache.go:88 wrong\n\nAct as a <role>."; !strings.Contains(body, want) {
		t.Errorf("the first line lost its indent, want %q in:\n%s", want, body)
	}
}

func TestNewWritesACRLFDescriptionWithLFEndings(t *testing.T) {
	dir := newProject(t)
	_, body := newFiled(t, dir, "crlf finding", "", "cache.go:88 wrong\r\nsecond line\r\n")
	if strings.Contains(body, "\r") {
		t.Errorf("a CRLF description left a carriage return in the body: %q", body)
	}
	if !strings.Contains(body, "cache.go:88 wrong\nsecond line\n\nAct as a <role>.") {
		t.Errorf("the description is not in the body: %q", body)
	}
}

// Without --found-in, a description quoting another scaffold's provenance line
// names where that one was found, not this one, so draft must not read it.
func TestDraftIgnoresAProvenanceLineQuotedInTheDescription(t *testing.T) {
	dir := newProject(t)
	t.Cleanup(func() { draftRunner = "" })
	write(t, filepath.Join(dir, "work", "F01-vet.md"), "---\ntitle: vet\ndeps:\nkind: feature\n---\n\n1. Vet.\n")
	newFiled(t, dir, "quoted provenance", "", "B114 reads:\n\nFound while working F1, vet.\n")
	brief := capture(t, func() {
		if err := execute(t, "draft", "B1"); err != nil {
			t.Fatalf("draft B1: %v", err)
		}
	})
	if strings.Contains(brief, "filed while reviewing F1") {
		t.Errorf("draft took a quoted provenance line for this item's:\n%s", brief)
	}
}

// -f is optional and adds nothing when absent. Pinned to bytes, because a
// scaffold that changed shape for everybody to make room for the flag changes
// what accept deletes and what draft quotes.
func TestNewWithoutADescriptionWritesTodaysBytes(t *testing.T) {
	dir := newProject(t)
	path, body := newFiled(t, dir, "plain scaffold", "", "")
	want := "---\ntitle: plain scaffold\ndeps: \nkind: bug\nseverity: M\n---\n\n" +
		"STOP: requirements not yet written\n" +
		"drover refuses to launch this item while this line is present. Delete it\n" +
		"once the list below is real.\n\n" +
		"Act as a <role>. <One paragraph in your own words: what is broken, or what\n" +
		"must exist, and where in the codebase it lives. Name files.>\n\n" +
		"Requirements:\n\n1.\n2.\n\n"
	if body != want {
		t.Errorf("%s:\n got %q\nwant %q", path, body, want)
	}
}

func TestNewRefusesADescriptionItCannotWrite(t *testing.T) {
	dir := newProject(t)
	t.Cleanup(func() { newFile = "" })
	for _, c := range []struct{ name, text, want string }{
		{"empty", "  \n\t\n", "Omit -f"},
		{"carries the stop marker", "what I saw\n\nSTOP: requirements not yet written\n", `"STOP: requirements not yet written"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			newFile = filepath.Join(t.TempDir(), "finding.md")
			write(t, newFile, c.text)
			err := execute(t, "new", "bug", "a title")
			if Code(err) != ExitRefused {
				t.Fatalf("exited %d (%v), want %d", Code(err), err, ExitRefused)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not say %q", err, c.want)
			}
			if files, _ := filepath.Glob(filepath.Join(dir, "work", "*.md")); len(files) != 0 {
				t.Errorf("the refusal still wrote %v", files)
			}
		})
	}
}

// A scaffold filed without -f holds only the provenance line, and the
// description is in the audit note on the item it names. draft is where whoever
// writes the requirements reads, so the note has to reach it.
func TestDraftCarriesTheAuditNotesOfTheItemAFindingCameFrom(t *testing.T) {
	dir := newProject(t)
	t.Cleanup(func() { draftRunner = "" })
	write(t, filepath.Join(dir, "work", "F01-narrow-the-allow-list.md"),
		"---\ntitle: narrow the allow-list\ndeps:\nkind: feature\n---\n\n1. Narrow it.\n")
	write(t, filepath.Join(dir, "work", "F02-quiet-item.md"),
		"---\ntitle: quiet item\ndeps:\nkind: feature\n---\n\n1. Nothing.\n")
	write(t, filepath.Join(dir, "ledger.jsonl"),
		`{"id":"F1","level":"claimed","code_by":"agent","note":"older note on cached copies","date":"2026-10-07"}`+"\n"+
			`{"id":"F2","implementer":"someone","merged":true,"date":"2026-10-08"}`+"\n"+
			`{"id":"F1","level":"reviewed","code_by":"agent","independent":true,"note":"NEW-ITEM: unversioned proxy-cached objects are never re-vet targets","date":"2026-10-09"}`+"\n")

	newFiled(t, dir, "re-vet skips proxy-cached objects", "F1", "")
	newFiled(t, dir, "a quiet finding", "F2", "")

	brief := capture(t, func() {
		if err := execute(t, "draft", "B1"); err != nil {
			t.Fatalf("draft B1: %v", err)
		}
	})
	newer := strings.Index(brief, "--- audit note on F1, 2026-10-09, reviewed, code by agent ---\nNEW-ITEM: unversioned proxy-cached objects are never re-vet targets\n")
	older := strings.Index(brief, "--- audit note on F1, 2026-10-07, claimed, code by agent ---\nolder note on cached copies\n")
	if newer < 0 || older < 0 || newer > older {
		t.Errorf("want both notes on F1, newest first (newer at %d, older at %d):\n%s", newer, older, brief)
	}
	if !strings.Contains(brief, "the reviewer's\ndescription of the finding") {
		t.Errorf("the notes are not labeled as the reviewer's description:\n%s", brief)
	}

	quiet := capture(t, func() {
		if err := execute(t, "draft", "B2"); err != nil {
			t.Fatalf("draft B2: %v", err)
		}
	})
	if !strings.Contains(quiet, "the ledger records no audit notes against F2.") {
		t.Errorf("an item with no notes is not reported as having none:\n%s", quiet)
	}
	if strings.Contains(quiet, "--- audit note") {
		t.Errorf("a note from another item reached the prompt:\n%s", quiet)
	}
}
