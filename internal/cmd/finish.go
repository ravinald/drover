package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/register"
	"github.com/ravinald/drover/internal/reports"
	runnerpkg "github.com/ravinald/drover/internal/runner"
	"github.com/ravinald/drover/internal/sev"
	"github.com/spf13/cobra"
)

var (
	finishRunner    string
	finishForce     bool
	finishDelete    bool
	finishNoTracker bool
	// finishInDrive is set while drive reconciles, the one caller that runs finish
	// inside another command rather than from a terminal.
	finishInDrive bool
)

var finishCmd = &cobra.Command{
	Use:   "finish [id]",
	Short: "Record a merged item: ledger row, report, back to the trunk.",
	Long: `Run this after merging. It does the mechanical half of finishing an item, which
is otherwise four commands nobody remembers in the same order twice.

It deliberately does NOT audit. Finishing is mechanical; auditing is judgement, and
a tool that did both would let the thing that merged the work also certify it.

Omit the id to finish the item whose branch you are standing on.

The merge check asks the forge first, then git ancestry: [git] merge_check when
the project declares one, else gh, and ancestry whenever the forge says no. A
squash or rebase merge is invisible to ancestry, so its no proves nothing — but
its yes is trustworthy, which is what catches a branch merged by direct push to
the trunk. The message names which authority answered.

[git] merge_check is the authority for a forge that is not GitHub. drover runs
it from the project root with the branch and the trunk appended as the last two
arguments; exit 0 means merged, any non-zero exit means not merged rather than
an error, and ancestry still gets its say.

It refuses while run, draft or review holds this checkout, and exits 3 before
it fetches or switches anything: returning to the trunk is a branch switch, and
each of those has a session working on a branch in this tree. A drive holding
the checkout is no refusal, since its items run in worktrees of their own; finish
still refuses, exit 3, while a live session holds the item's own worktree,
because finishing removes it. Either refusal names the holder's pid. There is no
--force for it and no --wait. Bringing the trunk up to date waits, up to 30
seconds, for a drive doing the same.

Finishing reads the follow-up list, because this is the moment a follow-up owned
by the item is orphaned: its every owner has now merged, so no run is scheduled
to carry it. finish prints one line naming how many, the worst severity among
them, and what discharges them, which is another item declaring this id on its
deps: line. It is a line and not a refusal: carrying a follow-up past a merge is
legitimate. An unreachable follow-up list drops the line rather than reporting
zero, and nothing finish learns about it can stop it recording the merge.`,
	Example: `  drover finish                     # the item whose branch you are standing on
  drover finish B1                  # by id
  drover finish B1 --delete-branch  # and drop the local branch
  drover finish B1 --force          # merged under a name the template does not render

  The ledger row names the runner the recorded run used; --runner overrides it.
  A --runner [runners] does not declare warns and records anyway: here the value
  is attribution, not the executable run, draft and review refuse an unknown
  name for.

  finish refuses on a branch it cannot see merged and exits 3. If you are not on
  the tree anyway, use: drover mark-merged B1, then
    drover audit B1 <level> --code-by <who> [--reviewer <name>]

  A project with [git] enabled = false has no merge to verify, so finish refuses
  there by design and names those same two commands.

  The orphaned line, when the follow-up list holds rows this finish stranded:
    · 7 follow-ups owned by 11 are now orphaned (worst H): nothing carries them
      until an item declares deps: 11 (drover follow-ups 11)`,
	Args: optionalID("finish [id]"),
	RunE: func(cmd *cobra.Command, args []string) error {
		runner := finishRunner
		if finishForce && supervising() {
			return Refusal{fmt.Errorf(
				"finish --force is the operator's whatever [supervise] merge_drive_pr says: it records a merge drover cannot see.\n" +
					"Report the item's state and let the operator run it")}
		}
		s, err := open()
		if err != nil {
			return err
		}
		s.offline = finishNoTracker
		// finish is the merge check and nothing else is: every other step it takes
		// is a file write that `mark-merged` and `audit` also do. Refusing here names the
		// two commands that finish an item in a project with no branches, rather
		// than letting git answer with an exit status.
		if !s.cfg.Git.On() {
			return Refusal{fmt.Errorf(
				"[git] enabled is false — there is no branch to see merged.\n" +
					"Record completion with: drover mark-merged <id>, then\n" +
					"  drover audit <id> <level> --code-by <who> [--reviewer <name>]")}
		}
		// Before the merge check, before the fetch, and before the return to
		// the trunk. A run, draft or review has a session on a branch in this
		// checkout, and a finish typed in a second terminal switches that tree to
		// the trunk underneath it. finish takes no lease of its own, because it
		// runs in seconds, so these reads are its whole half of the rule: the
		// checkout's, and that of the item worktree removeItemTree deletes.
		if err := s.leaseFree(); err != nil {
			return err
		}
		it, err := s.itemArg(args)
		if err != nil {
			return err
		}
		id := it.ID.Bare()
		if err := s.itemNotDriving("finish", id); err != nil {
			return err
		}
		if err := s.itemTreeFree(it); err != nil {
			return err
		}

		branch, err := s.branchFor(it)
		if err != nil {
			return err
		}
		if supervising() {
			open, err := s.supervisorEligible(id, branch)
			if err != nil {
				return err
			}
			if err := s.supervisorMayFinishDenied(id); err != nil {
				return err
			}
			// The merge is finish's under a supervisor, so the comparison above
			// refuses before anything merges, and --match-head-commit binds the
			// merge to the head it compared. Drive's reconciliation records what
			// the forge merged and never issues one.
			if open != "" && !finishInDrive {
				ctx := cmd.Context()
				if ctx == nil {
					ctx = context.Background()
				}
				if err := s.mergeBranchAt(ctx, branch, open, false); err != nil {
					return fmt.Errorf("merge the pull request for %s at %s: %w", branch, shortSHA(open), err)
				}
				fmt.Printf("  ✓ merged the pull request for %s at %s\n", branch, shortSHA(open))
			}
		}
		trunk := s.trunk()

		// Refusing on an unmerged branch is the whole check: recording a
		// completion for work still sitting on a branch makes the ledger claim
		// something the tree does not support.
		answer, how, err := s.mergedInto(branch, trunk)
		if err != nil {
			return err
		}
		if answer != mergeYes && !finishForce {
			return Refusal{refuseFinish(branch, id, how, answer)}
		}
		switch answer {
		case mergeYes:
			fmt.Printf("  ✓ %s is merged (%s)\n", branch, how)
		case mergeUnvouched:
			// Louder than the other --force, because this is the one that records
			// a completion for work that may not exist. The operator reading
			// scrollback at nine in the morning sees which of the two they waved
			// through.
			fmt.Printf("  ! %s: %s\n", branch, how)
			fmt.Printf("      recording anyway (--force). Nothing here can show this branch ever held a commit\n")
			fmt.Printf("      of its own, so the completion rests on your reading of the tree rather than drover's\n")
		default:
			fmt.Printf("  ! %s does not look merged (%s) — recording anyway (--force)\n", branch, how)
		}

		// The merge has happened, so for the operator this reports and records
		// rather than refuses: check carries the finding until a review names the
		// merged head. A supervisor is refused, because merge_drive_pr covers a
		// pull request drive's review read, and this one holds more than that.
		merged := s.mergedHead(branch)
		reviewed, unreviewed, uerr := s.unreviewedSince(id, merged)
		switch {
		case uerr != nil:
			fmt.Printf("  · could not list the commits after the last passing review (%s): %v\n", shortSHA(reviewed), uerr)
		case len(unreviewed) > 0 && supervising():
			return Refusal{fmt.Errorf(
				"%s merged at %s holding %d %s after the last passing review read %s, which no review read:\n%s\n"+
					"merge_drive_pr covers what drive's review read. Report it and let the operator finish %s and run drover review %s",
				id, shortSHA(merged), len(unreviewed), plural(len(unreviewed), "commit", "commits"), shortSHA(reviewed),
				commitList(unreviewed, "  "), id, id)}
		case len(unreviewed) > 0:
			fmt.Printf("  ! %d %s merged after the last passing review read %s, and no review read %s:\n%s\n",
				len(unreviewed), plural(len(unreviewed), "commit", "commits"), shortSHA(reviewed),
				plural(len(unreviewed), "it", "them"), commitList(unreviewed, "      "))
			fmt.Printf("      drover check reports UNREVIEWED MERGE until drover review %s reads %s and an audit names it:\n"+
				"      drover audit %s reviewed --head %s --code-by <who> [--reviewer <name>] \"<what you read>\"\n",
				id, shortSHA(merged), id, merged)
		}

		// A prior `drover mark-merged` already wrote the row. That is one of finish's five
		// steps, not a reason to skip the other four, so this is idempotent rather
		// than fatal — otherwise recording the merge by hand strands the branch.
		wrote := false
		if !s.ledger.State(id).Merged {
			warnUndeclaredRunner(errOut, runner, s.cfg.Runners)
			yes := true
			agent := s.attribution(runner, id)
			row := ledger.Row{ID: id, Merged: &yes, MergedRunner: agent, MergedHead: merged,
				ReviewedHead: reviewed, Unreviewed: unreviewed, Date: ledger.Today()}
			if wrote, err = s.recordDone(row); err != nil {
				return err
			}
			if wrote {
				fmt.Printf("  ✓ ledger: merged, implementer %s\n", agent)
			}
		}
		if !wrote {
			fmt.Printf("  · ledger: already done, left alone\n")
			evidence := ledger.Row{ID: id, MergedHead: merged, ReviewedHead: reviewed, Unreviewed: unreviewed, Date: ledger.Today()}
			switch added, err := s.recordMergeEvidence(evidence); {
			case err != nil:
				return err
			case added:
				fmt.Printf("  ✓ ledger: merged at %s, compared with the last passing review\n", shortSHA(merged))
			}
		}

		// Dispositions first: closing an issue changes what the register holds,
		// and finishRegister counting before the writes would report a row this
		// same finish had just closed. The order alone is not enough: the read
		// right after a close can still return the issue open, so finishRegister
		// is also told what was closed and drops it rather than trusting the read.
		var closed []int
		if finishNoTracker {
			s.reportDispositions(it)
		} else {
			closed = s.applyDispositions(it, how)
		}
		// Drive's reconciliation skips the tracker for the dispositions alone:
		// those answer somebody's report, while a mirrored issue was filed by
		// drover as its own index entry and lags the ledger until written.
		if finishNoTracker && !finishInDrive {
			s.reportMirror(it)
		} else {
			s.mirrorAround(it)
		}
		// Dormancy is decided against the ledger, so this reads after the row
		// above and nowhere earlier: asked one step sooner, every row routed
		// here still has an owner nobody has merged.
		s.finishRegister(it, closed)

		// A missing run log is normal for an interactive run; it costs the
		// snapshot, not the finish.
		switch vd, _, err := s.reportData(it); {
		case err != nil:
			fmt.Printf("  · no report captured: %v\n", err)
		default:
			path := s.reportPath(it)
			written, werr := reports.Write(path, vd)
			switch {
			case werr != nil:
				fmt.Printf("  · report not saved: %v\n", werr)
			case written:
				fmt.Printf("  ✓ report: %s\n", rel(s.cfg.Root, path))
			default:
				fmt.Printf("  · report already saved: %s\n", rel(s.cfg.Root, path))
			}
		}

		// Before the branch delete below, which git refuses for a branch some
		// worktree still has checked out. A tree holding uncommitted work stays,
		// and so does its branch.
		if err := s.removeItemTree(it); err != nil {
			fmt.Printf("  · worktree kept: %v\n", err)
		}
		// A drive reconciling what merged leaves the operator's checkout where it
		// is: its items never ran there, and the next one brings the trunk up to
		// date before branching.
		if !finishInDrive {
			if err := s.backToTrunk(trunk); err != nil {
				return err
			}
		}

		if finishDelete {
			if err := gitAt(s.cfg.Root, "branch", "-d", branch); err != nil {
				fmt.Printf("  · branch kept: %v\n", err)
			} else {
				fmt.Printf("  ✓ deleted %s\n", branch)
			}
		}

		s.postFinish(it, trunk)

		if st := s.ledger.State(id); !st.Audited() {
			fmt.Printf("\nStill unrecorded — how hard anyone looked:\n")
			// The unaudited case is the one a human reviewer reads, so the prompt
			// carries --reviewer: an agent reviewer already left the row `review`
			// wrote when it launched, and without a name a human who read the diff
			// records the same absence as nobody having looked.
			fmt.Printf("  drover audit %s <claimed|reviewed|verified> --code-by <who> [--reviewer <name>] \"<note>\"\n", id)
		}
		return nil
	},
}

