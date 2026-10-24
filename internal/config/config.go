// Package config reads the per-project .drover.toml that tells drover where a
// project keeps its backlog and what "done" costs there.
package config

import (
	"fmt"
	"net/url"
	"os"
	stdpath "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/dashboard"
	"github.com/ravinald/drover/internal/runner"
	"github.com/ravinald/drover/internal/sev"
)

// FileName is the per-project config, found by walking up from the working
// directory. Its directory is the project root every relative path resolves
// against, so the root is discovered rather than assumed: a root derived from a
// script's own location breaks the moment the script is symlinked.
const FileName = ".drover.toml"

// SchemaVersion is the config format this build understands.
//
// A config written against another version would load with the keys this build
// does not read silently dropped, so the mismatch is refused and the refusal
// names what to do.
const SchemaVersion = 2

// Config is one project's answer to where things live and what gates them.
type Config struct {
	SchemaVersion int `toml:"schema_version"`
	// DroverVersion is the release this config was last reviewed against,
	// written by init and by `drover config --stamp`. A binary of a newer minor
	// release refuses to run over it until someone has looked: a key added
	// since reads as its zero value, which is indistinguishable from a choice.
	DroverVersion string `toml:"drover_version"`
	Project       string `toml:"project"`

	Paths       Paths                  `toml:"paths"`
	IDs         IDs                    `toml:"ids"`
	Frontmatter Frontmatter            `toml:"frontmatter"`
	Verify      map[string]string      `toml:"verify"`
	Prompts     Prompts                `toml:"prompts"`
	Review      Review                 `toml:"review"`
	Status      Status                 `toml:"status"`
	Severity    Severity               `toml:"severity"`
	Intake      Intake                 `toml:"intake"`
	Agents      Agents                 `toml:"agents"`
	Git         Git                    `toml:"git"`
	Loop        Loop                   `toml:"drive"`
	Finish      Finish                 `toml:"finish"`
	Sequences   map[string]string      `toml:"numbered_files"`
	GitHub      GitHub                 `toml:"github"`
	Forge       Forge                  `toml:"forge"`
	Usage       Usage                  `toml:"usage"`
	Register    Register               `toml:"register"`
	Backlog     Backlog                `toml:"backlog"`
	Run         Run                    `toml:"run"`
	Runners     map[string]runner.Spec `toml:"runners"`
	// Supervise maps supervised action keys to policies, beside the three keys
	// that are not actions: template, notify and the ntfy table. Read it
	// through Policy, SuperviseTemplate, Notify and Ntfy.
	//
	// A map rather than a struct because the action keys are a catalog the cmd
	// package owns (RegisterActions), and walking it as an open table keeps a
	// project's policy lines out of the retired-key report while still carrying
	// them through `drover config` unchanged. An omitted key takes the action's
	// default, so leaving one out is never a gap.
	Supervise map[string]any `toml:"supervise"`

	// Root is the directory holding the config. Not read from the file.
	Root string `toml:"-"`

	// sources are the files this config was layered from, global first.
	sources []string

	// shapes are Frontmatter.Shape parsed, filled at load so no caller can meet
	// a spec validate already refused. Read it through Shapes.
	shapes map[string]backlog.Shape

	// layers record what each file wrote, in the order it was layered: the
	// per-user file first where one had a say, then the project's. A decoded
	// value cannot answer which file chose it, because TOML merges tables across
	// the two decodes and applyDefaults fills in the rest. Read it through
	// Declared, ProjectDeclared, OpenTable and InheritedTable.
	layers []layer
}

// Paths are project-relative except Logs, which is absolute or empty.
type Paths struct {
	Backlog string `toml:"backlog"`
	Ledger  string `toml:"ledger"`
	Reports string `toml:"reports"`
	Specs   string `toml:"specs"`
	Skills  string `toml:"skills"`
	// Shelf holds retired bodies: items deliberately not going to be done,
	// moved out of Backlog rather than deleted. It has no default, because
	// defaulting it would make every project that never wanted one behave
	// differently the day it upgraded. drover reads ids from the filenames
	// there and never parses what is in them.
	Shelf string `toml:"shelf"`
	// RootMarker must exist for the project root to be considered sane. It
	// catches a config that travelled to a tree it does not describe.
	RootMarker string `toml:"root_marker"`
	// Logs defaults to $XDG_CACHE_HOME/drover/<project>. Run logs are multi-MB
	// and belong outside the repo.
	Logs string `toml:"logs"`
	// LogPrefix is prepended to every log filename. It exists so a project
	// migrating from another driver keeps its existing runs discoverable.
	LogPrefix string `toml:"log_prefix"`
	// Register is the append-only JSONL the file backend reads. Only
	// [register] backend = "file" opens it; every other backend ignores it.
	Register string `toml:"register"`
}

// IDs bounds the planned backlog, names the intake namespaces and says where a
// new id is allocated from.
type IDs struct {
	// PlannedMax rejects an out-of-range planned id as a typo. Zero means
	// unbounded, which is right for a project with no closed plan.
	PlannedMax int `toml:"planned_max"`
	// Namespaces maps a prefix to the kind of work it holds: B = "bug".
	Namespaces map[string]string `toml:"namespaces"`
	// Allocator is one of Allocators(). Empty resolves through IDAllocator, so
	// a config written before the key existed allocates from disk as it did.
	//
	// Off the network by default because "git" pushes a ref on every new item,
	// and a project that never named a shared remote must not start writing to
	// origin the day it upgraded.
	Allocator string `toml:"allocator"`
	// Remote is the git remote name or URL holding the counter under "git".
	// Empty resolves to origin through IDRemote.
	Remote string `toml:"remote"`
}

// The places an id can be allocated from.
const (
	AllocatorLocal = "local"
	AllocatorGit   = "git"
)

// Allocators lists the allocators, default first.
func Allocators() []string { return []string{AllocatorLocal, AllocatorGit} }

// IDAllocator resolves the allocator, local when nothing declares one.
func (c *Config) IDAllocator() string {
	if c.IDs.Allocator != "" {
		return c.IDs.Allocator
	}
	return AllocatorLocal
}

// IDRemote resolves the remote the git allocator pushes to.
func (c *Config) IDRemote() string {
	if c.IDs.Remote != "" {
		return c.IDs.Remote
	}
	return "origin"
}

// Frontmatter declares the metadata schema. It stays closed — an unknown key is
// an error, because a typo'd `deps` would render a blocked item READY, which is
// a wrong answer rather than a degraded one. Declaring the set per project is
// what lets a second project add its own axis without forking the tool.
type Frontmatter struct {
	// Required keys must be present. They may be empty; presence is the point.
	Required []string `toml:"required"`
	Optional []string `toml:"optional"`
	// Shape maps a declared key to what its value may hold: "int",
	// "enum:C,H,M,L" or "idlist". It is optional and additive — a project
	// writing none is checked exactly as it was before the table existed —
	// because making it required would fail every project on the day it
	// upgraded. drover check reports a value that fails one as BAD VALUE.
	Shape map[string]string `toml:"shape"`
}

