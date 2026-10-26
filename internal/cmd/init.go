package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

// buildFiles are checked in order for root_marker. The marker proves a config
// describes the tree it was found in, so it wants a file that is present in a
// fresh clone and absent everywhere else.
var buildFiles = []string{
	"go.mod", "package.json", "Cargo.toml", "pyproject.toml",
	"pom.xml", "build.gradle", "Gemfile", "mix.exs",
}

var (
	initVerify   string
	initUserFlag bool
	initDefaults bool
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a starting .drover.toml, or with --user the per-user config.",
	Long: `Fills in what can be read off the tree — the project name, the tracker repo,
a root marker — and comments the rest with the choice spelled out.

The generated verify command is deliberately empty. Nothing can infer the command that
must pass before an item merges, and an item that runs without one hands its
agent no way to verify the work. drover check reports the empty verify command, so a
half-configured project fails its own validation rather than running.

A directory with no work tree gets [git] enabled = false, which turns branch
automation and the merge check off rather than letting them fail at the point of
use. This is inferred once, into a file you can correct; drover never probes for
.git at run time.

A per-user config at ~/.config/drover/config.toml (or $XDG_CONFIG_HOME) supplies
defaults for every project, and init leaves out the keys it already sets. A key
repeated here shadows the global value rather than inheriting it.

--user writes that per-user config instead, walking through what every project
on this machine shares, and touches no project. Run it once after installing.
Put a setting there when it is the same in every project you drive and would
otherwise be retyped: where work lives, which runner you use, the bar you hold a
review to. Leave it out when it is a property of one project. It offers the
verify command no default: it differs per project, and a machine-wide value would
satisfy drover check for a project where it is wrong, which is the one thing
the empty verify command exists to prevent. --defaults skips the walk and writes the same
file with every default taken; it is also the only form that works without a
terminal.`,
	Example: `  drover init                       # asks for the verify command, then writes .drover.toml
  drover init --verify "make check" # the form for a script: asks nothing
  drover init --verify "go test ./..." && drover check   # exits 0
  drover agents --write DROVER.md   # then generate the instructions from it
  drover init --user                # the per-user config: walk the questions
  drover init --user --defaults     # take every default, no questions

  Run it at the project root. Config lookup walks up, so a config above this
  directory already governs the tree and init says so rather than shadowing it.
  --user writes $XDG_CONFIG_HOME/drover/config.toml, or
  ~/.config/drover/config.toml, and refuses if one is already there.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if initUserFlag {
			if initVerify != "" {
				return fmt.Errorf("--verify is a project setting and --user writes no project; run drover init --verify in the project")
			}
			return initUser()
		}
		if initDefaults {
			return fmt.Errorf("--defaults answers the --user questions; plain init asks only for the verify command, which --verify answers")
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		path := filepath.Join(wd, config.FileName)
		if _, err := os.Stat(path); err == nil {
			return Refusal{fmt.Errorf("%s already exists — edit it, or delete it to start over", config.FileName)}
		}
		// Config lookup walks up, so a config above this directory already governs
		// this tree. Nesting one inside it is legitimate in a monorepo and a
		// mistake everywhere else, and only the operator knows which.
		if root, err := config.Find(wd); err == nil {
			fmt.Printf("note: %s already governs this tree from %s\n", config.FileName, root)
		}

		global, err := globalKeys()
		if err != nil {
			return err
		}
		if global.path != "" {
			fmt.Printf("note: %s supplies defaults; the keys it sets are left out of %s\n",
				global.path, config.FileName)
		}

		gate := initVerify
		if gate == "" && !global.has("verify.default") && interactive() {
			// One question, and the only one init asks. It is the single reason a
			// generated project fails its own check, so answering it here is the
			// difference between a first run that exits 0 and one that hands the
			// newcomer a finding as their introduction to the tool.
			p := newPrompter(os.Stdin, os.Stdout)
			gate = p.ask("The command that must pass before an item merges (empty to fill in later):", "")
			if err := p.err; err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			fmt.Println()
		}

		body := renderConfig(forInit, wd, global, gate, nil, nil)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil { // #nosec G306
			return err
		}
		fmt.Printf("  ✓ wrote %s\n", config.FileName)

		// Read back rather than reusing the values written: this proves the
		// generated file parses and validates before anything is told it worked.
		cfg, err := config.Load(wd)
		if err != nil {
			return fmt.Errorf("wrote %s but it does not load: %w", config.FileName, err)
		}
		for _, dir := range []string{cfg.Paths.Backlog, cfg.Paths.Reports} {
			if err := os.MkdirAll(cfg.Abs(dir), 0o750); err != nil {
				return err
			}
			fmt.Printf("  ✓ %s/\n", dir)
		}
		if cfg.Git.On() {
			added, err := ignoreMarker(wd)
			if err != nil {
				return err
			}
			if added {
				fmt.Printf("  ✓ .gitignore ignores %s\n", markerIgnore)
			}
		}

		// Recorded after the config loads, so the row names a project that exists.
		enroll(cfg)

		steps := []string{}
		if cfg.Verify["default"] == "" {
			steps = append(steps, "fill [verify] default with the command that must pass before an item merges")
		}
		steps = append(steps,
			"drover check",
			fmt.Sprintf("write your first item at %s/B01-<slug>.md, or drover intake <issue>", cfg.Paths.Backlog),
			"drover agents --write DROVER.md — tells a coding agent how to drive this",
			"drover agents --pointer DROVER.md >> AGENTS.md — the line that sends it there")

		// The gate is what makes a project usable, so a global config that
		// already supplies one leaves nothing here that check would refuse.
		switch {
		case cfg.Verify["default"] == "":
			fmt.Printf("\nNot yet usable. Next:\n")
		case gate != "":
			fmt.Printf("\nGate set to %q. Next:\n", cfg.Verify["default"])
		default:
			fmt.Printf("\nGate inherited from %s. Next:\n", global.path)
		}
		for i, step := range steps {
			fmt.Printf("  %d. %s\n", i+1, step)
		}
		return nil
	},
}

// globalDefaults is what the per-user config already supplies, so the generated
// project file can leave those keys alone.
type globalDefaults struct {
	path string
	keys map[string]bool
}

func (g globalDefaults) has(key string) bool { return g.keys[key] }

// declares reports whether the per-user config wrote the key or anything under
// it. It is has for a leaf and the whole question for an open table, whose
// entry names nothing can enumerate: a global that opened [frontmatter.shape]
// and closed no key declared a policy, and asking after the three names a
// fresh config suggests would read that policy as three gaps.
func (g globalDefaults) declares(key string) bool {
	if g.keys[key] {
		return true
	}
	prefix := key + "."
	for k := range g.keys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func globalKeys() (globalDefaults, error) {
	path, keys, err := config.GlobalKeys()
	if err != nil {
		return globalDefaults{}, err
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return globalDefaults{path: path, keys: set}, nil
}

// mode says which caller the config generator is rendering for.
//
// One generator serves both, and the layout is the same. What differs is what
// may be read off the tree. `drover init` is writing a file that does not
// exist, so the build system in the directory and the presence of a work tree
// are the only evidence there is, and writing them over a per-user value is
// deliberate: a global root_marker naming go.mod would refuse to load every
// project that is not a Go module.
//
// `drover config` prints a file a reader pastes. A line there that shadows an
// inherited value pins today's global into the project file for good, and the
// next edit to the global silently misses this project. So inference fills a
// key nothing supplies and never one the per-user config already does.
type mode int

const (
	forInit mode = iota
	forReport
)

// inferable reports whether a value read off the tree may be written for key.
// A key the project declared is not inference at all: that is the project's own
// value, and it comes back whatever the global says.
func (m mode) inferable(g globalDefaults, w written, key string) bool {
	return w.has(key) || m == forInit || !g.has(key)
}

// suggest renders a line for a key the project never declared. init writes it
// live, because there is no file yet and this is the file. A report writes it
// commented: a live line there is a decision the project never made, and one
// that resolves differently from the source the moment the per-user config or
// drover's own default moves. [frontmatter.shape] filled in where the project
// left it absent tightened validation it never asked for; `default = ""`
// written under an inherited [verify] pinned NO VERIFY once the global gained a
// verify. The rule is one rule so that the next key cannot repeat either.
func (m mode) suggest(text string) string {
	if m == forReport {
		return commented(text)
	}
	return text
}

// header opens a section. In a report it is live only where the project's own
// file wrote something under it, so the copy declares no table the source did
// not.
func header(m mode, w written, name string) string {
	if m == forReport && !w.declares(name) {
		return "\n# [" + name + "]\n"
	}
	return "\n[" + name + "]\n"
}

func renderConfig(m mode, root string, global globalDefaults, gate string, w written, bare []string) string {
	var b strings.Builder
	// schema_version is never taken from w. This renders the version this build
	// understands, which is the whole question `drover config` answers; the gap
	// against what a file declares is reported on its own line by --diff.
	fmt.Fprintf(&b, "schema_version = %d\n", config.SchemaVersion)
	// A release build stamps what it wrote against; a dev build has no release
	// to name, and writing "dev" would read as stale to every binary after it.
	if stamp, ok := releaseStamp(); ok {
		fmt.Fprintf(&b, "drover_version = %q\n", stamp)
	}
	fmt.Fprintf(&b, "project = %s\n", w.or("project", strconv.Quote(filepath.Base(root))))

	marker := rootMarker(root)
	var paths strings.Builder
	if !global.has("paths.backlog") || !global.has("paths.ledger") || !global.has("paths.reports") {
		paths.WriteString("# Defaults shown. The backlog is prose about unfinished work: add .drover/ to\n")
		paths.WriteString("# .gitignore to keep it out of the published tree.\n")
	}
	for _, p := range []struct{ key, name, pad, def string }{
		{"paths.backlog", "backlog", "     ", `".drover/backlog"`},
		{"paths.ledger", "ledger", "      ", `".drover/ledger.jsonl"`},
		{"paths.reports", "reports", "     ", `".drover/reports"`},
	} {
		paths.WriteString(live(m, global, w, p.key, p.name, p.pad, p.def))
	}
	// root_marker is written even when the global config sets one, for init: it
	// names a file that must exist in *this* tree, and a global value naming
	// go.mod would refuse to load every project that is not a Go module. A
	// report prints no such line, because the project is already running on the
	// inherited value and a paste would replace it.
	//
	// A project that declared one keeps it even where this tree holds no build
	// file drover recognizes, because the project named a file it knows about
	// and dropping the line would resolve every path against a stranger.
	if (marker != "" || w.has("paths.root_marker")) && m.inferable(global, w, "paths.root_marker") {
		paths.WriteString("# Proves this config describes this tree, so a copy that travels to another\n")
		paths.WriteString("# repo refuses instead of resolving every path against a stranger.\n")
		line := "root_marker = " + w.or("paths.root_marker", strconv.Quote(marker)) + "\n"
		if !w.has("paths.root_marker") {
			line = m.suggest(line)
		}
		paths.WriteString(line)
	}
	if paths.Len() > 0 {
		b.WriteString(header(m, w, "paths"))
		b.WriteString(paths.String())
	}

	allocator := allocatorLines(m, w)
	if !block(&b, m, global, w, "ids", []string{"namespaces"},
		"# The kind of work each id prefix holds. B1 reads as a bug, F3 as a feature.\n"+
			openTable(m, global, w, "ids.namespaces", `namespaces = { B = "bug", E = "enhancement", F = "feature" }`+"\n")+
			"# planned_max bounds a closed backlog, rejecting an out-of-range planned id as\n"+
			"# a typo. Omitted means unbounded, which is right until a plan is written.\n"+
			w.line("ids.planned_max", "# planned_max = 25\n")+allocator) && allocator != "" {
		b.WriteString(header(m, w, "ids") + allocator)
	}

	block(&b, m, global, w, "frontmatter", []string{"required", "optional"},
		"# The schema is closed: an undeclared key is an error, because a typo'd deps\n"+
			"# reads as no dependencies and renders a blocked item READY. Required keys may\n"+
			"# be empty but not absent; presence is what proves the author considered them.\n"+
			live(m, global, w, "frontmatter.required", "required", " ", `["title", "deps"]`)+
			live(m, global, w, "frontmatter.optional", "optional", " ",
				`["kind", "issue", "closes", "severity", "skills", "spec", "runner"]`))

	// Its own block because a per-user config commonly sets required and optional
	// and no shape. Folded into the block above, such a project inherits the two
	// lists and gets no shape at all, so every value they close goes unchecked.
	shapeBlock(&b, m, global, w)

	verifyBlock(&b, m, global, w, gate)

	block(&b, m, global, w, "agents", []string{"principles", "addendum"},
		"# The bar a reviewer holds the code to, in one sentence. `drover agents` reads\n"+
			"# everything else it needs off this file; this is the one thing it cannot, and it\n"+
			"# lives here so regenerating the instructions cannot lose it.\n"+
			w.line("agents.principles", "# principles = \"\"\n")+
			"# A file whose text the generated block carries verbatim, for what a project\n"+
			"# wants every agent to know beyond the principles. Editing it means regenerating.\n"+
			w.line("agents.addendum", "# addendum = \".drover/agents-addendum.md\"\n"))

	block(&b, m, global, w, "intake", []string{"stop_marker"},
		"# Holds a scaffolded item inert until a human writes its requirements. Those\n"+
			"# requirements become the audit checklist, so one lifted mechanically from a\n"+
			"# bug report is a checkbox nobody can falsify.\n"+
			live(m, global, w, "intake.stop_marker", "stop_marker", " ", `"STOP: requirements not yet written"`))

	// Commented, not set: both are optional, and a project that never wanted
	// one must not gain a finding by upgrading. They are written because a
	// reader who does not know they exist cannot go looking for them, and
	// dropped for a project that already declared one, where an invitation to
	// name what it named reads as though drover had not seen the file. Its own
	// values come through below.
	if !w.declares("runners") {
		b.WriteString("\n# [runners] declares what `drover run` executes. Only claude is built in, so a\n")
		b.WriteString("# project driving codex or gemini names it here.\n")
		b.WriteString("# [runners.codex]\n")
		b.WriteString("# interactive = [\"codex\"]\n")
		b.WriteString("# headless    = [\"codex\", \"exec\", \"--json\"]\n")
	}
	if !w.has("register.backend") {
		b.WriteString("\n# [register] backend selects where follow-ups live: \"github\" (the default, read\n")
		b.WriteString("# through gh), \"file\" (an append-only JSONL written by drover follow-up), or\n")
		b.WriteString("# \"none\". Choosing \"file\" means naming no tracker anywhere this project\n")
		b.WriteString("# reads, the per-user config included: naming one and declaring there is\n")
		b.WriteString("# none is refused at load.\n")
		b.WriteString("# [register]\n")
		b.WriteString("# backend = \"file\"\n")
	}

	if line := mirrorLine(m, w); line != "" {
		b.WriteString(header(m, w, "backlog") + line)
	}

	var git string
	switch repo := isWorkTree(root); {
	case w.has("git.enabled"):
		git = "enabled = " + w.or("git.enabled", "") + "\n"
	case !m.inferable(global, w, "git.enabled"):
		// Inherited, so nothing is written: this tree being a work tree is a
		// fact, and a line stating it here would override the per-user value
		// the project is running on.
	case !repo:
		git = m.suggest("# No work tree here. Branch automation and the merge check are off: run\n" +
			"# executes in place, finish refuses and names `mark-merged` instead, status omits the\n" +
			"# here-line. Everything else — the backlog, the ledger, the audit levels —\n" +
			"# is files on disk and works unchanged. Set true after `git init`.\n" +
			"enabled = false\n")
	// Written only against a global that turned git off, because the value it
	// would otherwise carry is the one drover already assumes.
	case global.has("git.enabled"):
		git = fmt.Sprintf("# This tree is a work tree, overriding %s.\nenabled = true\n", global.path)
	}
	git += mergeMethodLine(m, global, w)
	if git != "" || w.declares("git") {
		b.WriteString(header(m, w, "git") + git)
	}

	var gh string
	switch repo := originRepo(root); {
	case w.has("github.repo"):
		gh = "repo = " + w.or("github.repo", "") + "\n"
	case !m.inferable(global, w, "github.repo"):
		// The origin remote is read off this tree the way the marker and the
		// work tree are, and writing it over an inherited repo would route
		// intake labels at whatever the remote said the day this was printed.
	case repo != "":
		gh = m.suggest(fmt.Sprintf("repo = %q\n", repo))
	default:
		gh = "# repo = \"owner/name\"   # unset lets gh infer from the git remote\n"
	}
	if gh != "" || w.declares("github") {
		b.WriteString(header(m, w, "github") + gh)
	}
	return carry(b.String(), w, bare)
}

// mergeMethodLine reports [git] merge_method with where its value came from. init
// writes nothing, since the default is the value drover already assumes and a
// live line would make every existing project STALE CONFIG for not repeating it.
// The per-user file is named rather than inherited from: Load refuses the key
// there, so a value in it is one nothing is running on.
func mergeMethodLine(m mode, g globalDefaults, w written) string {
	switch {
	case w.has("git.merge_method"):
		return "# How drive merges, set by this project.\nmerge_method = " + w.or("git.merge_method", "") + "\n"
	case m == forInit:
		return ""
	case g.has("git.merge_method"):
		return fmt.Sprintf("# merge_method is set in %s, and drover refuses it there: it is per-project, so move it here.\n", g.path)
	}
	return fmt.Sprintf("# merge_method = %q   # drover's default; nothing sets it\n", config.MergeMethods()[0])
}

// allocatorLines reports [ids] allocator and remote with where each value came
// from. init writes neither, for mirrorLine's reason: the default is what drover
// already assumes, and "git" writes to a remote nobody has chosen yet.
func allocatorLines(m mode, w written) string {
	if m == forInit && !w.has("ids.allocator") && !w.has("ids.remote") {
		return ""
	}
	var b strings.Builder
	if w.has("ids.allocator") {
		b.WriteString("# Where new ids are reserved, set by this project. git pushes refs/drover/ids\n" +
			"# to the remote below before any body is written.\n" +
			"allocator = " + w.or("ids.allocator", "") + "\n")
	} else {
		fmt.Fprintf(&b, "# allocator = %q   # drover's default; nothing sets it\n", config.AllocatorLocal)
	}
	if w.has("ids.remote") {
		b.WriteString("remote = " + w.or("ids.remote", "") + "\n")
	} else {
		fmt.Fprintf(&b, "# remote = %q   # drover's default; nothing sets it\n", "origin")
	}
	return b.String()
}

// mirrorLine reports [backlog] mirror with where its value came from. init
// writes nothing, for merge_method's reason: the default is what drover already
// assumes. The per-user file never appears here, because Load refuses the key
// there: one value in it would publish every project's items on the machine.
func mirrorLine(m mode, w written) string {
	switch {
	case w.has("backlog.mirror"):
		return "# Where accepted items are published, set by this project. github makes their\n" +
			"# text public wherever the tracker is.\nmirror = " + w.or("backlog.mirror", "") + "\n"
	case m == forInit:
		return ""
	}
	return fmt.Sprintf("# mirror = %q   # drover's default; nothing sets it\n", config.MirrorNone)
}

// block appends a [name] section unless the global config already supplies
// every key it would set, and reports whether it did. A key repeated in a project file shadows the global
// value rather than inheriting it, so the next edit to the global file would
// silently miss this project.
//
// A key the project already declared brings the section back whatever the
// global says. The omission above is advice to a file being written for the
// first time; a project that has already made the decision is only being shown
// what it decided, and leaving the section out would move its value into the
// carried block at the foot of the file, away from the comment explaining it.
func block(b *strings.Builder, m mode, g globalDefaults, w written, name string, keys []string, body string) bool {
	for _, k := range keys {
		if dotted := name + "." + k; !g.has(dotted) || w.has(dotted) {
			b.WriteString(header(m, w, name) + body)
			return true
		}
	}
	return false
}

// isWorkTree reports whether root sits inside a git work tree.
//
// This is the one thing init infers rather than declares, and it is safe here
// for the reason it is not safe at run time: init infers it once, into a file
// an operator then reads and can correct. Inferring it on every run means the
// tool changes what it does when a directory is copied.
func isWorkTree(root string) bool {
	out, err := gitcmd.Command(root, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func init() {
	initCmd.Flags().StringVar(&initVerify, "verify", "",
		"the command that must pass before an item merges (skips the question)")
	initCmd.Flags().BoolVar(&initUserFlag, "user", false,
		"write the per-user config every project on this machine layers under, instead of .drover.toml")
	initCmd.Flags().BoolVar(&initDefaults, "defaults", false,
		"with --user, take every default and ask nothing; the only form that works without a terminal")
	root.AddCommand(initCmd)
}

func rootMarker(root string) string {
	for _, name := range buildFiles {
		if fileExists(filepath.Join(root, name)) {
			return name
		}
	}
	return ""
}

// originRepo reads owner/name off the origin remote, and returns empty for
// anything it cannot parse confidently. A wrong repo routes intake labels to
// somebody else's tracker, so guessing is worse than omitting.
func originRepo(root string) string {
	out, err := gitcmd.Command(root, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	if i := strings.Index(url, "github.com"); i >= 0 {
		url = strings.TrimLeft(url[i+len("github.com"):], ":/")
	} else {
		return ""
	}
	if parts := strings.Split(url, "/"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return url
	}
	return ""
}

// markerIgnore is the .gitignore line for the session marker. The whole
// directory rather than the marker alone: it is also where the backlog lives by
// default, and the config init writes already advises keeping that out of the
// published tree.
var markerIgnore = runner.MarkerDir + "/"

// ignoreMarker appends markerIgnore to dir's .gitignore unless a line there
// already names the marker, and reports whether it wrote.
func ignoreMarker(dir string) (bool, error) {
	path := filepath.Join(dir, ".gitignore")
	b, err := os.ReadFile(path) // #nosec G304 -- the project's own .gitignore
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		switch strings.TrimSpace(line) {
		case markerIgnore, "/" + markerIgnore, runner.MarkerDir, "/" + runner.MarkerDir + "/" + runner.MarkerName, runner.MarkerDir + "/" + runner.MarkerName:
			return false, nil
		}
	}
	add := markerIgnore + "\n"
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		add = "\n" + add
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) // #nosec G302 G304 -- .gitignore is world-readable by convention
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(add); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}