// warnAmendedDispositions names each partial disposition an amend has since
// outdated. It warns and never refuses: the merge already happened, and the
// answer may still be right under the new requirements, but a partial leaves
// the issue open, and one judged against a requirement that no longer stands is
// the likeliest to be wrong in that direction.
func (s *session) warnAmendedDispositions(it backlog.Item) {
	id := it.ID.Bare()
	for _, number := range backlog.Ints(it.Get(closesKey)) {
		res, recorded := s.ledger.Resolution(id, number)
		if !recorded || res.Applied != "" || res.Amended == "" || res.Disposition != ledger.DispositionPartial {
			continue
		}
		fmt.Printf("  ! %s: partial was recorded before the amend on %s, against requirements that have since changed\n",
			s.registerRefTo(os.Stdout, number), res.Amended)
		fmt.Printf("      drover resolve %s %d <%s> --code-by <who> \"<what shipped>\"\n",
			id, number, strings.Join(ledger.Dispositions(), "|"))
	}
}

// reportDispositions names what applyDispositions would have written without
// writing it. Closing an issue is visible to whoever filed it, so it belongs to
// a command an operator typed for that purpose rather than to a step some
// longer-running command takes on the way past.
func (s *session) reportDispositions(it backlog.Item) {
	claimed := backlog.Ints(it.Get(closesKey))
	if len(claimed) == 0 {
		return
	}
	s.warnAmendedDispositions(it)
	var pending []int
	for _, number := range claimed {
		if res, recorded := s.ledger.Resolution(it.ID.Bare(), number); recorded && res.Applied == "" {
			pending = append(pending, number)
		}
	}
	if len(pending) == 0 {
		return
	}
	fmt.Printf("  · --no-tracker: %s left open\n", s.registerRefsTo(os.Stdout, pending))
	fmt.Printf("      drover finish %s\n", it.ID.Bare())
}