// Prompts points at the templates that assemble what each session is handed:
// the implementing run, the reviewer, and `draft`, which needs what proposing
// an item's numbered requirements takes rather than what executing them does.
// One section because all three answer the same question, and a project
// overriding one usually wants the others beside it.
type Prompts struct {
	Implement string `toml:"implement"`
	Draft     string `toml:"draft"`
	Review    string `toml:"review"`
}

// Review bounds how often a review may send one item back.
type Review struct {
	// MaxReviewRounds is how many times a review may send one item back before
	// `request-changes` refuses. Zero, and an absent key, mean no cap.
	//
	// Off by default, and absent means the same as zero because nothing reads
	// the difference. A number drover picked would change when `request-changes` refuses
	// for every project on the day it upgraded, and a cap is a policy about how
	// much a loop may spend, which drover does not know. Set it higher than the
	// worst item that went round the loop and merged correctly.
	MaxReviewRounds int `toml:"max_review_rounds"`
}

// Status shapes the table.
type Status struct {
	// Column is the frontmatter key shown beside the id. An item with no value
	// for it falls back to its namespace kind, which is what makes one setting
	// serve both a planned backlog and an intake queue.
	Column string `toml:"column"`
	// ColumnPrefix is prepended to that value: "W" renders wave 3 as W3.
	ColumnPrefix string `toml:"column_prefix"`
	// Timezone is the zone status reports every time in: UTC, local, or an
	// IANA name. Empty is UTC. The ledger keeps each writer's own offset, so
	// without one setting a report shows -07:00 beside Z.
	Timezone string `toml:"timezone"`
	// Refresh is how often, in seconds, builtin:html asks the browser to
	// reload it. Absent is 60; 0 writes no refresh at all, which is why it is a
	// pointer.
	Refresh *int `toml:"refresh"`
	// Index is the href of the cross-project index, the page status
	// --all-projects --html renders, relative to this project's page or
	// absolute. builtin:html links its header back to it when set.
	Index string `toml:"index"`
	// Render is the files status renders through a project's own template.
	// drive keeps each one current while it runs, and status --render writes
	// them all once.
	Render []Render `toml:"render"`
}

