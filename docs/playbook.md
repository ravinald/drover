# Playbook

[Lifecycle](lifecycle.md) is the order the commands go in. This is the four judgment calls drover leaves to you inside that order, each of which is wrong in a way that costs a run:

1. How much work one item carries.
2. Whether the run is interactive or `--headless`.
3. Who reviews it, how many rounds that is allowed to take, and what the audit level you record is worth.
4. Whether to hand the queue to `drover drive`, which merges what it drives.

drover enforces the sequence and refuses the steps that would let one actor both do the work and certify it. It has no opinion at all about these four. The fourth is the largest, because it is where the first three stop being advice: what you decided about item size, headless runs and who reviews is what stands between a session and your trunk for the rest of the night.

## 1. Size the item

One item is one implementing session's worth of work, with an outcome an independent reviewer can prove. The body reaches that session verbatim, its top-level numbered lines become the review contract, its `deps:` line decides when it may run, and its file citations decide what it may run beside. So writing an item is also choosing how drover spends sessions and schedules them. A short body helps only while it still carries what the session needs to finish; every fact it leaves out is a search the session pays for, or a round the review sends back.

### One outcome

Aim for one coherent change and three to five falsifiable requirements. Five is a writing limit rather than a parser limit: it is about where a reviewer stops holding the whole contract in their head while reading the diff. Count decisions and the ways to verify them, not lines. One requirement carrying six unrelated changes is six requirements.

Split when the parts have different acceptance criteria, need different expertise, or could merge on separate days. Keep a fix together with the regression test that proves it, and a stored-format change together with its migration and a test against representative old data.

A mechanical sweep can touch a hundred files and still be one item. A rename, a file move or a dependency bump is one scripted transformation, the build fixes it causes, and one review. Split it across several items and each one repeats the discovery and the review, and no decision gets easier. Keep new behavior out of the sweep, so the review checks that nothing was missed rather than arguing design.

Count the cost before you queue a batch. Six items are at least twelve sessions before a single correction round.

### Chains

`deps:` is for real prerequisites. A login handler that needs the session store depends on the store item; two changes that could each ship alone need no dependency because you happened to write them in that order.

The ids go on one line separated by spaces, `deps: F1 F2`. A YAML list or a comma-separated value is malformed, and `drover check` reports it. Take each id from what `drover new`, `split` or `intake` printed rather than guessing the next one. A dependent runs only once everything it names has merged, and its prompt carries each dependency plus one hop of the follow-ups they own, so a dependency nobody needed costs context as well as waiting.

Never write downstream work against an interface nobody has settled. Finish the prerequisite, or agree its contract, and then write the dependent against that evidence. Queue a prerequisite ahead of its dependent in the same drive and the dependent runs after it merges.

### Evidence

Read the code and its tests before writing. Give the session enough to skip a search or a decision, and no more:

- **Problem:** the input or action, what happens, and what should.
- **Reproduction:** a command or a small fixture, with its actual output for a bug. Label a hypothesis you have not tested as a hypothesis.
- **Code:** repository-relative paths, the symbols involved, and line citations you checked against the trunk. Spelling and case matter, and so does anything that exists only on a dependency's branch.
- **Constraints:** compatibility, ownership, failure behavior, and the behavior around the change that has to survive it.
- **Scope:** what belongs in this change, and what belongs somewhere else.

Leave the investigation history, long logs and abandoned approaches out unless one of them explains a constraint. Point at the spec section or fixture rather than pasting it. `skills` and `spec` frontmatter are only for a project that declares those keys, and only for material this item needs.

Shared rules about commits, review and reporting live in the project's templates and agent instructions. Repeat them in every body and each item gets longer, and the rules get one more copy to drift.

### Provable requirements

Write each requirement as a result someone can observe: under this condition, this behavior holds, and this evidence shows it. Name a technique only when it is a real constraint, and otherwise let the session find the smallest change that meets the contract.

| Weak requirement          | Checkable requirement                                                                                                |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| Improve retries.          | After two transient failures, the third successful attempt returns its result; a fake transport records three calls. |
| Handle errors gracefully. | A permission error returns at once with the original cause and makes no second request.                              |
| Improve performance.      | On the named fixture, one lookup makes at most one backend request; a counting fake asserts the bound.               |
| Add tests.                | A regression test reproduces the reported input and fails against the old implementation.                            |

