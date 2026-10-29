package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Command groups, by kind. Five headings beat one alphabetical list of thirty,
// but a group is not a step and this listing is not the lifecycle: cobra sorts
// within a group, `shelve` and `follow-ups` are no step at all, and `check` is
// listed once under Setup while it is also the gate every review round closes
// on. docs/lifecycle.md is the order; the listing is the map of what exists.
const (
	groupSetup   = "setup"
	groupBacklog = "backlog"
	groupRun     = "run"
	groupFinish  = "finish"
	groupAudit   = "audit"
)

var groups = []*cobra.Group{
	{ID: groupSetup, Title: "Setup:"},
	{ID: groupBacklog, Title: "Backlog:"},
	{ID: groupRun, Title: "Execution:"},
	{ID: groupFinish, Title: "Finishing:"},
	{ID: groupAudit, Title: "Audit:"},
}

// safety is how far a command's effects reach past this process. Every command
// prints its own, because the alternative is a caller inferring it from the
// name, and `intake` reads like a query while it labels a live tracker issue.
type safety string

const (
	readOnly   safety = "read-only"
	localWrite safety = "local writes"
	outward    safety = "outward-facing"
)

// actor is whether an agent may run a command unsupervised. It is a narrower
// question than safety: `init` writes a file and `finish` writes a ledger row, but
// only one of them certifies work that somebody else has to trust.
type actor string

const (
	// operator belongs to the person. Either it certifies work — and a session
	// cannot certify its own — or it reaches something outside the tree.
	operator actor = "operator"
	// either is safe from a coding session as well as a terminal.
	either actor = "either"
)

// classification is the agent-facing contract for one command: where it sits in
// the lifecycle, how far it reaches, what it changes when it does, and whose
// step it is. It lives in one table rather than on each command literal so that
// two commands cannot drift into describing the same reach in different words.
type classification struct {
	group string
	class safety
	// who may run it, and why the boundary is where it is. A command marked
	// operator refuses inside a session drover launched; see actor.go.
	who actor
	// because completes "the operator's step, because ...", so it reads as one
	// sentence in --help and as a cell in the generated instructions. Empty on
	// an `either` command, which needs no justification.
	because string
	// effect names what the command changes, and which flag escalates it when
	// the default invocation changes nothing. Placeholders in braces resolve
	// against the project's own config; see helpVars.
	effect string
	// reach is the short annotation the generated safety table carries, set
	// only where a command's name does not already say what it touches.
	reach string
}

// linkRead is the one forge read every command printing an issue or pull
// request number can make: the repository's address, asked of gh once, only
// when stdout or stderr is a terminal and no URL is already in hand.
const linkRead = " On a terminal, one gh repo view, bounded at 5s, supplies the address a printed #n links to."

// mirrorKept is the exit every mirror write leaves alone: the file is the
// contract and is already written, so a tracker nobody can reach costs the copy
// and never the command. See mirrorWarn.
const mirrorKept = " A mirror write that fails is reported and the exit stays 0."

// idPush is the one write past the tree every allocating command can make.
// sessionMarker is what every launch writes beside the session it starts, for
// a command run in that tree by something drover did not exec.
const sessionMarker = " Before each session it launches, writes .drover/session in the tree that session works in, a JSON object naming its actor, session id, project root, drover's pid and start time, and removes it when the session exits; a marker there whose pid is running refuses the launch with exit 3, and one whose pid is gone is replaced and reported on stderr."

const idPush = " Under [ids] allocator = \"git\" it first reserves the id by pushing refs/drover/ids to {idremote} with --no-verify, so no pre-push hook runs, and refuses with exit 3 when that remote refuses the ref or does not answer within 30s; --local allocates from disk for that one call."