// applyDispositions writes to the tracker what the reviewer recorded, once the
// merge is real.
//
// This is the one place drover closes anything, and it is here rather than in
// review for the reason finish exists at all: an issue closed before the branch
// merges is closed on a promise. The judgement is somebody else's and already
// made — finish executes it and never forms one, which is the same line it draws
// between recording a completion and auditing it.
//
// Every failure past this point is reported and swallowed. The merge happened
// and the ledger says so; a tracker nobody could reach must not turn that into
// a finish the operator has to run again.
//
// It returns the issues it closed, which the register read that follows cannot
// be trusted to have stopped returning.
func (s *session) applyDispositions(it backlog.Item, how string) (closed []int) {
	id := it.ID.Bare()
	claimed := backlog.Ints(it.Get(closesKey))
	if len(claimed) == 0 {
		return nil
	}
	s.warnAmendedDispositions(it)
	if s.cfg.RegisterBackend() != config.BackendGitHub {
		fmt.Printf("  · %s names %s, but %s — nothing to write to\n",
			id, renderInts(claimed), s.cfg.RegisterDeclaration())
		return nil
	}

	path := s.cfg.Abs(s.cfg.Paths.Ledger)
	for _, number := range claimed {
		// Decided from the ledger under its lock rather than from s.ledger: a
		// drive finishing the same merge in another process can have applied the
		// issue, or be mid-call applying it, since this one read the file. The
		// link is taken in the same locked step, so of two processes reading
		// together only one goes on to the tracker.
		now := time.Now()
		var res ledger.Resolution
		var recorded bool
		claim := ledger.Stamp(driveStamp(ledger.Row{ID: id, Issue: number,
			Locking: now.UTC().Format(time.RFC3339), Date: ledger.Today()}))
		took, err := ledger.AppendUnless(path, claim, func(l *ledger.Ledger) bool {
			res, recorded = l.Resolution(id, number)
			return res.Applied != "" || !recorded || !res.Lockable(now)
		})
		if err != nil {
			fmt.Printf("  · %s not written, because it could not be claimed: %v\n", s.registerRefTo(os.Stdout, number), err)
			continue
		}
		switch {
		case res.Applied != "":
			fmt.Printf("  · %s: already %s\n", s.registerRefTo(os.Stdout, number), res.Applied)
			continue
		case !recorded:
			// Named and not acted on. An issue nobody judged is the state this
			// whole path exists to end, and guessing a disposition here would
			// recreate it one level down.
			fmt.Printf("  ! %s: no disposition recorded — left open\n", s.registerRefTo(os.Stdout, number))
			fmt.Printf("      drover resolve %s %d <%s> --code-by <who> \"<what shipped>\"\n",
				id, number, strings.Join(ledger.Dispositions(), "|"))
			continue
		case !took:
			fmt.Printf("  · %s: already being applied\n", s.registerRefTo(os.Stdout, number))
			continue
		}
		s.ledger.Rows = append(s.ledger.Rows, claim)
		appended()

		if res.Closes() && res.CodeBy != "" && !res.Independent {
			fmt.Printf("  ! %s: closing on a disposition the author of the code recorded (--code-by %s) for %s.\n", s.registerRefTo(os.Stdout, number), res.CodeBy, id)
			fmt.Printf("      Whoever wrote the code is certifying that it resolves the report. Recording it as stated.\n")
		}

		body := dispositionComment(id, it.Title(), how, res)
		applied := ledger.AppliedCommented
		if res.Closes() {
			applied = ledger.AppliedClosed
			err = gh.Close(s.labels(), number, body)
		} else {
			err = gh.Comment(s.labels(), number, body)
		}
		if err != nil {
			fmt.Printf("  · %s not %s: %v\n", s.registerRefTo(os.Stdout, number), applied, err)
			if err := s.record(ledger.Row{ID: id, Issue: number,
				Locking: ledger.LockReleased, Date: ledger.Today()}); err != nil {
				fmt.Printf("      and unlocking failed, so it holds until %s: %v\n",
					now.Add(ledger.LockLease).Format("15:04"), err)
			}
			continue
		}
		// Recorded only after the tracker took it. A row written first would
		// tell the next finish that a comment nobody posted is already there.
		if err := s.record(ledger.Row{ID: id, Issue: number,
			Applied: applied, Date: ledger.Today()}); err != nil {
			fmt.Printf("  · %s %s, but the ledger row failed: %v\n", s.registerRefTo(os.Stdout, number), applied, err)
			continue
		}
		if res.Closes() {
			closed = append(closed, number)
			fmt.Printf("  ✓ %s closed\n", s.registerRefTo(os.Stdout, number))
		} else {
			fmt.Printf("  ✓ %s commented (%s), left open\n", s.registerRefTo(os.Stdout, number), res.Disposition)
		}
	}
	return closed
}

// dispositionComment is what the tracker is told. It names the item, how the
// merge was established and what the reviewer concluded, because the audience
// is whoever filed the issue and has never heard of the backlog that closed it.
func dispositionComment(id, title, how string, res ledger.Resolution) string {
	var b strings.Builder
	switch res.Disposition {
	case ledger.DispositionSatisfied:
		fmt.Fprintf(&b, "Resolved by %s: %s.", id, title)
	case ledger.DispositionPartial:
		fmt.Fprintf(&b, "Partly addressed by %s: %s. Leaving this open for the rest.", id, title)
	default:
		fmt.Fprintf(&b, "%s: %s merged without addressing this. Leaving it open.", id, title)
	}
	if how != "" {
		fmt.Fprintf(&b, "\n\nMerge: %s.", how)
	}
	if res.Note != "" {
		fmt.Fprintf(&b, "\n\n%s", res.Note)
	}
	switch {
	case res.CodeBy == "":
	case res.Independent:
		b.WriteString("\n\n_Reviewed independently._")
	default:
		b.WriteString("\n\n_Reviewed by the author of the code._")
	}
	return b.String()
}