Use distinct top-level `1. `, `2. `, `3. ` lines. drover reads them anywhere in the body, not only under a "Requirements" heading, so write background steps as bullets or they become requirements too. Keep the whole claim on the first line: the audit checklist carries that line, and the lines under it stay in the body only.

Mandatory behavior goes in the numbered list. A compatibility promise under "Notes" has no number, so no review traces it and no audit confirms it. When an operator's words prompted the item, the requirement carries their wording and placement, not only the title.

Choose tests for the risk: the reported failure, the boundary next to it, and any state or compatibility property the change could break. "Test every edge case", a coverage target and a test that restates the implementation each cost a session time and prove nothing. Name a focused command the session can iterate with; the project's verify command is still the bar it finishes against, and drover picks one verify command per item rather than composing several.

### Operator work

A headless session has to be able to finish every ordinary requirement with the files, tools and credentials it has. Settle design questions before you accept the item. "Choose a storage model with the team" is a decision nobody has made yet, not a requirement.

A requirement that needs a deploy, a live host, ssh, a dev run or a person's judgment opens with `[operator]`:

```markdown
4. [operator] On the staging deployment, confirm that an existing session remains valid after the upgrade and record the result.
```

The run is told not to attempt it, the review cannot send the item back over it, and the merge waits for `drover signoff`. The tag assigns the work to a person; it does not waive it. Left untagged, the same line costs review rounds no session can win, so `accept` refuses a requirement naming live work without the tag, and `drover check` reports one already accepted as `LIVE REQUIREMENT`. Prose after the numbered list, such as a post-merge note or a `Live check:` line, is not a requirement and is not checked. Where you can, prove the code with a local fixture and keep the live check for the operator.

### Narrow citations

Under `drive --jobs`, two items whose cited paths overlap never run at once, and a directory overlaps every file below it. Cite the files the change touches rather than the tree they sit in. Do cite a shared file the change edits: hiding it makes the two items collide at the merge instead of at dispatch.

Mark a paragraph that cites files only as background:

```markdown
<!-- drover: context -->
Read `internal/session/store.go` for the existing ownership convention. This item does not change that store.

Changes belong in `internal/auth/login.go` and its tests.
```

The marker covers the paragraph it opens, up to the next blank line, and the paths in it do not hold the item apart from anything. A numbered requirement line inside that paragraph still counts. The marker only changes scheduling; the session still reads the file and the review still checks the citation.

### A body that runs well

The following example is a fictional resolver bug. Swap in your own paths, symbols, reproduction and commands, and leave the project's stop marker where it is until the operator accepts the item.

```markdown
---
title: share cache entries for names with a trailing dot
deps:
kind: bug
---

STOP: requirements not yet written

Resolve builds its cache key from the raw name, so resolving `example.com` and `example.com.` makes two backend requests where one entry should serve both. Cache expiry and eviction already work and are outside this fix.

Evidence: `internal/resolver/resolver.go` builds the key in `Resolve` before normalizing the name. The counting backend fixture in `internal/resolver/resolver_test.go` reproduces the duplicate request.

1. In `internal/resolver/resolver.go`, `Resolve` treats a name with one terminal dot and the same name without it as the same cache key, in either lookup order.
2. An empty name returns the existing validation error without calling the backend.
3. Tests in `internal/resolver/resolver_test.go` assert one backend call for each lookup order and none for the empty name; `go test ./internal/resolver` passes.

Out of scope: cache expiry, eviction, and the `Resolve` signature.
```

The outcome is small, the evidence says where to start, and the reviewer can trace every claim. The scope line stops an unrelated redesign. It does not excuse a regression the branch introduces or a defect that blocks a requirement: both go back to the branch.

### Accepting it

Create the scaffold with `drover new` or `drover intake`, let `drover draft` propose the body, then read it before you accept it. The following assumes the scaffold is `B1` and the project declares a `claude` runner:

```bash
drover draft B1 --runner claude --headless
```

Before accepting, check that:

- It has one outcome, with no unrelated cleanup and no open design decision.
- Every mandatory behavior has its own numbered requirement and a way to prove it.
- File citations match the trunk, and new files are named as new.
- Each dependency is needed, and no other item already owns this contract.
- Every ordinary requirement can finish unattended, and operator work is tagged.
- The reproduction and the focused verification command are concrete.
- Migrations and compatibility obligations are in scope where the change needs them.
- Background citations are marked, so they do not hold independent work apart.

Then accept it and look at what the session will receive:

```bash
drover accept B1
drover run B1 --dry-run
```

`accept` removes the stop marker. The dry run prints the body, the dependency context, the runner and the verify command the session will get. A clean preview means the launch will start; it says nothing about whether the requirements are right.

After acceptance, `drover check` reports structural findings that scaffolds are exempt from. A contract that has to change goes through `drover amend`, which keeps the old list and the reason. A run that misses the contract gets `drover request-changes` with the requirement number, the reproduction and the expected behavior. A property every later pass must keep goes in with `drover invariant`. A sent-back section a later pass settled is cleared with `drover close-changes`, so the next session does not redo finished work.

## 2. Interactive or headless

```bash
drover run B1              # you drive the session
drover run B1 --headless   # unattended; the event stream goes to a log
```

The flag is not a display preference. It decides whether the rest of the lifecycle has anything to read.

|                      | interactive             | `--headless`                                                    |
| -------------------- | ----------------------- | --------------------------------------------------------------- |
| Who steers           | you, mid-run            | nobody, once it starts                                          |
| Terminal             | the runner's own output | a rendered feed: assistant prose, tool calls as `·Read` markers |
| Log                  | none                    | `<stem>-<slug>.<timestamp>.jsonl` in the log directory          |
| `drover report <id>` | nothing to read         | the run's self-report and its outcome line                      |
| `drover status`      | stays `[>]` ready       | `[~]` implemented, once a success result arrives                |
| Turn count and cost  | unrecorded              | in the terminal result event                                    |
| Ledger stage row     | one, naming the runner  | the same one                                                    |

**The stage row is written before the launch in both modes**, from the resolved runner name. So `finish` and `mark-merged` can attribute the work either way, and neither needs `--runner` to do it.

**Everything else in that table is headless only**, and the consequence surprises people: an interactive session that did real work leaves `drover status` showing the item `[>]` ready, because "implemented" is defined as a log carrying a successful result event and an interactive run writes no log. The item is not lost. It is not distinguishable from one nobody has touched.

The same gap reaches the reviewer. With no log there is no self-report, and the review prompt says so:

```text
No run log for this item, so there is no self-report to check against. Review
the code on its own terms and say so in the note.
```

That is a legitimate review and a weaker one. The reviewer is checking the diff against the contract with nothing to compare the run's own account to, so a requirement the session quietly dropped shows up only if the reviewer independently notices it is missing.

### What headless needs from the runner

The headless argv has to emit a JSONL event stream on stdout. That is what `[runners]` declares:

```toml
[runners]
claude = { interactive = ["claude"], headless = ["claude", "-p", "--output-format", "stream-json", "--verbose"] }
```

A runner whose headless line writes prose instead produces a log drover cannot parse, `drover report` renders `_(no result event in the log — the run may have been interrupted)_`, and `status` never leaves ready. Nothing errors. Check a new runner's headless mode on one throwaway item before you trust it with a real one.

### What headless is not

**It is not detached.** The child inherits your stdin and your process group, so closing the terminal kills it. Use `tmux`, `screen` or your own `nohup` if the run has to outlive the session you started it from.

**It is not unrecoverable when it dies.** The event stream is teed to the log before it is rendered, so an interrupted run leaves everything the agent emitted up to the kill, and the result reader walks backwards to the last line that parses rather than reading the truncated final one. A killed run has a readable log; what it does not have is a result event, which is why it stays ready.

### Which to pick

Interactive for the first item in a new project, and for any item you are not sure is sized right. You watch it go wrong at turn three instead of reading about it afterwards, and a badly sized item announces itself early.

Headless for everything after that, and always for anything you intend to review or audit. The saved report, the `[~]` state and the reviewer's copy of what the run reported all come from that log, and there is no way to reconstruct them later.

## 3. Who reviews

```bash
drover review B1                   # print the prompt; you read it
drover review B1 --runner claude   # hand it to a session
drover review B1 --runner codex --headless
```