var classified = map[string]classification{
	// Setup
	"init": {group: groupSetup, class: localWrite, who: either,
		effect: "Writes .drover.toml and the directories it names, in the working directory, and in a git work tree appends .drover/ to .gitignore unless a line there already names it or the marker. Refuses if one is already there. --user writes the per-user config under $XDG_CONFIG_HOME/drover, or ~/.config/drover, instead, refuses if one is already there, and touches no project. init supervise appends a [supervise] table to .drover.toml when it has none, and otherwise prints one."},
	"projects": {group: groupSetup, class: readOnly, who: either,
		effect: "Reads the per-user registry beside the global config. Needs no project."},
	"schema": {group: groupSetup, class: readOnly, who: either,
		effect: "Prints to stdout. Needs no project, so it answers in a tree with no config; --frontmatter is the exception and reads the project's own."},
	"agents": {group: groupSetup, class: readOnly, who: either,
		effect: "Prints to stdout. --write <path> replaces only the text between the markers in that file, and --pointer prints the stanza that sends AGENTS.md there."},
	"check": {group: groupSetup, class: readOnly, who: either,
		effect: "Reads only. Where a [runners] entry is the t3 bridge and this host holds [t3] provider_logs, reads [t3] token_file, renews an expired grant at the token endpoint it names without writing the file, and asks [t3] url for its version with one initialize call, bounded at 5s together. The one write is a renewal whose server rotated the refresh token: the grant is written back to [t3] token_file, because the refresh token on disk is then spent. Exits 2 on a finding, which is what a CI job keys on."},
	"config": {group: groupSetup, class: readOnly, who: either,
		effect: "Prints to stdout. Reads .drover.toml without validating it, so it answers for a config Load would refuse. There is no --write: no TOML round-trip preserves the comments that carry a project's reasoning. --stamp rewrites the drover_version line in .drover.toml and no other."},

	// Backlog
	"status": {group: groupBacklog, class: readOnly, who: either,
		effect: "Reads {backlog} and {ledger}, and the forge through gh: one pull request list per invocation, plus the follow-up count in the footer, and on a terminal one gh repo view for the address a printed #n links to. --no-tracker skips all three, and links only a URL already in hand. Under [usage] weekly_budget it also reads the per-user project registry and each registered project's config and ledger, to measure the budget across the account, and it reads them whatever [usage] says for the newest rate_limit reading, which the rate_limit object and the account footer line report. Writes nothing without -o or --render: -o writes the rendered --template or --json to that path through a temp file and a rename, refusing a file already there without --force, and --render writes every [status] render path, replacing what is there. Both are local writes."},
	"show": {group: groupBacklog, class: readOnly, who: either,
		effect: "Reads {backlog}, {ledger}, the run logs, the tracker, and the item's pull request through gh, and on a terminal one gh repo view for the address a printed #n links to. --no-tracker skips the forge, and links only a URL already in hand."},
	"intake": {group: groupBacklog, class: outward, who: operator,
		because: "it relabels a live issue on a tracker other people read",
		effect:  "Writes a body under {backlog} and appends one origin row to {ledger} naming the command, who ran it and the issue, AND, when the issue is a follow-up, removes the follow-up label from it on your tracker through gh." + linkRead + idPush + " --list reads open tracker issues through gh, and {backlog} to see which are already items, writes nothing, and stays open to a launched session.",
		reach:   "relabels a tracker issue"},
	"new": {group: groupBacklog, class: localWrite, who: either,
		effect: "Writes one scaffolded body under {backlog} and appends one origin row to {ledger} naming the command, who ran it and --found-in. Touches no tracker." + idPush},
	"skills": {group: groupBacklog, class: readOnly, who: either,
		effect: "Reads only. --from copies guides into {skills}; --prune deletes from it."},
	"shelve": {group: groupBacklog, class: localWrite, who: either,
		effect: "Renames one file from {backlog} into {shelf}. Nothing else moves, and no row is written. Under [backlog] mirror = \"github\" it relabels the item's issue drover:deferred and closes it as not planned." + mirrorKept},
	"unshelve": {group: groupBacklog, class: localWrite, who: either,
		effect: "Renames one file from {shelf} back into {backlog}. Nothing else moves, and no row is written. Under [backlog] mirror = \"github\" it rewrites the item's issue from the file (the requirements only once accepted), relabels it to the item's state, reopens it unless the item is recorded done, and adds its blocked-by links." + mirrorKept},

	// Execution
	"draft": {group: groupRun, class: outward, who: either,
		effect: "With no runner it assembles the prompt and prints it, changing nothing. --runner, or [run] draft_runner, hands that prompt to a runner session, and the agent it launches writes the item's body at {backlog}/<stem>-<slug>.md. --headless streams that session to <stem>-<slug>-draft.<timestamp>.jsonl in the log directory and, once it exits, appends one usage row to {ledger} when that log holds a result event carrying usage, unless a usage row already names the session. drover writes neither file itself, and nothing here deletes the stop marker." + sessionMarker,
		reach:  "launches a session when given a runner"},
	"run": {group: groupRun, class: outward, who: either,
		effect: "--dry-run reads only. Otherwise switches the git branch and launches a runner session. [runners] in .drover.toml decides what executes, and the launch prints the resolved argv first. Under [backlog] mirror = \"github\" it relabels the item's issue drover:in-progress before the launch and back to the item's state after the session ends." + mirrorKept + " --headless appends one usage row to {ledger} once the session exits, when its log holds a result event carrying usage, unless a usage row already names the session." + sessionMarker,
		reach:  "switches branch, launches a session"},
	"review": {group: groupRun, class: outward, who: either,
		effect: "With no runner it assembles the prompt and prints it, changing nothing. --runner, or [run] review_runner, hands that prompt to a runner session instead, and --headless streams the session to its own log beside the run logs and, once it exits, appends one usage row to {ledger} when that log holds a result event carrying usage, unless a usage row already names the session. It reads the tracker for the issues the item claims to resolve. It never switches the branch: review runs before the merge, on the branch run left the tree on." + sessionMarker,
		reach:  "launches a session when given a runner"},
	"accept": {group: groupRun, class: localWrite, who: operator,
		because: "a run cannot accept its own requirements",
		effect:  "Deletes one line from the item's body under {backlog} and appends one body row to {ledger} carrying the digest of what it wrote. Reads the trunk and the branch of each unmerged item waiting on it through git first, and without --yes refuses a body citing what only such a branch has. Under [backlog] mirror = \"github\" it publishes the item: opens an issue carrying its title, requirements, state label and blocked-by links and writes the number into the item's issue key with a second body row, or rewrites the issue it already names from the file, and relabels and links the issues of its direct dependents. A mirror write that fails is reported and the exit stays 0, except an opened issue whose number will not write back to the body, which fails the accept." + linkRead},
	"unaccept": {group: groupRun, class: localWrite, who: operator,
		because: "returning an accepted contract to a scaffold un-decides what a person decided",
		effect:  "Restores one line at the top of the item's body under {backlog}, and appends one body row to {ledger} carrying the digest of what it wrote. Reads {ledger} to refuse a merged item. Under [backlog] mirror = \"github\" it relabels the item's issue drover:draft, reopens it if closed, and relabels the issues of its direct dependents." + mirrorKept},
	"drive": {group: groupRun, class: outward, who: operator,
		because: "it launches the sessions it drives, unattended and over a queue, and no session may start that",
		effect:  "Checks each item's branch out into a worktree of its own under $XDG_STATE_HOME/drover/worktrees and never switches your checkout, pushes each branch to origin, and launches an implementing and a reviewing session per item in that worktree, headless, appending a start row to {ledger} that takes the drive's id, D1 and up, once its queue is read, an end row naming its exit code and each item's outcome when it exits, a stage row per launch, one row per review pass it read a result from or tree it parked on, a park row naming the cause and its detail for each item it parks, a head row before each push whenever the branch holds a commit the trunk does not, a row naming each pull request it opens or finds, and a usage row per session whose log holds a result event carrying usage, every one of them carrying the drive's id. Before it reads its queue and again after its end row, runs usage --sync: reads every run log in the log directory and appends a usage row for each one no usage row names, then reads Claude Code's transcripts for the project root and every worktree drive made for it, appends an interactive usage row to {ledger} per session per finished local day that no run log names, and rewrites " + usageSyncName + " beside {ledger}; a failure there is reported and never stops the drive. Loops an item while its reviews send work back, bounded by [review] max_review_rounds, and parks it on a review whose rows do not say what it found. Opens one pull request per item whose review came back clean and whose [drive] merge_gate exits 0, and merges it with [git] merge_method, through the forge's own --auto when the trunk has required status checks (read once per drive); a parked item gets no pull request, and a branch touching [drive] protected_paths, a dep a split added and --no-merge each keep the pull request and hold the merge. Before the merge gate reads a branch, merges in whatever origin's trunk gained since the push, parking trunk_conflict on a conflict, and runs the item's own verify command on the merged tree in its worktree before pushing again, never forced. A tree that fails is checked against the trunk alone, in a temporary worktree removed afterwards: when the trunk passes, the merge is pushed and the verify command's output is sent back to the item through request-changes, counting against max_review_rounds; when the trunk fails too, the item parks trunk_red, nothing is pushed and the drive stops. Before the verify command or the merge gate reads a pushed head, waits for its checks and classifies each red one: an infrastructure failure is rerun with gh run rerun --failed, up to [forge] reruns per head, and one the trunk fails too parks trunk_red and stops the drive, each appending a classification row to {ledger}. Once the queue drains, on a drive that is not stopping, reads the checks once more on each head it parked infra_failure, and sends one whose checks all passed and that its branch still points at through the merge gate and the merge, appending a green check row to {ledger} first. Where the trunk has none, they cannot be read, or the forge refuses auto-merge, waits for the head's checks, runs the merge gate again and on exit 0 merges without --auto and finishes the item as finish does, ledger row and tracker closes included. An --auto the forge merged on the spot is finished the same way, after the merge gate reads the merged head, and a merge gate refusing that head stops the drive; --follow then adds the ready items either finish unblocked to the queue. A merge the forge queued is polled every 20s while an item in the queue depends on it, holding that item, and finished in this drive when it lands; past [drive] merge_wait (30m unless set, \"0\" never waits) or the drive's deadline, or once the pull request closes unmerged, the dependent skips, and the merge is finished by the next drive's reconciliation, which is skipped while your checkout holds uncommitted work. An item whose sessions leave uncommitted work parks with its worktree kept for you to inspect, and the queue moves on; a clean worktree is removed when its item is done. After each merge it makes or finishes, and under --update-branches (which launches nothing), merges origin's trunk the same way into every other item's branch whose recorded pull request the forge still reports open, in that item's worktree, runs its verify command there and pushes it, never forced; a conflict is found with git merge-tree, merged and pushed nothing, and its worktree kept for whoever [supervise] resolve_conflict names. A [drive] merge_gate exiting 4 is retried once after one more branch update. Stashes and cleans nothing, and resets only its own branch update merge: one that conflicts is aborted, and one whose merged tree does not pass, or whose verify command is interrupted, is undone with git reset --keep, which refuses rather than discard uncommitted work. --jobs N runs up to N items' implementing and reviewing sessions at once, each in its own worktree, and passes every step that reads or moves the trunk (the branch update, the item's [verify] run, the push after review, the pull request and the merge) through one stage, one item at a time in dispatch order; two items whose bodies cite an overlapping path never run at once, nor does an item beside a dep named earlier in the same drive, and a stop or an interrupt dispatches nothing more and parks each running item stopped once the stage it is in ends. --dry-run resolves every step and launches nothing, and at --jobs above 1 prints the waves it would dispatch and each pair it holds apart. Under [backlog] mirror = \"github\" it relabels each item's issue drover:in-progress around its implementing session, and relabels, closes and links issues as finish does for each item it finishes; a mirror write that fails is reported and never parks or stops the drive. Rewrites its own lease in the checkout's git dir with the ids it holds queued and running, and re-renders every [status] render path after each ledger row it appends and each change to that queue, replacing what is there; a render that fails is reported once and never parks or stops the drive. Emits started once it holds the lease and has read its queue, and an exit carrying a summary of what merged and what it left undone, and sends each event to every [notify] destination that routes it, at that destination's priority for it: an ntfy destination POSTs to its url, reading its token_file, bounded at 10s, and a command destination runs its argv from the project root with the event on stdin, bounded at 30s. Destinations do not wait on each other; [notify] coalesce folds events of one kind into one message, quiet_hours holds those routed below high until the window ends, and a destination that fails is reported once and never parks or stops the drive. Reads every usage row of the drive from {ledger} for the exit's weighted tokens, and resolves the repository through one gh repo view, bounded at 5s, the first time a message links a pull request. Appends every event it emits, one object per line, to $XDG_STATE_HOME/drover/drives/<project>/<drive id>.jsonl, emptied when the drive takes its id; a write that fails is reported once and never parks or stops the drive. --events adds the same objects to the --json stream. --detach starts the same drive in a new session (setsid) with stdin on /dev/null and stdout and stderr in <drive id>.log beside that file, and returns once it holds the lease. --enqueue rewrites the queue in the running drive's lease, under a lock file beside it, to append the ids, and starts a drive with them when none is running. --pause, --resume and --stop write a request into the running drive's lease under the same lock and wait for its answer there; --stop drains the drive as an interrupt does, sending no signal; a resume rereads .drover.toml and the templates and, where an item's verify command changed, runs the new one in that item's worktree and sends a refusal back to it through request-changes. --cleanup reads only; --cleanup --apply removes clean worktrees no process holds with git worktree remove and deletes local branches holding no commit past the trunk, and under [supervise] cleanup_interrupted_drive = \"auto\" every drive does that before reading its queue. Before the first launch, runs the sign-in check of each runner it will launch where drover knows one (claude auth status, codex login status), and refuses on a failure. Kills any process a session started and left running when it exited. --dry-run on Linux with strace on PATH runs each queued item's verify command once in your checkout, under strace, to list what it reads; [supervise] worktree_links_gap = \"off\" skips it. A stale_citations hold at operator appends a park row to {ledger} before any session. Before each dispatch and each stage that launches a session, under usage_admission, reads the per-user project registry and each registered project's config and ledger for the newest rate_limit reading; appends a usage_warning row to {ledger} for each [usage] notify level it crosses that no registered project's ledger already records, and sends it to each [notify] destination routing usage_warning; and on a hold pauses through the lease as --pause does, lifting the hold at the window's reset when that falls inside --for or --until." + sessionMarker + linkRead,
		reach:   "runs up to --jobs items' sessions at once, switches branches, pushes to origin, launches sessions, runs the item's verify command after a branch update, opens pull requests, merges what the merge gate passes, closes issues when it finishes a merge it made"},
	"supervise": {group: groupRun, class: outward, who: operator,
		because: "it starts the session that drives the queue on the operator's behalf, and no launched session may start that",
		effect:  "--dry-run prints the prompt and launches nothing. Otherwise launches the runner interactively in the checkout at the project root, with DROVER_ACTOR=supervisor and a prompt rendered from [supervise] template or the built-in one, and prints the resolved argv first. That session may run drive while [supervise] enqueue is supervisor or auto, finish while merge_drive_pr is supervisor and the ledger shows a pull request drive opened with a passed review, and any other command that is a supervised action only at supervisor or auto; accept, unaccept, signoff, intake, mark-merged, unmark-merged, open-pr and supervise stay refused to it. The sessions a supervised drive launches are agents. supervise itself writes nothing but the session marker." + sessionMarker,
		reach:   "launches a supervising session"},
	"notify": {group: groupRun, class: outward, who: either,
		effect: "With no flag prints its help and sends nothing. --test sends one synthetic event, needs_operator or the kind --event names, through every [notify] destination: POSTs it to each ntfy destination's url, reading its token_file, and runs each command destination's argv with it on stdin. Writes nothing.",
		reach:  "posts to each [notify] ntfy destination, runs each [notify] command destination"},
	"t3": {group: groupRun, class: outward, who: either,
		effect: "login runs an OAuth sign-in against [t3] url, opening a browser unless --no-browser and listening on a loopback port for the redirect, and writes the grant to [t3] token_file at mode 0600; login --check reads that file and, for an expired grant carrying a refresh token, renews it at the server's token endpoint and rewrites it. discover calls initialize and tools/list and writes nothing. run launches one thread on the server, in the T3 Code project [t3] project names and the directory it runs in, follows it with t3_thread_wait and t3_thread_read until its run ends, reads that thread's provider event log under [t3] provider_logs, and prints the log on stdout; it interrupts the run it launched whenever it exits before the run ends, and never answers a request or touches a thread it did not launch. run, thread --new and thread --from-event refuse with exit 3 inside a session drover launched, except that run is that session when drover launched it. thread --new launches one thread and prints its id and link without following it; thread --stop interrupts the named thread's newest run. thread --from-event reads one event line on stdin, as a [notify] command destination sends it, and for needs_operator and needs_supervisor runs drover show <id> --no-tracker and opens a thread under [t3] operator_permission titled for the item and action, or sends to the thread the drive already opened for that item while T3 Code still holds it open; it reads the drive's event file for that thread and prints one thread_opened line, which the drive appends to that file. Any other event kind opens nothing. Every subcommand reads the token file, and none writes the ledger.",
		reach:  "launches and interrupts T3 Code threads, writes [t3] token_file"},
	"attach": {group: groupRun, class: readOnly, who: either,
		effect: "Reads the drive's event file under $XDG_STATE_HOME/drover/drives/<project>, {ledger} and this checkout's lease, and under --session the session logs its stage events name in the log directory, opened for reading only. Never signals the drive or writes anything it reads, so an interrupt leaves the drive running."},
	"watch": {group: groupRun, class: readOnly, who: either,
		effect: "Reads the drive's event file under $XDG_STATE_HOME/drover/drives/<project>, {ledger} and this checkout's lease. Never signals the drive or writes anything it reads."},
	"drives": {group: groupRun, class: readOnly, who: either,
		effect: "Reads {ledger} and this checkout's lease. --all reads every project in the per-user registry instead, each project's config, ledger and lease, and needs no project."},
	"report": {group: groupRun, class: readOnly, who: either,
		effect: "Reads the run log. --save writes under {reports}, and refuses to overwrite a report a human filled in."},
	"request-changes": {group: groupRun, class: localWrite, who: either,
		effect: "Rewrites one section of the item's body under {backlog}, below the review heading, keeps a copy of the evidence under {reports}, and appends one body row and one changes row to {ledger}. Refuses the write when the numbered requirements would change, when every finding traces to an [operator] requirement, and when the item is at [review] max_review_rounds. Reaches no tracker."},
	"close-changes": {group: groupRun, class: localWrite, who: either,
		effect: "Rewrites one section of the item's body under {backlog}, below the review heading, replacing what an earlier review sent back with the statement that it is closed. Refuses when the body carries no such section, when the latest review sent work back itself, and when the numbered requirements would change. Appends one body row to {ledger} carrying the digest of what it wrote and no changes row, keeps no round, and reaches no tracker."},
	"split": {group: groupRun, class: localWrite, who: either,
		effect: "Writes one scaffold under {backlog} and one origin row for it to {ledger}, naming the command, who ran it and the item it splits. Under --after (the default) it rewrites the deps line of the item it splits; under --before the scaffold's own deps line names that item instead and the item is left as it was. The scaffold is inert until a human runs accept, which no launched session can. Under --after it appends one body row to {ledger} carrying the digest of what it wrote to the item it splits, and reads the trunk and that item's branch through git to warn about each citation only the branch has. Moves no code. Under --after and [backlog] mirror = \"github\" it relabels the split item's issue to its new state and relabels the issues of its direct dependents; otherwise it reaches no tracker." + mirrorKept + idPush},
	"invariant": {group: groupRun, class: localWrite, who: either,
		effect: "Appends to one section of the item's body under {backlog}, above the review heading. Refuses the write when the numbered requirements would change. Appends one body row to {ledger} carrying the digest of what it wrote and reaches no tracker."},
	"amend": {group: groupRun, class: localWrite, who: operator,
		because: "it changes an accepted contract, and a session would be moving the target it is measured against",
		effect:  "Replaces the numbered list in one body under {backlog}, keeps the replaced list under {reports}, and appends one body row carrying the digest of what it wrote and one amend row naming the requirement count before and after and the reason to {ledger}. Refuses a scaffold, a merged item and a list prose splits. Reaches no tracker."},
	"touch": {group: groupRun, class: localWrite, who: operator,
		because: "it vouches for a hand edit to an accepted contract, and a session vouching for an edit is the findings-by-hand drive parks",
		effect:  "Appends one body row to {ledger} carrying the current digest of one body under {backlog} and the reason, recording a hand edit as the operator's. Writes nothing else and reaches no tracker."},
	"link": {group: groupRun, class: localWrite, who: either,
		effect: "Rewrites the closes line in one body under {backlog}, and reads the tracker to check each number before it does. Appends one body row to {ledger} carrying the digest of what it wrote. Writes nothing to a tracker." + linkRead},
	"signoff": {group: groupRun, class: localWrite, who: operator,
		because: "it certifies a requirement the session was told not to attempt, and no session certifies its own work",
		effect:  "Appends one signoff row to {ledger}, for one [operator] requirement of the item. Writes nothing else and reaches no tracker."},
	"resolve": {group: groupRun, class: localWrite, who: either,
		effect: "Appends one disposition row to {ledger}, for one issue the item's closes key names. --link widens that key first, reading the tracker the way link does and recording the body digest link records. Writes to no tracker: finish applies what this records." + linkRead},

	// Landing
	"open-pr": {group: groupFinish, class: outward, who: operator,
		because: "an open pull request is the claim that a branch may merge, and no session makes that claim about its own work",
		effect:  "Runs [drive] merge_gate, reads the recorded review pass from {ledger}, and opens one pull request at the forge through gh when both passed. Opens none for a branch that came back or parked, reports the one already open rather than a second, and closes no issue: the body references what the closes key names and finish acts on the dispositions. Appends one row to {ledger} naming the pull request it opened or found, unless the latest one already does." + linkRead,
		reach:   "runs [drive] merge_gate, opens a pull request at the forge"},
	"finish": {group: groupFinish, class: outward, who: operator,
		because: "the operator merges, so the operator records the merge",
		effect:  "Appends to {ledger}, saves a report under {reports}, then fetches, checks out the trunk and fast-forwards it. Asks gh whether the branch merged, runs [git] merge_check when one is configured, and deletes the branch under --delete-branch. Refuses a branch git ancestry calls merged that no head row shows holding a commit of its own, since one that never moved is an ancestor of the trunk. That refusal reads differently from the one about a branch nothing calls merged, because the remedies differ; --force records either on your word. Reads the follow-up list to report the follow-ups this merge left orphaned and the owned issues no closes key named, and never fails on either. Never audits. Under [backlog] mirror = \"github\" it relabels the item's issue drover:merged, closes it, and relabels the issues of its direct dependents; a mirror write that fails is reported and never fails the finish." + linkRead + " --no-tracker skips it and the mirror writes both, printing the accept that brings the issues up to date.",
		reach:   "fetches, pulls the trunk, asks gh, may run [git] merge_check"},
	"mark-merged": {group: groupFinish, class: localWrite, who: operator,
		because: "it is the ledger half of finish, and claims a merge nobody else witnessed",
		effect:  "Appends one completion row to {ledger}. Under [backlog] mirror = \"github\" it relabels the item's issue drover:merged, closes it, and relabels the issues of its direct dependents." + mirrorKept},
	"unmark-merged": {group: groupFinish, class: localWrite, who: operator,
		because: "withdrawing a completion contradicts a row somebody already read",
		effect:  "Appends one tombstone row to {ledger}. Deletes nothing. Under [backlog] mirror = \"github\" it relabels the item's issue to its state, reopens it, and relabels the issues of its direct dependents." + mirrorKept},

	// Level
	"audit": {group: groupAudit, class: localWrite, who: either,
		effect: "Appends one audit row to {ledger}."},
	"unaudited": {group: groupAudit, class: readOnly, who: either,
		effect: "Reads {ledger}, the dependency graph and the tracker."},
	"follow-ups": {group: groupAudit, class: readOnly, who: either,
		effect: "Reads the follow-up list: the tracker through gh, or {register}." + linkRead},
	"usage": {group: groupAudit, class: localWrite, who: either,
		effect: "With no flag prints its help and writes nothing. --backfill reads every run log in the log directory and appends one usage row to {ledger} per log whose result carries usage and whose session no usage row names. --sync does what --backfill does first, then reads Claude Code's transcripts for the project root and for every worktree drive made for it, appends one interactive usage row to {ledger} per session per finished local day that no run log names, and rewrites " + usageSyncName + " beside {ledger}, the offset it read each transcript to and the days it holds until they end. --dry-run reads the same and writes neither."},
	"follow-up": {group: groupAudit, class: localWrite, who: either,
		effect: "Appends one row to {register}. Refuses under any [register] backend but file, where the tracker or nothing at all owns the write."},

	// Ungrouped: cobra lists these under Additional Commands.
	"version": {class: readOnly, who: either,
		effect: "Prints build metadata. Needs no project."},
}