// Location is the zone Timezone names. Load has validated it, so the error
// path is unreachable on a loaded config and falls back to UTC.
func (s Status) Location() *time.Location {
	loc, err := ParseZone(s.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// RefreshSeconds is Refresh, and its default on a Status no load filled in.
func (s Status) RefreshSeconds() int {
	if s.Refresh == nil {
		return 60
	}
	return *s.Refresh
}

// checkIndex accepts a URL reference a browser follows from a page: a path,
// relative or absolute, or an http, https or file URL. Any other scheme is
// one html/template would neutralize into a dead link, and is better refused
// where the author typed it.
func checkIndex(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.TrimSpace(ref) != ref {
		return fmt.Errorf("it has leading or trailing whitespace")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return fmt.Errorf("not a URL reference: %v", err)
	}
	switch u.Scheme {
	case "", "http", "https", "file":
		return nil
	}
	return fmt.Errorf("scheme %q is not one a page links to (known: http, https, file, or none for a path)", u.Scheme)
}

// ParseZone reads a status time zone: UTC, local, or an IANA name.
func ParseZone(name string) (*time.Location, error) {
	switch name {
	case "", "UTC":
		return time.UTC, nil
	case "local":
		return time.Local, nil
	}
	return time.LoadLocation(name)
}

// Render is one template and the file it renders to. Both resolve against the
// project root unless absolute, and a leading ~/ resolves against $HOME. A
// template named builtin:<name> is one drover ships rather than a file.
type Render struct {
	Template string `toml:"template"`
	Path     string `toml:"path"`
}

// Severity colours the rank an item declares, so a backlog of audit findings
// can be scanned rather than read.
//
// Nothing here can reject an item. An absent, empty or unrecognised value ranks
// last and renders with no escape sequence, because severity orders a view and
// a backlog that will not load over a typo'd rank helps nobody.
type Severity struct {
	// Key is the frontmatter key holding the rank. It must also be declared in
	// [frontmatter]; drover check says so when it is not.
	Key string `toml:"key"`
	// Ranks maps a rank to a palette name. Keyed by letter or word — both
	// resolve to the same rank, so { C = "red" } and { Critical = "red" } are
	// the same table.
	Ranks map[string]string `toml:"ranks"`
}

// SeverityConfigured reports whether a config wrote a [severity] block.
// Severity defaults on for every project, so "the key is undeclared" is only a
// finding when someone configured the block and got the key wrong. Otherwise it
// is the ordinary state of a project that does not use it.
func (c *Config) SeverityConfigured() bool { return c.Declared("severity") }

// RunnerKeyConfigured reports whether a config set [run] runner_key itself
// rather than taking the default, and is asked for the same reason: the key
// defaults to "runner" for every project, so an undeclared one is ordinary
// unless somebody set it themselves.
func (c *Config) RunnerKeyConfigured() bool { return c.Declared("run.runner_key") }

// SevRank ranks a raw frontmatter value.
func (c *Config) SevRank(raw string) sev.Rank { return sev.Of(raw) }

// SevColour resolves a raw frontmatter value to SGR parameters, empty when the
// value does not rank or its rank is mapped to no colour.
func (c *Config) SevColour(raw string) string {
	r := sev.Of(raw)
	if r == sev.Unranked {
		return ""
	}
	code, _ := sev.Colour(c.Severity.Ranks[r.Letter()])
	return code
}

// Run governs launching.
type Run struct {
	// Branch is a template over the prompt data less .FollowUps,
	// which come from the tracker: a name derived from them would change when
	// gh went unreachable, leaving finish looking for a branch run never made.
	// The default gives a planned item its bare slug and prefixes an intake
	// item with its padded id, because an issue-derived slug collides far more
	// readily and committing onto someone else's branch is silent.
	Branch string `toml:"branch"`
	// DefaultRunner is used when neither the command line nor the item's
	// frontmatter names one.
	DefaultRunner string `toml:"default_runner"`
	// RunnerKey is the frontmatter key that pins a runner per item.
	RunnerKey string `toml:"runner_key"`
	// ReviewRunner is the runner `review` hands the brief to when the command
	// line names none. It has no default and no per-item key: unset means
	// review prints, and the reviewing agent is a property of how a project
	// audits rather than of what an item asks for. An item's RunnerKey names
	// who implements it, and reviewing with that same value is the
	// self-certification `audit` refuses to record as verified.
	ReviewRunner string `toml:"review_runner"`
	// DraftRunner is the runner `draft` hands its brief to when the command
	// line names none. Unset means draft prints. Like ReviewRunner it has no
	// per-item key: an item that does not yet have requirements cannot name who
	// should write them.
	DraftRunner string `toml:"draft_runner"`
	// Trunk is the branch work merges into. Empty means ask origin, then main.
	Trunk string `toml:"trunk"`
}

// Intake governs items scaffolded from an issue tracker.
type Intake struct {
	// StopMarker holds a scaffolded item inert until a human writes its
	// requirements. Those requirements become the audit checklist, so one lifted
	// mechanically from a bug report is a checkbox nobody can falsify.
	StopMarker string `toml:"stop_marker"`
}

// Agents shapes what `drover agents` generates.
type Agents struct {
	// Principles is the bar a reviewer holds the code to: the one thing about a
	// project drover cannot read off its own config. It lives here rather than
	// as a hand-edit in the generated file so that regenerating cannot lose it,
	// which is the failure the generator exists to prevent.
	Principles string `toml:"principles"`
	// Addendum names a file whose text the generated block carries verbatim,
	// after everything drover writes. Principles holds one sentence; a project's
	// conventions, where its notes live, a dashboard to keep current are prose,
	// and prose hand-edited between the markers is deleted by the next
	// regeneration. Read through AddendumPath. The file may live outside the
	// repository, so ~/ and absolute paths are allowed.
	Addendum string `toml:"addendum"`
}

// AddendumPath is where [agents] addendum resolves, or empty when unset.
func (c *Config) AddendumPath() string { return c.Abs(c.Agents.Addendum) }

// Git declares whether the project has a work tree drover may drive.
//
// Branch automation and the merge check are the only parts of drover that need
// one: run switches to the item's branch before launching, finish verifies the
// merge before writing the ledger row, status names the branch you are standing
// on. Everything else — the backlog, the ledger, the audit levels, the verify
// command — is
// files on disk and works in a directory that was never a repository.
//
// It is declared rather than probed for the reason [register] backend is. Reading
// it off whether .git happens to be present means the tool changes what it does
// when a directory is copied, and the failure is silent in the direction that
// matters: a run that quietly skipped the branch switch leaves the work on
// whatever was already checked out.
type Git struct {
	// Enabled is a pointer so that unset and false stay distinguishable, and so
	// that the zero value is the safe reading. A plain bool defaulting to true
	// would mean every Config built without Load — every test fixture, every
	// caller assembling one by hand — silently had branch automation off.
	//
	// Read it through On, never directly.
	Enabled *bool `toml:"enabled"`
	// MergeCheck gives a forge that is not GitHub an authority over whether a
	// branch merged. When set it answers instead of gh: finish runs it from the
	// project root with the branch and the trunk appended as the last two
	// arguments, and reads exit 0 as merged. Any non-zero exit means not merged
	// rather than an error, so a check that has to distinguish the two reports
	// the failure on stderr and exits non-zero anyway; git ancestry is still
	// consulted after a no, and its yes still stands.
	//
	// Unset, gh answers. Ancestry alone answers neither: on a squash- or
	// rebase-merging forge it reports every merged branch unmerged forever,
	// which makes --force the ordinary path and leaves finish checking nothing.
	MergeCheck []string `toml:"merge_check"`
	// MergeMethod is how drive asks the forge to merge: one of MergeMethods,
	// passed to `gh pr merge` as its flag. It describes the repository rather
	// than the person driving it, so the per-user file may not set it: one value
	// there would squash every project on the machine, including the one whose
	// history depends on merge commits.
	MergeMethod string `toml:"merge_method"`
}

// MergeMethods are the values [git] merge_method accepts, default first.
func MergeMethods() []string { return []string{"squash", "merge", "rebase"} }

// On reports whether drover may drive a work tree here. Unset means yes: a
// project that says nothing about git is the overwhelming majority, and the
// declaration exists for the one that has none.
func (g Git) On() bool { return g.Enabled == nil || *g.Enabled }

// Loop governs `drover drive`, the unattended walk of the queue.
//
// Whether a head may merge is the gate's answer alone: a required-check list
// and how long a run may take are each a property of the project, and the whole
// answer a loop needs is an exit status. A gate that has to wait waits in its
// own process, where the project already knows what it is waiting for. What
// drive reads of the checks before the gate runs is [forge]'s.
//
// MergeGate is executed, like [runners] and [git] merge_check and unlike
// [verify], which renders into a prompt as text for the agent to run. See the
// trust table in concepts.md before adding another executed key, and note that
// gh is on that list without being a key: open-pr and the merge run it, so the
// forge is a binary dependency no configuration declares.
type Loop struct {
	// MergeGate is the project's go/no-go, run from the project root with the
	// branch and the trunk appended as the last two arguments.
	//
	// Three exit codes rather than merge_check's two, because a red check means
	// opposite things depending on what reddened it and only the command that
	// read the forge knows which it saw:
	//
	//	0  green, merge
	//	1  red, and this diff caused it: the finding goes back to the branch
	//	2  red, and the code did not cause it: stop the drive
	//
	// Anything else is read as 2. An unclassified failure stops rather than
	// loops, because a trunk that did not receive this merge is the trunk every
	// later item builds on, and a forge that is not answering will not answer
	// for the next item either.
	//
	// drive merges what it drives, so undeclared this is a config error and
	// drive refuses before its first launch: the operator asked for a merge and
	// named nothing that could say green. --no-merge is the run that needs no
	// verify, and there an absent one stops each item green-and-reviewed.
	MergeGate []string `toml:"merge_gate"`
	// DenyPaths are globs a merge refuses to carry, matched against the files a
	// branch changed.
	//
	// An agent that edits the gate, a workflow or this config in its own pull
	// request changes what every later item is measured by, and the
	// change would be reviewed by the same loop it just altered. Empty means the
	// built-in set; a project that means to allow one says so here.
	DenyPaths []string `toml:"protected_paths"`
	// TreeLinks are root-relative paths drive symlinks into each item's
	// worktree, beside the top directory of every [paths] entry inside the root,
	// which it links unasked. A worktree holds what git tracks and nothing more,
	// so a file the session or the gate reads that lives outside version control
	// (an overlay's docs, an agent's settings) is absent there until linked. Each
	// link has to be one git ignores, or it would be the first thing the item's
	// own dirty check reports.
	TreeLinks []string `toml:"worktree_links"`
	// TreeSetup runs in a worktree drive has just created, before the first
	// session: dependencies a fresh checkout lacks, like node_modules. A non-zero
	// exit parks that item with the output and leaves the queue running.
	TreeSetup []string `toml:"worktree_setup"`
	// MaxLoad is the 1-minute load average above which drive holds dispatch,
	// under [supervise] host_load_admission at auto. Zero means the CPU count:
	// a host running two drives side by side turned passing tests into flakes
	// well before it ran out of memory, and the CPU count is where a load
	// average starts to mean waiting. Read it through LoadCeiling.
	MaxLoad float64 `toml:"max_load"`
}

// LoadCeiling is [drive] max_load, or the CPU count where none is declared.
func (l Loop) LoadCeiling() float64 {
	if l.MaxLoad > 0 {
		return l.MaxLoad
	}
	return float64(runtime.NumCPU())
}

// Finish is the [finish] table: what runs once an item has merged.
//
// After is executed, like [drive] merge_gate: see the trust table in
// concepts.md. It runs only while [supervise] post_finish_check is above off,
// so declaring it alone changes nothing a project already does.
type Finish struct {
	// After is an argv run from the project root after every finish, with
	// DROVER_ITEM, DROVER_PR and DROVER_SHA in its environment. Exit 0 records
	// a post_finish ledger row; anything else files a follow-up against the item
	// and stops for the operator. Neither undoes the merge or fails the finish.
	After []string `toml:"post_check"`
}

// SequencePlaceholder is the one token a [numbered_files] glob carries, standing for
// the run of digits whose order matters after a merge.
const SequencePlaceholder = "{n}"

// SequenceGlob splits a [numbered_files] glob at its placeholder. Each half is a
// path.Match pattern of its own: the number sits between them, so neither half
// may reach across it.
func SequenceGlob(glob string) (before, after string) {
	before, after, _ = strings.Cut(glob, SequencePlaceholder)
	return before, after
}

// validateSequences refuses a [numbered_files] glob drive could not read a number
// out of. A glob that never matches would check nothing while reading as a
// declared check, which is the failure the table exists to close.
func (c *Config) validateSequences(path string) error {
	for _, name := range sortedKeys(c.Sequences) {
		glob := c.Sequences[name]
		if n := strings.Count(glob, SequencePlaceholder); n != 1 {
			return fmt.Errorf("%s: numbered_files.%s = %q carries %d %s placeholders; it needs exactly one, where the number sits",
				path, name, glob, n, SequencePlaceholder)
		}
		if glob == "" || stdpath.IsAbs(glob) || filepath.IsAbs(glob) || glob != stdpath.Clean(glob) || strings.HasPrefix(glob, "../") {
			return fmt.Errorf("%s: numbered_files.%s = %q must be a clean path relative to the repository root", path, name, glob)
		}
		before, after := SequenceGlob(glob)
		for _, g := range []string{strings.Replace(glob, SequencePlaceholder, "0", 1), before, after} {
			if _, err := stdpath.Match(g, ""); err != nil {
				return fmt.Errorf("%s: numbered_files.%s = %q is not a glob path.Match reads: %v", path, name, glob, err)
			}
		}
	}
	return nil
}

// GitHub names the label vocabulary. Labels classify a follow-up; who owns one
// is a link rather than a label (see gh.Issue.Owners).
type GitHub struct {
	// Repo is "owner/name". Leave it unset to let gh infer from the project
	// root's git remote; set it when the config does not live in the work tree.
	Repo string `toml:"repo"`
	// FollowUpLabel marks membership: an issue carrying it is a follow-up, and
	// an issue without it is the tracker's business and not drover's. The query
	// asks for it server-side, so a project's ordinary backlog is never read.
	FollowUpLabel  string `toml:"follow_up_label"`
	SeverityPrefix string `toml:"severity_prefix"`
	PageLimit      int    `toml:"page_limit"`
}

// Forge is drive's reading of a head's check runs, between the wait for them to
// finish and [drive] merge_gate. A red check is the forge's own infrastructure,
// the trunk's, or the code's, and only the last is the item's to fix. Read
// Reruns and ChecksWait through ForgeReruns and ForgeChecksWait, which resolve
// an undeclared key to its default.
type Forge struct {
	// InfraPatterns are log lines, matched case-insensitively as substrings of a
	// failed job's log, that mark it as the infrastructure failing. They add to
	// the built-in list rather than replacing it.
	InfraPatterns []string `toml:"infra_patterns"`
	// Reruns is how many times drive reruns one head's infrastructure failures
	// before reading the next as the code's. 0 never reruns.
	Reruns int `toml:"reruns"`
	// ChecksWait is how long a head may show no check run at all before drive
	// reports that no workflow ran on it, as a Go duration.
	ChecksWait string `toml:"checks_wait"`
}

// The [forge] defaults, applied where the key is read so that a declared 0 stays
// distinguishable from an undeclared key.
const (
	DefaultReruns     = 2
	DefaultChecksWait = 10 * time.Minute
)

// ForgeReruns is [forge] reruns, or its default when undeclared.
func (c *Config) ForgeReruns() int {
	if !c.Declared("forge.reruns") {
		return DefaultReruns
	}
	return c.Forge.Reruns
}

// ForgeChecksWait is [forge] checks_wait, or its default when undeclared or
// unreadable; validate refuses an unreadable one at load.
func (c *Config) ForgeChecksWait() time.Duration {
	d, err := time.ParseDuration(c.Forge.ChecksWait)
	if c.Forge.ChecksWait == "" || err != nil {
		return DefaultChecksWait
	}
	return d
}

func (c *Config) validateForge(path string) error {
	if c.Forge.Reruns < 0 {
		return fmt.Errorf("%s: forge.reruns must be 0 (never rerun) or positive", path)
	}
	if w := c.Forge.ChecksWait; w != "" {
		d, err := time.ParseDuration(w)
		if err != nil || d < 0 {
			return fmt.Errorf("%s: forge.checks_wait is %q: give a duration such as \"10m\"", path, w)
		}
	}
	for _, p := range c.Forge.InfraPatterns {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%s: forge.infra_patterns holds an empty pattern, which would match every log", path)
		}
	}
	return nil
}

// Usage is how status reads the usage rows: the window its report-level totals
// cover, the budget it measures them against, and when a session has run long
// enough to name. Read SessionMaxHours and ContextWarn through
// UsageSessionMaxHours and UsageContextWarn, which resolve an undeclared key to
// its default.
type Usage struct {
	// WeeklyBudget is the operator's estimate of the weekly limit in weighted
	// tokens. The service publishes no such figure, so this is a guess, and 0
	// means none was made.
	WeeklyBudget int64 `toml:"weekly_budget"`
	// Reset is when the weekly limit starts over, as "Sat 20:00
	// America/Los_Angeles". Empty means the window is the last 7 days.
	Reset string `toml:"reset"`
	// SessionMaxHours is how long a session may run, first row to last, before
	// status names it.
	SessionMaxHours int `toml:"session_max_hours"`
	// ContextWarn is the largest context, in tokens, a session may reach before
	// status names it.
	ContextWarn int64 `toml:"context_warn"`
}

// The [usage] defaults, applied where the key is read.
const (
	DefaultSessionMaxHours = 24
	DefaultContextWarn     = 400000
)

// UsageSessionMaxHours is [usage] session_max_hours, or its default.
func (c *Config) UsageSessionMaxHours() int {
	if c.Usage.SessionMaxHours <= 0 {
		return DefaultSessionMaxHours
	}
	return c.Usage.SessionMaxHours
}

// UsageContextWarn is [usage] context_warn, or its default.
func (c *Config) UsageContextWarn() int64 {
	if c.Usage.ContextWarn <= 0 {
		return DefaultContextWarn
	}
	return c.Usage.ContextWarn
}

func (c *Config) validateUsage(path string) error {
	if c.Declared("usage.weekly_budget") && c.Usage.WeeklyBudget <= 0 {
		return fmt.Errorf("%s: usage.weekly_budget is %d: give a positive count of weighted tokens, or leave it unset for no budget", path, c.Usage.WeeklyBudget)
	}
	if c.Usage.WeeklyBudget > 0 && c.Usage.Reset == "" {
		return fmt.Errorf("%s: usage.weekly_budget is measured from the limit's weekly reset, and usage.reset is unset: "+
			"add one, as reset = \"Sat 20:00 America/Los_Angeles\"", path)
	}
	if c.Usage.Reset != "" {
		if _, err := ParseReset(c.Usage.Reset); err != nil {
			return fmt.Errorf("%s: usage.reset: %v", path, err)
		}
	}
	if c.Declared("usage.session_max_hours") && c.Usage.SessionMaxHours <= 0 {
		return fmt.Errorf("%s: usage.session_max_hours is %d: give a positive number of hours", path, c.Usage.SessionMaxHours)
	}
	if c.Declared("usage.context_warn") && c.Usage.ContextWarn <= 0 {
		return fmt.Errorf("%s: usage.context_warn is %d: give a positive count of tokens", path, c.Usage.ContextWarn)
	}
	return nil
}

// Reset is a weekly instant: a weekday and a wall-clock time in a zone.
type Reset struct {
	Day          time.Weekday
	Hour, Minute int
	Zone         *time.Location
}

// ParseReset reads "<weekday> <HH:MM> <IANA zone>", as "Sat 20:00
// America/Los_Angeles". The weekday takes its three-letter or full English
// name, in any case.
func ParseReset(v string) (Reset, error) {
	f := strings.Fields(v)
	if len(f) != 3 {
		return Reset{}, fmt.Errorf("%q is not a weekday, a 24-hour time and a zone, as \"Sat 20:00 America/Los_Angeles\"", v)
	}
	var r Reset
	day, ok := weekdays[strings.ToLower(f[0])]
	if !ok {
		return Reset{}, fmt.Errorf("%q: %q is not a weekday (Sun, Mon, Tue, Wed, Thu, Fri, Sat)", v, f[0])
	}
	r.Day = day
	t, err := time.Parse("15:04", f[1])
	if err != nil {
		return Reset{}, fmt.Errorf("%q: %q is not a 24-hour time such as 20:00", v, f[1])
	}
	r.Hour, r.Minute = t.Hour(), t.Minute()
	if r.Zone, err = time.LoadLocation(f[2]); err != nil || strings.EqualFold(f[2], "local") {
		return Reset{}, fmt.Errorf("%q: %q is not an IANA zone such as America/Los_Angeles", v, f[2])
	}
	return r, nil
}

var weekdays = func() map[string]time.Weekday {
	m := map[string]time.Weekday{}
	for d := time.Sunday; d <= time.Saturday; d++ {
		m[strings.ToLower(d.String())] = d
		m[strings.ToLower(d.String()[:3])] = d
	}
	return m
}()

// Last is the most recent reset at or before now. It is computed on the zone's
// own calendar, so a reset at 20:00 stays at 20:00 across a daylight-saving
// change.
func (r Reset) Last(now time.Time) time.Time {
	local := now.In(r.Zone)
	back := (int(local.Weekday()) - int(r.Day) + 7) % 7
	at := time.Date(local.Year(), local.Month(), local.Day()-back, r.Hour, r.Minute, 0, 0, r.Zone)
	if at.After(now) {
		at = time.Date(local.Year(), local.Month(), local.Day()-back-7, r.Hour, r.Minute, 0, 0, r.Zone)
	}
	return at
}

// The register sources. A project picks exactly one: the read path is a single
// call, and everything above it ranks, routes and counts one gh.Index.
const (
	BackendGitHub = "github"
	BackendFile   = "file"
	BackendNone   = "none"
)

// Backends lists the sources in the order the reference documents them.
func Backends() []string { return []string{BackendGitHub, BackendFile, BackendNone} }

// Register selects where the follow-up register lives. It is a section of its own
// rather than a key under [github] because the whole point is a project that
// has no GitHub, and a setting reached through the block it turns off reads as
// an option of the thing it replaces.
type Register struct {
	// Backend is one of Backends(). Empty resolves through RegisterBackend,
	// which keeps a config that never names this key loading unchanged.
	Backend string `toml:"backend"`
}

// RegisterBackend resolves the source. Unset is the tracker, which is what the
// overwhelming majority of projects want and the only backend that needs no
// declaration.
func (c *Config) RegisterBackend() string {
	if c.Register.Backend != "" {
		return c.Register.Backend
	}
	return BackendGitHub
}

// RegisterDeclaration names the line that turned the register off, for the
// command reporting there is nothing to read.
func (c *Config) RegisterDeclaration() string {
	return `[register] backend = "` + c.RegisterBackend() + `"`
}

// The places an accepted item can be mirrored to.
const (
	MirrorNone   = "none"
	MirrorGitHub = "github"
)

// Mirrors lists the mirror targets, default first.
func Mirrors() []string { return []string{MirrorNone, MirrorGitHub} }

// Backlog governs where an item is visible beyond the file that holds it.
//
// The file stays the contract whatever this says. A mirror is one-way: drover
// writes the item's requirements to an issue and never reads them back, because
// an issue body anyone can edit cannot promise that the run and the audit read
// the same text. The issue is the index and the discussion surface, nothing more.
type Backlog struct {
	// Mirror is one of Mirrors(). Empty resolves through BacklogMirror, so a
	// config written before the key existed publishes nothing.
	//
	// Off by default because turning it on publishes item text: a project whose
	// backlog lives in a private overlay would leak it the day it upgraded.
	Mirror string `toml:"mirror"`
}

// BacklogMirror resolves the mirror target, none when nothing declares one.
func (c *Config) BacklogMirror() string {
	if c.Backlog.Mirror != "" {
		return c.Backlog.Mirror
	}
	return MirrorNone
}

// Prefixes returns the configured namespace keys in a stable order.
func (c *Config) Prefixes() []string {
	out := make([]string, 0, len(c.IDs.Namespaces))
	for k := range c.IDs.Namespaces {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Abs resolves a project-relative path against the root.
//
// A leading ~ expands to the home directory. Without that a config naming
// ~/.cache/x silently resolves inside the project, which for a log directory
// means writing multi-MB streams into the repo it was configured to keep them
// out of.
func (c *Config) Abs(rel string) string {
	if rel == "" {
		return ""
	}
	if rel == "~" || strings.HasPrefix(rel, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(rel, "~"), "/"))
		}
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(c.Root, rel)
}

// LogDir is where run logs land: the configured path, else a per-project
// directory under the user's cache.
func (c *Config) LogDir() string {
	if c.Paths.Logs != "" {
		return c.Abs(c.Paths.Logs)
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "drover", c.Project)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "drover", c.Project)
}