Review runs **before the merge**, on the branch `run` left the tree standing on, and never switches branches. A requirement it finds unmet is this item unfinished: `drover request-changes B1 -f findings.md` writes the report into the body, `drover run B1` returns to the same branch, and the review runs again.

Headless, it ends by saying what the pass amounted to, in the tokens a `drive` parks on: `Pass: silent` for one that recorded neither an audit nor a `request-changes`, with the runner's own denial named where its terminal event carried one. Nothing is recorded, so `drover open-pr` still asks the ledger. Interactive prints no token, since there is no log to read.

### What drover enforces

Five things, and they are the floor rather than the answer:

- **`review` never reads the item's `runner` frontmatter.** That key names who implements. Reviewing under it would let the agent that wrote the code audit it, which is the case the rest of this list exists for.
- **`audit` cannot record `verified` from whoever wrote the code.** When the session or `--reviewer` recording it matches a `--code-by` entry, it refuses at the write path, names the entry and exits 3. Whoever wrote the code cannot certify every requirement against it.
- **An independent audit warns** when the ledger records one runner for both stages, names both, records the row and exits 0. drover cannot tell two sessions of one runner from one session auditing itself. Only you can, so it says what it sees and lets you record what you know.
- **`drover request-changes` refuses a finding outside the contract.** A review's send-back has to name, for each finding, the numbered requirement it fails or a `BRANCH-REGRESSION`: a defect the branch's own diff introduced that would break something in use. Anything else is a note for the audit, and a round spent on it is one the loop never gets back.
- **`drover request-changes` refuses past `[review] max_review_rounds`**, once you set one. Off by default, because how many rounds a loop may spend is a question about your budget rather than about correctness.

### How many rounds

A review loop can converge without terminating. Each pass is correct on its own, the diffs get smaller, and nothing stops: an item can take eight rounds where the last four change nothing a user would meet. Reading one review at a time, the tool agrees with every one of them.

Two things make that visible. From the second pass the prompt opens the step that sorts findings with how many runs, reviews and audits the ledger holds and how big the last three commits were, so a reviewer weighing a marginal finding can see the trend rather than only its one predecessor. And `drover status` names a repeated pass beside `READY` and `implemented`, which is the number that justifies overriding one.

Set `[review] max_review_rounds` when nobody is watching the loop:

```toml
[review]
max_review_rounds = 4
```

Past it `drover request-changes` exits 3 and the item parks for you. The prompt still tells the reviewer to route every finding as on any other pass and record it with `request-changes`, and says drover will refuse the send-back, keep the findings and park the item: nothing is filed, and nothing merges with them. Whether it takes another round is your decision. Set it higher than the worst item that went round the loop and merged correctly.

The escape is `drover request-changes --over-limit`, and it is yours rather than the reviewer's: it refuses inside a session drover launched, because a refusal that names its own override is an instruction and the agent is the party being bounded. If you find yourself passing it, raise the number instead.

### Same runner for both stages

`drover run B1 --runner claude` then `drover review B1 --runner claude`.

**For it:** nothing extra to install, one vendor to authenticate, one bill. A fresh session has no memory of the reasoning that produced the diff, so it re-derives intent from the contract and the code. That is enough to catch the requirement quietly dropped at turn forty, which the implementing session will never catch, because that session believes it did the work.

**Against it:** the blind spots correlate. If the model misread an API contract while implementing, it misreads it the same way while reviewing, and the review confirms the bug with a clean report. Independence is what the audit ladder prices, and two sessions of one model are less independent than the level implies. Recording an independent level there is an assertion the ledger cannot support; drover warns and writes what you tell it, so the record is as honest as you are.

**Still worth running:** this is worth much more than nothing. The failure it does not catch is the one the model is systematically wrong about. The failures it does catch (dropped requirements, an untested branch, a verify command nobody re-ran) are most of them.

### A different runner for the review

`drover run B1 --runner claude` then `drover review B1 --runner codex`.

**For it:** the blind spots stop correlating. Different training, different tool-use habits, different failure modes, so a misreading that survived implementation has to survive a second one that did not produce it. The ledger records both, `drover show B1` prints them under the audit history, and an independent level is an assertion the record supports:

```text
  audit history
    implemented by  claude
    reviewed by     codex
    2026-08-26  verified     code by agent
```