// finishRegister reports what the register says about the merge just recorded:
// the routed issues this finish walked past, and the rows it stranded. One
// read answers both, and it happens after applyDispositions. closed is what that
// step closed, dropped here as well: the tracker can still list an issue open on
// the read straight after the close, which can report a follow-up dormant a moment
// after this same finish closed it.
//
// finish is the one command holding both halves of the transition: the merge that
// makes a routed row dormant, and the count of what was routed. The next
// surface that would say it is `drover follow-ups`, which nobody runs at the moment
// an item has just cleared its merge gate.
//
// It is a line rather than a refusal. Carrying a follow-up past a merge is legitimate
// and common, so nothing learned here may stop finish recording the merge that
// already happened: an unreachable register drops the line, and every other
// step runs unchanged.
//
// What it reports is what check reports, off the same index: a read that hit
// the page limit is a floor, and it says so whether the count is zero or not.
func (s *session) finishRegister(it backlog.Item, closed []int) {
	id := it.ID.Bare()
	index := s.register()
	// A project that declared no register is not told about one.
	if index.Off {
		return
	}
	if !index.OK {
		fmt.Printf("  · follow-up list unread, so orphaned follow-ups went uncounted: %v\n", index.Reason())
		return
	}

	scoped := index
	scoped.Issues = slices.DeleteFunc(index.For(id), func(is gh.Issue) bool {
		return slices.Contains(closed, is.Number)
	})
	s.finishUnlinked(it, scoped.Issues)
	n := s.registerCounts(scoped).Orphaned
	// A page that stopped early makes the count a floor, and a floor of zero is
	// the one zero that may not print nothing: silence there reads as "this
	// merge stranded nothing" when nobody counted past the limit.
	truncated := index.Truncated(s.cfg.GitHub.PageLimit)
	if n == 0 && !truncated {
		return
	}
	if n > 0 {
		fmt.Printf("  · %d %s owned by %s %s now orphaned (%s): nothing carries %s until an item declares deps: %s (drover follow-ups %s)\n",
			n, plural(n, "follow-up", "follow-ups"), id, plural(n, "is", "are"), worstOrphaned(scoped.Issues, s.orphaned),
			plural(n, "it", "them"), id, id)
	}
	if !truncated {
		return
	}
	// check labels the same condition off the same index, so the two agree
	// word for word rather than describing one page limit two ways.
	const floor = "the follow-up list hit the %d-issue page limit, so the orphaned count is a floor"
	if n == 0 {
		fmt.Printf("  · no follow-up owned by %s counted as orphaned, and "+floor+"\n", id, s.cfg.GitHub.PageLimit)
		return
	}
	fmt.Printf("  · "+floor+"\n", s.cfg.GitHub.PageLimit)
}

// worstOrphaned names the highest severity among the orphaned follow-ups, in the
// letters the rest of drover ranks with. Seven L follow-ups and one H are
// different situations and a count alone renders them identically.
//
// It ranks the orphaned subset rather than everything owned here: a live H
// beside four orphaned L follow-ups would otherwise report an H nothing has
// stranded.
func worstOrphaned(rows []gh.Issue, orphaned func([]string) bool) string {
	worst := sev.Unranked
	for _, is := range rows {
		if !orphaned(is.Owners) {
			continue
		}
		if r := sev.Of(is.Severity); r > worst {
			worst = r
		}
	}
	if l := worst.Letter(); l != "" {
		return "worst " + l
	}
	return "none ranked"
}

// backToTrunk returns the tree to the trunk and brings it level, reporting what
// it could not do rather than failing on it.
//
// Nothing here deletes a branch. A drive that parked an item leaves the branch
// and the work on it as the evidence somebody reads in the morning, and finish
// deletes only under its own flag.
//
// The pull is under the trunk lock and the switch is not: a drive's syncTrunk
// reads which branch is out to choose how to move the ref, and a switch landing
// between that read and the move costs it one reported failure, while two
// moves racing each other is what the lock is for.
func (s *session) backToTrunk(trunk string) error {
	if err := gitAt(s.cfg.Root, "switch", trunk); err != nil {
		return fmt.Errorf("switch to %s: %w", trunk, err)
	}
	release, err := s.trunkLock("finish")
	if err != nil {
		fmt.Printf("  · on %s, but not brought up to date: %v\n", trunk, err)
		return nil
	}
	defer release()
	if err := gitAt(s.cfg.Root, "pull", "--ff-only"); err != nil {
		fmt.Printf("  · on %s, but pull failed: %v\n", trunk, err)
		return nil
	}
	fmt.Printf("  ✓ on %s, up to date\n", trunk)
	return nil
}

// trunk is the branch work merges into: the configured one, else whatever
// origin says its HEAD is, else main.
func (s *session) trunk() string {
	if b := s.cfg.Run.Trunk; b != "" {
		return b
	}
	out, err := gitcmd.Command(s.cfg.Root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return strings.TrimPrefix(name, "origin/")
		}
	}
	return "main"
}

// mergedInto answers whether a branch merged, and says which authority
// answered so a wrong result is diagnosable.
//
// Two sources in order: the forge — [git] merge_check when the project declares
// one, else gh — then git ancestry. Ancestry is consulted on a definitive no as
// well as on an outage, because a branch merged by direct push to the trunk
// leaves no pull request behind and the forge answers no with full confidence.
// Ancestry produces false negatives under squash and rebase merges, and one
// false positive: a branch that never held a commit of its own sits at or behind
// the trunk, which `git branch --merged` lists as merged. Trusted, that records
// an item finished with none of its code on the trunk, and the operator finds
// out by reading the trunk's log. So an ancestry yes stands only for a branch
// the ledger saw carrying work (see carriedWork).
// mergeAnswer is why mergedInto answered as it did. Two different noes reach
// finish, and one --force covering both would make the dangerous one quiet: a
// branch name the template no longer renders is benign, and a branch git ancestry
// calls merged that nothing shows carrying a commit would record the item
// finished with none of its code on the trunk.
type mergeAnswer int

const (
	mergeYes       mergeAnswer = iota // the forge or vouched-for ancestry says so
	mergeNo                           // nothing says this branch landed
	mergeUnvouched                    // ancestry says merged and no row shows it carrying work
)

func (s *session) mergedInto(branch, trunk string) (mergeAnswer, string, error) {
	merged, how, err := s.forgeSaysMerged(branch, trunk)
	if err != nil {
		return mergeNo, "", err
	}
	if merged {
		return mergeYes, how, nil
	}
	// Fetch first, so a stale trunk does not answer for the remote.
	_ = gitAt(s.cfg.Root, "fetch", "--quiet")
	ok, err := branchMerged(s.cfg.Root, branch, trunk)
	if err != nil {
		return mergeNo, "", err
	}
	if ok {
		if !s.carriedWork(branch) {
			return mergeUnvouched, how + "; git ancestry says merged, but no row shows the branch holding commits of its own", nil
		}
		return mergeYes, how + "; git ancestry says merged", nil
	}
	return mergeNo, how + "; git ancestry says no, which a squash merge would also say", nil
}

