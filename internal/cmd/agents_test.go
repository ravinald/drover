package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runner"
)

func fixture(t *testing.T, extra string) *session {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module x\n")
	write(t, filepath.Join(dir, config.FileName), schemaLine+`project = "example"

[paths]
backlog      = "work"
ledger      = "ledger.jsonl"
reports     = "reports"
specs       = "docs"
skills      = "guides"
root_marker = "go.mod"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
optional = ["api", "ui_only"]

[intake]
stop_marker = "STOP: requirements not yet written"

[verify]
default = "make test"
api     = "make test-api"
ui_only = "make test-ui"

[github]
repo         = "owner/example"
follow_up_label   = "hazard"
severity_prefix   = "severity:"
`+extra)
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n")

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("fixture config does not load: %v", err)
	}
	c, err := backlog.Load(cfg.Abs(cfg.Paths.Backlog), cfg.Prefixes(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load(cfg.Abs(cfg.Paths.Ledger))
	if err != nil {
		t.Fatal(err)
	}
	return &session{cfg: cfg, backlog: c, ledger: l}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The generator exists so instructions cannot describe a project other than the
// one they sit in. Every value below is one a hand-filled copy got wrong at
// least once by outliving the config it was copied from.
func TestAgentsRendersProjectValues(t *testing.T) {
	s := fixture(t, "\n[agents]\nprinciples = \"no raw SQL, permissions per object\"\n")
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"work/<NN>-<slug>.md",                   // paths.backlog
		"`B` — bug",                             // ids.namespaces
		"`hazard`",                              // github.follow_up_label
		"`severity:<critical|high|medium|low>`", // github.severity_prefix
		"`Owner: <owning id>`",                  // the owner link
		"`make test`",                           // the default gate
		"`api: yes` | `make test-api`",
		"STOP: requirements not yet written",
		"no raw SQL, permissions per object",
		"`docs/`",                  // paths.specs
		"`guides/<name>/SKILL.md`", // paths.skills
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generated instructions omit %q", want)
		}
	}
}

// A run that carries a gate the instructions never mention is a run whose
// reviewer re-runs the wrong command, and the flagged gates are exactly the ones
// a reader would not guess.
func TestAgentsListsVerifyCommandsInPrecedenceOrder(t *testing.T) {
	s := fixture(t, "")
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	def := strings.Index(body, "| no flag set |")
	api := strings.Index(body, "| `api: yes` |")
	ui := strings.Index(body, "| `ui_only: yes` |")
	if def < 0 || api < 0 || ui < 0 {
		t.Fatalf("verify table incomplete: default=%d api=%d ui_only=%d", def, api, ui)
	}
	// SelectVerify tries names in sorted order, so api wins over ui_only when both
	// are set. A table in any other order teaches the wrong precedence.
	if def >= api || api >= ui {
		t.Errorf("verify table out of precedence order: default=%d api=%d ui_only=%d", def, api, ui)
	}
}

// Unset principles must be visible. A review step that silently loses its
// standard reads as a review with no standard.
func TestAgentsNamesAnUnsetPosture(t *testing.T) {
	s := fixture(t, "")
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "[agents] principles") {
		t.Error("unset principles render without naming the key that fills it")
	}
}

// The markers are the whole reason --write is safe to run repeatedly: a project
// keeps its own prose in the same file, and regeneration must not eat it.
func TestWriteAgentsReplacesOnlyTheMarkedBlock(t *testing.T) {
	s := fixture(t, "")
	path := filepath.Join(s.cfg.Root, "AGENTS.md")
	write(t, path, "# House rules\n\nOurs, not drover's.\n")

	block, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeAgents("AGENTS.md", block); err != nil {
		t.Fatal(err)
	}
	first := read(t, path)
	if !strings.Contains(first, "# House rules") {
		t.Fatal("appending the block destroyed the hand-written preamble")
	}

	// Second pass with a changed config: the block updates, the prose does not
	// move, and nothing accumulates.
	s2 := fixture(t, "")
	s2.cfg.Root = s.cfg.Root
	s2.cfg.Verify["default"] = "make everything"
	block2, err := s2.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.writeAgents("AGENTS.md", block2); err != nil {
		t.Fatal(err)
	}
	second := read(t, path)

	if !strings.Contains(second, "# House rules") {
		t.Error("regeneration ate the hand-written preamble")
	}
	if !strings.Contains(second, "make everything") {
		t.Error("regeneration did not update the block")
	}
	if strings.Contains(second, "`make test`") {
		t.Error("the superseded verify command survived regeneration")
	}
	if n := strings.Count(second, agentsBegin); n != 1 {
		t.Errorf("%d begin markers after two writes, want 1", n)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An agent reading a project's own instructions learns whether that project
// splits implementing from reviewing. Unset, the paragraph stays out: most
// projects run one runner and a line about a second one would describe a shape
// they do not have.
func TestAgentsNamesTheReviewingRunner(t *testing.T) {
	s := fixture(t, "\n[run]\nreview_runner = \"codex\"\n")
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "`codex`") {
		t.Error("generated instructions do not name the reviewing runner")
	}

	plain, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "reviewing runner") {
		t.Error("a project with no review_runner is told it splits the roles")
	}
}