// RequiredKeys are the frontmatter keys every item must declare. They satisfy
// backlog.Schema together with Known.
func (c *Config) RequiredKeys() []string { return c.Frontmatter.Required }

// Shapes are the declared value contracts, keyed by frontmatter key. Empty for
// the project that declares none, which is every project until it says
// otherwise.
func (c *Config) Shapes() map[string]backlog.Shape { return c.shapes }

// DeclaredKeys are every frontmatter key, required before optional, in the
// order the project wrote them. A command adding a line to a fence puts it
// where this says, so the file reads like the ones beside it.
func (c *Config) DeclaredKeys() []string {
	out := make([]string, 0, len(c.Frontmatter.Required)+len(c.Frontmatter.Optional))
	out = append(out, c.Frontmatter.Required...)
	return append(out, c.Frontmatter.Optional...)
}

// Known reports whether key is declared in the frontmatter schema.
func (c *Config) Known(key string) bool {
	for _, k := range c.Frontmatter.Required {
		if k == key {
			return true
		}
	}
	for _, k := range c.Frontmatter.Optional {
		if k == key {
			return true
		}
	}
	return false
}

// Find walks up from dir looking for the config file, and returns the directory
// holding it. A session drive launched names its project in runner.EnvRoot, and
// that wins over the walk: the session works in a worktree whose own copy of the
// config, when it has one, points every record path into a tree that is removed
// once the item lands.
func Find(dir string) (string, error) {
	if root := os.Getenv(runner.EnvRoot); root != "" {
		if _, err := os.Stat(filepath.Join(root, FileName)); err != nil {
			return "", fmt.Errorf("%s names %s, which holds no %s: unset it to find the project from the working directory",
				runner.EnvRoot, root, FileName)
		}
		return root, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, FileName)); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			// Naming the walk matters: a reader standing deep in a configured
			// project sees this only when the root is genuinely wrong, and should
			// not be told to initialise a project that already exists above them.
			return "", fmt.Errorf(
				"no %s in %s or any parent directory — run `drover init` in the project root to create one",
				FileName, dir)
		}
		abs = parent
	}
}