// carriedWork reports whether the ledger recorded the branch at a commit the
// trunk did not contain, and the branch still holds that commit.
//
// The row is the only source that can say it after a merge: once merged, every
// commit the branch ever held is the trunk's too, and the reflog records a
// fast-forward onto the trunk as readily as a commit made on the branch. drover
// writes the row before each push, after checking the head is off the trunk.
// Requiring the branch to still hold the recorded head is what stops an old row
// vouching for a branch since reset back to the trunk.
//
// It fails toward "no": work committed outside any drover launch, or a branch
// rebased after its last row, reads as never started, which costs the operator
// `finish --force` rather than a completion for work that does not exist.
func (s *session) carriedWork(branch string) bool {
	if s.ledger == nil {
		return false
	}
	for _, head := range s.ledger.Heads(branch) {
		if gitAt(s.cfg.Root, "merge-base", "--is-ancestor", head, branch) == nil {
			return true
		}
	}
	return false
}

// recordHead writes the row carriedWork reads, when the branch holds a commit
// the trunk does not. A branch at or behind the trunk writes nothing, which is
// the whole of what the row distinguishes.
func (s *session) recordHead(id, branch string) error {
	trunk := s.trunk()
	if trunk == "" {
		return nil
	}
	out, err := gitcmd.Command(s.cfg.Root, "rev-parse", "--verify", "--quiet", branch+"^{commit}").Output()
	if err != nil {
		return fmt.Errorf("could not resolve %s: %w", branch, err)
	}
	head := strings.TrimSpace(string(out))
	err = gitcmd.Command(s.cfg.Root, "merge-base", "--is-ancestor", head, trunk).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return s.record(ledger.Row{ID: id, Branch: branch, Head: head, Date: ledger.Today()})
	default:
		return fmt.Errorf("could not ask whether %s is on %s: %w", branch, trunk, err)
	}
}

// reviewedHead is the head the item's latest passing review read, and ok false
// when no review has passed. It holds to one rule: a head becomes the baseline
// only on a verdict open-pr would accept as passing, so nothing finish or check
// reads can call a review passed that open-pr refused.
//
// Drive's verdicts are its pass rows: passed, or standing with a close-changes
// row after it before anything else ran or was audited, which open-pr reads as
// passed. A manual review is a review stage row and what follows it up to the
// next stage or pass row, and passes with no request for changes inside it, an
// independent audit at reviewed or verified naming a head, and no sent-back
// block standing. For the latest review that last condition is the item file as
// it stands, judged by bodyAccepts the way open-pr's manual path judges it.
// For an earlier one the file then is gone, so the ledger answers: a request
// for changes or a refusal stands until a close-changes row or a passed pass
// row. A block removed by hand edit reads as standing there, which compares
// from an older head and lists more rather than less.
//
// A passed pass row without a head was written before pass rows carried one and
// covers no particular head, so it answers ok with no head and a caller compares
// nothing. A passing manual window whose audit names no head is skipped instead:
// it says nothing about what the review read, and comparing from the earlier
// head over-reports where answering nothing would go silent.
func reviewedHead(l *ledger.Ledger, id string, bodyAccepts func() bool) (head string, ok bool) {
	lastReview := -1
	for i, r := range l.Rows {
		if r.ID == id && r.Stage == ledger.StageReview {
			lastReview = i
		}
	}
	open, sentBack, standing := false, false, false
	at := -1
	var audit ledger.Row
	pending, pendingOK := "", false
	closeWindow := func() {
		if open && !sentBack && audit.Head != "" && audit.Independent &&
			(audit.Level == ledger.LevelReviewed || audit.Level == ledger.LevelVerified) {
			accepted := !standing
			if at == lastReview {
				accepted = bodyAccepts != nil && bodyAccepts()
			}
			if accepted {
				head, ok = audit.Head, true
			}
		}
		open, sentBack, audit = false, false, ledger.Row{}
	}
	for i, r := range l.Rows {
		if r.ID != id {
			continue
		}
		if r.Changes != "" || r.Refused != "" {
			standing = true
		}
		switch {
		case r.Pass != "":
			// The pass row is drive's reading of its own window, audit included.
			open, sentBack, audit = false, false, ledger.Row{}
			pending, pendingOK = "", false
			switch r.Pass {
			case passPassed:
				head, ok, standing = r.Head, true, false
			case passStanding:
				pending, pendingOK = r.Head, true
			}
		case r.Cleared:
			standing = false
			if pendingOK {
				head, ok = pending, true
				pending, pendingOK = "", false
			}
		case r.Stage != "":
			closeWindow()
			pending, pendingOK = "", false
			open, at = r.Stage == ledger.StageReview, i
		case r.Changes != "" || r.Refused != "":
			pending, pendingOK = "", false
			if open {
				sentBack = true
			}
		case r.Level != "":
			pending, pendingOK = "", false
			if open {
				audit = r
			}
		}
	}
	closeWindow()
	return head, ok
}

// bodyAccepts is open-pr's manual-path reading of the item file: no sent-back
// block standing, a body digest drover recorded, and the file still matching it.
func (s *session) bodyAccepts(it backlog.Item) bool {
	c := s.ledger.Counts(it.ID.Bare())
	return !s.blockStands(it) && c.LastBody != "" && !s.bodyEdited(it, c.LastBody)
}

// unreviewedCommits is each commit on head after from, as "<short hash>
// <subject>", oldest first, leaving out what a review owes nothing for: the
// trunk's own commits and the merge that brought them in, and a commit whose
// subject opens with review:, which the reviewer wrote. Any other merge stays
// listed, since a merge can carry a change of its own (a conflict resolution,
// or anything committed with --no-commit) and only the trunk's are excused.
//
// A merge is the trunk's when every parent past the first is a commit the
// trunk's first-parent history holds, here or at origin: every tip a branch
// update merges in is one. Ancestry alone cannot say it once the pull request
// has merged, since every branch commit is then on the trunk too, and a side
// branch merged into the item is reachable only through a second parent, so
// its commits stay listed.
//
// A commit missing from this clone, a squash-merged branch's head being the
// usual one, is fetched from origin by hash first.
func unreviewedCommits(root, trunk, from, head string) ([]string, error) {
	for _, c := range []string{from, head} {
		if gitcmd.Command(root, "cat-file", "-e", c+"^{commit}").Run() != nil {
			_ = gitcmd.Command(root, "fetch", "--quiet", "origin", c).Run()
		}
	}
	onTrunk, err := trunkMergedIn(root, trunk, from, head)
	if err != nil {
		return nil, err
	}
	args := []string{"log", "--reverse", "--format=%h%x00%P%x00%s", from + ".." + head}
	for p := range onTrunk {
		args = append(args, "^"+p)
	}
	args = append(args, "--")
	out, err := gitcmd.Command(root, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", shortSHA(from), shortSHA(head), err)
	}
	var commits []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, "\x00", 3)
		if len(f) != 3 || strings.HasPrefix(f[2], "review:") {
			continue
		}
		if parents := strings.Fields(f[1]); len(parents) > 1 && allOn(onTrunk, parents[1:]) {
			continue
		}
		commits = append(commits, f[0]+" "+f[2])
	}
	return commits, nil
}

