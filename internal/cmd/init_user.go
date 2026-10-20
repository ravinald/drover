package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/runner"
	"golang.org/x/term"
)

// prompter walks a set of questions over a reader and a writer, so the walk is
// drivable by a test rather than only by a person at a keyboard.
//
// A read error is sticky rather than returned per question. A wizard that
// abandoned the walk halfway would leave the operator having answered four
// questions for nothing, so the remaining questions take their defaults and the
// caller checks err once, before anything is written.
type prompter struct {
	in  *bufio.Scanner
	out io.Writer
	err error
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewScanner(in), out: out}
}

// read returns the trimmed line, or empty once the input is spent.
func (p *prompter) read() string {
	if p.err != nil {
		return ""
	}
	if !p.in.Scan() {
		if p.err = p.in.Err(); p.err == nil {
			p.err = io.EOF
		}
		return ""
	}
	return strings.TrimSpace(p.in.Text())
}

// ask puts one question and returns the answer, or def when the operator just
// pressed return. The default is shown because a wizard that hides what it
// would do unasked is worse than no wizard.
func (p *prompter) ask(question, def string) string {
	shown := def
	if shown == "" {
		shown = "none"
	}
	fmt.Fprintf(p.out, "%s\n  [%s] ", question, shown)
	if answer := p.read(); answer != "" {
		return answer
	}
	return def
}

// choose puts a numbered menu and returns the index picked. Anything that is
// not one of the numbers re-asks rather than silently taking the default: the
// operator typed something, and treating a typo as assent is how a wizard
// writes a config nobody chose.
func (p *prompter) choose(question string, options []string, def int) int {
	for {
		fmt.Fprintf(p.out, "%s\n", question)
		for i, opt := range options {
			fmt.Fprintf(p.out, "  %d) %s\n", i+1, opt)
		}
		fmt.Fprintf(p.out, "  [%d] ", def+1)
		answer := p.read()
		if p.err != nil {
			return def
		}
		if answer == "" {
			return def
		}
		if n := int(answer[0] - '0'); len(answer) == 1 && n >= 1 && n <= len(options) {
			return n - 1
		}
		fmt.Fprintf(p.out, "  not one of 1-%d\n\n", len(options))
	}
}

// answers is what the walk collected. It is a value rather than a half-written
// file so that nothing reaches disk until every question has been answered and
// the operator has seen the result.
type answers struct {
	// Central puts every project's backlog under one home directory instead of
	// inside each repository.
	Central  bool
	Verify   string
	Runner   string
	Bug      string
	Posture  string
	Required string
	Optional string
}

func defaultAnswers() answers {
	return answers{
		Runner:   "claude",
		Bug:      "B",
		Required: "title, deps",
		Optional: "kind, issue, closes, severity, skills, spec, agent",
	}
}

// initUser writes the per-user config: the machine-wide half of configuration,
// run once after installing, beside the project file plain init writes.
func initUser() error {
	path := config.GlobalPath()
	if path == "" {
		return fmt.Errorf("no home directory to write a per-user config into; set XDG_CONFIG_HOME")
	}
	if _, err := os.Stat(path); err == nil {
		return Refusal{fmt.Errorf("%s already exists — edit it, or delete it to start over", path)}
	}

	a := defaultAnswers()
	if !initDefaults {
		if !interactive() {
			return Refusal{fmt.Errorf(
				"init --user asks questions and there is no terminal to ask on.\nRun it from a shell, or pass --defaults to write the file with every default taken")}
		}
		var err error
		if a, err = walk(newPrompter(os.Stdin, os.Stdout), a); err != nil {
			return err
		}
	}

	body := renderGlobal(a)
	fmt.Printf("\n%s\n\n%s", path, body)

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	fmt.Printf("\n  ✓ wrote %s\n", path)

	fmt.Printf("\nNext, in each project:\n")
	fmt.Printf("  1. drover init — writes .drover.toml, leaving out what this file supplies\n")
	if a.Verify == "" {
		fmt.Printf("  2. fill [verify] default with the command that must pass before an item merges\n")
		fmt.Printf("  3. drover check\n")
	} else {
		fmt.Printf("  2. drover check\n")
	}
	return nil
}

// interactive reports whether there is a terminal to ask questions on. Stdin is
// the one that matters: a run whose answers come from a pipe or a heredoc has
// nobody to re-ask when an answer is not one of the offered numbers. Asked of
// the terminal rather than the file mode, because /dev/null is a character
// device too, and a walk reading it takes every default without a question.
func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) // #nosec G115 -- a file descriptor fits an int
}