// Load finds and reads the config for the tree containing dir, layering the
// project's .drover.toml over the per-user config at GlobalPath.
//
// Layering is the TOML decoder's own semantics rather than a merge of two
// parsed structures: decoding the global file and then the project file into
// the same value gives per-key override for scalars, per-key merge for tables,
// and wholesale replacement for arrays. So a project that overrides one gate
// keeps the rest, and a project that writes frontmatter.required replaces the
// list entirely — which is what an author declaring a schema means, and the one
// place a merge would have produced a set nobody wrote.
func Load(dir string) (*Config, error) {
	root, err := Find(dir)
	if err != nil {
		return nil, err
	}
	return loadRoot(root)
}

// LoadAt reads the config of the project rooted at root and nowhere else: no
// walk up from it and no runner.EnvRoot. A caller holding a list of roots, such
// as the per-user registry, asks about each one, and either lookup would answer
// about some other project: the variable names the one a session runs under,
// and the walk from a deleted project finds whatever encloses it.
func LoadAt(root string) (*Config, error) {
	if _, err := os.Stat(filepath.Join(root, FileName)); os.IsNotExist(err) {
		return nil, fmt.Errorf("no %s in %s", FileName, root)
	} else if err != nil {
		return nil, err
	}
	return loadRoot(root)
}