func allOn(set map[string]bool, commits []string) bool {
	for _, c := range commits {
		if !set[c] {
			return false
		}
	}
	return true
}

// trunkMergedIn is each merge parent past the first, among the merges in
// from..head, that lies on the trunk's first-parent history: the tips branch
// updates brought in, whose history a log of from..head leaves out.
func trunkMergedIn(root, trunk, from, head string) (map[string]bool, error) {
	out, err := gitcmd.Command(root, "rev-list", "--merges", "--parents", from+".."+head).Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list %s..%s: %w", shortSHA(from), shortSHA(head), err)
	}
	var parents []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) > 2 {
			parents = append(parents, f[2:]...)
		}
	}
	if len(parents) == 0 {
		return nil, nil
	}
	var refs []string
	for _, ref := range []string{"refs/heads/" + trunk, "refs/remotes/origin/" + trunk} {
		if gitcmd.Command(root, "rev-parse", "--verify", "--quiet", ref).Run() == nil {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("neither %s nor origin/%s resolves here, so a merge from the trunk cannot be told from any other", trunk, trunk)
	}
	out, err = gitcmd.Command(root, append([]string{"rev-list", "--first-parent"}, refs...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list --first-parent %s: %w", trunk, err)
	}
	trunkHistory := map[string]bool{}
	for c := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		trunkHistory[c] = true
	}
	onTrunk := map[string]bool{}
	for _, p := range parents {
		if trunkHistory[p] {
			onTrunk[p] = true
		}
	}
	return onTrunk, nil
}

// unreviewedSince is unreviewedCommits from the item's latest passing review
// to head, with that review's head. No passing review, or one that covers no
// particular head, compares nothing.
func (s *session) unreviewedSince(id, head string) (reviewed string, commits []string, err error) {
	reviewed, ok := reviewedHead(s.ledger, id, func() bool {
		it, err := s.item(id)
		return err == nil && s.bodyAccepts(it)
	})
	if !ok || reviewed == "" || head == "" || ledger.SameCommit(reviewed, head) {
		return reviewed, nil, nil
	}
	commits, err = unreviewedCommits(s.cfg.Root, s.trunk(), reviewed, head)
	return reviewed, commits, err
}

// mergedHead is the commit the merged pull request carried: what the forge
// reports, else the branch as this clone holds it, else as origin does.
func (s *session) mergedHead(branch string) string {
	if len(s.cfg.Git.MergeCheck) == 0 {
		if _, head, merged, err := gh.MergedPRHead(s.labels(), branch); err == nil && merged && head != "" {
			return head
		}
	}
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		if head, err := revParse(s.cfg.Root, ref); err == nil {
			return head
		}
	}
	return ""
}

// recordMergeEvidence appends the heads finish compared to a completion that
// carries none, a mark-merged row being the usual one, and reports whether it
// wrote. A completion that already carries them, or no standing completion, is
// left alone, so a second finish adds nothing and the completion row keeps the
// attribution and report path it was written with.
func (s *session) recordMergeEvidence(row ledger.Row) (bool, error) {
	if row.MergedHead == "" {
		return false, nil
	}
	row = ledger.Stamp(driveStamp(row))
	wrote, err := ledger.AppendUnless(s.cfg.Abs(s.cfg.Paths.Ledger), row, func(l *ledger.Ledger) bool {
		return !l.State(row.ID).Merged || l.MergeEvidence(row.ID)
	})
	if err != nil || !wrote {
		return false, err
	}
	s.ledger.Rows = append(s.ledger.Rows, row)
	appended()
	return true, nil
}

// commitList is commits one per line under indent, for a message a person reads.
func commitList(commits []string, indent string) string {
	return indent + strings.Join(commits, "\n"+indent)
}

// refuseFinish is the refusal for each kind of no, because the remedy differs and
// one sentence covering both taught the operator to reach for --force either way.
func refuseFinish(branch, id, how string, answer mergeAnswer) error {
	if answer == mergeUnvouched {
		return fmt.Errorf("%s: %s.\n"+
			"git lists a branch that never moved as merged, because it is an ancestor of the trunk, so an "+
			"ancestry yes alone would record a completion for work that may not be on it.\n"+
			"If the work is on the trunk, --force records it on your word. If a run died before its first "+
			"commit, drover drive %s runs it again from the start", branch, how, id)
	}
	return fmt.Errorf("%s does not look merged (%s) — merge it first, or pass --force if it merged under "+
		"a name this project's [run] branch template no longer renders", branch, how)
}

// forgeSaysMerged asks the project's merge authority. A no is an answer rather
// than an error, because mergedInto consults ancestry either way; the source
// string is the only thing that separates an authority that said no from one
// nothing could reach.
func (s *session) forgeSaysMerged(branch, trunk string) (bool, string, error) {
	if argv := s.cfg.Git.MergeCheck; len(argv) > 0 {
		return mergeCheck(s.cfg.Root, argv, branch, trunk)
	}
	n, merged, err := gh.MergedPR(s.labels(), branch)
	switch {
	case err != nil:
		return false, "gh unavailable", nil
	case merged:
		return true, fmt.Sprintf("PR #%d", n), nil
	}
	return false, "no merged PR for this branch", nil
}

// mergeCheck runs [git] merge_check with the branch and the trunk appended. A
// non-zero exit is the command reporting "not merged"; anything that stops it
// running at all is a misconfiguration, and reporting that as "not merged"
// would turn a typo in the config into a refusal nobody can explain.
func mergeCheck(root string, argv []string, branch, trunk string) (bool, string, error) {
	full := make([]string, 0, len(argv)+2)
	full = append(full, argv...)
	full = append(full, branch, trunk)
	if _, err := exec.LookPath(full[0]); err != nil {
		return false, "", fmt.Errorf("[git] merge_check: %q is not on PATH", full[0])
	}
	cmd := exec.Command(full[0], full[1:]...) // #nosec G204 -- argv is project config, not user input
	cmd.Dir = root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, "[git] merge_check says merged", nil
	case errors.As(err, &exit):
		return false, "[git] merge_check says no", nil
	}
	return false, "", fmt.Errorf("[git] merge_check: %w: %s", err, strings.TrimSpace(stderr.String()))
}

func branchMerged(root, branch, trunk string) (bool, error) {
	cmd := gitcmd.Command(root, "branch", "--merged", trunk, "--format=%(refname:short)")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// git's own diagnosis, not its exit status. "not a git repository" and
		// "malformed object name main" are different problems with the same
		// status 128, and the number alone sent a reader to neither.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return false, fmt.Errorf("list merged branches: %s", msg)
		}
		return false, fmt.Errorf("list merged branches: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == branch {
			return true, nil
		}
	}
	return false, nil
}