// Instructions an agent cannot follow are worse than instructions it does not
// have: a project with no branches must not be told to wait for a merge.
func TestAgentsDropsTheMergeSectionWithoutGit(t *testing.T) {
	s := fixture(t, "\n[git]\nenabled = false\n")
	out, err := s.renderAgents()
	if err != nil {
		t.Fatalf("renderAgents: %v", err)
	}
	if strings.Contains(out, "drover finish") && !strings.Contains(out, "finish` refuses and exits 3") {
		t.Errorf("instructions still owner through finish:\n%s", out)
	}
	if !strings.Contains(out, "[git] enabled = false") {
		t.Errorf("instructions do not say why there is no merge step:\n%s", out)
	}
}

// The generated block is what an agent reads before it reads any --help, so a
// command's class reaching it while what it changes does not leaves the reader
// with half the contract. `split` is the case: with the class table saying
// local writes and the sentence saying it appends no ledger row living only in
// --help, that copy can drift into claiming the row unnoticed.
func TestAgentsSaysWhatEachCommandChanges(t *testing.T) {
	s := fixture(t, "")
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	vars := varsFor(s.cfg)
	for name, k := range classified {
		if want := substitute(k.effect, vars); !strings.Contains(body, want) {
			t.Errorf("the generated block does not say what %s changes:\n%s", name, want)
		}
	}
	// Checked on the rendered row rather than on the string it came
	// from: the block may not tell an agent split appends a
	// ledger row while split's own Safety line denies it. Read through the same
	// predicate as TestSplitWritesNoLedgerRow, so reversing the decision and
	// giving split the write stays one edit rather than an edit and a test.
	split := tableRow(body, "split")
	if split == "" {
		t.Fatalf("no split row in the generated block:\n%s", body)
	}
	if got, want := mentionsLedgerRow(split), mentionsLedgerRow(splitCmd.Example); got != want {
		t.Errorf("the generated block claims a ledger row for split: %t, its Safety line: %t\n%s",
			got, want, split)
	}
	// Resolved against the session's config rather than the working directory:
	// this test runs from internal/cmd, where a cwd-based load finds drover's
	// own project and renders its paths into another project's instructions.
	if !strings.Contains(body, "Writes one scaffold under work") {
		t.Error("split's effect did not resolve against the fixture's own backlog path")
	}
}

// tableRow returns the generated block's row for one command, from the table
// saying what each one changes. Scoped to a row because the block names most
// commands in its prose too, and a search over the whole text would judge a
// sentence somebody wrote by hand as the generated cell.
func tableRow(body, command string) string {
	prefix := "| `" + command + "` | "
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// A path substituted into a command effect renders as one cell, and only
// there: a path holding a pipe or a newline is legal config, and either would
// split the row it lands in. --help prints the same effect plainly, so the
// escape belongs to the table and not to the sentence.
func TestEffectPathsStayInTheirTableCell(t *testing.T) {
	for _, tc := range []struct {
		name, backlog, wantEffect string
	}{
		{"pipe", "work|items", `work\|items`},
		{"backslash-pipe", `work\|items`, `work\\\|items`},
		{"newline", "work\nitems", "work items"},
		{"crlf", "work\r\nitems", "work  items"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, "")
			s.cfg.Paths.Backlog = tc.backlog
			body, err := s.renderAgents()
			if err != nil {
				t.Fatal(err)
			}

			split := tableRow(body, "split")
			if got := unescapedPipes(split); got != 3 {
				t.Errorf("split row has %d table delimiters, want 3:\n%s", got, split)
			}
			if !strings.Contains(split, "Writes one scaffold under "+tc.wantEffect+" and") {
				t.Errorf("split row does not render the backlog as %q:\n%s", tc.wantEffect, split)
			}

			for _, e := range CommandEffects(varsFor(s.cfg)) {
				if e.Command == "split" && !strings.Contains(e.Effect, "under "+tc.backlog+" and") {
					t.Errorf("the plain effect --help prints was escaped: %q", e.Effect)
				}
			}
		})
	}
}

