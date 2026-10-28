# Lifecycle

One item, start to finish. [Concepts](concepts.md) explains why each store is where it is; this page is the order you touch them in, and what each step refuses to do for you.

Every step is a command a person can type. Nothing here needs an agent, and the same sequence drives one: [Agent guide](agent-guide.md) is which of these steps a coding session may take and which it may not, and [Automation](automation.md) is the machine-readable contract underneath both.

For a first run (install, `init --user`, `init`, and the choices each of those needs), read [Getting started](getting-started.md) instead. This page begins after those, at the point where a project already loads.

## Sequence

```text
write ─> check ─> run ─> review ─> audit ─> open-pr ─> merge ─> finish
                   ^        │                                        │
                   └────────┘ request-changes                        │
                   │                                                 │
                   └──────────────────── report ─────────────────────┘
```

`run`, `open-pr` and `finish` are the only steps that touch git, and `open-pr` is the only one that reaches the forge before the merge. `audit` is the only step that records judgment, and it closes the review pass rather than waiting for the merge: the session that has just traced every requirement is the one holding what the row has to say. The independence the row needs comes from `--code-by`, which refuses `verified` from whoever wrote the code whenever the level is written, so waiting for the branch to disappear buys nothing and costs the reader. A merged item nobody audited is what `drover unaudited` reports, and a later row still counts: the most recent level wins, in either direction.

**Review sits before the merge, and the arrow back is the point of putting it there.** A requirement the review finds unmet is this item unfinished, not a new defect: the branch is still open and no pull request exists yet, so `drover run <id>` returns to the same branch and the review runs again. Only a defect owned by a _different_ item becomes a tracker row. Filing an unmet requirement instead of returning it merges the item incomplete and leaves an issue whose only owner just shipped, which is how a backlog fills with rows nothing closes.

### The whole loop, and who may run each step

| Step                                               | Command                                                 | A launched session may run it                                                                  |
| -------------------------------------------------- | ------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Take in a tracker issue                            | `drover intake <n>`                                     | no: it relabels a live issue                                                                   |
| Draft the requirements                             | `drover draft <id>`                                     | yes: it writes one body and clears no marker                                                   |
| Accept the contract                                | `drover accept <id>...`                                 | no: a run cannot accept its own requirements                                                   |
| Change an accepted contract                        | `drover amend <id> -f <file> --reason "<why>"`          | no: a session would be moving the target it is measured against                                |
| Implement                                          | `drover run <id> --headless`                            | no: a session cannot launch another                                                            |
| Review                                             | `drover review <id> --runner <x> --headless`            | no, for the same reason                                                                        |
| Send the review's findings back to the branch      | `drover request-changes <id> -f <file>`                 | **yes**: it rewrites one section of the body and appends a body digest row and a `changes` row |
| Record a property every later pass must keep       | `drover invariant <id> -f <file>`                       | **yes**: it appends to one section of the body, and writes no ledger row but its body digest   |
| Close a block whose findings the branch fixed      | `drover close-changes <id>`                             | **yes**: it rewrites that section, and writes no ledger row but its body digest                |
| Move work a contract never named into its own item | `drover split <id> --into <kind> "<title>" -f <file>`   | **yes**: it scaffolds one item and blocks one of the two on the other                          |
| Link an issue this run fixed                       | `drover link <id> <n>`                                  | **yes**: it rewrites one frontmatter line and reads the tracker to check the number            |
| Judge each `closes` issue                          | `drover resolve <id> <n> <disposition> --code-by <who>` | **yes**: it writes one ledger row and writes to no tracker                                     |
| Correct what the review returned                   | `drover run <id> --headless`, then review again         | no                                                                                             |
| Record how hard anyone looked                      | `drover audit <id> <level> --code-by <who>`             | **yes**, at the level your authorship allows                                                   |
| Open the pull request                              | `drover open-pr <id>`                                   | no: an open one says the branch may merge, and no session says that about its own work         |
| Meet an `[operator]` requirement                   | `drover signoff <id> <n> "<what was checked>"`          | no: it certifies a requirement the session was told not to attempt                             |
| Merge                                              | your forge                                              | not applicable                                                                                 |
| Record the merge, write the tracker                | `drover finish <id>`                                    | no: the operator merges, so the operator records it                                            |
| Confirm nothing was left unjudged                  | `drover check`                                          | yes: it is read-only                                                                           |

Three commands carry the mechanical core (`run`, `review`, `finish`). The loop is not complete without `audit`, which is the only record of whether anyone checked, and `check`, which reports the issue nobody judged.

## 1. Write the item

One file at `<paths.backlog>/<stem>-<slug>.md`. The directory is the backlog, so there is no index to update.

```markdown
---
title: Config schema, choices and system checks
deps: 1
---

Act as a Senior Engineer hardening a plugin against a dependency that publishes no API.

<Why this item exists, and what a reader needs to know before the list.>

Requirements:

1. Add `pkg/config.go` reading settings lazily, per instance rather than at import, so a bad value cannot break startup.
2. Every configured regex compiles at access time, and a failure names the key.

Out of scope: the resolver itself.
```

Three things about that file are not style.

**The numbered list is the contract.** `report --save` turns every top-level `N.` line into an audit checkbox. A requirement written as prose is not on the checklist, so nobody will ever confirm it.

**A requirement only a person can meet opens with `[operator]`.** A live check over ssh, a deploy, anything the session's permission mode blocks: write it as `3. [operator] psql on the dev host shows the migration applied`. The prompt lists it under its own heading and tells the session not to attempt it, `report --save` marks its checkbox `(operator)`, and the rest of the lifecycle treats it as the hold described under [Requirements only the operator can meet](#requirements-only-the-operator-can-meet).

**Frontmatter is a closed schema**, declared in `[frontmatter]`. An undeclared key is a parse error, because a typo'd `deps` reads as no dependencies and renders a blocked item READY: a wrong answer rather than a degraded one. Required keys may be empty; they may not be absent.

**The filename carries the slug.** There is no `slug` key, and writing one is a parse error. Renaming the file changes its branch, its log names and its report path, so run `check` after any rename.

Or scaffold an empty one and fill it in:

```bash
drover new bug "the resolver drops a trailing dot"
drover new feature "publish a release binary" --sev H
```