// sameCommit reports two refs resolving to the same object.
//
// A branch nobody advanced still resolves to the trunk it was cut from, and
// that is the one case where "merged" is true and no work exists.
//
// One ref per call, because `git rev-parse --verify` accepts exactly one
// revision and answers "Needed a single revision" to two. Dropping --verify
// would take the pair, and would also echo an unknown name back verbatim
// instead of failing, so two refs that do not exist would compare equal.
func sameCommit(root, a, b string) (bool, error) {
	first, err := resolveCommit(root, a)
	if err != nil {
		return false, err
	}
	second, err := resolveCommit(root, b)
	if err != nil {
		return false, err
	}
	return first == second, nil
}

// resolveCommit is one ref's object id, or an error naming the ref.
func resolveCommit(root, ref string) (string, error) {
	cmd := gitcmd.Command(root, "rev-parse", "--verify", ref+"^{commit}")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("resolve %s: %s", ref, msg)
		}
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("resolve %s: git named no object", ref)
	}
	return id, nil
}

func gitAt(root string, args ...string) error {
	cmd := gitcmd.Command(root, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func init() {
	finishCmd.Flags().StringVar(&finishRunner, "runner", "",
		"runner to attribute the work to (default: the recorded run's runner, else [run] default_runner)")
	finishCmd.Flags().BoolVar(&finishForce, "force", false, "record completion even though the branch is not merged")
	finishCmd.Flags().BoolVar(&finishDelete, "delete-branch", false, "delete the local branch after finishing")
	finishCmd.Flags().BoolVar(&finishNoTracker, "no-tracker", false,
		"record the finish without closing or commenting on any issue")
	root.AddCommand(finishCmd)
}

// finishUnlinked names the routed issues this merge did not touch.
//
// applyDispositions walks the closes key alone, so an issue routed here by
// label and absent from that key is passed over in silence — the merge that
// may have fixed it merges, the report stays open, and nobody who reads either
// one learns that the two were related. Saying it at the merge point is the
// last moment the branch still exists to check against.
//
// A line, never a refusal. Carrying a routed issue past a merge is the ordinary
// case: it is the backlog saying a later run owes the work. What was missing is
// the operator being told which reading they just chose.
//
// The remedy is only reachable where the key is declared. [frontmatter] is
// closed and closes is not in every project's lists, so a project that never
// declared it gets this line on every finish and a refusal from every link it
// recommends, with nothing connecting the two.
func (s *session) finishUnlinked(it backlog.Item, owned []gh.Issue) {
	claimed := backlog.Ints(it.Get(closesKey))
	var skipped []int
	for _, is := range owned {
		if !containsInt(claimed, is.Number) {
			skipped = append(skipped, is.Number)
		}
	}
	if len(skipped) == 0 {
		return
	}
	id := it.ID.Bare()
	fmt.Printf("  · %s open and owned by %s, not in %s, so nothing was written to %s: %s\n",
		plural(len(skipped), "issue is", "issues are"), id, closesKey,
		plural(len(skipped), "it", "them"), s.registerRefsTo(os.Stdout, skipped))
	if !s.cfg.Known(closesKey) {
		fmt.Printf("      %s is in neither [frontmatter] list, so no item here can name what it resolves:\n"+
			"      add it to required or optional in .drover.toml, then drover link %s <issue>\n", closesKey, id)
		return
	}
	fmt.Printf("      if this merge fixed one: drover link %s <issue>, resolve it, then finish %s again\n", id, id)
}

// postFinish runs [finish] post_check once an item has finished, while [supervise]
// post_finish_check is above off.
//
// Nothing it learns reaches finish's exit. The merge already happened on a
// trunk other people pull, and a check reporting that what merged is broken is
// a finding about the trunk, not a reason to pretend the item did not finish: it
// files a follow-up against the item so the work has an owner, and stops for the
// operator.
func (s *session) postFinish(it backlog.Item, trunk string) {
	argv := s.cfg.Finish.After
	if len(argv) == 0 {
		return
	}
	id := it.ID.Bare()
	if p, _ := s.cfg.Policy("post_finish_check"); p == config.PolicyOff {
		fmt.Printf("  · [finish] post_check did not run: [supervise] post_finish_check is off\n")
		return
	}
	pr := ""
	prNum, _ := s.ledger.PR(id)
	if prNum > 0 {
		pr = strconv.Itoa(prNum)
	}
	sha, err := resolveCommit(s.cfg.Root, "origin/"+trunk)
	if err != nil {
		sha, _ = resolveCommit(s.cfg.Root, trunk)
	}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is project config, as [drive] merge_gate is
	cmd.Dir = s.cfg.Root
	cmd.Env = append(os.Environ(), "DROVER_ITEM="+id, "DROVER_PR="+pr, "DROVER_SHA="+sha)
	out, runErr := cmd.CombinedOutput()
	first := strings.TrimSpace(firstLine(strings.TrimSpace(string(out))))
	if runErr == nil {
		if first == "" {
			first = "(no output)"
		}
		if err := s.record(ledger.Row{ID: id, PostFinish: first, Date: ledger.Today()}); err != nil {
			fmt.Printf("  · [finish] post_check passed and the row went unrecorded: %v\n", err)
			return
		}
		fmt.Printf("  ✓ post-finish: %s\n", first)
		return
	}

	why := runErr.Error()
	if first != "" {
		why += ": " + first
	}
	title := fmt.Sprintf("[finish] post_check failed once %s finished: %s", id, trunc(why, 120))
	body := fmt.Sprintf("`%s` exited non-zero after %s finished at %s.\n\n```\n%s\n```\n",
		strings.Join(argv, " "), id, sha, lastLines(strings.TrimSpace(string(out)), 40))
	fmt.Printf("  ! [finish] post_check failed: %s\n", why)
	where, ferr := s.fileFollowUp(id, title, body)
	if ferr != nil {
		fmt.Printf("    nothing filed: %v\n", ferr)
	} else {
		fmt.Printf("    filed %s against %s\n", where, id)
	}
	if events != nil {
		events.needsOperator(id, "post_finish_check", "[finish] post_check failed: "+why, "", prNum)
		return
	}
	sink := newEventSink(s.cfg, false)
	sink.needsOperator(id, "post_finish_check", "[finish] post_check failed: "+why, "", prNum)
	sink.settle()
}

// fileFollowUp files a follow-up owned by one item, in whichever follow-up list
// the project keeps, and says where it went.
//
// On the tracker the owner is a link, never a label: a sub-issue of the owner's
// issue when the item names one, and otherwise an Owner: line in the body.
func (s *session) fileFollowUp(id, title, body string) (string, error) {
	switch s.cfg.RegisterBackend() {
	case config.BackendFile:
		path := s.cfg.Abs(s.cfg.Paths.Register)
		f, err := register.Read(path)
		if err != nil {
			return "", err
		}
		row := register.Row{Number: f.Max + 1, Title: title, Owners: []string{id}, State: register.StateOpen}
		if err := register.Append(path, row); err != nil {
			return "", err
		}
		return fmt.Sprintf("#%d in %s", row.Number, rel(s.cfg.Root, path)), nil
	case config.BackendGitHub:
		parent, body := s.ownerLink(id, body)
		if s.offline {
			how := fmt.Sprintf("with an %q line in its body", gh.OwnerLine(id))
			if parent != 0 {
				how = fmt.Sprintf("as a sub-issue of #%d", parent)
			}
			return "", fmt.Errorf("--no-tracker: file it with gh, labeled %s, %s", s.cfg.GitHub.FollowUpLabel, how)
		}
		var labels []gh.Label
		if s.cfg.GitHub.FollowUpLabel != "" {
			labels = append(labels, s.followUpLabel())
		}
		n, err := gh.CreateIssue(s.labels(), title, body, labels, parent)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("#%d", n), nil
	}
	return "", fmt.Errorf("%s has no follow-up list to file into", s.cfg.RegisterDeclaration())
}

// ownerLink is how a follow-up drover files names its owner: the owner's issue
// as the sub-issue parent when the item names one, and otherwise body with an
// Owner: line appended.
func (s *session) ownerLink(id, body string) (int, string) {
	if it, err := s.item(id); err == nil {
		if n := issueOf(it); n != 0 {
			return n, body
		}
	}
	return 0, strings.TrimRight(body, "\n") + "\n\n" + gh.OwnerLine(id) + "\n"
}

// followUpLabel is the membership label as a write applies it, created first
// when the repository lacks it.
func (s *session) followUpLabel() gh.Label {
	return gh.Label{Name: s.cfg.GitHub.FollowUpLabel, Color: "D93F0B",
		Description: "drover follow-up: deferred work an item owns"}
}

// supervising is runner.Supervising, named here because finish's own runner
// variable shadows the package.
func supervising() bool { return runnerpkg.Supervising() }

// supervisorMayFinish holds a supervisor's finish to merge_drive_pr: the gate
// already checked the policy, and this checks that the branch is one the policy
// covers. The ledger half is a pull request drive opened and a passing review
// drive recorded; the head half is that the pull request holds no commit after
// the head that review read, by unreviewedCommits' exclusions. The forge half,
// green checks and a mergeable pull request, is the supervisor's to read before
// it runs finish, and the merge refuses a pull request the forge will not merge.
func (s *session) supervisorMayFinish(id, branch string) error {
	_, err := s.supervisorEligible(id, branch)
	return err
}

// supervisorEligible is supervisorMayFinish with the head of the pull request
// the forge still holds open for branch, which is what finish merges, or empty
// when none is open: merged already, or a forge drover could not ask.
func (s *session) supervisorEligible(id, branch string) (open string, err error) {
	p, _ := s.cfg.Policy("merge_drive_pr")
	if why := driveMergeGap(s.ledger, id, branch); why != "" {
		return "", Refusal{fmt.Errorf(
			"finish under a supervisor is the merge_drive_pr action, at policy %q in this project, and %s finishes only when %s.\n"+
				"Report the item's state and let the operator merge and finish it", p, id, why)}
	}
	head := ""
	if !s.offline {
		if _, h, ok, err := gh.OpenPRHead(s.labels(), branch); err == nil && ok {
			head, open = h, h
		}
	}
	if head == "" {
		head = s.mergedHead(branch)
	}
	reviewed, commits, err := s.unreviewedSince(id, head)
	switch {
	case err != nil:
		return "", Refusal{fmt.Errorf(
			"finish under a supervisor is the merge_drive_pr action, which covers what drive's review read, and drover could not list what %s holds after %s, the head %s's last passing review read: %w.\n"+
				"Report it and let the operator merge and finish %s", branch, shortSHA(reviewed), id, err, id)}
	case len(commits) > 0:
		return "", Refusal{fmt.Errorf(
			"%s's pull request head %s holds %d %s after %s, the head its last passing review read, and no review read %s:\n%s\n"+
				"merge_drive_pr covers what drive's review read. Report it: drover review %s reads the head, and the operator merges and finishes it",
			id, shortSHA(head), len(commits), plural(len(commits), "commit", "commits"), shortSHA(reviewed),
			plural(len(commits), "it", "them"), commitList(commits, "  "), id)}
	}
	return open, nil
}

// supervisorMayFinishDenied holds a supervisor's finish to merge_protected_paths. A
// pull request drive held on a denied path is a separate action from one it
// merely opened, so passing merge_drive_pr says nothing about it; the
// protected_paths park drive recorded is the evidence. A park older than the
// latest drive pull request row belongs to an earlier pull request, and the
// re-driven one touched no denied path or it would have parked again.
func (s *session) supervisorMayFinishDenied(id string) error {
	p, _ := s.cfg.Policy("merge_protected_paths")
	if supervisorMay(p) || !deniedSinceLastPR(s.ledger, id) {
		return nil
	}
	return Refusal{fmt.Errorf(
		"finish under a supervisor is the merge_protected_paths action here, at policy %q in this project, because drive held %s's pull request on a [drive] protected_paths file.\n"+
			"Report the item's state and let the operator merge and finish it", p, id)}
}

// deniedSinceLastPR reports whether id carries a protected_paths park row
// appended after its latest drive-stamped pull request row.
func deniedSinceLastPR(l *ledger.Ledger, id string) bool {
	denied := false
	for _, r := range l.Rows {
		switch {
		case r.ID != id:
		case r.Park == "protected_paths":
			denied = true
		case r.PR > 0 && r.Drive != "":
			denied = false
		}
	}
	return denied
}

// driveMergeGap is the first merge_drive_pr ledger condition id's rows fail,
// worded as the condition, or empty when they meet both: a pull request row a
// drive wrote for branch, and a latest review pass of passed that drive
// recorded, with no launch, audit or changes row after it.
func driveMergeGap(l *ledger.Ledger, id, branch string) string {
	var pr *ledger.Row
	for i, r := range l.Rows {
		if r.ID == id && r.PR > 0 {
			pr = &l.Rows[i]
		}
	}
	if pr == nil || pr.Drive == "" || pr.Branch != branch {
		return "the ledger names a pull request for " + branch + " on a row drive wrote"
	}
	var pass *ledger.Row
	for i, r := range l.Rows {
		if r.ID == id && r.Pass != "" {
			pass = &l.Rows[i]
		}
	}
	if token, _, stale, ok := l.Pass(id); !ok || token != passPassed || stale || pass.Drive == "" {
		return "its latest review pass is passed and came from a session drover launched"
	}
	return ""
}