// escalations are the flag-qualified invocations whose reach differs from their
// command's default. They live beside the classification so that --help and the
// safety table `drover agents` generates cannot disagree about what writes.
var escalations = map[safety][]string{
	readOnly:   {"run --dry-run", "intake --list", "usage --dry-run"},
	outward:    {"drive --detach", "drive --stop"},
	localWrite: {"report --save", "agents --write", "config --stamp", "skills --from", "skills --prune", "status -o", "status --render"},
}

// label is how a class is named to a reader deciding whether to call something.
func (s safety) label() string {
	switch s {
	case readOnly:
		return "Read-only, always safe"
	case localWrite:
		return "Local writes only"
	case outward:
		return "Outward-facing, use deliberately"
	}
	return string(s)
}

// rootSafety is the safety paragraph in the root help, wrapped to the width the
// rest of that text uses. It reads SafetyTable so the root listing and each
// command's own line cannot disagree.
func rootSafety() string {
	var b strings.Builder
	for _, row := range SafetyTable() {
		names := make([]string, 0, len(row.Commands))
		for _, c := range row.Commands {
			names = append(names, strings.NewReplacer("`", "", "(", "— ", ")", "").Replace(c))
		}
		label := map[string]string{
			readOnly.label():   "read-only     ",
			localWrite.label(): "local writes  ",
			outward.label():    "outward-facing",
		}[row.Class]
		fmt.Fprintf(&b, "  %s  %s\n", label, wrapList(names, 62, "                  "))
	}
	return b.String()
}

