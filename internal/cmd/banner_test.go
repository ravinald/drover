package cmd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/runner"
)

// Everything drover resolved, in one block. The fields exist because an
// operator watching a launch has no other way to see them: the body and the
// template decide what the agent is told, and a wrong one produces a session
// that looks entirely normal until the diff arrives.
func TestLaunchInfoNamesEverythingItResolved(t *testing.T) {
	var out strings.Builder
	launchInfo{
		ID: "B75", Stage: "implement", Branch: "b75-slug", Round: 2,
		Item: "work/B75-slug.md", Prompt: "templates/prompt.md",
		Runner: "claude", Binary: "/usr/local/bin/claude",
		Argv: "claude -p <prompt>", Log: "/tmp/B75.jsonl",
	}.write(&out)

	for _, want := range []string{
		"==> B75 · implement · branch b75-slug · round 2",
		"==> item   work/B75-slug.md",
		"==> prompt templates/prompt.md",
		"==> runner claude: /usr/local/bin/claude",
		"==> argv   claude -p <prompt>",
		"==> log    /tmp/B75.jsonl",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("preamble does not carry %q:\n%s", want, out.String())
		}
	}
}

// A label with nothing behind it reads as a value drover resolved to nothing,
// which is a different fact from one the command never had. An interactive
// session writes no log, and a single-pass command counts no rounds.
func TestLaunchInfoSkipsWhatTheCallerLeftEmpty(t *testing.T) {
	var out strings.Builder
	launchInfo{ID: "B75", Stage: "review", Branch: "b75-slug",
		Item: "work/B75-slug.md", Prompt: "built-in review",
	}.from("codex", runner.Spec{Interactive: []string{"codex", "exec"}}, runner.Interactive).write(&out)

	got := out.String()
	for _, unwanted := range []string{"log", "round"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("preamble invents a %s line:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "==> runner codex: ") {
		t.Errorf("preamble lost the runner:\n%s", got)
	}
}

// A project with no branches gets the reason in the branch's place, because
// "no branch" and "branch missing" are the two readings of an absence and only
// one of them is a problem.
func TestLaunchInfoReportsWhyThereIsNoBranch(t *testing.T) {
	var out strings.Builder
	launchInfo{ID: "B75", Stage: "implement",
		NoGit: "in /srv/proj (no branch: [git] enabled is false)",
	}.write(&out)
	if !strings.Contains(out.String(), "· in /srv/proj (no branch:") {
		t.Errorf("preamble drops the reason there is no branch:\n%s", out.String())
	}
}

// The one distinction printing the template is for. A project that meant to
// override one and left the key unset runs on drover's default, and the
// rendered prompt reads like a prompt either way.
func TestPromptPathSeparatesTheProjectTemplateFromTheBuiltIn(t *testing.T) {
	s := fixture(t, "")
	if got := s.promptPath("", "default"); got != "built-in default" {
		t.Errorf("unset template: got %q, want %q", got, "built-in default")
	}
	if got := s.promptPath("templates/prompt.md", "default"); got != "templates/prompt.md" {
		t.Errorf("declared template: got %q, want %q", got, "templates/prompt.md")
	}
}

// What a drive commits to, before it takes the terminal. Every field here is
// one an operator would otherwise confirm by reading .drover.toml and the flags
// they just typed back to themselves.
func TestDriveHeaderNamesTheQueueTheRunnersAndTheBounds(t *testing.T) {
	t.Cleanup(func() { driveRunner, driveMax, driveNoMerge = "", 0, false })
	s := fixture(t, "\n[review]\nmax_review_rounds = 3\n")
	driveMax = 2

	var out strings.Builder
	s.driveHeader(&out, s.backlog.Items, "codex", time.Time{})
	got := out.String()

	for _, want := range []string{
		"==> drive · 1 item: 1",
		"implement per item (default claude) · review codex",
		"prompts: implement built-in default · review built-in review",
		"bounds: unbounded, at most 2 items · merge: [drive] merge_gate decides",
		"max review rounds: 3 per item",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("header does not carry %q:\n%s", want, got)
		}
	}
}

// --runner names one implementer for the whole queue, so the header says that
// rather than "per item", which would send an operator to check frontmatter
// the flag has already overridden.
func TestDriveHeaderNamesAnOverriddenImplementer(t *testing.T) {
	t.Cleanup(func() { driveRunner, driveNoMerge = "", false })
	s := fixture(t, "")
	driveRunner, driveNoMerge = "gemini", true

	var out strings.Builder
	s.driveHeader(&out, s.backlog.Items, "codex", time.Now().Add(time.Hour))
	got := out.String()

	if !strings.Contains(got, "implement gemini · review codex") {
		t.Errorf("header does not name the overridden implementer:\n%s", got)
	}
	if !strings.Contains(got, "--no-merge: every item stops green-and-reviewed") {
		t.Errorf("header does not say the merge is held:\n%s", got)
	}
	if !strings.Contains(got, "bounds: until ") {
		t.Errorf("header does not carry the deadline:\n%s", got)
	}
}

// The three forms a printed path takes. The log directory sits outside the tree
// by default, so the relative form alone would render it as a stack of "..".
func TestDisplayPathPicksTheFormAnOperatorCanPaste(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "git", "proj")

	for _, c := range []struct{ name, path, want string }{
		{"inside the tree", filepath.Join(root, ".drover", "logs", "B75.jsonl"),
			filepath.Join(".drover", "logs", "B75.jsonl")},
		{"under home", filepath.Join(home, ".cache", "drover", "proj", "B75.jsonl"),
			filepath.Join("~", ".cache", "drover", "proj", "B75.jsonl")},
		{"home itself", home, "~"},
		{"outside both", filepath.Join("/srv", "logs", "B75.jsonl"),
			filepath.Join("/srv", "logs", "B75.jsonl")},
		{"nothing", "", ""},
	} {
		if got := displayPath(root, c.path); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A sibling of the project root starts with the same bytes as the root and is
// not inside it. Comparing prefixes rather than the relative path is how such a
// directory comes to render as a path inside the tree that is not there.
func TestDisplayPathDoesNotClaimASiblingDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "git", "proj")

	sibling := filepath.Join(home, "git", "proj-notes", "B75.jsonl")
	want := filepath.Join("~", "git", "proj-notes", "B75.jsonl")
	if got := displayPath(root, sibling); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