// A backslash run before a pipe is doubled so the pipe keeps its own escape; an
// odd run would pair its last backslash with the pipe's escape and split anyway.
func TestTableCellKeepsTheEscapeABackslashWouldConsume(t *testing.T) {
	for in, want := range map[string]string{
		`a|b`:     `a\|b`,
		`a\|b`:    `a\\\|b`,
		`a\\|b`:   `a\\\\\|b`,
		`a\b`:     `a\b`,
		`trail\`:  `trail\`,
		"a\\\nb":  `a\ b`,
		"a\x00|b": "a\x00\\|b",
	} {
		got := tableCell(in)
		if got != want {
			t.Errorf("tableCell(%q) = %q, want %q", in, got, want)
		}
		if n := unescapedPipes("| " + got + " |"); n != 2 {
			t.Errorf("tableCell(%q) = %q splits into %d delimiters, want 2", in, got, n)
		}
	}
}

// unescapedPipes counts the delimiters GFM splits a row on: a backslash pairs
// with the character after it, so only a pipe outside such a pair counts.
func unescapedPipes(row string) int {
	n := 0
	for i := 0; i < len(row); i++ {
		switch row[i] {
		case '\\':
			i++
		case '|':
			n++
		}
	}
	return n
}

// A gate command renders inside a code span, where GFM runs no second
// backslash pass, so no table escape of a pipe survives into the command an
// agent copies. The template prints it byte for byte instead.
func TestVerifyCommandRendersVerbatim(t *testing.T) {
	for _, gate := range []string{`a\|b`, `make build && make check`} {
		s := fixture(t, "")
		s.cfg.Verify["default"] = gate
		body, err := s.renderAgents()
		if err != nil {
			t.Fatal(err)
		}
		row := gateRow(body, "no flag set")
		if want := "`" + gate + "` |"; !strings.HasSuffix(row, want) {
			t.Errorf("verify command %q renders as %q, want the row to end %q", gate, row, want)
		}
	}
}

// gateRow returns the gates table's row whose first cell is when.
func gateRow(body, when string) string {
	prefix := "   | " + when + " | "
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// A schema list that names closes without saying what reads it gets items
// written without it, and their issues stay open after the merge.
func TestAgentsExplainsClosesAndIssueWhereDeclared(t *testing.T) {
	const closes, issue = "`closes` holds the tracker issue numbers", "`issue` is the tracker issue `drover intake`"

	s := fixture(t, "")
	s.cfg.Frontmatter.Optional = append(s.cfg.Frontmatter.Optional, "closes", "issue")
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{closes, "drover resolve", "--link", "`fixed`", "NO DISPOSITION", issue, "names it in both"} {
		if !strings.Contains(body, want) {
			t.Errorf("block declaring closes and issue lacks %q", want)
		}
	}

	plain, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, bullet := range []string{closes, issue} {
		if strings.Contains(plain, bullet) {
			t.Errorf("block declaring neither key renders %q", bullet)
		}
	}
}

// addendumProject is a project whose [agents] addendum names notes.md, holding
// text. The template action in it is there to prove nothing renders it.
func addendumProject(t *testing.T) *session {
	t.Helper()
	s := fixture(t, "\n[agents]\naddendum = \"notes.md\"\n")
	write(t, filepath.Join(s.cfg.Root, "notes.md"), addendumText)
	return s
}

const addendumText = "Our notes live in the vault.\n\nKeep {{ .Project }} out of the dashboard.\n"

// The addendum is the last thing inside the markers, as written, under a
// heading drover supplies; --bare carries the same text without the markers.
func TestAddendumRendersVerbatimAtTheEndOfTheBlock(t *testing.T) {
	s := addendumProject(t)
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	section := "\n\n## Project addendum\n\n" + strings.TrimRight(addendumText, "\n")
	if !strings.HasSuffix(body, section+"\n\n"+agentsEnd+"\n") {
		t.Fatalf("the addendum is not the last thing before the closing marker:\n%s", body)
	}
	if !strings.Contains(body, "{{ .Project }}") {
		t.Error("the addendum's template action was rendered rather than carried as written")
	}
	generated, err := s.generatedAgents()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(body, section) < strings.Index(body, generated)+len(generated) {
		t.Error("the addendum sits before the end of the generated text")
	}

	agentsBare = true
	t.Cleanup(func() { agentsBare = false })
	bare, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(bare, section+"\n") || strings.Contains(bare, agentsEnd) {
		t.Errorf("--bare does not end on the addendum:\n%s", bare)
	}
}

// A marker line or an open fence in the addendum would end the block early, so
// the next read of the file finds a different block than the one written.
func TestAddendumThatWouldEndTheBlockIsRefused(t *testing.T) {
	for name, text := range map[string]string{
		"marker": "before\n" + agentsEnd + "\nafter\n",
		"fence":  "```sh\nunclosed\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := addendumProject(t)
			writeBlock(t, s, "AGENTS.md")
			write(t, s.cfg.AddendumPath(), text)
			if _, err := s.renderAgents(); err == nil || !strings.Contains(err.Error(), s.cfg.AddendumPath()) {
				t.Errorf("rendered an addendum that ends the block early: %v", err)
			}
			// A block written before the edit is stale with no way to
			// regenerate it, so check has to say why.
			if kinds := instructionKinds(t, s); !kinds["BAD ADDENDUM"] {
				t.Errorf("check reported %v, want BAD ADDENDUM", kinds)
			}
		})
	}
}