// wrapList joins names onto lines no wider than width, indenting continuations.
func wrapList(names []string, width int, indent string) string {
	var lines []string
	cur := ""
	for _, n := range names {
		switch {
		case cur == "":
			cur = n
		case len(cur)+1+len(n) <= width:
			cur += " " + n
		default:
			lines = append(lines, cur)
			cur = n
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n"+indent)
}

// SafetyRow is one class and the commands in it, rendered for the instructions
// `drover agents` generates. It reads the same table --help does, because two
// hand-kept lists of what writes will disagree and neither will report it.
type SafetyRow struct {
	Class    string
	Commands []string
}

// SafetyTable groups every command by reach, worst last: a reader scanning for
// what is safe finds it first, and what needs deliberation reads as the tail.
func SafetyTable() []SafetyRow {
	rows := make([]SafetyRow, 0, 3)
	for _, class := range []safety{readOnly, localWrite, outward} {
		var names []string
		for name, k := range classified {
			if k.class != class {
				continue
			}
			entry := "`" + name + "`"
			if k.reach != "" {
				entry += " (" + k.reach + ")"
			}
			names = append(names, entry)
		}
		sort.Strings(names)
		for _, flagged := range escalations[class] {
			names = append(names, "`"+flagged+"`")
		}
		rows = append(rows, SafetyRow{Class: class.label(), Commands: names})
	}
	return rows
}

// CommandEffect is one command and what it changes, for the instructions
// `drover agents` generates. The class table beside it groups by reach and
// stops there: it says `split` writes locally and not what it writes. That
// sentence is the one an agent acts on, and a copy held only in --help drifts
// from what the command does, down to naming a ledger row it does not write.
type CommandEffect struct {
	Command string
	// Effect is the same sentence --help renders, with the project's own paths
	// already resolved. It stays plain text: the template escapes it for the
	// table cell it renders into, and --help prints it unescaped.
	Effect string
}

// CommandEffects renders every classified command's effect against vars, sorted
// by name. Sorted because `agents --write` replaces a block in a tracked file:
// map order would rewrite the table on every run and read as a change in CI.
func CommandEffects(vars map[string]string) []CommandEffect {
	rows := make([]CommandEffect, 0, len(classified))
	for name, k := range classified {
		rows = append(rows, CommandEffect{Command: name, Effect: substitute(k.effect, vars)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Command < rows[j].Command })
	return rows
}

// OperatorCommands are the commands that refuse inside a session drover
// launched, each with the reason, for the instructions a project generates.
// Sorted, because a map iteration would reorder the table on every run and
// make `agents --write` look like a change in CI.
func OperatorCommands() []SafetyRow {
	rows := make([]SafetyRow, 0, len(classified))
	for name, k := range classified {
		if k.who != operator {
			continue
		}
		rows = append(rows, SafetyRow{Class: "`" + name + "`", Commands: []string{k.because}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Class < rows[j].Class })
	return rows
}

// PointerStanza is the paragraph a project pastes into AGENTS.md when the
// generated block lives in its own file. AGENTS.md is a plain-markdown
// convention with no import syntax, so the pointer is a link an agent may or
// may not follow — which puts the whole design on one question: what has to be
// true even for the agent that skips it.
//
// The answer is the actor boundary, so the names come off classified rather
// than out of a literal. A stanza that named five commands after a sixth gained
// an Actor line would read as permission to run the one it left out.
func PointerStanza(path string) string {
	ops := OperatorCommands()
	names := make([]string, 0, len(ops))
	for _, row := range ops {
		names = append(names, row.Class)
	}
	body := fmt.Sprintf("This project's work items, completion ledger and audit trail are driven by "+
		"`drover`. **Read [%s](%s) before running any drover command.** %s refuse inside a "+
		"session drover launched and exit 3, because each certifies work somebody else has to "+
		"trust or reaches a tracker other people read. `run`, `draft --runner` and "+
		"`review --runner` refuse there too: a launched session cannot launch another.",
		path, path, joinAnd(names))
	return fmt.Sprintf("## Tooling: drover\n\n%s\n\n`drover agents` prints the same text if %s is missing.\n",
		wrapIndent(body, 79, 0), path)
}

// joinAnd renders a list the way a sentence does, so the stanza reads as prose
// rather than as a table that lost its pipes.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return "No commands"
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// rootOperator is the operator paragraph in the root help, read from the same
// table each command's own Actor line reads.
func rootOperator() string {
	var b strings.Builder
	for _, row := range OperatorCommands() {
		name := strings.ReplaceAll(row.Class, "`", "")
		fmt.Fprintf(&b, "  %-13s ", name)
		b.WriteString(wrapIndent(row.Commands[0], 76, 16))
		b.WriteString("\n")
	}
	return b.String()
}

var installOnce sync.Once

// install applies the agent-facing help surface to the assembled command tree:
// lifecycle groups on the root listing, a safety line on every command, and
// placeholder expansion against the config this tree actually resolved. It runs
// from Execute rather than an init, because package init order would fire it
// before half the commands have registered themselves.
func install() {
	installOnce.Do(func() {
		for _, g := range groups {
			root.AddGroup(g)
		}
		// cobra's completion command documents a shell feature no run uses and
		// pads the listing a caller reads first.
		root.CompletionOptions.HiddenDefaultCmd = true

		for _, c := range root.Commands() {
			if k, ok := classified[c.Name()]; ok {
				c.GroupID = k.group
			}
		}

		// Rendered from the same table every command's own Safety line reads,
		// because a list kept here by hand drifts from it: it calls a command
		// read-only that the table classifies outward, or misses one entirely.
		root.Long = strings.Replace(root.Long, "{safety}", rootSafety(), 1)
		root.Long = strings.Replace(root.Long, "{operator}", rootOperator(), 1)

		base := root.HelpFunc()
		root.SetHelpFunc(func(c *cobra.Command, args []string) {
			expand(c, helpVars())
			out := c.OutOrStdout()
			if !colorTo(out) {
				base(c, args)
				return
			}
			var buf bytes.Buffer
			c.SetOut(&buf)
			base(c, args)
			c.SetOut(out)
			_, _ = io.WriteString(out, styleHelp(buf.String()))
		})
	})
}

var (
	helpHeading = regexp.MustCompile(`^[A-Z][A-Za-z ]*:$`)
	helpLabel   = regexp.MustCompile(`^(Safety|Actor):`)
)

// styleHelp colors cobra's rendered help: each section heading, the label of
// the Safety and Actor lines, and under Examples each drover command apart
// from the comment beside it. It reads the text cobra already laid out, so
// the plain help is the same bytes with or without it.
func styleHelp(text string) string {
	lines := strings.Split(text, "\n")
	examples := false
	for i, line := range lines {
		switch {
		case helpHeading.MatchString(line):
			examples = line == "Examples:"
			lines[i] = sgr(line, pal.Heading, true)
		case helpLabel.MatchString(line):
			n := strings.IndexByte(line, ':') + 1
			lines[i] = sgr(line[:n], pal.Heading, true) + line[n:]
		case examples && strings.HasPrefix(strings.TrimLeft(line, " "), "drover "):
			lead := len(line) - len(strings.TrimLeft(line, " "))
			// Two spaces before the #, so an issue number is not a comment.
			cmd, comment, found := strings.Cut(line[lead:], "  #")
			trimmed := strings.TrimRight(cmd, " ")
			styled := line[:lead] + sgr(trimmed, pal.Command, true) + cmd[len(trimmed):]
			if found {
				styled += "  " + sgr("#"+comment, pal.Muted, true)
			}
			lines[i] = styled
		}
	}
	return strings.Join(lines, "\n")
}

// expand resolves the config placeholders in one command's help and appends its
// safety line. Help renders once per process, so it mutates in place.
func expand(c *cobra.Command, vars map[string]string) {
	c.Long = substitute(c.Long, vars)
	c.Example = substitute(c.Example, vars)
	c.Flags().VisitAll(func(f *pflag.Flag) { f.Usage = substitute(f.Usage, vars) })
	// A subcommand answers to its parent's class: `init supervise` writes
	// exactly where `init` says it does, and the table is keyed on the names the root
	// listing carries, so a nested command has no entry of its own to find.
	name := c.Name()
	if p := c.Parent(); p != nil && p.Parent() != nil {
		name = p.Name()
	}
	if k, ok := classified[name]; ok {
		c.Long = strings.TrimRight(c.Long, "\n") + "\n\n" +
			wrap(fmt.Sprintf("Safety: %s. %s", k.class, substitute(k.effect, vars)), 78)
		if k.who == operator {
			c.Long += "\n\n" + wrap(fmt.Sprintf(
				"Actor: operator. Refuses inside a session drover launched, because %s. "+
					"Report the item's state instead and let the operator run it. "+
					"A person steering from inside a tree a session marker seals exports %s=%s first.",
				k.because, runner.EnvActor, runner.ActorOperator), 78)
		}
	}
}

// wrapIndent folds one paragraph to width, continuing at indent columns and
// assuming the caller already sits at indent. The root listing is a two-column
// block, so a wrapped reason has to clear the name rather than the margin.
func wrapIndent(s string, width, indent int) string {
	var out strings.Builder
	pad := strings.Repeat(" ", indent)
	col := indent
	for i, word := range strings.Fields(s) {
		switch {
		case i == 0:
			col = indent + len(word)
		case col+1+len(word) > width:
			out.WriteString("\n" + pad)
			col = indent + len(word)
		default:
			out.WriteString(" ")
			col += 1 + len(word)
		}
		out.WriteString(word)
	}
	return out.String()
}

// wrap folds one paragraph to width. A resolved path can triple the length of
// a safety line, and an unwrapped one at 150 columns is the line a reader skips.
func wrap(s string, width int) string {
	var out strings.Builder
	col := 0
	for i, word := range strings.Fields(s) {
		switch {
		case i == 0:
			col = len(word)
		case col+1+len(word) > width:
			out.WriteString("\n")
			col = len(word)
		default:
			out.WriteString(" ")
			col += 1 + len(word)
		}
		out.WriteString(word)
	}
	return out.String()
}

func substitute(s string, vars map[string]string) string {
	if s == "" {
		return s
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// helpVars resolves the placeholders in help text against the project the
// process is standing in, for the help cobra renders.
func helpVars() map[string]string {
	wd, err := os.Getwd()
	if err != nil {
		return varsFor(nil)
	}
	cfg, err := config.Load(wd)
	if err != nil {
		return varsFor(nil)
	}
	return varsFor(cfg)
}

// varsFor resolves the placeholders against one project's config. A nil config
// answers with the key each value comes from, so help read in a bare tree still
// names where to set it rather than inventing a path. `agents` passes the
// session's own config rather than reloading from the working directory: drover
// runs from anywhere inside the tree, and instructions generated two levels
// down must not resolve a path against the directory the operator happened to
// stand in.
func varsFor(cfg *config.Config) map[string]string {
	v := map[string]string{
		"backlog":  "[paths] backlog",
		"ledger":   "[paths] ledger",
		"reports":  "[paths] reports",
		"register": "[paths] register",
		"skills":   "[paths] skills",
		"shelf":    "[paths] shelf",
		"runners":  "the built-in claude, until a project declares [runners]",
		"verify":   "[verify] default",
		"idremote": "[ids] remote (origin unless set)",
	}
	if cfg == nil {
		return v
	}
	set := func(key, val string) {
		if val != "" {
			v[key] = val
		}
	}
	set("backlog", cfg.Paths.Backlog)
	set("ledger", cfg.Paths.Ledger)
	set("reports", cfg.Paths.Reports)
	set("register", cfg.Paths.Register)
	set("skills", cfg.Paths.Skills)
	set("shelf", cfg.Paths.Shelf)
	set("verify", cfg.Verify["default"])
	set("runners", runnerList(cfg))
	set("idremote", cfg.IDRemote())
	return v
}

func runnerList(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Runners))
	for name := range cfg.Runners {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 1 {
		for i, name := range names {
			if name == cfg.Run.DefaultRunner {
				names[i] = name + " [default]"
			}
		}
	}
	return strings.Join(names, ", ")
}

// needID is ExactArgs(1) with a message that names the recovery command. Every
// hand-written error in drover says what to do next; cobra's "accepts 1 arg(s),
// received 0" is the one place that contract would break, and it breaks it
// for the reader least able to guess: an unattended run with no usage line,
// since the root silences usage on error.
func needID(use string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return fmt.Errorf("no id given: drover %s\nrun `drover status` for the ids; they are bare (B1), not padded (B01)", use)
		case len(args) > 1:
			return fmt.Errorf("one id at a time: drover %s (got %d arguments: %s)", use, len(args), strings.Join(args, " "))
		}
		return nil
	}
}

// needIDs takes none or many, for a command that walks a queue. No ids is the
// queue itself rather than a missing argument, so the only thing left to reject
// is the padded form a filename listing invites.
func needIDs(use string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		for _, a := range args {
			if a == "" {
				return fmt.Errorf("empty id: drover %s\nrun `drover status` for the ids; they are bare (B1), not padded (B01)", use)
			}
		}
		return nil
	}
}

// needSomeIDs is needIDs for a command with no queue to fall back on: none is
// a missing argument, any number past that is a list.
func needSomeIDs(use string) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("no id given: drover %s\nrun `drover status` for the ids; they are bare (B1), not padded (B01)", use)
		}
		return needIDs(use)(c, args)
	}
}

// needIssue is ExactArgs(1) for a tracker issue number, which is a different
// namespace from a drover id and gets confused with one.
func needIssue(use, hint string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return fmt.Errorf("no number given: drover %s\n%s", use, hint)
		case len(args) > 1:
			return fmt.Errorf("one at a time: drover %s (got %d arguments: %s)", use, len(args), strings.Join(args, " "))
		}
		return nil
	}
}

// optionalID is MaximumNArgs(1) with the same recovery message.
func optionalID(use string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("one id at a time: drover %s (got %d arguments: %s)", use, len(args), strings.Join(args, " "))
		}
		return nil
	}
}