func loadRoot(root string) (*Config, error) {
	path := filepath.Join(root, FileName)

	c := &Config{}

	if g := GlobalPath(); g != "" {
		if _, err := os.Stat(g); err == nil {
			if err := c.layerGlobal(g); err != nil {
				return nil, err
			}
			c.sources = append(c.sources, g)
		}
	}
	c.sources = append(c.sources, path)

	globalNtfy := c.Supervise[SuperviseNtfyKey]
	md, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c.layerNtfy(globalNtfy)
	c.absorbDeclared(md, path, true)
	// Checked before unknown keys, because the keys a stale config carries are
	// unknown *as a consequence of* the version being old. Reporting
	// "unknown key(s): [github.caveat_label paths.bodies]" first names the
	// symptom and leaves the reader to work out that a rename happened.
	if err := c.versionMatches(c.versionPath(path)); err != nil {
		return nil, err
	}
	if err := unknownKeys(path, md); err != nil {
		return nil, err
	}
	c.Root = root

	// Hoisted ahead of validate because expandPaths renders {{ .Project }}: a
	// config with no project would otherwise resolve every templated path to
	// the same directory and then report something further down the file.
	if c.Project == "" {
		return nil, fmt.Errorf("%s: project is required — it names the log directory and the status banner", path)
	}
	if err := c.expandPaths(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.validate(path); err != nil {
		return nil, err
	}
	return c, nil
}

// validateRegister refuses a backend that contradicts the tracker settings
// beside it, for the reason two GitHub settings that disagree are refused:
// picking one silently is how a project ends up reading a register it thought
// it had configured somewhere else.
func (c *Config) validateRegister(path string) error {
	backend := c.Register.Backend
	switch backend {
	case "", BackendGitHub, BackendFile, BackendNone:
	default:
		return fmt.Errorf("%s: register.backend is %q (known: %s)",
			path, backend, strings.Join(Backends(), ", "))
	}
	if backend == BackendFile && c.GitHub.Repo != "" {
		return fmt.Errorf("%s: register.backend is %q and github.repo names %q — drop one", path, backend, c.GitHub.Repo)
	}
	return nil
}

// validateAllocator refuses an allocator drover does not know and a remote git
// would read as something else. The remote reaches git's argv, so one opening
// with a dash would be parsed as an option rather than a repository.
func (c *Config) validateAllocator(path string) error {
	switch c.IDs.Allocator {
	case "", AllocatorLocal, AllocatorGit:
	default:
		return fmt.Errorf("%s: ids.allocator is %q (known: %s)",
			path, c.IDs.Allocator, strings.Join(Allocators(), ", "))
	}
	remote := c.IDs.Remote
	if strings.HasPrefix(remote, "-") || strings.ContainsFunc(remote, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("%s: ids.remote is %q: name a git remote or URL, with no leading dash or whitespace", path, remote)
	}
	return nil
}

// validateAddendum refuses an addendum path Abs would resolve somewhere the
// author did not mean. Only ~/ expands, so ~bob/notes.md and a bare ~ would
// finish inside the project, or on the home directory itself. Whether the file
// is there is not a load error: check reports it, and refusing every command
// over a missing notes file would stop the one that reports it.
func (c *Config) validateAddendum(path string) error {
	a := c.Agents.Addendum
	if strings.HasPrefix(a, "~") && !strings.HasPrefix(a, "~/") {
		return fmt.Errorf("%s: agents.addendum is %q: only ~/ expands to the home directory, so name the file as ~/path or an absolute path", path, a)
	}
	if strings.ContainsFunc(a, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return fmt.Errorf("%s: agents.addendum is %q: a path with a control character in it is a typo", path, a)
	}
	return nil
}

// validateMirror refuses a mirror drover cannot keep. Publishing writes the new
// issue number into the item's issue key, and an item that cannot carry the key
// would be published again by every accept, one duplicate issue per run.
func (c *Config) validateMirror(path string) error {
	mirror := c.Backlog.Mirror
	switch mirror {
	case "", MirrorNone:
		return nil
	case MirrorGitHub:
	default:
		return fmt.Errorf("%s: backlog.mirror is %q (known: %s)",
			path, mirror, strings.Join(Mirrors(), ", "))
	}
	if !c.Known(backlog.KeyIssue) {
		return fmt.Errorf("%s: backlog.mirror is %q and [frontmatter] does not declare %q, "+
			"so no item can record the issue it was published as: add it to optional", path, mirror, backlog.KeyIssue)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = SchemaVersion
	}
	if c.Paths.Ledger == "" {
		c.Paths.Ledger = ".drover/ledger.jsonl"
	}
	if c.Paths.Backlog == "" {
		c.Paths.Backlog = ".drover/backlog"
	}
	if c.Paths.Reports == "" {
		c.Paths.Reports = ".drover/reports"
	}
	if c.Paths.Register == "" {
		c.Paths.Register = ".drover/register.jsonl"
	}
	if len(c.Frontmatter.Required) == 0 {
		c.Frontmatter.Required = []string{"title", "deps"}
	}
	if c.Severity.Key == "" {
		c.Severity.Key = "severity"
	}
	// Canonicalised to letters so a table written with words resolves the same,
	// and so validate below reports the rank the author wrote.
	if c.Severity.Ranks == nil {
		c.Severity.Ranks = map[string]string{"C": "red", "H": "orange", "M": "yellow", "L": "blue"}
	} else {
		ranks := make(map[string]string, len(c.Severity.Ranks))
		for k, v := range c.Severity.Ranks {
			if letter := sev.Of(k).Letter(); letter != "" {
				ranks[letter] = v
			} else {
				ranks[k] = v
			}
		}
		c.Severity.Ranks = ranks
	}
	if c.GitHub.FollowUpLabel == "" {
		c.GitHub.FollowUpLabel = "follow-up"
	}
	if c.GitHub.SeverityPrefix == "" {
		c.GitHub.SeverityPrefix = "severity:"
	}
	if c.GitHub.PageLimit == 0 {
		c.GitHub.PageLimit = 300
	}
	if c.Status.Refresh == nil {
		refresh := 60
		c.Status.Refresh = &refresh
	}
	if c.Verify == nil {
		c.Verify = map[string]string{}
	}
	if c.Run.Branch == "" {
		c.Run.Branch = "{{ if .Planned }}{{ .Slug }}{{ else }}{{ lower .Stem }}-{{ .Slug }}{{ end }}"
	}
	if c.Run.DefaultRunner == "" {
		c.Run.DefaultRunner = "claude"
	}
	if c.Run.RunnerKey == "" {
		c.Run.RunnerKey = "runner"
	}
	if c.Git.MergeMethod == "" {
		c.Git.MergeMethod = MergeMethods()[0]
	}
	if len(c.Runners) == 0 {
		c.Runners = runner.Builtin()
	}
}

// VersionError is Load's refusal of a config whose schema_version this build
// does not read.
//
// A type rather than a message, so a caller can route on the reason. A version
// gap is something a report can describe, and every other load failure is one
// nothing can see past; the version number alone cannot tell them apart, since
// a per-user file that failed to parse for its own reason leaves the project's
// schema_version looking as old as it ever was.
type VersionError struct {
	// Path is the file that declared the version refused, which is the
	// per-user file where the project inherits the key rather than writing it.
	Path string
	msg  string
}

func (e *VersionError) Error() string { return e.msg }

// versionPath is the file whose schema_version the layered config carries: the
// last layer that wrote the key, and the project's own where none did.
//
// Naming the project file for a version the per-user file declared sends the
// reader to edit a file that says nothing about it.
func (c *Config) versionPath(project string) string {
	return c.writer("schema_version", project)
}

// versionMatches refuses a config this build cannot read. An older one is named
// with the keys that changed since, because every key it carries will be
// reported as unknown otherwise and the cause will not appear anywhere.
func (c *Config) versionMatches(path string) error {
	refuse := func(format string, args ...any) error {
		return &VersionError{Path: path, msg: fmt.Sprintf(format, args...)}
	}
	if c.SchemaVersion == SchemaVersion {
		return nil
	}
	return refuse("%s: schema_version %d, this build understands %d",
		path, c.SchemaVersion, SchemaVersion)
}

func (c *Config) validate(path string) error {
	if err := c.versionMatches(path); err != nil {
		return err
	}
	// The marker proves the config describes this tree. Without it a config
	// copied to the wrong repo resolves every path against a stranger.
	if c.Paths.RootMarker != "" {
		if _, err := os.Stat(c.Abs(c.Paths.RootMarker)); err != nil {
			return fmt.Errorf("%s: root_marker %q not found under %s",
				path, c.Paths.RootMarker, c.Root)
		}
	}
	if c.IDs.PlannedMax < 0 {
		return fmt.Errorf("%s: ids.planned_max must be 0 (unbounded) or positive", path)
	}
	if c.Review.MaxReviewRounds < 0 {
		return fmt.Errorf("%s: review.max_review_rounds must be 0 (no cap) or positive", path)
	}
	if !slices.Contains(MergeMethods(), c.Git.MergeMethod) {
		return fmt.Errorf("%s: git.merge_method is %q (known: %s)",
			path, c.Git.MergeMethod, strings.Join(MergeMethods(), ", "))
	}
	for prefix := range c.IDs.Namespaces {
		if prefix == "" {
			return fmt.Errorf("%s: ids.namespaces has an empty prefix", path)
		}
		for _, r := range prefix {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("%s: namespace prefix %q must be uppercase ASCII — it becomes a filename component", path, prefix)
			}
		}
	}
	if err := c.validateAllocator(path); err != nil {
		return err
	}
	if err := c.validateAddendum(path); err != nil {
		return err
	}
	if len(c.Finish.After) > 0 && strings.TrimSpace(c.Finish.After[0]) == "" {
		return fmt.Errorf("%s: finish.post_check names no command: its first element is the binary to run", path)
	}
	if c.Loop.MaxLoad < 0 {
		return fmt.Errorf("%s: drive.max_load is %g: give a load average above 0, or leave it unset for the CPU count", path, c.Loop.MaxLoad)
	}
	if err := c.validateForge(path); err != nil {
		return err
	}
	if err := c.validateUsage(path); err != nil {
		return err
	}
	if _, err := ParseZone(c.Status.Timezone); err != nil {
		return fmt.Errorf("%s: status.timezone is %q: %v (known: UTC, local, or an IANA name such as America/Los_Angeles)",
			path, c.Status.Timezone, err)
	}
	for i, r := range c.Status.Render {
		if r.Template == "" || r.Path == "" {
			return fmt.Errorf("%s: status.render[%d] needs both template and path", path, i)
		}
		if name, ok := dashboard.Builtin(r.Template); ok {
			if _, err := dashboard.Source(name); err != nil {
				return fmt.Errorf("%s: status.render[%d].template: %v", path, i, err)
			}
		}
	}
	if c.Status.Refresh != nil && *c.Status.Refresh < 0 {
		return fmt.Errorf("%s: status.refresh is %d: it is seconds between reloads, or 0 for none", path, *c.Status.Refresh)
	}
	if err := checkIndex(c.Status.Index); err != nil {
		return fmt.Errorf("%s: status.index is %q: %v", path, c.Status.Index, err)
	}
	// A rank outside the vocabulary, or a colour outside the palette, is a
	// value the author just typed. Rendering it plain would hide the typo, and
	// the whole point of the block is that the colour is visible.
	for rank, colour := range c.Severity.Ranks {
		if sev.Of(rank) == sev.Unranked {
			return fmt.Errorf("%s: severity.ranks has unknown rank %q (known: %s)",
				path, rank, strings.Join(sev.Letters(), ", "))
		}
		if _, ok := sev.Colour(colour); !ok {
			return fmt.Errorf("%s: severity.ranks[%s] = %q is not a colour (known: %s)",
				path, rank, colour, strings.Join(sev.ColourNames(), ", "))
		}
	}
	if err := c.validateRegister(path); err != nil {
		return err
	}
	if err := c.validateMirror(path); err != nil {
		return err
	}
	if err := c.validateSequences(path); err != nil {
		return err
	}
	if err := c.validateSupervise(path); err != nil {
		return err
	}
	// A shape drover cannot read would check nothing while reading as though it
	// did, which is the whole gap the table closes. Refused at load for the
	// reason a typo'd colour is: the author just typed it, and no item is at
	// fault. Keyed on a key the schema does not declare it is the author's
	// mistake too, but nothing about it is fatal, so check reports that one.
	c.shapes = make(map[string]backlog.Shape, len(c.Frontmatter.Shape))
	for _, key := range sortedKeys(c.Frontmatter.Shape) {
		shape, err := backlog.ParseShape(c.Frontmatter.Shape[key])
		if err != nil {
			return fmt.Errorf("%s: frontmatter.shape.%s: %v", path, key, err)
		}
		c.shapes[key] = shape
	}
	// A key in both lists makes "required" a lie that reads as satisfied.
	for _, r := range c.Frontmatter.Required {
		for _, o := range c.Frontmatter.Optional {
			if r == o {
				return fmt.Errorf("%s: frontmatter key %q is both required and optional", path, r)
			}
		}
	}
	return nil
}