**Against it:** a second vendor, a second authentication, a second bill. The reviewer also reads cold, so it will occasionally flag something the implementer decided on purpose. The prompt narrows that by carrying the numbered contract, each dependency's audit level, the open follow-ups, each dependency's saved report and the run's own self-report, but it does not eliminate it. And a runner that writes good code is not automatically good at reading a diff against a contract; review is a different skill, and the only way to find out is to run one and read what it produced.

**What it still misses:** two agents are not two people. Both may carry the same misconception about a library from the same training data, so independence here is a gradient rather than a switch. `verified` records that someone traced every requirement to shipped code. It does not record that they were right.

### The strongest form

A person records the last rung, and `--reviewer` names them so the record does not read as an anonymous session:

```bash
drover audit B1 verified --code-by agent --reviewer sam "R1-R4 traced; R3 unmet, sent back"
```

`--reviewer` is also what a named `--code-by` entry is compared against: code by `agent,sam` recorded `verified` with `--reviewer sam` exits 3, and with `--reviewer dana` records. Omitting it records nobody, never a default, because a name drover chose is a reviewer nobody can go and ask.

### Decide once, in config

```toml
[run]
default_runner = "claude"
review_runner  = "codex"
```

`review_runner` has no default. Unset, `drover review` prints the prompt and launches nothing, which is what a pipe gets and what a human reviewer wants. Naming a second binary there is what keeps the level honest without anyone having to remember a flag. `draft_runner` is the same key for the drafting stage, and only `default_runner` is also readable from an item's own frontmatter.

### The whole split, end to end

```bash
drover intake 12                             # scaffold, marker planted
drover draft B1 --runner claude --headless   # draft the requirements
drover accept B1                             # you read the list and accept it
drover run B1 --runner claude --headless     # implement
drover review B1 --runner codex --headless   # review, before the merge
drover audit B1 verified --code-by agent "R1-R4 traced to shipped code"
drover finish B1                             # after merging: ledger, report, trunk
```

Four actors, three of them agents, and the one step none of them performs is the third line.

## 4. Whether to drive

```bash
drover drive --dry-run             # the queue, the runners, the bounds; nothing launched
drover drive --for 8h              # implement, review, merge, until the clock runs out
drover drive --for 8h --no-merge   # the same night, with every merge held
drover drive B1 B4                 # these two, in this order
```

**Running `drive` is you saying the code may merge.** drover merges an item that comes through green and reviewed, and nobody reads it first. That is the decision, and it is not softened by the fact that you will read the pull requests in the morning: by then they are commits on your trunk, and the next item in the queue was implemented against them.

What stands between a session and the trunk is `[drive] merge_gate` and the review. Both have to speak:

- **The merge gate has to exit 0.** Exit 1 sends its own output back to the branch through `request-changes` and the item runs again. Exit 4 says the head is only behind the trunk, so drive brings the branch level and asks once more. Anything else stops the whole drive, because a trunk that did not receive this merge is the trunk every later item builds on. A project that declares no merge gate is refused before the first launch rather than per item, since nothing there could say green; `--no-merge` is the exception.
- **The review has to have recorded a conclusion.** A review that wrote neither an audit nor a `request-changes` parks the item, and so does one that died, one that left a log drover cannot read, one at `[review] max_review_rounds` and one whose last two rounds sent back the same evidence. A parked item is never merged. drover cannot tell a pass that found nothing from a pass that found something and did not write it down, and the guess that merges is the one it will not make.
- **`[drive] protected_paths` is left for a human.** A branch that edits the merge gate, a workflow or `.drover.toml` changes what every later item that night is measured by, and that change would be reviewed by the loop it just altered.
- **The pull request is drover's to open, not a session's.** It opens one only where the merge gate and the review have both spoken, so an open pull request in the morning means both did. Work that came back or parked leaves a pushed branch and nothing else, and reading the forge is enough to tell the two apart.

The merge itself is `gh pr merge --match-head-commit` on the commit the merge gate passed, never the forge's `--auto`, so a push that lands after the merge gate read the head is refused rather than merged. drive then asks the forge whether the pull request merged and finishes the item in the same drive, so the items queued behind it run that night rather than the next. Branch protection still applies: the forge refuses a merge its rules do not allow, and the item parks `merge_failed`.