// An addendum that will not render hides only the comparison: the marker
// finding and the marker refusal read the host file alone.
func TestBadAddendumKeepsTheMarkerFindings(t *testing.T) {
	s := fixture(t, "\n[agents]\naddendum = \"notes.md\"\n")
	write(t, filepath.Join(s.cfg.Root, "AGENTS.md"), agentsEnd+"\nstranded\n"+agentsBegin+"\n")
	kinds := instructionKinds(t, s)
	if !kinds["BAD ADDENDUM"] || !kinds["BAD MARKERS"] {
		t.Errorf("check reported %v, want BAD ADDENDUM and BAD MARKERS", kinds)
	}
	if found := staleFiles(s.cfg); len(found) != 1 || found[0].Name != "AGENTS.md" {
		t.Errorf("staleFiles = %+v, want the malformed AGENTS.md", found)
	}
}

// Missing, unreadable and a directory each stop `agents` with exit 1 naming the
// resolved path, and each is a check finding.
func TestAnUnreadableAddendum(t *testing.T) {
	for name, setup := range map[string]func(path string){
		"missing":   func(string) {},
		"directory": func(path string) { _ = os.Mkdir(path, 0o750) },
		"unreadable": func(path string) {
			write(t, path, "secret\n")
			_ = os.Chmod(path, 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := fixture(t, "\n[agents]\naddendum = \"notes.md\"\n")
			path := filepath.Join(s.cfg.Root, "notes.md")
			setup(path)
			if name == "unreadable" {
				if _, err := os.ReadFile(path); err == nil { // #nosec G304 -- test file
					t.Skip("running as a user who reads a mode-0 file")
				}
			}
			t.Chdir(s.cfg.Root)
			var err error
			capture(t, func() { err = execute(t, "agents") })
			if Code(err) != ExitError || !strings.Contains(err.Error(), path) {
				t.Errorf("agents ended %v (exit %d), want exit %d naming %s", err, Code(err), ExitError, path)
			}
			if kinds := instructionKinds(t, s); !kinds["BAD ADDENDUM"] {
				t.Errorf("check reported %v, want BAD ADDENDUM", kinds)
			}
		})
	}
}

// ~/ is the home directory, so an addendum can live in a notes vault or a
// dotfiles repo rather than in the project.
func TestAddendumTildeResolvesAgainstHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "vault"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "vault", "drover.md"), "From the vault.\n")
	s := fixture(t, "\n[agents]\naddendum = \"~/vault/drover.md\"\n")
	if got, want := s.cfg.AddendumPath(), filepath.Join(home, "vault", "drover.md"); got != want {
		t.Errorf("AddendumPath = %q, want %q", got, want)
	}
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "## Project addendum\n\nFrom the vault.") {
		t.Errorf("the ~/ addendum is not in the block:\n%s", body)
	}
}