`new` allocates the next free id in the namespace matching the kind (see [Item ids](concepts.md#item-ids): `B1` is the first bug, `F1` the first feature) and writes the same scaffold `intake` does, minus the issue. A project with no tracker needs it to create an item at all, and a project with one still wants it: work that starts as a decision rather than as a bug report has no issue to take in.

To scaffold from a tracker issue instead:

```bash
drover intake --list                 # what the tracker holds that drover has not
drover intake 42                     # namespace from the issue's labels
drover intake 42 --kind enhancement  # override
```

`intake --list` is the import queue: every open issue carrying a label that matches an id namespace, minus the ones a body already names. It is not the follow-up list. A row there is work somebody is going to do; a row in `drover follow-ups` is work nobody has priced, and an issue carrying neither appears in neither.

That writes the body and quotes the issue as raw material. The scaffold's severity is the tracker's own rank, read from the `severity_prefix` label; an issue carrying no rank drover recognizes scaffolds `M` if it is a bug and leaves the key empty otherwise, which is a fallback rather than an assessment.

It does **not** write the requirements. The scaffold carries `[intake] stop_marker` and drover refuses to launch or even preview the item while it is there, because requirements lifted mechanically from a bug report are checkboxes nobody can falsify.

### Draft the requirements

Writing a good list means reading the code the item touches and citing files and lines, which is work an agent is suited to. `draft` assembles what that takes:

```bash
drover draft B1                              # print the prompt
drover draft B1 --runner claude --headless   # hand it to a drafting agent
```

The prompt carries the originating issue read fresh from the tracker, `[agents] principles`, the numbered-list contract and why it exists, the rest of the backlog with paths so a requirement belonging to a sibling gets filed there rather than absorbed, and the body file to write into. Point `[prompts] draft` at your own to change what it says.

`draft` writes nothing itself and records nothing. The agent writes the body, the same division `run` and `review` keep.

**The stop marker survives the draft.** The prompt tells the agent to leave it, and no flag on `draft` removes it. Deleting it is a human reading the drafted list and accepting it, which is the only thing that makes the body runnable, so an unsupervised agent cannot produce an item that executes. `draft` refuses an item whose marker is already gone and exits 3: a body past intake is somebody's accepted contract, and a redraft would overwrite it.

### Accept the requirements

```bash
drover accept B1                  # the list is the contract; run may launch
drover accept B4 B5 E2            # several at once; a bad id is named and skipped
drover unaccept B1                # put the marker back and redraft
```

Given several ids, `accept` takes each in turn. One that fails is reported on stderr by name and the rest are still accepted; the run then exits non-zero (3 when every failure was a refusal, 1 otherwise) and its last line lists the ids left unaccepted.

`accept` deletes the block `intake` wrote and nothing else: the marker line, the two explanation lines under it, and the blank that separates them from the body. `unaccept` writes the same block back, so an item accepted and then unaccepted is byte-identical to the scaffold it came from, and a body whose block a human deleted by hand gets the explanation back with the marker rather than a bare line.

The lines under the marker are matched against `intake`'s own text rather than scanned to the next blank. A marker somebody typed by hand with a paragraph directly under it loses the marker line alone, and the paragraph stays where its author put it.

It stays a separate command rather than a flag on `draft` for the reason the marker exists at all. A flag is something a runner's argv can carry; a command an operator types is not. Nothing that launches a session can reach `accept`.

Both name a state rather than a transition, so both are idempotent: `accept` on a body with no marker and `unaccept` on one that already has it each report what already holds and exit 0. `unaccept` refuses an item the ledger records merged and exits 3: a merged item is not a scaffold, and `unmark-merged` is the command for withdrawing a completion.

The marker is matched as a whole line outside any fenced block. An item quoting it in a config sample, which is routine in a backlog of items about intake, is somebody's accepted contract and not a scaffold.

#### What accept checks

`accept` is the last look before a body becomes the contract, so it reads the body for what a lint can see:

| Check                                                                                                                                                                                                                                      | `accept`                                                                                                 | Elsewhere                                                                                                                           |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| An angle-bracket placeholder such as `<provider>`, in prose or in a code span with no space in it (`internal/<provider>/client.go`). A fenced block and a code span holding a space (`drover show <id>`) are syntax examples and not read, and so is a span holding one HTML or SVG element or capitalized component and nothing else (`<h1>`, `<Dialog>`). Write a variable part in braces, `internal/{provider}/client.go`, which is never a placeholder. | Refused, naming each line, unless `--yes`.                                                               | `check` reports `PLACEHOLDER` on an accepted item until it merges.                                                                  |
| A line carrying one of `[intake] hold_markers`, by default `OPERATOR DECISION PENDING` and `BEFORE ACCEPT`, outside a fenced block.                                                                                                        | Refused, naming each line, unless `--yes`.                                                               | `check` reports `HOLD MARKER` on an accepted item until it merges.                                                                  |
| A path or name only the branch of an unmerged item waiting on this one has. See [Which way a split points](#which-way-a-split-points).                                                                                                     | Refused unless `--yes`.                                                                                  | `check` reports `BRANCH CYCLE`.                                                                                                     |
| A cited directory in the project tree.                                                                                                                                                                                                     | One note per directory: under `drive --jobs` the item waits for any item citing a path below it. Exit 0. | `new` prints the same note and `check` reports `BROAD CITATION` for a top-level directory, both while the body is still a scaffold. |
| The deps line, the stop marker and the actor.                                                                                                                                                                                              | Refused. `--yes` never overrides these.                                                                  |                                                                                                                                     |

A passing mention is not a file the item touches. Open the paragraph with `<!-- drover: context -->` on its first line and its citations count for neither the note nor dispatch's overlap set, which `drive --dry-run --jobs N` shows in the pairs it holds apart. A numbered requirement line inside that paragraph still counts:

```text
<!-- drover: context -->
Nothing in `backend/` parses a retail barcode today.
```

### Change an accepted contract

Once accepted, the numbered list is what every run and review is measured against, so no command a session can reach changes it: `request-changes`, `close-changes` and `invariant` each refuse a write that would move the count. When the contract itself is wrong (a requirement belongs to a different design, or to another item), the operator changes it with `amend`:

```bash
drover amend 7 -f reqs.md --reason "R4 belongs to the cache redesign"
drover amend 7 -f reqs7.md --reason "R4 moves to 9" --moved 9     # a move is amend on both
drover amend 9 -f reqs9.md --reason "R4 of 7 lands here" --moved 7
```

The file is the new numbered list and nothing else. `amend` replaces the body's one contiguous list in place, keeps the list it replaced beside the saved reports as `<stem>-<slug>.amend-N.md` with the reason, and appends a body row carrying the new digest and an `amend` row naming the count before and after, the reason and, with `--moved`, the other item. It refuses a scaffold (write its list by hand and `accept` it), a merged item, and a list that prose splits in two. It is the operator's: it refuses inside any session drover launched, supervisor included, because `move_requirement` sits at `operator` with no higher ceiling.

Do it before sending the item back. `request-changes` refuses evidence that names a requirement by number (`R3`, "requirement 3") the body's list does not hold, unless an `amend` since the last round removed it, and evidence that says requirements were removed or replaced when no `amend` has run since the last round. That is a heuristic over the evidence's prose, read outside code fences: a prompt to look, not a proof. Under `--over-limit` it warns instead and writes with `--yes`.

An edit by hand works on the file and breaks the review that follows. Every drover command that writes a body records its digest, and `drive` reads a body that no longer matches the last one recorded as a reviewer writing findings by hand, so the next clean pass parks `body_edited`. `drover check` reports such an item as `HAND EDITED`, and `drive` parks it `hand_edited` before launching any session, until the edit is recorded: `amend` for a change to the numbered requirements, or `touch` for prose.

```bash
drover touch 7 --reason "reworded the background paragraph"
```

`touch` appends one body row carrying the current digest and the reason, and keeps nothing else, so a requirements change recorded with it leaves no trace of the list it replaced. It is the operator's under the same refusals as `amend`. A scaffold is never reported, since writing its requirements by hand is the workflow, and neither is an item with no digest on record, nor one a live session in this checkout is working.

## 2. Check

```bash
drover check
```

Exit 0 clean, exit 2 on findings, exit 1 if it could not run the validation at all. Put it in CI; run it after any rename.

Findings are `NO BACKLOG`, `NO SHELF`, `SHELF IS BACKLOG`, `NO VERIFY`, `UNDECLARED KEY`, `NO REQUIREMENTS`, `UNREADABLE`, `BAD VALUE`, `BAD RUNNER`, `BRIDGE SIGNED OUT`, `BRIDGE UNREACHABLE`, `BRIDGE VERSION`, `BAD LEDGER`, `ORPHAN ROW`, `SHELF CLASH`, `MISSING DEP`, `BAD DEPS`, `BRANCH CYCLE`, `MISSING REPORT`, `BAD REGISTER`, `NO LABEL`, `NO REGISTER`, `DUPLICATE ISSUE`, `ORPHANED FOLLOW-UP`, `NO CLOSES KEY`, `UNLINKED`, `NO DISPOSITION`, `UNMIRRORED`, `MIRROR DRIFT`, `MIRROR STALE`, `MIRROR SHELVED`, `STALE CONFIG`, `SUPERVISOR UNSET`, `BAD TEMPLATE`, `STALE BLOCK`, `STALE POLICY`, `BAD ADDENDUM`, `NO POINTER`, `BAD MARKERS`, `PROTECTED UNMATCHED`, `SEQUENCE CLASH`, `SESSION MARKER`, `PLACEHOLDER`, `HOLD MARKER`, `BROAD CITATION` and `HAND EDITED`. [Concepts](concepts.md#validation) says what each means.

Then see where the item sits:

```bash
drover status            # what is left, its state, its audit level
drover status --all      # every item, merged rows included; the totals never changed
drover status --sev C,H  # only these ranks; the totals still count them all
drover status --by-sev   # worst severity first instead of id order
drover show 2            # one item, including its full audit history
drover show 2 detail     # the same, then the body a run is handed
```

`--sev none` selects the items declaring no rank drover recognizes. Where `[status] column` names the severity key, the cell is colored by rank.

## 3. Preview the run

```bash
drover run 2 --dry-run
```

This touches nothing and prints what the runner will be handed: the prompt template rendered against the item, its dependencies, the resolved `spec:` paths, the vendored guides, the selected verify command, and the open follow-ups owned by this item and its direct dependencies.

Read it. The three things worth checking before spending a run:

```bash
drover run 2 --dry-run | grep -A20 'Open follow-ups' # what it inherits
drover run 2 --dry-run | grep 'The verify command passes'           # which verify command it got
drover skills                                        # any guide not vendored
```

A verify command named for a frontmatter key wins when that key is `yes`, names are tried in sorted order, and the first match wins. drover selects **one** verify command and never composes several, so a flagged verify command must be a superset of the default. A check that lives in only one of them stops running the moment two flags are set together.

## 4. Run

```bash
drover run 2              # interactive
drover run 2 --headless   # event stream to a log, report captured on exit
```

`run` switches to the branch `[run] branch` renders for the item, then hands the prompt to the runner. Dependencies are advisory here: an unmerged dep is reported, not enforced, because the operator sometimes has a reason.

`run` refuses to switch over a dirty tree and exits 3, listing up to ten of the paths `git status --porcelain` reports and counting the rest. A `git switch` carries uncommitted changes onto the branch it switches to, so work left from another item would become part of this one's diff and nothing downstream would say so. Every path git does not ignore counts, untracked files included even where `status.showUntrackedFiles` is `no`, and `.drover.toml` and the backlog among them, since either can be an item's work. So the paths drover writes its own records to (ledger, reports, register file, logs, and the backlog a review rewrites) have to be ignored or kept outside the host repo, or `run` and `drive` refuse on them, and the refusal names the ones it found. Nothing is stashed, reset or cleaned: commit the work on the branch it belongs to or discard it, then run again. `drive` applies the same check when it starts, before reconciliation finishes anything, then before each item's switch and after each implement stage and review; see [`drive --json`](automation.md#drive---json) for the three causes it parks on.

A project with [`[git] enabled = false`](configuration.md#git) has no branch to switch to. `run` executes in place and its prompt names none, because the prompt is an instruction and naming a branch nothing will create is a wrong one.

Only a headless run leaves a log, and only a headless run captures a report. Capturing after an interactive session would reach back to a stale log and misattribute the report.

`run` appends one ledger row naming the runner it resolved, before the launch. That row is what `finish` and `mark-merged` read to attribute the work, so neither has to be told with `--runner` and neither falls back to a literal `claude`. Deriving it from the newest log instead breaks the moment an item is run twice with two runners, and is not available at all after an interactive run.

An agent that ran an item outside drover records the fact by appending one line to the ledger itself. That protocol is why the ledger is text rather than a database: it has to be writable from a sandbox with no binary and no network.

## 5. Review

```bash
drover review 2
```

Assembles the reviewer's prompt: the numbered contract, every dependency with how well it is audited, the open follow-ups, the dependencies' saved reports, the run's own report, the issues the item's `closes` key names, and the verify command. Seven things a reviewer would otherwise gather from five commands, and the failure mode of gathering them by hand is skipping one silently.

A headless review reports what its pass amounted to when the runner returns, in the same tokens and the same sentences a `drive` parks on: `drover review B1 --runner codex --headless` ends on `Pass: no_record`, then the outcome and the next step described under [what the review leaves you to do](#what-the-review-leaves-you-to-do). Without it, a review that recorded neither an audit nor a send-back would leave a stage row and nothing else, and the operator would have to work out whether no findings was good news. Where the runner's terminal event names what it refused the session, `no_record` and `died` name that too, so a review denied every command reads as one that could not reach `drover audit` rather than one that found nothing. An interactive review prints nothing after the runner returns: it writes no log, so there is no terminal event to read, and every pass would read as `died`.

Nothing is recorded. A pass row is `drive`'s, and [`drover open-pr`](#7-open-the-pull-request-merge-then-finish) reads one as the authority that a branch may merge; a review nobody supervised does not acquire that standing by having printed a word.

The `closes` issues are read from the tracker as the prompt is built, so the reviewer judges the current report rather than whatever the body quoted. Each one gets a `drover resolve` line; `finish` writes them to the tracker after the merge. See [issue dispositions](concepts.md#issue-dispositions).

An issue the run fixed that the key never named is not on that list, and `resolve` refuses it. `drover link <id> <n>` puts it there, checking the number against the tracker before `resolve` accepts it; `resolve --link` does both in one step. Leaving the key alone is the other legitimate answer and says a later run owes the work. `drover check` asks the question as `UNLINKED` while the branch is still open.

The prompt asks it too. Open follow-ups split into the rows owned by this item, each with a `resolve --link` line pre-filled, and the rows carried one hop from its dependencies, which are listed with their owners so a review does not re-file one as new. A row this branch discharged is linked and judged there; a row it did not touch is left undisposed, which keeps it open and owned for a later run.

### What the review leaves you to do

Given a runner and `--headless`, `review` launches the session and, once it exits, reads back its log and the rows it recorded and ends on three lines on stderr:

```text
Pass:       changes_requested
Outcome:    the review sent work back; the findings are in the item body, under its review heading
Next steps: drover run B2, then drover review B2 --runner codex
```

An interactive review prints none of them. It leaves no log, so there is no terminal event to read and every pass would report as `died`; `review` returns the runner's exit and nothing else.

The reading is the one `drive` makes of a pass, from the log and the rows. The next step repeats the runner and `--headless`, so the command pastes as the pass it follows.

| The pass                                                                                                | Next steps                                                                                                                                                                                                                        |
| ------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Sent work back (`changes_requested`)                                                                    | `drover run <id>`, then review it again                                                                                                                                                                                           |
| Sent back the same evidence, or a finding it marked still open, twice in a row (`repeating`)            | `drover show <id>` and the two kept rounds the park names, then narrow the contract, split the item or override the review by hand; another run meets the same finding                                                            |
| Wanted to send work back at `[review] max_review_rounds` (`at_limit`)                                   | `drover split <id>` for what is left, or raise the cap and run again                                                                                                                                                              |
| An audit, no send-back, and a block an earlier round sent back still open in the body (`findings_open`) | `drover request-changes <id> -f <file>` if its findings are still open; `drover close-changes <id> && drover open-pr <id>` if they are closed, since `open-pr` reads a `findings_open` pass followed by `close-changes` as passed |
| An audit, no send-back, and no body digest to compare against (`no_digest`)                             | Read the audit with `drover show <id>`; if it holds, `drover open-pr <id> --force`                                                                                                                                                |
| An audit, no send-back, and a body changed outside drover since the review launched (`body_edited`)     | Diff the body against its history, then `drover request-changes <id> -f <file>` the findings, or `drover open-pr <id> --force` if the change was not a finding                                                                    |
| An audit, no send-back, and the body as drover last wrote it (`passed`)                                 | `drover open-pr <id>`                                                                                                                                                                                                             |
| Nothing recorded (`no_record`)                                                                          | Review again, or record what you found with `drover audit <id>`                                                                                                                                                                   |
| No terminal event in the log (`died`)                                                                   | Read the log, then review again                                                                                                                                                                                                   |
| The runner's service or account failed (`faulted`)                                                      | Review again once the service recovers                                                                                                                                                                                            |
| A stream no schema reads (`unreadable`)                                                                 | `drover show <id>` lists what the review recorded                                                                                                                                                                                 |

`drive` ends the same way, once per item after its count line, and adds its own causes: a merge queued at the forge, a pull request held by `--no-merge`, `protected_paths` or a dependency a review split out, a merge gate that refused the diff or would not run, and every skip. The follow-up is on stderr in both commands, so `drive --json` keeps stdout to the document.

### Where a finding goes

The prompt sends each one to its owner, and picking wrong costs in both directions: a backlog fills with rows nothing closes, or a pull request collects round after round over findings nobody would have scheduled. Two questions decide it, in order. The contract and the branch's own diff say where a finding goes: `git diff <trunk>...HEAD` names what the run answers for, and a regression it introduced is the run's whether or not a requirement mentions it. What the defect would break in use then says whether it is worth a run at all, because the diff answers who caused it and not whether a pull request should stay open.

<!-- prettier-ignore -->
| Token | What you found | Where it goes |
| --- | --- | --- |
| `BRANCH-REQUIREMENT` | A numbered requirement of **this** item, unmet | Back to the branch. Report which and how you know; the operator re-runs `drover run <id>` and the review runs again. Nothing is filed, and no pull request opens until a pass comes back clean. |
| `BRANCH-REGRESSION` | A defect this branch's own diff introduced that would break something in use | Back to the branch, whether or not a requirement names it. One that would break nothing is `FIX-IN-REVIEW` or `NEW-ITEM`; when you cannot tell, it is this row. In the report a real one evaporates; on another item it falls on somebody who did not cause it. |
| `BRANCH-BLOCKER` | A pre-existing defect blocking a numbered requirement | Back to the branch. It is somebody else's code and this run's problem anyway: the contract cannot be discharged without touching it. Your report says why the diff is wider than the requirements name. |
| `SPLIT-OUT` | A finding already sent back to the branch whose fix changes a contract this item's numbered requirements never named: another subsystem, every caller of an interface, a storage or wire format | A new item, with this one blocked on it. `drover split <id> --into <kind> "<title>" -f <file>` scaffolds it and adds it to this item's `deps:`, so this one stays blocked and unmerged until the operator accepts that item and it is finished. The finding and the blame stay where they were sent; what moves is the implementation. A wider diff is not a different contract, and work you could describe as one more requirement on this list belongs on this list. |
| `FIX-IN-REVIEW` | A defect no requirement traces to, that no check can observe, and that you can correct in the file you found it in | Corrected in this pass, on the branch already checked out, committed alone with a `review:` subject and named in the audit note. Nothing goes back and nothing re-runs. Touch anything a requirement traces to and you are an author of the code: `--code-by` names you, and `verified` is refused. |
| `FOLLOW-UP-TO-OWNER` | A pre-existing defect outside the diff, blocking no requirement here | Filed against the item that owns the gap, not the one that noticed it, and named in the report. |
| `NEW-ITEM` | A defect no item owns that would break nothing in use and that you cannot correct here, this branch's own included | A new item: `drover new <kind> "<title>" --found-in <id> -f <file>`, the new id named in the report and the audit note, and the branch goes on to merge. The `-f` file is the finding, written for whoever writes the requirements: the file and line, the command that reproduces it with its actual output, and what is wrong. The audit note names the new id and does not carry the description. The scaffold is inert until a human accepts it, so filing one schedules nobody. Without an intake stop marker `new` refuses and the finding is `REPORT-ONLY`. |
| `REPORT-ONLY` | Anything you would not spend a run on, would not file, and cannot correct here | The report, and nowhere else. A finding nobody would schedule needs no tracker row and no backlog body. |

The three `BRANCH-` rows, `FIX-IN-REVIEW` and `SPLIT-OUT` are why review sits before the merge: each needs a branch that is still open. Afterwards the branch is gone and the same gaps have nowhere to go but the tracker, which is the situation the owner rule was written for and the wrong default for work still in flight.

Sending a finding back to the branch means writing it into the item body: `drover request-changes <id> -f <file>` replaces the `## What the review sent back` section and touches nothing else. That body is the whole prompt `run` assembles: a reviewer's report and an audit note both reach the operator and neither reaches the next run, so a gap recorded only in those is one the next run rediscovers or ships again. The section is replaced rather than appended, because a block naming gaps a later pass closed sends the next run to re-verify finished work. The numbered requirements are the contract and stay as they are; only the evidence under that heading changes.

A section ends at the next `##` line, so evidence written with `-f` has its own headings pushed below `###` before it is written. Without that, a reviewer's `## Evidence` would split the block: the replacement would end up above it, everything under it would stay, and the next run would read those settled findings as current instructions. Narrowing the boundary instead would break the other half of the rule, since a project's prompt template writes its own `##` sections into a body and those survive a send-back. Nothing about a heading's position says which of the two wrote it, so the evidence is what gets corrected. A top-level `N.` becomes a bullet for the same reason, and both moves are reported on stderr, so a writer knows their file reached the body in a different form.

Each round's evidence is kept beside the saved reports as `<stem>-<slug>.changes-N.md`, and both prompts list the earlier rounds as headlines with their paths. The ledger's `changes` row is a digest, which separates a review repeating itself from one finding something new and says nothing about either round; the list says what the rounds were about. Beside the digest the row carries `reqs`, the requirement numbers the evidence's headings name, traced the way the `[operator]` refusal traces them. `drive` parks a pass as `repeating` on either rule: the last two `changes` rows carry the same digest, or both carry `still_open` and the two lists share a requirement. The reviewer writes `still_open` with `drover request-changes <id> -f <file> --still-open N[,N...]`, naming the requirements whose finding from the previous request it reproduced as still open, and `request-changes` refuses a number that request did not name. The second rule catches a reviewer rewording one finding, where every rewording is a new digest, and it is the reviewer's word rather than drover's inference because only the reviewer knows whether it reproduced the old finding or found a new one. Equal `reqs` alone parks nothing: a requirement such as "every refused object answers 403" admits any number of distinct defects, and a reviewer writing new probes each round sends the same number back while the branch improves. `[review] max_review_rounds` bounds that loop, and parks it `at_limit`. That matters because the pattern is the diagnosis: five send-backs reading "record the cache hit", "persist by key", "compare the metadata", "take it from the open handle", "publish by rename" are one guarantee nobody stated, and each of them on its own reads as a clean fix to a real defect.

### Which way a split points

`drover split` writes the dependency between the two items, and `--after` and `--before` choose its direction.

- **`--after`** (the default): the item split waits on the new one. Its `deps:` line gains the new id, and it stays blocked until the new item merges. Use it when the new item's work stands on the trunk alone, which is the usual case: a shared subsystem this item tripped over, fixable from a fresh checkout of the trunk.
- **`--before`**: the new item waits on the item split. The scaffold's own `deps:` line names the original, which gains no dep and may merge first; whatever already blocked it still does, and `split` says so. Use it when the new item's fix reaches code only the original's branch has written, such as call sites of a function the original added.

Choose wrong and the two deadlock. The new item runs from the trunk, so it cannot change code that exists only on the original's branch, and the original cannot merge until the new item does. So drover reads the new item's citations against the trunk and against the original's branch: every path cited with a line part (the same citations `stale_citations` reads), and every backticked name that looks like a function or type, found with a whole-word `git grep` over those cited files on each ref. The names are a heuristic: a definition in a file the body never cites is missed, and a word spelled like a name can match.

Each citation only the branch has is reported as `B cites X, which only A's branch has; with --after these items deadlock`:

| Where                 | What happens                                                                                                                        |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `split` under --after | A warning on stderr. The scaffold is still written, because its proposal is still being edited.                                     |
| `accept` of the split | A refusal, and nothing is accepted. `drover accept <id> --yes` overrides it, and only it: never the stop marker or the actor check. |
| `check`               | A `BRANCH CYCLE` finding for any unmerged dependency, split or not, so a pair written before this check existed is still found.     |

Every one of the three prints the same remedy, naming the ids: `drop B2 from the deps line of 1, and add 1 to B2's`. That is a hand edit of two `deps:` lines. Shelving the new item and splitting again is not a remedy, because `shelve` refuses an id another item's `deps:` line names, and under `--after` the original names it.

The scaffold records the direction in a line under the stop marker (`Split from 1 with --after: 1 waits on this item.`), and `drover show` names it on both items. `show` takes the direction from the two `deps:` lines rather than that record, so a pair flipped by hand reads `--before` and notes that the split recorded `--after`.

### A block the branch closed is closed, not sent back

The pass that finds every finding in the block closed says so with `drover close-changes <id>`, which replaces the section with the statement that it is closed and writes no `changes` row, only the digest of the body it wrote. `-f <file>` carries what the reviewer traced, per finding, below that statement.

Both of the other two answers are wrong. Leaving the block standing sends the next run to re-verify finished work, because the prompt outranks the run's own reading of the tree: nine settled gaps arrive as nine open ones. Saying "nothing came back" with `request-changes` writes a `changes` row inside the window after the latest review stage row, and that is the row `open-pr` reads as work still outstanding, so the reviewer refuses the branch it just passed and spends a round against `[review] max_review_rounds` doing it. The split is the one `invariant` already has: the ledger records whether a review sent work back, so the command recording the opposite records nothing there.

`close-changes` refuses a body with no such section, where there is nothing to discharge, and refuses while the latest review's own `changes` row stands, because a block a reviewer wrote this pass is current whatever a later command says about it. One disposition per pass: send work back, or close it.

While the block is open, the prompt `drover review` prints ends on drover's own instruction naming both exits, whatever the project's `review.tmpl` says. A review that runs `close-changes` and then audits reads as `passed` in a drive and merges; one that audits and does neither reads as `findings_open` and parks (see [`drive --json`](automation.md#drive---json)).

### A property outlives the round that found it

A finding names the case that broke. What the case taught you is a property, and the body has a third section for it: `drover invariant <id> -f <file>` appends under `## Invariants` and replaces nothing, ever.

The two other sections both lose it. Requirements are frozen once accepted, and `request-changes` refuses a write that would change their count. The findings section is replaced every round. So a property the third pass established was written as a finding, closed by a fix aimed at the case that exposed it, and deleted by the fourth pass's send-back. The fifth run meets the same guarantee as a fresh surprise: five fixes that each pass the verify command, each close one case, and each open the one beside it.

Write the property, not the defect. "Replacing an object preserves whatever mode an operator set on it" belongs here; "the fix dropped the mode to 0644" is the finding that taught it and belongs in the findings section. Both prompts render this section above the findings, so every later pass reads what must hold before it reads what broke.

An unmet requirement does not lower the audit level either way. `verified` records how hard the reviewer looked, not how the item scored, so tracing all four and returning one is still a full trace.

The prose lives in `Destinations()` in `internal/prompts`, which `review.tmpl` and `agents.tmpl` both render; `internal/cmd/doctrine_test.go` fails when this table and that value disagree in either direction.

Put the project-specific half of your review doctrine in `[prompts] review` rather than in a document an agent has to remember to consult.

### Three stages, three runners

Drafting, implementing and reviewing are separate acts, each with its own command and its own runner, so no agent audits or executes what it wrote:

```bash
drover draft B1 --runner claude --headless    # draft the requirements
drover accept B1                              # a human accepts the list
drover run B1 --runner codex --headless       # implement
drover review B1 --runner gemini --headless   # review, before the merge
drover audit B1 verified --code-by agent "R1-R4 traced to shipped code"
```

`B1` is an intaken item, because `draft` only ever applies to a scaffold. A hand-written item never carried the marker and is already somebody's contract.

| Stage    | Handed                                                                                                                                       | Records                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `draft`  | The issue, the principles, the numbered-list contract, the siblings, the body path.                                                          | Nothing. The agent writes the body.                                                                                    |
| `run`    | The prompt: the item, its deps, the resolved specs and guides, the verify command, the inherited follow-ups.                                 | A `stage: implement` row naming the runner, before the launch. A headless run also leaves a log and a captured report. |
| `review` | The contract, each dependency's audit level, the open follow-ups, the dependencies' saved reports, the run's own report, the verify command. | A `stage: review` row naming the runner and the session id. The level itself comes from `audit`, separately.           |

Each runner comes from its own key: `[run] draft_runner`, `[run] default_runner`, `[run] review_runner`. Only the middle one is also readable from the item's `runner` frontmatter, and that is deliberate. That key names who implements the item; drafting under it would let the agent that drafted a contract be chosen to execute it, and reviewing under it would have the implementer certify its own work, which `audit` refuses to record as `verified`. Leave a stage's key unset and that command prints its prompt instead of launching, which is what a pipe gets.

**The one step no agent performs** is the second row: a human reads the drafted list and runs `drover accept`. Until they do, `run` refuses the item and so does `--dry-run`. That is the boundary drover does not automate, and `accept` is a command rather than a flag precisely so that no runner's argv can carry it.

Nor can its environment. drover exports `DROVER_ACTOR=agent` into every session it launches, and `accept` refuses there, exit 3. So do `unaccept`, `amend`, `touch`, `signoff`, `intake`, `finish`, `mark-merged`, `unmark-merged`, `open-pr`, `drive` and `supervise`, each for a reason `drover --help` prints beside it, and a launched session cannot launch another. The same values go into `.drover/session` in the tree the session works in, so a session another program opens there is sealed too. `env -u DROVER_ACTOR` defeats all of it, and `DROVER_ACTOR=operator` is how a person steering from inside a sealed tree says so: it stops a command being run because it appeared in a code block, not one being run on purpose. `audit` is deliberately exempt. A review session is independent of the implement session, drover launched both, and the actor variable cannot tell them apart. The session id in `DROVER_SESSION`, recorded on each stage row, can, and that is what `--code-by agent` resolves against.

`review` does not switch branches. It runs before the merge, on the branch `run` left the tree on.

A `--headless` review streams to `<id>-<slug>-review.<timestamp>.jsonl` in the same log directory as the run, and a `--headless` draft to `<id>-<slug>-draft.<timestamp>.jsonl`. Both are kept apart from `<id>-<slug>.<timestamp>.jsonl` so `drover report` still reads what the run reported rather than what a session about the item said.

Afterwards the ledger holds one stage row per launch, one completion row from `finish`, and one audit row per level naming its `--code-by`. `drover show 2` prints the two runners under its audit history:

```text
  audit history
    implemented by  claude
    reviewed by     codex
    2026-08-26  verified     code by agent
```

drover does not enforce that the two runners differ. An independent audit warns when the record names one runner for both stages, names both, records the row and exits 0: deciding that two sessions of one runner are one operator is not a judgment it can make, and a refusal there would teach people to record a runner they did not use.

An item run with `--runner codex` while its frontmatter pins `runner: claude` carries both in its saved report, labeled apart: `- runner (requested): claude` above `- Runner codex`. The frontmatter says who the item asked for. Only the stage row says who answered.

The run's own report:

```bash
drover report 2           # what the last run reported
drover report 2 --save    # write it beside a checklist of the numbered requirements
```

A run's own report is unverified. It misleads by omission.

What the review finds gets **filed with its owner, not absorbed**. A defect owned by a sibling or an unbuilt follow-on is filed against _that_ item:

| What you found                                   | Where it goes                                                                                                                                                                            |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| follow-up carried forward                        | issue: `<follow_up_label>` + `<severity_prefix>critical\|high\|medium\|low`, a sub-issue of the owning item's issue, or an `Owner: <owning id>` line in the body when that item has none |
| a reproducible defect worth its own unit of work | issue with your tracker's bug label, then `drover intake <n>`                                                                                                                            |

Ownership travels one hop. A prompt carries the issues owned by the item itself plus those owned by each id on its own `deps:` line, and nothing further. Attribute to the item that **owns** the gap, not the one that noticed it, then confirm the run you expect to close it lists that owner as a direct dependency.

## 6. Audit

```bash
drover audit 2 verified --code-by agent "R1-R7 traced to shipped code; R4 unmet, filed #31"
```

The review pass ends on this row, before the merge. Merging says an item shipped and says nothing about whether anyone checked it, so without a level an item backed only by its agent's own report and one whose every requirement was re-confirmed look identical.

| Level      | Means                                                          |
| ---------- | -------------------------------------------------------------- |
| `claimed`  | The agent's own report was captured. Nobody else looked.       |
| `reviewed` | A review pass ran against the code.                            |
| `verified` | **Every** numbered requirement confirmed against shipped code. |

`--code-by <who>` is required and is a separate axis: who wrote the code. `<who>` is `agent` for a session drover launched, a person's name, or a comma list of them (`agent,sam`). `agent` resolves per session against the item's implement stage rows, so a review session drover launched is independent of the implement session; any other entry is compared as written against `--reviewer`. **`verified` needs an auditor who differs from every entry**, and drover refuses at the write path with exit 3, naming the entry that matched. A `FIX-IN-REVIEW` correction no numbered requirement traces to is not code the level certifies, so it does not make the session an author of it. Anything else does.

The ladder measures the reviewer, not the item. `verified` means every requirement was traced, including ones that turned out unmet; the gap goes back to the branch while the branch is open and becomes a follow-up filed against its owner once it is not, and either way the level stands. Drop to `reviewed` when you could not trace a requirement, never when you traced it and disliked the answer.

The most recent row wins, not the strongest. Correcting downward is the point, and `audit` prints `(DOWNGRADED from …)` so it is never silent.

## 7. Open the pull request, merge, then finish

`drover open-pr <id>` opens the pull request, and nothing else does. It runs `[drive] merge_gate` and reads back what the last review concluded, so an open pull request means both have passed rather than that somebody finished writing. A branch whose review sent work back gets none: the fix belongs on the branch, and a document saying the branch may merge would be saying it about work that came back. `--force` opens one on your own judgment, which is what a project declaring no merge gate has instead.

Then merge it however your project merges pull requests: by hand, with `gh pr merge`, or a button in a browser. drover has no opinion about how a merge happens, and `finish` records one it did not perform.

The one place drover executes a merge is `drover drive`, which merges what it drives, and even there it asks rather than decides: `[drive] merge_gate` is a command the project declares, drover reads its exit status, and the merge itself goes through the forge's own `--auto` where the trunk has required checks, so branch protection and those checks are the authority. What drover decides is _when_, never _how_, and `--no-merge` is how an operator drives the queue and keeps the _when_ for themselves.

Before each merge gate run reads a pushed head, drive waits for its checks and classifies every red one, because a merge gate that sees a red check sends it back to the item whoever caused it. A failure the forge's infrastructure caused (a rate limit, a runner out of disk, a job that ran no step, a canceled run) is rerun with `gh run rerun --failed`, up to `[forge] reruns` times per head; one the trunk fails too parks the item `trunk_red` without sending anything back and stops the drive; anything else goes to the merge gate as before. A head no workflow reported on is named as such rather than read as red. See [`[forge]`](configuration.md#forge).

### Requirements only the operator can meet

A numbered requirement whose text opens with `[operator]` is a hold only a person can release, beside `protected_paths`. No session can discharge one, so drover keeps the loop from spending rounds on it:

- **The run** reads it under a separate heading telling it not to attempt it.
- **The review** cannot send the item back over it. `drover request-changes` reads each heading in the evidence (outside code fences) as one finding and traces it through the `R<n>` it names; when every finding traces to an `[operator]` requirement, it refuses with exit 3, names them, and writes nothing. Anything else goes through as before: a heading naming a session requirement, one carrying any `BRANCH-` token, and one naming no requirement at all. The only headings it sets aside are those opening with `Settled`, `Holds`, `Held`, `Closed` or `Resolved`, and the refusal lists each one it read that way.
- **The pull request** opens as usual, with each one lacking a signoff listed as pending in `drover show` and `drover status`.
- **The merge** holds. drive parks the item `awaiting_signoff` with the pull request open and emits `needs_operator` naming each requirement (the `signoff` action, at `operator` and no higher).

The operator meets it and records what they checked:

```bash
drover signoff F5 6 "ssh dev-guest-1 'systemctl is-active webapp' printed active"
```

`signoff` appends one row to the ledger and is sealed: it refuses inside any session drover launched, supervisor included, for the reason `accept` does. Once every `[operator]` requirement carries a row, merge the pull request and `drover finish` it, or let the next drive reach the merge and go through.

### A check after finish

`[finish] post_check` is an argv run after every finish, by `drover finish` or by a drive finishing what it merged, while [`[supervise] post_finish_check`](configuration.md#supervise) is above `off` (its default). It runs from the project root with `DROVER_ITEM`, `DROVER_PR` (empty where no pull request was recorded) and `DROVER_SHA` (origin's trunk after the merge) in its environment. This is where a project puts a check somebody would otherwise run by hand: `ssh dev ... psql` against `schema_migrations`, or build, deploy and exercise on a dev server.

| Exit     | drover does                                                                                                    |
| -------- | -------------------------------------------------------------------------------------------------------------- |
| `0`      | appends a `post_finish` ledger row carrying the first line of its output                                       |
| non-zero | files a follow-up owned by the item, prints where, and emits `needs_operator` (the `post_finish_check` action) |

Neither undoes the merge or changes finish's exit: the trunk already carries the commit, and a failed check is work somebody owes on it rather than a reason to pretend the item was never finished. Under `[register] backend = "none"`, or `--no-tracker` with the tracker backend, nothing is filed and the line says so. Outside a drive, `needs_operator` reaches each [`[notify]`](configuration.md#notify) destination routing it, and nothing else. See [`[finish]`](configuration.md#finish).

### When the forge has no auto-merge

Some forges have nothing to wait on. A private GitHub repository on the free plan cannot protect a branch, and a trunk with no required status checks has nothing an auto-merge would wait for. There the merge gate is what authorizes the merge: nothing at the forge will wait for the checks or apply branch protection, so `[drive] merge_gate` is the only authority left, and drive merges only on its exit 0.

drive does not wait for the forge to refuse `--auto` to find this out. GitHub refuses it (`enablePullRequestAutoMerge`) only while the head's checks are still running; once they have finished, the same request with nothing to wait on merges on the spot and `gh` reports success. Which path an item took then depended on whether its CI finished before its review did, and an item whose CI finished first merged without the merge gate reading its head. So where `[drive] merge_gate` is declared, drive reads the trunk's required status checks through `gh api` once per drive, and when there are none (GitHub answers 404, or 403 where the plan cannot protect the branch) or the read fails, it never asks for `--auto` and says so on stderr. Where there are some, it asks for `--auto` as before, and still falls back here if the forge refuses it, matching that refusal by its wording and never on any other error from the merge.

An `--auto` request that succeeds is checked rather than trusted: drive asks the forge whether the pull request merged. A merge the forge queued reads `ready_to_merge` and the next drive finishes it. One the forge made on the spot reads `merged_immediately` and is finished in this drive, so items that depend on it run behind it; where a merge gate is declared, it reads the merged head first with `DROVER_GATE_MERGED=1` set, so a merge gate does not call that head behind the trunk (see [`[drive]`](configuration.md#drive)), and a refusal leaves the item unfinished and stops the drive, since the trunk now carries a head the merge gate refused and nothing can unmerge it.

On the path below, drive:

1. Brings the branch level with the trunk, as below.
2. Waits for every check run on the head commit to finish, reading them through `gh api`, and classifies each red one as [`[forge]`](configuration.md#forge) describes: an infrastructure failure is rerun and waited on again, and one the trunk fails too parks `trunk_red`. Nothing else is decided there: a head with no checks after `[forge] checks_wait`, checks still running after an hour or at `--for`/`--until`, and a forge it cannot read all go to the merge gate as they stand.
3. Runs the merge gate against that head. It already passed once, before the pull request opened, and that pass is not reused, because the checks it read may still have been running.
4. On exit 0, runs `gh pr merge --<method> --match-head-commit <sha>` without `--auto`, where `<method>` is [`[git] merge_method`](configuration.md#git-merge_method), so a push arriving after the merge gate read the head refuses rather than merges.
5. Finishes the item in the same drive, as `finish` does: the ledger row, the saved report and the tracker closes. The item reports `merged_by_drover`.

A merge gate that refuses there parks the item `gate_refused` with the pull request open and the merge gate's last lines in the reason; an exit 2 also stops the drive, as it does before the pull request opens. Any other refusal from the forge (a conflict, a branch policy, a review requirement) parks `merge_failed` as before, because the forge saying no to this pull request is not the merge gate's to overrule. A finish the operator's checkout cannot take cleanly is left to the next drive's reconciliation, and the reason says so.

### Keeping each branch level with the trunk

Every merge moves the trunk under each item after it in the same drive, and a merge gate that requires an up-to-date branch would refuse every one of them. So before the merge gate reads a branch, drive fetches origin's trunk and, if the branch does not already contain it, merges it into the branch, pushes, and waits for the new head's checks. The merge is a merge rather than a rebase so the push is a fast-forward: nothing is ever force-pushed. A conflict aborts the merge and parks the item `trunk_conflict` naming the files, with the branch on the remote where the last push left it.

The merge makes a tree no session tested, and a forge whose CI runs only on pull requests and on the trunk registers no checks for a push to an item branch, so waiting on checks proves nothing about it. Two items that each add a file the other makes invalid merge without a textual conflict and fail together. So after the merge and before the push, drive runs the item's own verify command on the merged tree, in the item's worktree: the verify command `SelectGate` picks for the item's frontmatter, run as `sh -c` with `DROVER_ROOT` set, as the item's run was told to run it. stderr names the verify command and the two trunk commits before it runs, and its exit status and duration after.

- **It passes:** the push and the checks follow, as above.
- **It fails:** drive runs the same verify command once on origin's trunk alone, in a temporary worktree detached at the merged commit, given `[drive] worktree_links` and `[drive] worktree_setup` like an item's and removed afterwards. The answer is kept for the rest of the drive by trunk commit and verify command, so a second item updated to the same commit does not pay for it again.
  - **The trunk passes alone:** the failure comes from the branch meeting what the trunk gained. drive pushes the merge commit, so the next run starts from the tree that failed, and sends the item back with the trunk's range (up to 20 commit subjects) and the verify command's output. The item parks `gate_refused` with no pull request opened, and the round counts against `[review] max_review_rounds`; at the cap, `request-changes` refuses and the park reason says so.
  - **The trunk fails alone:** no item can fix it. The item parks `trunk_red`, nothing is sent back, the merge is undone and not pushed, and the drive stops.
  - **The trunk's worktree will not set up:** the item parks `gate_unrunnable`, and the merge is undone and not pushed.
- **No verify command applies:** drive says so on stderr and carries on as if it had passed. A project with no `[verify]` has chosen no verify command.

An interrupt during either run parks the item `interrupted`, undoes the merge and keeps the item's worktree as the verify command left it. A branch already level with the trunk runs none of this: its own sessions ran its verify command. Where the forge has no auto-merge, the branch update before the merge does the same, with the pull request already open: a failure parks `gate_refused` with the pull request left open and unmerged, so `--match-head-commit` never sees a head the verify command failed. The row's `update_branch` field records what happened; see [`drover drive --json`](automation.md).

origin's trunk is the reference rather than the local one, because the local trunk can carry the operator's unpushed commits and merging them into an item's branch would publish them under that item. A project with no `[drive] merge_gate` gets no branch update, since nothing would read the result. One with a merge gate gets it under `--no-merge` as well, the `[verify]` rerun included, since the merge gate still reads the branch and an item left on its branch should be green on the tree it was left with.

What the branch changed is measured from where it left the trunk, with origin's trunk counted alongside the local one. The branch update moves origin's trunk into the branch and leaves the local trunk where it was, so a diff against the local trunk alone would charge the item with every file origin gained since: a `protected_paths` hold would park it for a `Makefile` it never touched. The `protected_paths` hold, the review's scope count and its recent-commits list all read from that base.

### Updating the branch at dispatch

The branch update above runs after the review, so without another one a branch a sibling's merge left behind would be implemented and reviewed on the stale tree first, then conflict, then park for a person. So before each implement session, and before a `--review-only` review, drive merges origin's trunk into the item's branch in its worktree, while `[supervise] resolve_conflict` is anything but `off`. A lost review's rerun and a drive resumed at the review boundary skip it, because each is about the head it already had.

- **A branch already holding the trunk** merges nothing.
- **A worktree holding uncommitted work or an unconcluded merge** is never merged into, since the merge would refuse and the abort after it would discard the work. When the branch is level, the round goes on as it would without the branch update; otherwise, and always when `MERGE_HEAD` is present, drive merges nothing and the item parks `uncommitted_work`, its worktree as it stood.
- **A clean merge** is committed and the session starts on it. Nothing is pushed until the session's own push.
- **A conflict at `auto`** is left in the worktree, conflict markers and `MERGE_HEAD` in place, and becomes the implementing session's first task. Its prompt names the trunk commit merged, each conflicted file, and for each file the items whose trunk commits since the branch's merge base touched it, read from the ledger's head and completion rows, with their numbered requirements. The session is told to resolve every conflict keeping both sides, to commit the merge before any other work, and to name in its report what each side kept. The review of that round gets the same list and reads every conflicted hunk against both parents: a resolution that drops a behavior one of those items requires is a `BRANCH-REGRESSION`. The review reads that list from the ledger and the branch, not from the drive that found the conflict, so a lost review's rerun, a re-drive after a drive stopped before the review, and a resumed drive each get it, until a review of the item concludes. A `--review-only` round that conflicts becomes an implement round, since the review has nothing to read until the merge is resolved. A session that ends with the merge unconcluded parks `uncommitted_work`.
- **The bound at `auto`.** One conflict round per item per drive: a second conflict parks `trunk_conflict` with a `needs_operator` naming the files and the worktree. A conflict round counts against `[review] max_review_rounds`, through a ledger `changes` row, so an item already at the cap parks the same way instead of starting one.
- **A conflict at `supervisor` or `operator`** aborts the merge, launches nothing and parks `trunk_conflict`, with a `needs_supervisor` or `needs_operator` naming the branch, the files and the worktree, which is left standing for whoever resolves it.

Each dispatch branch update emits an `update_branch` stage event with a `result`; see [`drive --events`](automation.md#drive---events).

### Updating the other open pull requests

A merge moves the trunk under every pull request drive left open as well: one waiting at the forge for its checks or its queued merge, and one held for the operator by `protected_paths`. Those items are past their turn at the trunk stage, so nothing above reaches them again. After each merge drive makes or finishes, reconciliation's included, and while `[supervise] update_open_prs` is `auto`, drive walks every item whose ledger records a pull request on the branch the item renders today, that is not merged, and that the forge still reports open. One at a time, inside the trunk stage the merge already holds, it merges origin's trunk into each one's branch in the item's worktree, runs the item's verify command on the merged tree and pushes, never forced: the same steps as the item's own branch update, without the wait for checks. The item stays where it was.

- **A branch already holding the trunk** is left alone.
- **A local branch that disagrees with origin's**, a worktree holding uncommitted work, or a worktree another drive holds is left alone and named on stderr.
- **A verify command that fails** while the trunk alone passes pushes the merge and sends the item back, as above. A trunk that fails alone pushes nothing and stops the drive.
- **A conflict**, found with `git merge-tree` before anything is checked out, merges and pushes nothing and goes to whoever `[supervise] resolve_conflict` names: at `supervisor` a `needs_supervisor` event, at `operator` a `needs_operator` event, each naming the branch, the files and the worktree left standing to resolve in; at `off` the item parks `trunk_conflict` and a clean worktree is removed. At `auto` nothing parks and no session starts, because the branch update runs inside the trunk stage: the clean worktree is removed and the item's next dispatch, in this drive under `--follow` or in the next one, takes the conflict to its session as [Updating the branch at dispatch](#updating-the-branch-at-dispatch) describes.

`drover drive --update-branches` does the same over every open drive pull request and exits, for a merge made outside drive. It launches nothing and takes no ids. `update_open_prs` at `supervisor` or `operator` emits one hand-off per merge instead of updating the branch.

`[drive] merge_gate` exiting `4` says the head is behind the trunk and nothing else was asked. While `[supervise] retry_moved_trunk` is `auto`, drive updates the branch as above and runs the merge gate once more, before the pull request opens and at the merge alike. A second `4` parks `gate_refused` with nothing sent back; at any other policy the first `4` parks it, after the hand-off. drive does not need the `4` to find this out: before it accepts an exit `1` it fetches the trunk and tests whether `origin/<trunk>` is an ancestor of the head, and a head that does not hold it is handled as a `4`, with the reason saying the merge gate exited 1 on a head behind the trunk. It also fetches just before each merge gate run and updates a branch the trunk moved past while its checks settled; that update counts as the one retry, so a trunk that moves on every attempt parks rather than loops, and a fetch that finds it moved once the retry is spent parks without running the merge gate. At any policy other than `auto` a behind answer still parks with the hand-off, whether or not that update already ran. Exit `4` is the faster path, not the only one.

### Following what a finish unblocks

`drover drive --follow` re-reads the ready queue after each finish and appends, in queue order, the items that list the finished item in `deps` and that the drive has not already listed. A chain of dependent items then runs in one drive, bounded by `--max-items`, `--for` and `--until` as any other queue is. Only a finish in the same drive counts, which in practice means a `merged_by_drover` item: a merge the forge queued completes after drive has moved on, so nothing is unblocked until the next drive's reconciliation records it. Without `--follow` the list drive started with is the list it walks.

```bash
drover finish                    # the checked-out branch's item
drover finish 2                  # by id
drover finish 2 --delete-branch  # and drop the local branch once the trunk has it
drover finish 2 --no-tracker     # record the merge, close nothing
```

`--no-tracker` records the merge and writes to no issue. The `closes` numbers whose dispositions are still unapplied are named, with `drover finish <id>` to apply them later; nothing is lost, because the disposition rows are what the tracker write reads and they stay where they were. `drover drive`'s reconciliation finishes under this flag always: closing an issue is visible to whoever filed it, and adopting a merge some earlier drive left behind is recovery rather than a finish anyone asked for. The one finish drive makes with the tracker included is a merge it performed itself on the merge gate's word, moments earlier, which is the finish an operator would have typed next.

`--delete-branch` runs last, after the ledger row, the saved report and the return to the trunk. It is a safe delete: git refuses a branch whose commits are nowhere else, and `finish` reports `branch kept` with the reason rather than failing, so an item finished under `--force` on an authority that turned out to be wrong still has its work on disk.

`finish` appends the completion row, saves the report, applies whatever the reviewer recorded against the item's `closes` issues, and returns the tree to the trunk. It **refuses on a branch it cannot see merged** and exits 3, because recording a completion for work still sitting on a branch makes the ledger state something the tree does not support. Work that merged under a name the branch template does not render needs `--force`.

### Finishing while a drive is running

It finishes. A drive runs its items in worktrees of their own, so what it does in your checkout is move the trunk ref, push branches, and add and remove worktrees. A merge the forge queued mid-drive is yours to finish at once.

Three collisions remain, and `finish` handles each:

- **A session in your checkout.** `run`, `draft` and `review` work in it, so `finish` reads [the checkout lease](automation.md#the-checkout-lease) first, refuses while one of them holds it, and exits 3 before it fetches or switches anything.
- **A session in the item's worktree.** Finishing removes that tree, so `finish` refuses, exit 3, while a live session holds its lease, and names the pid and the path.
- **Two fast-forwards of the trunk.** The drive's sync before it branches an item and finish's return to the trunk take one lock, and each waits up to 30 seconds for the other rather than refusing.

The drive recording the same merge is not a collision: whichever of the two writes the completion row second finds the first one's row and writes none.

```text
drover: drover run B1 (pid 41234) is holding this checkout since 2026-09-17T09:12:03Z, on b1-first.
Two commands in one tree is how a branch switch lands under a live session.
Wait for it to finish, or stop it: kill 41234
```

Those are the two answers, and there is no third: no `--force` here, because nothing is stale for one to rescue, and no `--wait`, because the holder runs in hours and the merge you were finishing is still merged when it finishes.

`drover status` reports the same holder and refuses nothing. A second checkout of the same repository (`git worktree add`) has its own lease and its own answer, so two trees never block each other; that is also why each item a drive works on holds the lease of its own worktree, and the operator's checkout is never the tree a session is editing.

`finish` removes the item's worktree, if a drive left one, before it deletes the branch. git refuses to remove a worktree holding uncommitted work, and `finish` leaves that one, and its branch, where they are.

### What "cannot see merged" means

The forge answers first, then git ancestry:

1. [`[git] merge_check`](configuration.md#git-merge_check), the command a project on GitLab, Gitea or a self-hosted remote declares. Exit 0 is merged.
2. `gh` when no `merge_check` is set: a merged pull request for the branch.
3. Git ancestry, whenever that first answer was no, whether because `gh` was unreachable or because it looked and found nothing.

Ancestry runs last and never alone. A squash or rebase merge replaces the branch's commits, so `git branch --merged` reports a merged branch unmerged forever; its no proves nothing. Its yes catches a branch merged by direct push to the trunk, where there is no pull request to find. The refusal and the ✓ both name which authority answered, so a wrong answer points at the source that produced it rather than at `finish`.

Its yes has one false positive, and the ledger is what closes it. A branch nobody committed to sits at or behind the trunk, which `git branch --merged` lists the same way it lists one whose work went in, so a run that ended without committing reads as merged. drover records the branch and its head before every push, only when that head is off the trunk (see [the ledger](concepts.md#the-ledger)), and an ancestry yes stands only for a branch one of those rows vouches for and that still holds the recorded commit. Without one the answer is no, and the source string says the branch never started. Work committed outside any drover launch reads that way too, which costs `--force` rather than a completion recorded for code that is not on the trunk.

That leaves two refusals, and they are not the same. A branch name this project's `[run] branch` template does not render is benign, and `--force` is the answer. A branch git ancestry calls merged that no row shows carrying a commit is the other one, and `finish` says so in its own words: the work may not be on the trunk at all, so `--force` records it on your reading rather than drover's, and a run that died before its first commit wants re-running instead. Reconciliation names such an item and leaves it alone rather than attempting a finish that would refuse.

Without `merge_check`, a squash-merging forge that is not GitHub leaves ancestry as the only authority, and every item then needs `--force`. `--force` is for the branch that merged under a name the template does not render; a refusal overridden every time has stopped protecting anything.

### Closing the issues the item resolves

An item's `closes` key names the tracker issues its work is meant to resolve. `finish` is the only place drover writes to them, and it writes only what a reviewer already recorded:

```text
drover resolve 2 47 fixed --code-by agent "traced to the handler"
drover resolve 2 100 partial --code-by agent "the migration is item 3"
drover finish 2
  ✓ #47 closed
  ✓ #100 commented (partial), left open
  ! #103: no disposition recorded — left open
```

An issue nobody judged is named and skipped, never guessed at. The applied rows make a second `finish` a no-op, and a tracker that cannot be reached costs the tracker write rather than the finish. `drover check` reports `NO DISPOSITION` for anything still unanswered after the merge. The full model, and why the pull request's own `Closes #N` keywords are not enough, is in [issue dispositions](concepts.md#issue-dispositions).

### What finish says about the follow-up list

A follow-up owned by an item goes orphaned the instant that item merges: every owner has merged, so no run is scheduled to carry it. `finish` is that instant, and it is the one command holding both facts at once, so it prints them:

```text
  · 7 follow-ups owned by 11 are now orphaned (worst H): nothing carries them until an item declares deps: 11 (drover follow-ups 11)
```

The severity is the worst among the orphaned rows, in the letters `drover follow-ups` and `drover unaudited` rank with, because seven `L` rows and one `H` row are different situations. What discharges them is named in the line rather than left to `follow-ups --help`: another item declaring this id on its `deps:` line inherits the rows.

A read that stopped at the page limit says so, in the words `drover check` uses for the same condition off the same index:

```text
  · the follow-up list hit the 300-issue page limit, so the orphaned count is a floor
```

That line prints on a zero too, and it is the only zero here that prints anything:

```text
  · no follow-up owned by 11 counted as orphaned, and the follow-up list hit the 300-issue page limit, so the orphaned count is a floor
```

Silence after a complete read means the merge stranded nothing. Silence after a page cut short would mean nobody counted, which is a different answer wearing the same face.

It is a line and not a refusal. Carrying a follow-up past a merge is legitimate and common, and a refusal here would make `--force` the ordinary path. Nothing `finish` learns about the follow-up list can stop it recording a merge that already happened: zero owned rows print nothing off a whole read, a project declaring no follow-up list is told nothing, and an unreachable one drops the line and says why rather than reporting zero.

`mark-merged` is the subset of `finish`: the ledger row alone, for a merge recorded by hand. Running it first does not block `finish`, which skips the row it finds and does the other four steps.

Both take `--runner` to name who to credit, the same flag `run`, `draft` and `review` take, and checked differently here. Those three refuse a name `[runners]` does not declare, because the value picks the executable to launch. `finish` and `mark-merged` take it as given and only warn, because an agent that ran the item outside drover has no `[runners]` entry to match. The warning names the value and the declared set, so a typo and an outside run are told apart by the operator, who is the only one who can.

A project with [`[git] enabled = false`](configuration.md#git) has no merge to verify, so `finish` refuses and exits 3 rather than asking git a question with no answer. `drover mark-merged <id>` then `drover audit <id> <level> --code-by <who>` is the whole of it there, and the refusal names both.

A withdrawal is an append, never a rewrite:

```bash
drover unmark-merged 2    # appends {"merged":false}, keeping the audit history intact
```

`finish` deliberately does not audit. Finishing is mechanical; auditing is judgment, and a command that did both would let the thing that merged the work also certify it.

## Retiring an item nobody will do

drover has no close state for an item that never ran. `finish` and `mark-merged` write a completion row and `unmark-merged` takes it back, and all three describe work that happened. An item written, reviewed and deliberately not going to be done is a fourth thing, and recording it as complete would be a false entry in an append-only ledger.

The mechanism is a directory. Name one in `[paths] shelf` and move the body into it:

```bash
drover shelve F3
drover unshelve F3                # if it turns out to be worth doing after all
```

Moving the file costs nothing, keeps the text, keeps the id spent, and invents no frontmatter key every command would then have to act on. The filename crosses unchanged, so the id and the slug survive the round trip and the branch the item would run on is the same one it always had.

`shelve` refuses three states rather than letting `check` report them afterwards. A project with no `[paths] shelf` is told which key to set. An id that would end up holding a body in both directories is the `SHELF CLASH` state below, and `unshelve` refuses the mirror image of it. An id another item still declares on its `deps:` line is refused with those dependents named, because shelving it would trade one retired item for a `MISSING DEP` finding against each of them.

What it does not refuse is a body that will not parse. Retiring one is the case the verb exists for, so `shelve` moves the file by path, the way `unshelve` already moves a body nothing has parsed. An id with no file behind it is still refused: there is nothing to move.

The shelf is read for ids and never for content. `intake` allocates above the highest id in either directory, so a namespace whose only bodies are shelved does not start reissuing them, and `check` reports `SHELF CLASH` if one id ends up in both places, which is the state where reviving a body would silently take over a live item's identity. Nothing else consults it. A shelved item does not appear in `status` under any flag and does not move a count, because a backlog that lists work nobody intends to do is what the shelf exists to avoid.

It is also how a completed item leaves the backlog. `check` reports `ORPHAN ROW` for a ledger id with no body, which is what stops a completion being recorded against nothing; shelve the body rather than deleting it and the row keeps something to point at. Deleting it is still a finding, because that record is gone.

A dependency on a shelved item stays a `MISSING DEP` finding. It blocks its dependent as hard as an item nobody ever wrote; `check` says which, because the fix is `drover unshelve` rather than an afternoon of writing. Reaching that state now takes a hand-run `mv`, since `shelve` names the dependents and refuses.

## Standing reports

Between items, two commands answer what to do next.

```bash
drover unaudited     # merged items nobody inspected, worst blast radius first
drover follow-ups    # the open follow-up list, all of it
drover follow-ups 2  # what one item owns
```

`unaudited` ranks by how many items transitively depend on the uninspected one, because that is how much rework a defect there would cost. It is the one report that walks the graph transitively; prompts deliberately do not.

## Without `gh`

Everything local stays exact. Completion, audit levels and the dependency graph are local reads and need no network.

| Command                 | Without `gh`                                                                                               |
| ----------------------- | ---------------------------------------------------------------------------------------------------------- |
| `status`                | Full table; the follow-up counter is omitted.                                                              |
| `unaudited`             | Full ranking; the follow-ups column prints `?`, never `0`.                                                 |
| `run`                   | Assembles and runs; carries no inherited-work note.                                                        |
| `finish`                | Every step, exit code unchanged; the orphaned follow-up line is dropped and a reason printed in its place. |
| `follow-ups` / `intake` | Refuse with a message.                                                                                     |

The `?` is deliberate. "Nobody filed one" and "nobody could ask" rank the same item very differently.

## Telling an agent all of this

Do not hand-write it. `drover agents` generates the instructions filled from this project's config: its paths, its verify command table, its label vocabulary, its frontmatter schema.

```bash
drover agents                       # to stdout
drover agents --write DROVER.md     # replace the marked block, touch nothing else
drover agents --pointer DROVER.md   # the stanza AGENTS.md needs to reach it
```

Set `[agents] principles` to the bar a reviewer holds the code to. It is the one thing that cannot be read off the rest of the config, and it lives there rather than as a hand-edit so that regenerating cannot lose it.

An agent that never read the generated block still has the binary. `drover --help` groups the commands in the order of this page and names the safety classes; each command's own `--help` carries a worked `Examples:` block and a `Safety:` line naming what it writes, with the paths of the project it is read in. [Automation](automation.md) describes that surface.