// walk puts every question in order and returns what was answered. It is split
// from the command so a test can drive it with a scripted reader.
func walk(p *prompter, a answers) (answers, error) {
	fmt.Fprintf(p.out, "Per-user drover config. Press return to take the shown default.\n\n")

	a.Central = p.choose("Where does each project keep its backlog, ledger and reports?", []string{
		"inside the project, under .drover/ — versioned with the code it describes",
		"centrally, under ~/.local/share/drover/<project>/ — one place, and nothing lands in a repository",
	}, 0) == 1
	fmt.Fprintln(p.out)

	a.Verify = p.ask("Verify command: the command that must pass before an item merges.\n"+
		"  Leave it empty unless every project you drive runs the same one — a wrong\n"+
		"  verify command inherited everywhere is worse than none reported per project.", a.Verify)
	fmt.Fprintln(p.out)

	a.Runner = p.ask(fmt.Sprintf("Default runner (declared: %s).",
		strings.Join(runnerNames(), ", ")), a.Runner)
	fmt.Fprintln(p.out)

	a.Bug = p.ask("Id prefix for a bug. Uppercase; it becomes a filename component.", a.Bug)
	fmt.Fprintln(p.out)

	a.Required = p.ask("Frontmatter keys every item must declare, comma separated.", a.Required)
	fmt.Fprintln(p.out)

	a.Optional = p.ask("Frontmatter keys an item may declare.", a.Optional)
	fmt.Fprintln(p.out)

	a.Posture = p.ask("Reviewer principles: the bar a reviewer holds the code to, in one sentence.\n"+
		"  drover cannot read this off anything, which is why it is asked.", a.Posture)

	if p.err != nil && p.err != io.EOF {
		return a, fmt.Errorf("reading answers: %w", p.err)
	}
	return a, nil
}

// runnerNames is the built-in runner vocabulary, for the prompt that offers it.
// init --user runs before any project exists, so there is no [runners] table to read.
func runnerNames() []string {
	names := keys(runner.Builtin())
	slices.Sort(names)
	return names
}

// renderGlobal writes the config the walk described. Every value carries the
// reason it is there, because the operator reading this file next is doing it
// months later and without the questions in front of them.
func renderGlobal(a answers) string {
	var b strings.Builder
	b.WriteString("# drover, per-user. Layered under every project's .drover.toml, which wins\n")
	b.WriteString("# on any key it sets. See `drover schema` for the full contract.\n")

	if a.Central {
		b.WriteString("\n[paths]\n")
		b.WriteString("# One place for every project's work, so nothing lands in a repository.\n")
		b.WriteString("# A global path must be absolute or ~-rooted, and names the project it is\n")
		b.WriteString("# resolving for with {{ .Project }}.\n")
		b.WriteString("backlog  = \"~/.local/share/drover/{{ .Project }}/items\"\n")
		b.WriteString("ledger   = \"~/.local/share/drover/{{ .Project }}/ledger.jsonl\"\n")
		b.WriteString("reports  = \"~/.local/share/drover/{{ .Project }}/reports\"\n")
	} else {
		b.WriteString("\n# [paths] is unset, so each project keeps its work under its own\n")
		b.WriteString("# .drover/. Setting it here would move every project at once.\n")
	}

	b.WriteString("\n[ids]\n")
	b.WriteString("# The kind of work each id prefix holds. Add a prefix and every project on\n")
	b.WriteString("# this machine can file that kind without repeating the table.\n")
	fmt.Fprintf(&b, "namespaces = { %s = \"bug\", E = \"enhancement\", F = \"feature\" }\n", a.Bug)

	b.WriteString("\n[frontmatter]\n")
	b.WriteString("# The schema is closed: an undeclared key is an error, because a typo'd deps\n")
	b.WriteString("# reads as no dependencies and renders a blocked item READY. A project that\n")
	b.WriteString("# writes either list replaces it whole rather than adding to it.\n")
	fmt.Fprintf(&b, "required = %s\n", tomlList(a.Required))
	fmt.Fprintf(&b, "optional = %s\n", tomlList(a.Optional))

	b.WriteString("\n[run]\n")
	fmt.Fprintf(&b, "default_runner = %q\n", a.Runner)

	if a.Verify != "" {
		b.WriteString("\n[verify]\n")
		b.WriteString("# Inherited by every project that does not name its own. A project whose\n")
		b.WriteString("# verification differs must override it, or it will merge on the wrong check.\n")
		fmt.Fprintf(&b, "default = %q\n", a.Verify)
	} else {
		b.WriteString("\n# [verify] is unset on purpose. Each project names the command that must pass\n")
		b.WriteString("# before an item merges, and `drover check` reports the empty verify command until it\n")
		b.WriteString("# does. A machine-wide value would satisfy that check for a project where\n")
		b.WriteString("# it is wrong.\n")
	}

	b.WriteString("\n[agents]\n")
	if a.Posture != "" {
		b.WriteString("# The bar a reviewer holds the code to: the one thing `drover agents` cannot\n")
		b.WriteString("# read off the rest of the config.\n")
		fmt.Fprintf(&b, "principles = %q\n", a.Posture)
	} else {
		b.WriteString("# The bar a reviewer holds the code to, in one sentence. `drover agents`\n")
		b.WriteString("# cannot read this off anything else, so unset principles mean every\n")
		b.WriteString("# project's instructions ship without one.\n")
		b.WriteString("# principles = \"\"\n")
	}
	return b.String()
}

// tomlList renders a comma-separated answer as a TOML array, dropping the empty
// entries a trailing comma leaves behind.
func tomlList(raw string) string {
	items := make([]string, 0, 4)
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			items = append(items, fmt.Sprintf("%q", f))
		}
	}
	return "[" + strings.Join(items, ", ") + "]"
}