// Policy is who carries out one supervised action: drive itself, a
// supervising session that reports what it did, the operator after drover stops
// and says so, or nobody, which leaves today's park standing.
type Policy string

// The policies, lowest first. A ceiling caps how far up this list a project may
// move an action, so the order is the contract and not a presentation choice.
const (
	PolicyOff        Policy = "off"
	PolicyOperator   Policy = "operator"
	PolicySupervisor Policy = "supervisor"
	PolicyAuto       Policy = "auto"
)

// Policies lists every policy, lowest first.
func Policies() []Policy {
	return []Policy{PolicyOff, PolicyOperator, PolicySupervisor, PolicyAuto}
}

// Above reports whether p hands an action further from the operator than q.
func (p Policy) Above(q Policy) bool {
	return slices.Index(Policies(), p) > slices.Index(Policies(), q)
}

// ActionBounds is one supervised action's default policy and the highest one a
// project may set.
type ActionBounds struct {
	Default Policy
	Ceiling Policy
}

// Two of the [supervise] keys that are settings rather than action keys; the
// third is SuperviseNtfyKey.
const (
	SuperviseTemplateKey = "template"
	SuperviseNotifyKey   = "notify"
)

// actions is the catalog's bounds, keyed by action. The catalog lives in the
// cmd package beside the commands that implement it, and cmd imports this
// package, so it is handed over at init rather than imported.
var actions map[string]ActionBounds

