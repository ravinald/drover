package cmd

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

//go:embed agents.tmpl
var agentsTemplate string

// Markers bound the generated block so a project can keep hand-written prose in
// the same file. Regeneration replaces what is between them and touches nothing
// else, which is what stops the block from being a one-time copy that drifts
// past the binary.
const (
	agentsBegin = "<!-- BEGIN drover -->"
	agentsEnd   = "<!-- END drover -->"
)

var (
	agentsWrite      string
	agentsBare       bool
	agentsPointer    string
	agentsPermissive bool
	agentsRestrict   bool
	agentsYes        bool
)

type agentsVerify struct {
	Name    string
	Command string
}

type agentsNamespace struct {
	Prefix string
	Kind   string
}

type agentsData struct {
	Project        string
	Bodies         string
	Ledger         string
	Reports        string
	Specs          string
	Skills         string
	Implement      string
	Draft          string
	Review         string
	ReviewRunner   string
	DraftRunner    string
	Branch         string
	VerifyCommands []agentsVerify
	// MergeGate is [drive] merge_gate joined into one line; step 6 of the
	// review loop reads it to say whether a reviewer re-runs the full verify
	// command.
	MergeGate  string
	Namespaces []agentsNamespace
	Required   []string
	Optional   []string
	// Closes and Issue say the project declares that key. Each carries
	// behavior a bare name in the schema list does not show: finish reads one
	// and ignores the other.
	Closes         bool
	Issue          bool
	StopMarker     string
	FollowUpLabel  string
	SeverityPrefix string
	Posture        string
	Safety         []SafetyRow
	// Effects is what each command changes, resolved against this project's
	// paths. The class table says how far a command reaches; this says what it
	// touches, from the same field --help renders, so the generated block and
	// the binary cannot disagree about it.
	Effects  []CommandEffect
	Operator []SafetyRow
	// Policies is the [supervise] catalog with this project's policies, and
	// SupervisorRefused the commands a supervising session may not run here:
	// the sealed set less drive where enqueue allows it, and every command
	// that is a supervised action at operator or off.
	Policies          []policyRow
	SupervisorRefused []string
	// Destinations is where a review finding goes, rendered from the one value
	// the review brief renders, so the file an agent loads first and the brief
	// it reads last cannot state opposite rules.
	Destinations []prompts.Destination
	// DocsRef pins the documentation links to the ref this binary was built
	// from, so an agent reads the surface it has rather than the one main has
	// moved on to.
	DocsRef string
	// Git is false for a project with no work tree. The merge section is the
	// only part of these instructions that assumes one, and instructions an
	// agent cannot follow are worse than instructions it does not have.
	Git bool
	// MergeDrivePR and MergeDenyPaths say a supervisor may merge at those
	// actions' policies, which moves where the review loop stops; MergeDo is
	// the conditions it may merge under.
	MergeDrivePR   bool
	MergeDenyPaths bool
	MergeDo        string
}

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Generate the agent instructions for this project, filled from its config.",
	Long: `Every placeholder in the old copy-and-fill boilerplate already lives in
.drover.toml: the paths, the verify table, the label vocabulary, the frontmatter
schema. Generating it means a project's instructions cannot describe a command
surface the installed binary no longer has, which is the failure this replaces —
a hand-filled AGENTS.md kept telling operators to record completion by hand for
months after finish shipped.

The one thing that cannot be read off the config is the bar a reviewer holds the
code to. Set [agents] principles and it rides in the generated text; leave it unset
and the output carries a placeholder that says so. Anything longer than that
sentence goes in a file named by [agents] addendum, whose text the block carries
verbatim under "## Project addendum", after everything drover generates.

Prints to stdout by default. --write <path> replaces the text between
` + agentsBegin + ` and ` + agentsEnd + ` in that file, appending the block if
the markers are absent, and touches nothing outside them.

The block records, on one line, the policy of every [supervise] action it was
rendered under. --write compares that line with the policy it is about to
render and prints each action that moves. A move toward auto widens what the
sessions reading the file are told they may do, so it is refused with exit 3,
writing nothing, unless --yes is passed or you confirm at a terminal. A move
that only narrows is printed and written.

Merging a pull request is the operator's by default. --permissive-merge sets
[supervise] merge_drive_pr and merge_protected_paths to supervisor for this project
and renders the block under that policy, so a supervising session may merge a
pull request drive opened once its review passed and its checks are green.
--restrictive-merge sets both back to operator. drover does not rewrite
.drover.toml (see config --help), so either flag appends a [supervise] table
when the file has none; when it has one, the flag prints the two lines to set
in it and exits 3 without writing the block, so the config and the block never
disagree. Without either flag --write renders whatever the config says, so a
later regeneration keeps the choice.

DROVER.md is the suggested target rather than AGENTS.md. The block runs to
roughly 190 lines, which in a project whose AGENTS.md also carries its own rules
leaves drover as most of the first thing an agent reads. --pointer prints the
short stanza that sends it there, generated from the same actor table, because a
hand-written pointer naming five refusing commands after a sixth gained an Actor
line is the drift this command exists to stop.`,
	Example: `  drover agents                     # print this project's instructions
  drover agents --write DROVER.md   # replace only the text between the markers
  drover agents --pointer DROVER.md # the stanza to paste into AGENTS.md
  drover agents --bare              # no markers, no generated-by note
  drover agents --write DROVER.md --permissive-merge --yes
                                    # a supervisor may merge drive's pull requests
  drover agents --write DROVER.md --restrictive-merge
                                    # every merge back with a person

  Regenerate after every config change. A hand-filled AGENTS.md kept telling
  operators to record completion by hand for months after finish shipped, which
  is the failure this replaces.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if agentsPointer != "" {
			fmt.Print(PointerStanza(agentsPointer))
			return nil
		}
		s, err := open()
		if err != nil {
			return err
		}
		record, err := s.mergeFlag(agentsPermissive, agentsRestrict, agentsWrite)
		if err != nil {
			return err
		}
		body, err := s.renderAgents()
		if err != nil {
			return err
		}
		if agentsWrite == "" {
			fmt.Print(body)
			return nil
		}
		if err := s.confirmPolicy(agentsWrite, agentsYes); err != nil {
			return err
		}
		if record != nil {
			if err := record(); err != nil {
				return err
			}
		}
		return s.writeAgents(agentsWrite, body)
	},
}

func (s *session) renderAgents() (string, error) {
	body, err := s.generatedAgents()
	if err != nil {
		return "", err
	}
	addendum, err := s.readAddendum()
	if err != nil {
		return "", err
	}
	if addendum != "" {
		body += "\n\n" + addendumSection(addendum)
	}
	block := fmt.Sprintf("%s\n<!-- generated by `drover agents`; edit .drover.toml and regenerate -->\n%s\n\n%s\n\n%s\n",
		agentsBegin, policyLine(s.cfg), body, agentsEnd)
	// The addendum is the one text in the block drover did not write, and a
	// marker line or an unclosed fence in it moves where findBlock reads the
	// block's end. Written anyway, the file is stale or malformed the moment
	// it lands, and nothing regenerating can fix it.
	if addendum != "" {
		if span := findBlock(block); !span.Found || span.Malformed || span.between(block) != strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(block, agentsBegin), agentsEnd+"\n")) {
			return "", fmt.Errorf("[agents] addendum %s holds a drover marker line or an unclosed code fence, "+
				"either of which ends the generated block early: remove the marker or close the fence", s.cfg.AddendumPath())
		}
	}
	if agentsBare {
		return body + "\n", nil
	}
	return block, nil
}

// addendumHeading opens the addendum's section, supplied by drover so that the
// file itself can be plain notes with no idea where it will be placed.
const addendumHeading = "## Project addendum"

// addendumSection is the addendum as the block carries it. Trailing newlines
// are dropped because the block's own spacing before the closing marker
// replaces them; everything else is the file as written.
func addendumSection(text string) string {
	return addendumHeading + "\n\n" + strings.TrimRight(text, "\r\n")
}

// addendumErr is why the configured addendum stops the block rendering, or nil.
// A render that fails before the addendum is placed is the config's or the
// template's defect, reported elsewhere, and not blamed on the addendum.
func (s *session) addendumErr() error {
	if _, err := s.readAddendum(); err != nil {
		return err
	}
	if _, err := s.generatedAgents(); err != nil {
		return nil
	}
	_, err := s.renderAgents()
	return err
}

// readAddendum returns the configured addendum's text, or empty when none is
// configured. A configured file that cannot be read is an error naming the
// resolved path, never an empty addendum: rendering without it would write a
// block that silently drops what the project asked every agent to know.
func (s *session) readAddendum() (string, error) {
	path := s.cfg.AddendumPath()
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's configured addendum
	if err != nil {
		return "", fmt.Errorf("[agents] addendum %s cannot be read: %w", path, err)
	}
	return string(b), nil
}

// generatedAgents is the text drover writes from the config and this binary,
// without markers or addendum.
func (s *session) generatedAgents() (string, error) {
	cfg := s.cfg
	d := agentsData{
		Project:        cfg.Project,
		Git:            cfg.Git.On(),
		Bodies:         cfg.Paths.Backlog,
		Ledger:         cfg.Paths.Ledger,
		Reports:        cfg.Paths.Reports,
		Specs:          cfg.Paths.Specs,
		Skills:         cfg.Paths.Skills,
		Implement:      cfg.Prompts.Implement,
		Draft:          cfg.Prompts.Draft,
		Review:         cfg.Prompts.Review,
		ReviewRunner:   cfg.Run.ReviewRunner,
		DraftRunner:    cfg.Run.DraftRunner,
		Branch:         cfg.Run.Branch,
		MergeGate:      strings.Join(cfg.Loop.MergeGate, " "),
		Required:       cfg.Frontmatter.Required,
		Optional:       cfg.Frontmatter.Optional,
		Closes:         cfg.Known(backlog.KeyCloses),
		Issue:          cfg.Known(backlog.KeyIssue),
		StopMarker:     cfg.Intake.StopMarker,
		FollowUpLabel:  cfg.GitHub.FollowUpLabel,
		SeverityPrefix: cfg.GitHub.SeverityPrefix,
		Posture:        cfg.Agents.Principles,
		Safety:         SafetyTable(),
		Effects:        CommandEffects(varsFor(cfg)),
		Destinations:   prompts.Destinations(),
		Operator:       OperatorCommands(),
		Policies:       policyRows(cfg),
		DocsRef:        docsRef(),
	}
	d.SupervisorRefused = SupervisorRefused(cfg)
	if p, _ := cfg.Policy("merge_drive_pr"); supervisorMay(p) {
		d.MergeDrivePR = true
		d.MergeDo = strings.ReplaceAll(mergeDrivePR, "{id}", "<id>")
	}
	if p, _ := cfg.Policy("merge_protected_paths"); supervisorMay(p) {
		d.MergeDenyPaths = true
	}
	if d.Posture == "" {
		// Named as unset rather than omitted: a review step that silently loses
		// its standard reads as a review with no standard.
		d.Posture = "<unset — put this project's quality and security bar in [agents] principles>"
	}

	// default first, then the flagged gates in the order SelectVerify tries them,
	// so the table reads in precedence order rather than alphabetically around
	// a default buried mid-list.
	if def, ok := cfg.Verify["default"]; ok && def != "" {
		d.VerifyCommands = append(d.VerifyCommands, agentsVerify{Name: "default", Command: def})
	}
	flagged := make([]string, 0, len(cfg.Verify))
	for name := range cfg.Verify {
		if name != "default" {
			flagged = append(flagged, name)
		}
	}
	sort.Strings(flagged)
	for _, name := range flagged {
		d.VerifyCommands = append(d.VerifyCommands, agentsVerify{Name: name, Command: cfg.Verify[name]})
	}

	for _, prefix := range cfg.Prefixes() {
		d.Namespaces = append(d.Namespaces, agentsNamespace{Prefix: prefix, Kind: cfg.IDs.Namespaces[prefix]})
	}

	t, err := template.New("agents").Option("missingkey=error").
		Funcs(template.FuncMap{"cell": tableCell}).Parse(agentsTemplate)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// tableCell makes s safe to render as one markdown table cell. A config value
// reaches the generated tables unvetted, and a pipe in it ends the cell early
// while a newline ends the row. A backslash run directly before a pipe is
// doubled, because GFM pairs a backslash with the character after it when it
// splits a row, so an odd run would consume the escape and split anyway.
func tableCell(s string) string {
	var b strings.Builder
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
			continue
		case '|':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteRune(r)
		case '\r', '\n':
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(' ')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteRune(r)
		}
		slashes = 0
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	return b.String()
}

// writeAgents replaces the marked block in place, or appends it. Anything
// outside the markers survives untouched, including a hand-written preamble
// that explains the project to a reader before drover explains itself.
func (s *session) writeAgents(path, block string) error {
	abs := s.sessionPath(path)
	target, unlink := s.sessionLink(abs)
	put := func(out, did string) error {
		if !unlink {
			if err := os.WriteFile(abs, []byte(out), 0o644); err != nil { // #nosec G306
				return err
			}
			fmt.Fprintln(errOut, did)
			return nil
		}
		if err := replaceLink(abs, []byte(out)); err != nil {
			return err
		}
		fmt.Fprintf(errOut, "wrote %s in this worktree; the link to %s is replaced, so the checkout's copy is unchanged\n", path, target)
		return nil
	}
	existing, err := os.ReadFile(abs) // #nosec G304 -- operator-named path
	switch {
	case os.IsNotExist(err):
		return put(block, "wrote "+path)
	case err != nil:
		return err
	}

	text := string(existing)
	span := findBlock(text)
	if !span.Found {
		return put(strings.TrimRight(text, "\n")+"\n\n"+block, "appended the drover block to "+path)
	}
	// An opening marker with no closing one after it means the file was edited
	// into a shape this cannot repair without guessing which half to keep.
	if span.Malformed {
		return Refusal{fmt.Errorf("%s has %s before %s, or no %s at all — fix the markers by hand",
			path, agentsEnd, agentsBegin, agentsEnd)}
	}

	lines := strings.Split(text, "\n")
	out := strings.Join(lines[:span.Start], "\n")
	if span.Start > 0 {
		out += "\n"
	}
	out += strings.TrimRight(block, "\n")
	if tail := lines[span.End+1:]; len(tail) > 0 {
		out += "\n" + strings.Join(tail, "\n")
	}
	return put(out, "regenerated the drover block in "+path)
}

// sessionPath is where path lands for this invocation. A launched session's
// project root is the operator's checkout, through DROVER_ROOT, so a session
// working in a drive worktree would otherwise write the checkout's file. There
// the path resolves against the same project root inside the session's own
// tree; anywhere else it resolves against the project root as always.
func (s *session) sessionPath(path string) string {
	abs := s.cfg.Abs(path)
	if !runner.Launched() || abs != filepath.Join(s.cfg.Root, path) {
		return abs
	}
	wd, err := os.Getwd()
	if err != nil {
		return abs
	}
	top, err := showToplevel(wd)
	if err != nil {
		return abs
	}
	checkout, err := showToplevel(s.cfg.Root)
	if err != nil || resolved(top) == resolved(checkout) {
		return abs
	}
	rel, err := filepath.Rel(resolved(checkout), resolved(s.cfg.Root))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return abs
	}
	return filepath.Join(top, rel, path)
}

// sessionLink reports when a session drover launched names, from its worktree,
// a link drive made there: a [drive] worktree_links entry pointing at the
// operator's checkout. Writing through it would put a branch's unmerged
// rendering in front of every command and agent in the checkout, so the write
// replaces the link with a file of the worktree's own instead.
//
// Only a link drive could have made qualifies, by the test linkPaths applies
// when it restores one: untracked, in a tree that is not the checkout. A
// tracked link is the branch's content, and replacing it would commit a
// typechange; a link in the checkout is the operator's, and a supervisor
// running there has no worktree copy to write instead.
func (s *session) sessionLink(link string) (target string, ok bool) {
	if !runner.Launched() {
		return "", false
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	dest, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(link), dest)
	}
	target = resolved(dest)
	top, err := showToplevel(filepath.Dir(link))
	if err != nil {
		return "", false
	}
	checkout, err := showToplevel(s.cfg.Root)
	if err != nil || resolved(top) == resolved(checkout) {
		return "", false
	}
	if rel, err := filepath.Rel(resolved(top), target); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return "", false
	}
	if gitcmd.Command(filepath.Dir(link), "ls-files", "--error-unmatch", "--", filepath.Base(link)).Run() == nil {
		return "", false
	}
	return target, true
}

// replaceLink puts a regular file holding data where the link at path is,
// through a rename so that a failed write leaves the link in place rather than
// nothing at all, and a reader never sees the path missing.
func replaceLink(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // #nosec G302 -- matches the plain write
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func init() {
	agentsCmd.Flags().StringVar(&agentsWrite, "write", "", "replace the drover block in this file instead of printing")
	agentsCmd.Flags().BoolVar(&agentsBare, "bare", false, "omit the markers and the generated-by note")
	agentsCmd.Flags().StringVar(&agentsPointer, "pointer", "", "print the stanza that sends an agent from AGENTS.md to this path, and exit")
	agentsCmd.Flags().BoolVar(&agentsPermissive, "permissive-merge", false,
		"with --write, set [supervise] merge_drive_pr and merge_protected_paths to supervisor, so a supervising session may merge drive's pull requests")
	agentsCmd.Flags().BoolVar(&agentsRestrict, "restrictive-merge", false,
		"with --write, set [supervise] merge_drive_pr and merge_protected_paths to operator, so every merge stays with a person")
	agentsCmd.Flags().BoolVar(&agentsYes, "yes", false, "write a block that widens a policy without asking")
	root.AddCommand(agentsCmd)
}