// drover config says where the addendum resolved and whether it read, which the
// value as written cannot.
func TestConfigNamesTheResolvedAddendum(t *testing.T) {
	s := addendumProject(t)
	t.Chdir(s.cfg.Root)
	path := filepath.Join(s.cfg.Root, "notes.md")
	for _, tc := range []struct{ state, want string }{{"present", path + ", readable"}, {"removed", path + ", which cannot be read"}} {
		if tc.state == "removed" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		for _, args := range [][]string{{"config"}, {"config", "--diff"}} {
			var err error
			out := capture(t, func() { err = execute(t, args...) })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("%v with the addendum %s does not say %q:\n%s", args, tc.state, tc.want, out)
			}
		}
	}
}

// A writer who cites a directory for context learns at the next --jobs drive
// that it ran alone; the instructions are where they read the rule first.
func TestAgentsTellsWritersToCiteFiles(t *testing.T) {
	body, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	named := strings.Index(body, "**Name files and lines.**")
	files := strings.Index(body, "**Cite files, not directories.**")
	if files < 0 || files < named {
		t.Fatalf("no directory bullet after \"Name files and lines\":\n%s", body)
	}
	for _, want := range []string{"drive --jobs", "plain prose, without", "backticks"} {
		if !strings.Contains(body[files:], want) {
			t.Errorf("directory bullet omits %q", want)
		}
	}
}

func TestAgentsLeavesTheFullGateToAMergeGate(t *testing.T) {
	gated, err := fixture(t, "\n[drive]\nmerge_gate = [\"./scripts/merge-gate.sh\"]\n").renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gated, "Re-run the verify command yourself") ||
		!strings.Contains(gated, "every merge waits on `./scripts/merge-gate.sh`") {
		t.Error("a project with a merge gate still tells reviewers to re-run the full verify command")
	}

	plain, err := fixture(t, "").renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain, "6. **Re-run the verify command yourself.**") {
		t.Error("a project with no merge gate lost the full re-run")
	}
}

// A [drive] worktree_links entry is a link into the operator's checkout, and a
// session's --write through it would hand every command there a branch's
// unmerged block. Inside a session the link gives way to a file of the tree's
// own; outside one, the link is the operator's and the write follows it.
func TestAgentsWriteInASessionLeavesTheLinkedCheckoutCopy(t *testing.T) {
	for _, c := range []struct {
		name    string
		actor   string
		linkSet bool
	}{
		{"in a session", runner.ActorAgent, false},
		{"outside a session", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := fixture(t, "")
			target := filepath.Join(s.cfg.Root, "AGENTS.md")
			write(t, target, "# House rules\n")
			tree := newRepo(t)
			link := filepath.Join(tree, "AGENTS.md")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			t.Chdir(tree)
			t.Setenv(runner.EnvActor, c.actor)

			block, err := s.renderAgents()
			if err != nil {
				t.Fatal(err)
			}
			out := captureErr(t, func() {
				if err := s.writeAgents("AGENTS.md", block); err != nil {
					t.Fatal(err)
				}
			})
			fi, err := os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if isLink := fi.Mode()&os.ModeSymlink != 0; isLink != c.linkSet {
				t.Errorf("AGENTS.md in the tree is a link: %v, want %v", isLink, c.linkSet)
			}
			got := read(t, target)
			if c.linkSet {
				if !strings.Contains(got, agentsBegin) {
					t.Errorf("outside a session the write did not reach the link's target:\n%s", got)
				}
				return
			}
			if got != "# House rules\n" {
				t.Errorf("a session's write changed the checkout's copy:\n%s", got)
			}
			if local := read(t, link); !strings.Contains(local, "# House rules") || !strings.Contains(local, agentsBegin) {
				t.Errorf("the tree's own copy lacks the preamble or the block:\n%s", local)
			}
			if !strings.Contains(out, "the link to ") || !strings.Contains(out, "so the checkout's copy is unchanged") {
				t.Errorf("the write did not say it replaced the link: %q", out)
			}
		})
	}
}