None of that is an opinion about whether your merge gate is any good. drover reads an exit status; whether the command behind it runs the tests that would catch the defect this queue is about to write is yours, and it is the question to answer before the first unattended night rather than after it. The same goes for the reviewer: an independent audit warns when one runner implemented and reviewed, and the warning is all drover can offer, because two sessions of one runner and one session auditing itself look identical from outside.

### Where each item runs

`drive` never switches your checkout. Each item gets a git worktree of its own branch under `$XDG_STATE_HOME/drover/worktrees` (`~/.local/state/drover/worktrees` by default), and its implementing session, its reviewing session and the merge gate all run there. Your checkout stays on whatever branch you left it on, with whatever you had in it.

That separation is what lets a night survive one bad item. Whatever a session leaves behind (a half-finished edit, a generated file its branch ignores and the trunk does not) stays in that item's worktree. The item parks as `uncommitted_work` with the worktree's path in its reason, the worktree is kept for you to inspect, and the drive moves to the next item, which starts in a clean worktree of its own. A worktree that ends clean is removed, and `finish` removes one left standing before it deletes the branch.

Two consequences to set up for before the first night:

- **Sessions find the project through `DROVER_ROOT`.** `drive` sets it to your checkout's root, so an agent's `drover audit` or `drover request-changes` writes the ledger and backlog you read, not a copy inside the worktree. A session standing in a different repository's project, such as a scratch one it initialized to try a command, gets that project instead: a worktree shares your checkout's common git dir and a separate repository does not, which is how drover tells the two apart.
- **A worktree holds only what git tracks.** Anything a session or your merge gate reads from outside version control has to be linked in with [`[drive] worktree_links`](configuration.md#drive). drive links only a path git would ignore, and a `.gitignore` entry with a trailing slash (`docs-internal/`) never matches the symlink, so write it without the slash. Dependencies a fresh checkout lacks (`node_modules`, say) come from `[drive] worktree_setup`. A branch you have checked out yourself cannot be checked out a second time, so its item parks as `worktree_failed` and the rest of the queue runs.

Your checkout matters to a drive in one place: reconciliation, which finishes what merged since the last drive by writing the ledger and saved reports there. A checkout holding uncommitted work skips reconciliation for that night, says so, and the queue runs anyway.

### When to hold the merge

`--no-merge` drives the whole queue and merges none of it, leaving each item green-and-reviewed on its branch. An item whose merge gate exited 0 has a pull request, because the flag holds the merge and nothing else, so most of what you read in the morning is a queue of branches somebody has already checked. Not every item reads that way: one whose review sent work back or parked has no pull request, a project declaring no merge gate gets none at all under this flag, and one parked on `protected_paths` or `blocked_on_deps` has a pull request waiting for you to merge.

Hold the merge as the exception, in one of three cases:

- The merge gate is new, and you want a night of its results before one of them decides a merge.
- The queue's items touch something a reviewer cannot certify: a migration, a wire format, anything whose blast radius outruns its diff.
- You are driving items somebody else owns.

If you find yourself reaching for it every night, the thing to fix is the merge gate or the review doctrine in `[prompts] review`, not the flag. A queue driven to green-and-reviewed and merged by hand the next morning costs you the mornings and buys nothing the merge gate was not already asked to decide.

### Bound every drive

`--for 8h` and `--until 06:00` are mutually exclusive and one of them belongs on every invocation; `--max-items` bounds the count on top of that. An unbounded drive over a queue whose reviews keep sending work back is a spend with no ceiling, which is the same failure `[review] max_review_rounds` bounds one item at a time.

`drover drive --dry-run` resolves the same queue under the same merge gate and launches nothing, so the count it prints is what the run would attempt. It is the read to take before the first night, and again after any change to `[runners]`, a template or the merge gate.

## Where to go next

- [Lifecycle](lifecycle.md): every step, and what each one refuses to do for you.
- [Concepts](concepts.md#the-audit-ladder): what each level means and why the ladder measures the reviewer rather than the item.
- [Configuration](configuration.md#runners): declaring a runner, and every key named on this page.
- [Prompt templates](prompts.md): the data a template reaches, if the built-in prompts say the wrong thing for your project.