// RegisterActions hands Load the supervised actions it validates [supervise]
// against. Until it is called, Load checks only that each value names a policy:
// no key can be refused as unknown and no ceiling enforced, which is the state
// of a caller that never links the catalog.
func RegisterActions(m map[string]ActionBounds) { actions = m }

// Policy is the effective policy for an action, and whether a config declared
// it rather than taking the catalog default. It reads the raw table, so it
// answers for an Inspection that was never validated as readily as for a Config
// Load returned.
func (c *Config) Policy(action string) (Policy, bool) {
	if s, ok := c.Supervise[action].(string); ok {
		return Policy(s), true
	}
	return actions[action].Default, false
}

// SuperviseTemplate is where [supervise] template resolves, or empty when unset.
func (c *Config) SuperviseTemplate() string {
	s, _ := c.Supervise[SuperviseTemplateKey].(string)
	return c.Abs(s)
}

// Notify is the [supervise] notify argv, empty when unset.
func (c *Config) Notify() []string {
	raw, _ := c.Supervise[SuperviseNotifyKey].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// validateSupervise refuses a [supervise] table drover would have to guess
// about. A policy above its ceiling is the one that matters: the ceiling is
// where a person has to stay in the loop, and a config that silently clamped
// it would read as having handed the action away.
func (c *Config) validateSupervise(path string) error {
	names := make([]string, 0, len(c.Supervise))
	for k := range c.Supervise {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, key := range names {
		v := c.Supervise[key]
		switch key {
		case SuperviseTemplateKey:
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s: supervise.template is %v: name a template file", path, v)
			}
			continue
		case SuperviseNtfyKey:
			if err := c.validateNtfy(path, v); err != nil {
				return err
			}
			continue
		case SuperviseNotifyKey:
			raw, ok := v.([]any)
			if !ok || len(raw) == 0 {
				return fmt.Errorf("%s: supervise.notify is %v: give an argv, such as [\"notify-send\", \"drover\"]", path, v)
			}
			for _, a := range raw {
				if _, ok := a.(string); !ok {
					return fmt.Errorf("%s: supervise.notify holds %v, which is not a string", path, a)
				}
			}
			continue
		}
		s, _ := v.(string)
		p := Policy(s)
		if !slices.Contains(Policies(), p) {
			return fmt.Errorf("%s: supervise.%s is %v (known: off, operator, supervisor, auto)", path, key, v)
		}
		if actions == nil {
			continue
		}
		b, known := actions[key]
		if !known {
			return fmt.Errorf("%s: supervise.%s is not a supervised action; `drover config` lists them with their policies", path, key)
		}
		if p.Above(b.Ceiling) {
			return fmt.Errorf("%s: supervise.%s = %q is above its ceiling %q: that action stays with %s whatever a project sets",
				path, key, p, b.Ceiling, b.Ceiling)
		}
	}
	return nil
}

// sortedKeys orders a table so that an error naming one entry names the same
// entry on the next run.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// layer is what one file wrote, recorded when it was decoded.
type layer struct {
	path string
	// own marks the project's .drover.toml, as opposed to the per-user file.
	own  bool
	keys []toml.Key
}

// declares reports whether the file wrote the dotted key or anything beneath
// it. A file writing ids.namespaces.B = "bug" never lists ids.namespaces among
// its keys, and it has declared that table as surely as one writing the header.
func (l layer) declares(key string) bool {
	prefix := key + "."
	for _, k := range l.keys {
		if s := k.String(); s == key || strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// absorbDeclared records every key one decoded file wrote, table headers
// included.
func (c *Config) absorbDeclared(md toml.MetaData, path string, own bool) {
	c.layers = append(c.layers, layer{path: path, own: own, keys: md.Keys()})
}

// Declared reports whether any layered file wrote the dotted key, or anything
// beneath it, rather than drover having filled it in.
//
// It answers the question SeverityConfigured and RunnerKeyConfigured both ask,
// so neither needs a field of its own. A key the per-user file supplies counts:
// a project repeating it would shadow the global value rather than inherit it.
func (c *Config) Declared(key string) bool {
	return slices.ContainsFunc(c.layers, func(l layer) bool { return l.declares(key) })
}

// ProjectDeclared reports whether the project's own .drover.toml wrote the
// dotted key, as opposed to inheriting it from the per-user file.
//
// Declared is the wrong question for anything that writes a line. A key the
// global supplies and this project does not is one this project inherits, and
// writing it into the project file pins today's global value: the original
// resolves whatever that file says next week, and the copy what it said today.
func (c *Config) ProjectDeclared(key string) bool {
	return slices.ContainsFunc(c.layers, func(l layer) bool { return l.own && l.declares(key) })
}
