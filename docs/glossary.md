# Glossary

Every word drover prints, or reads back from you, with what it means today. One line per term; the pages that explain each one in full are [concepts](concepts.md), [lifecycle](lifecycle.md), [configuration](configuration.md) and [automation](automation.md), and each command's `--help` carries its own flags and examples.

## Commands

Each command, as typed after `drover`.

| Term                    | Meaning                                                                                                              |
| ----------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `accept`                | Deletes the stop marker from a scaffold, making it runnable.                                                         |
| `agents`                | Prints the agent instructions generated from this project's config; `--write` writes them into a file.               |
| `attach`                | Prints a drive's events from the start and follows them until the drive exits.                                       |
| `amend`                 | Replaces an accepted, unmerged item's numbered requirements and keeps the old list.                                  |
| `audit`                 | Appends an audit row: the audit level, who wrote the code (`--code-by`) and who looked (`--reviewer`).               |
| `check`                 | Validates the config, the backlog, the ledger and the paths between them; exits 2 on findings.                       |
| `close-changes`         | Replaces the sent-back section of an item with a closed note, writing no request-changes row.                        |
| `completion`            | Prints a shell completion script; hidden from the listing.                                                           |
| `completion bash`       | The completion script for bash.                                                                                      |
| `completion fish`       | The completion script for fish.                                                                                      |
| `completion powershell` | The completion script for PowerShell.                                                                                |
| `completion zsh`        | The completion script for zsh.                                                                                       |
| `config`                | Prints the `.drover.toml` this binary would write for the project today.                                             |
| `draft`                 | Assembles the prompt for writing a scaffold's numbered requirements.                                                 |
| `drive`                 | Walks the ready queue unattended: implement, review, update branch, merge gate and merge.                            |
| `drives`                | Lists every drive this project has run, newest first, and what each item came to.                                    |
| `finish`                | After a merge: appends the completion row, saves the report, writes to the tracker and returns to the trunk.         |
| `follow-up`             | Parent of `follow-up open` and `follow-up close`, which write to a file-backed follow-up list.                       |
| `follow-up close`       | Closes one follow-up by number.                                                                                      |
| `follow-up open`        | Files a follow-up against an owner item.                                                                             |
| `follow-ups`            | Lists the open follow-ups: what the project knows is unresolved.                                                     |
| `help`                  | Prints help for any command.                                                                                         |
| `init`                  | Writes a starting `.drover.toml`; `init --user` writes the per-user config instead.                                  |
| `init supervise`        | Walks the supervise actions by category and sets each one's policy, up to its ceiling.                               |
| `intake`                | Turns a tracker issue into a scaffolded item; `intake --list` lists issues it could take in and has not.             |
| `invariant`             | Appends a property every later pass must keep to the item's `## Invariants` section.                                 |
| `link`                  | Adds an issue number to the item's `closes` key, so `finish` answers whoever filed it.                               |
| `mark-merged`           | Appends the completion row alone, for a merge recorded by hand.                                                      |
| `new`                   | Writes a scaffolded item with no tracker issue behind it.                                                            |
| `notify`                | Sends a test event, or a sample of one `--event`, to each `[notify]` destination and prints each answer.             |
| `open-pr`               | Runs the merge gate and opens the pull request for a branch the review passed.                                       |
| `projects`              | Lists every project drover has been pointed at on this machine.                                                      |
| `report`                | Prints the run's self-report beside a requirements checklist; `--save` writes it under `[paths] reports`.            |
| `request-changes`       | Writes review findings into the item body so the next run reads them.                                                |
| `resolve`               | Records the disposition of one issue the item's `closes` key names.                                                  |
| `review`                | Assembles the reviewer's prompt and, given a runner, launches a review session.                                      |
| `run`                   | Switches to the item's branch and hands its prompt to a runner; writes ledger stage `implement`.                     |
| `schema`                | Prints the JSON Schema for `.drover.toml`, or with `--frontmatter` the project's body contract.                      |
| `shelve`                | Moves a body to the shelf without recording it merged.                                                               |
| `show`                  | Prints everything drover knows about one item.                                                                       |
| `signoff`               | Records that the operator met one `[operator]` requirement.                                                          |
| `skills`                | Reports which guides the backlog references and which are vendored.                                                  |
| `split`                 | Scaffolds a new item for work the contract never named and adds it to this item's deps.                              |
| `status`                | Prints every item, its state and its audit level.                                                                    |
| `supervise`             | Launches a supervising session that handles what `[supervise]` allows.                                               |
| `t3`                    | The bridge to a T3 Code server, through the subcommands below.                                                       |
| `t3 discover`           | Prints the T3 Code server's name, version and tool catalog.                                                          |
| `t3 login`              | Signs drover in to the T3 Code server and writes `[t3] token_file`; `--check` only reports whether it can.           |
| `t3 run`                | The runner bridge: launches one T3 Code thread with the prompt, follows it to the end and prints a log drover reads. |
| `t3 thread`             | Launches a thread (`--new`), opens one for a `needs_operator` (`--from-event`), or interrupts one (`--stop`).         
| `touch`                 | Records a hand edit to an accepted item's prose as the operator's.                                                   |
| `unaccept`              | Restores the stop marker, returning an item to a scaffold.                                                           |
| `unaudited`             | Lists merged items nobody audited, worst blast radius first.                                                         |
| `unmark-merged`         | Appends a tombstone withdrawing a completion.                                                                        |
| `unshelve`              | Brings a shelved body back into the backlog.                                                                         |
| `usage`                 | Records session token usage in the ledger, from run logs (`--backfill`) or transcripts (`--sync`).                   |
| `version`               | Prints the version, commit and build date.                                                                           |
| `watch`                 | Blocks until a drive emits one of the named events, then prints it.                                                  |

## Flags

The flags whose names carry drover vocabulary. Each command's `--help` lists the rest.

| Term                            | Meaning                                                                                                       |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `--code-by <who>`               | On `audit` and `resolve`: who wrote the code, as `agent`, a person's name, or a comma list of both.           |
| `--reviewer <name>`             | On `audit`: the person or runner who looked, stored as `reviewer`.                                            |
| `--review-runner <runner>`      | On `drive`: the runner for review sessions, overriding `[run] review_runner`.                                 |
| `--runner <runner>`             | On `run`, `review` and `drive`: the runner to launch.                                                         |
| `--link`                        | On `resolve`: adds the issue to `closes` before recording the disposition.                                    |
| `--over-limit`                  | On `request-changes`: sends work back although the item is at `[review] max_review_rounds`.                   |
| `--update-branches`             | On `drive`: merges the trunk into every open drive pull request, runs its verify command, pushes, then exits. |
| `--no-merge`                    | On `drive`: drives the queue and merges nothing, leaving each item ready to merge.                            |
| `--review-only <id>`            | On `drive`: reviews one item's branch as it stands, with no implement run.                                    |
| `--follow`                      | On `drive`: adds the ready items each finish unblocked to the queue.                                          |
| `--enqueue`                     | On `drive`: appends ids to the queue of the drive running in this checkout.                                   |
| `--detach`                      | On `drive`: starts the drive in a session of its own, logging to a file.                                      |
| `--jobs <n>`                    | On `drive`: implement and review up to n items at once.                                                       |
| `--pause`, `--resume`, `--stop` | On `drive`: hold, reload and continue, or wind down the drive running in this checkout.                       |
| `--cleanup`                     | On `drive`: lists the worktrees, branches and report stubs interrupted drives left; `--apply` removes them.   |
| `--dry-run`                     | On `run` and `drive`: resolves every step and launches nothing.                                               |
| `--headless`                    | On `run` and `review`: runs unattended, streaming events to a log.                                            |
| `--verify <command>`            | On `init`: answers the verify command question without asking.                                                |
| `--user`                        | On `init`: writes the per-user config.                                                                        |
| `--list`                        | On `intake`: lists the tracker issues drover could take in and has not.                                       |
| `--coverage`                    | On `follow-ups`: only the rows nothing is scheduled to carry: blocked, orphaned or unowned.                   |
| `--live`                        | On `follow-ups`: hides rows whose every owner has merged.                                                     |
| `--into <kind>`                 | On `split`: the namespace of the new item.                                                                    |
| `--found-in <id>`               | On `new`: the item where the finding was noticed.                                                             |
| `--sev`, `--by-sev`             | On `status` and `new`: filter or order by severity rank.                                                      |
| `--no-tracker`                  | On `status` and `finish`: asks or writes nothing at the forge.                                                |
| `--json`                        | On every read command that has it: one stable object per row, the form scripts parse.                         |
| `-I`, `--ignore-stale`          | Runs although `.drover.toml` or the generated instructions do not match the binary.                           |

## Audit levels

How hard someone checked an item, stored in the ledger and in `--json` as `level`.

| Term                      | Meaning                                                                                       |
| ------------------------- | --------------------------------------------------------------------------------------------- |
| audit level               | The `level` an audit row records: how hard someone checked, not how good the item is.         |
| `claimed`                 | Only the agent's own self-report exists.                                                      |
| `reviewed`                | A review pass ran against the code.                                                           |
| `verified`                | Every numbered requirement was traced to shipped code. Needs a reviewer who wrote none of it. |
| `UNAUDITED`               | No audit row exists for the item.                                                             |
| `DOWNGRADED`              | The new audit row records a lower level than the last one.                                    |
| `<level> (no authorship)` | An audit row that records no authorship, so who wrote the code is unknown.                    |
| `agent`                   | The reserved `--code-by` value: a session drover launched, judged per session.                |

## Item states

The state `status` prints and `--json` carries as `state`.

| Term          | Meaning                                                                               |
| ------------- | ------------------------------------------------------------------------------------- |
| `scaffold`    | The body still carries the stop marker, so no run may start.                          |
| `ready`       | Every dep merged and nothing has run.                                                 |
| `blocked`     | A dep has not merged.                                                                 |
| `running`     | A session holds the item now.                                                         |
| `implemented` | A run finished clean; covers items awaiting review, ready to merge, and parked.       |
| `merged`      | The ledger records the item merged.                                                   |
| `broken`      | The body will not parse.                                                              |
| `shelved`     | The body sits on the shelf, set aside without a merge.                                |
| `parked`      | Drive stopped the item for a reason a person reads; shown as a note beside the state. |
| `at-limit`    | The item reached `[review] max_review_rounds`, so the next request-changes refuses.   |

## Park causes and details

Why drive stopped an item (ledger and `--json` `park`), and the underlying failure (`detail`). A review result can also be the park.

| Term                      | Meaning                                                                                          |
| ------------------------- | ------------------------------------------------------------------------------------------------ |
| `gate_refused`            | The merge gate exited non-zero for this diff.                                                    |
| `gate_unrunnable`         | The merge gate, the verify command or the numbered-files check would not start.                  |
| `trunk_conflict`          | Merging the trunk into the branch conflicted.                                                    |
| `trunk_red`               | The trunk fails the verify command on its own.                                                   |
| `forge_down`              | The merge gate failed for a reason the diff did not cause, and the drive stopped.                |
| `protected_paths`         | The branch changes a `[drive] protected_paths` file, so the merge waits for a person.            |
| `infra_failure`           | A forge check failed on infrastructure rather than code.                                         |
| `stopped`                 | The drive stopped or was interrupted while this item ran.                                        |
| `interrupted`             | A signal stopped the session.                                                                    |
| `stale_citations`         | Lines the item cites are missing from the trunk.                                                 |
| `awaiting_signoff`        | The pull request waits on an `[operator]` requirement.                                           |
| `blocked_on_deps`         | The pull request waits on work a review split out.                                               |
| `runner_fault`            | The runner failed on its service or account, not on the work, or a T3 Code run did not complete. |
| `push_failed`             | The branch would not push.                                                                       |
| `open_pr_failed`          | The merge gate passed and the pull request would not open.                                       |
| `merge_failed`            | The forge refused or failed the merge.                                                           |
| `ledger_unwritable`       | The ledger refused a row drive needed to record.                                                 |
| `no_commits`              | The run committed nothing.                                                                       |
| `uncommitted_work`        | The run left uncommitted work in its worktree.                                                   |
| `worktree_failed`         | The item's worktree could not be opened, linked or set up.                                       |
| `hand_edited`             | The body changed outside drover before any session launched.                                     |
| `branch_failed`           | A git step on the item's branch failed.                                                          |
| `implement_died`          | The implement session left no result.                                                            |
| `implement_unreadable`    | The implement session left a stream drover cannot read.                                          |
| `held`                    | The merge was withheld and the pull request left open.                                           |
| `runner_exit`             | Detail: the runner process exited non-zero.                                                      |
| `tool_denied`             | Detail: the session was refused a tool it needed.                                                |
| `background_left_running` | Detail: the session left a background process running.                                           |
| `stream_cut`              | Detail: the event stream ended mid-session.                                                      |
| `auth`                    | Detail: the runner had no credentials the service accepts.                                       |
| `oom`                     | Detail: the session was killed for memory.                                                       |
| `push_hook`               | Detail: a pre-push hook refused the branch.                                                      |
| `push_network`            | Detail: the push failed on the network.                                                          |
| `unknown`                 | Detail: drover could not classify the failure.                                                   |
| `unrecorded`              | Detail: a park no session caused named no detail; evidence is the reason.                        |

## Review results

Drive's reading of one review session, stored as `review_result`.

| Term                | Meaning                                                                                |
| ------------------- | -------------------------------------------------------------------------------------- |
| `review_result`     | The ledger and `--json` field holding one of the values in this table.                 |
| `passed`            | Nothing came back; the item goes on to the merge gate.                                 |
| `changes_requested` | Work went back to the branch.                                                          |
| `findings_open`     | An audit, no request-changes, and an earlier sent-back section still open in the body. |
| `no_digest`         | An audit, no request-changes, and no body digest to compare against.                   |
| `body_edited`       | The body changed outside drover after the review launched.                             |
| `no_record`         | The review recorded nothing.                                                           |
| `repeating`         | The review returned the same evidence twice.                                           |
| `at_limit`          | The review wanted to send work back and `max_review_rounds` refused it.                |
| `died`              | The session left no terminal event.                                                    |
| `unreadable`        | The session left a stream drover has no schema for.                                    |
| `faulted`           | The service or the account failed, not the work.                                       |

## Drive outcomes

What one item came to, on drive's end row and in `drive --json` `outcome`.

| Term                 | Meaning                                                                         |
| -------------------- | ------------------------------------------------------------------------------- |
| `ready_to_merge`     | Green and reviewed; nothing merged it.                                          |
| `merged_by_drover`   | drover merged it on the merge gate's word because the forge refused auto-merge. |
| `merged_immediately` | The forge's auto-merge merged it on the spot.                                   |
| `parked`             | Stopped; the park cause says why.                                               |
| `skipped`            | Not attempted: merged, scaffolded, blocked or broken.                           |
| `held`               | Kept back by a usage hold until the drive's deadline; nothing parked.           |

## Stages and events

Ledger stage rows, drive event stages, and the events `drive --events`, `attach` and `watch` emit.

| Term               | Meaning                                                                                                                |
| ------------------ | ---------------------------------------------------------------------------------------------------------------------- |
| `implement`        | Ledger stage: `run` handed the prompt to a runner.                                                                     |
| `review`           | Ledger and event stage: a review session launched.                                                                     |
| `interactive`      | `status` usage key: the `by_stage` entry for usage rows no stage launched, a Claude Code session the operator ran.     |
| `update_branch`    | Event stage: drive merged the trunk into the branch and ran the verify command.                                        |
| `gate`             | Event stage: the merge gate read the branch head, or a resume reran the verify command.                                |
| `pr`               | Event stage: drive opened the pull request.                                                                            |
| `merge`            | Event stage: drive asked the forge to merge.                                                                           |
| `finish`           | Ledger and event stage: the finish step after a merge.                                                                 |
| `started`          | Event: the drive holds its lease and read its queue; names the queue, `--jobs` and any deadline.                       |
| `queued`           | Event: an item joined the queue.                                                                                       |
| `dispatched`       | Event: an item left the queue for a session.                                                                           |
| `skipped`          | Event: an item left the queue without being attempted, with the state and the reason.                                  |
| `preflight`        | Event: a check drive runs before launching, such as `stale_citations`.                                                 |
| `stage`            | Event: an item reached one of the stages in this table.                                                                |
| `check`            | Event: drive classified a red forge check on a pushed head.                                                            |
| `parked`           | Event: an item parked.                                                                                                 |
| `filed`            | Event: a session filed a follow-up, with its coverage.                                                                 |
| `needs_operator`   | Event: an action stopped at operator policy and waits on a person.                                                     |
| `needs_supervisor` | Event: an action waits on the supervising session.                                                                     |
| `merged`           | Event: an item's pull request merged.                                                                                  |
| `merge_queued`     | Event: the forge accepted an item's auto-merge and has not merged it yet.                                              |
| `paused`           | Event: the drive paused.                                                                                               |
| `resumed`          | Event: the drive resumed, naming what changed.                                                                         |
| `config_changed`   | Event: the `.drover.toml` on disk differs from the one the running drive uses, which it does not apply until a resume. |
| `reexec_waiting`   | Event: a re-exec has waited past a minute on running items that do not hold.                                           |
| `usage_warning`    | Event: the account crossed a `[usage]` notify level; sent once per window per reset, and it pauses nothing.            |
| `thread_opened`    | Event: `t3 thread --from-event` opened a T3 Code thread for an item; carries `thread` and `url`.                         |
| `exit`             | Event: the drive ended, with a summary of what it merged and what it left undone and why.                              |
| `exit_undone`      | `[notify]` routing key: an exit that left an item parked, skipped or held.                                             |
| `held`             | Event on a `[notify]` command's stdin alone: what quiet hours held, sent as the window ends.                           |
| `answered`         | Event on a `[notify]` command's stdin alone: a reminded `needs_operator` was answered while the drive ran.             |
| round              | One implement session and the review that reads it.                                                                    |
| `infra`            | Check class: a red check blamed on the forge's infrastructure.                                                         |
| `trunk`            | Check class: a red check the trunk fails too.                                                                          |
| `code`             | Check class: a red check blamed on the diff.                                                                           |
| `none`             | Check class: no workflow reported on the head.                                                                         |
| `green`            | Check class: every check passed.                                                                                       |

## Update-branch results

What merging the trunk into a branch came to: the verify command's word in `update_branch.verify`, and the result of updating another open pull request or a branch at dispatch.

| Term         | Meaning                                                                     |
| ------------ | --------------------------------------------------------------------------- |
| `up_to_date` | Another open pull request already contained the trunk; nothing merged.      |
| `passed`     | The trunk merged in and the verify command passed on the result.            |
| `failed`     | The verify command failed on the merged tree, which the trunk passes alone. |
| `sequence`   | Numbered files the branch added clash with the trunk's.                     |
| `trunk_red`  | The trunk fails the verify command on its own.                              |
| `skipped`    | The project declares no verify command.                                     |
| `unrunnable` | The verify command did not finish on the merged tree.                       |
| `conflict`   | The trunk would not merge into another open pull request.                   |
| `kept`       | Another open pull request was not updated, for the reason given.            |
| `merged`     | At dispatch: the trunk was committed onto the branch before the session.    |
| `parked`     | At dispatch: a conflict stopped the item before any session started.        |

## Check finding kinds

The `kind` each `drover check` finding carries, in its table and in `check --json`.

| Term                  | Meaning                                                                               |
| --------------------- | ------------------------------------------------------------------------------------- |
| `NO BACKLOG`          | `[paths] backlog` names a directory that does not exist.                              |
| `NO SHELF`            | `[paths] shelf` is set and names a directory that does not exist.                     |
| `SHELF IS BACKLOG`    | The shelf and the backlog are the same directory.                                     |
| `NO VERIFY`           | `[verify] default` is empty.                                                          |
| `UNDECLARED KEY`      | A config key names a frontmatter key `[frontmatter]` does not declare.                |
| `NO REQUIREMENTS`     | An accepted item has no numbered requirements.                                        |
| `PLACEHOLDER`         | An accepted item's body still carries a `<placeholder>`.                              |
| `HOLD MARKER`         | An accepted item's body still carries an `[intake] hold_markers` line.                |
| `BROAD CITATION`      | A scaffold cites a top-level directory, which serializes it under `--jobs`.           |
| `HAND EDITED`         | An accepted item's body changed outside drover since its last digest.                 |
| `UNREADABLE`          | A backlog file did not parse.                                                         |
| `BAD VALUE`           | An item's value fails what `[frontmatter]` declares the key may hold.                 |
| `BAD RUNNER`          | A `[runners]` entry's argv is one its binary cannot use.                              |
| `BRIDGE SIGNED OUT`   | The `t3` bridge has no grant the T3 Code server accepts.                              |
| `BRIDGE UNREACHABLE`  | The T3 Code server did not answer `initialize`.                                       |
| `BRIDGE VERSION`      | The T3 Code server is not the version the bridge was tested on.                       |
| `BAD LEDGER`          | A ledger line is not valid JSON.                                                      |
| `ORPHAN ROW`          | The ledger names an id whose body is missing or unreadable.                           |
| `SHELF CLASH`         | One id has a body in both the backlog and the shelf.                                  |
| `MISSING DEP`         | A dep names an id whose body is missing, unreadable or shelved.                       |
| `BAD DEPS`            | A `deps:` line is written as a YAML list.                                             |
| `BRANCH CYCLE`        | A dep cites code only the dependent's unmerged branch has.                            |
| `MISSING REPORT`      | A ledger row names a saved report that does not exist.                                |
| `PROTECTED UNMATCHED` | A `[drive] protected_paths` glob matches no tracked file.                             |
| `SEQUENCE CLASH`      | Two tracked files share a number in a `[numbered_files]` glob.                        |
| `SESSION MARKER`      | A `.drover/session` is tracked, or names a drover pid that is not running.            |
| `BAD REGISTER`        | The follow-up list file exists and will not read.                                     |
| `NO LABEL`            | `[github] follow_up_label` names a label the repository does not define.              |
| `NO REGISTER`         | `[paths] register` names no file drover can ever have read.                           |
| `DUPLICATE ISSUE`     | An open follow-up is also the issue an open item was taken in from.                   |
| `ORPHANED FOLLOW-UP`  | Open follow-ups whose every owner has merged.                                         |
| `NO CLOSES KEY`       | Issues are owned by items and `[frontmatter]` declares no `closes` key.               |
| `UNLINKED`            | An open issue owned by an item that has run is missing from that item's `closes` key. |
| `UNMIRRORED`          | An accepted item names no issue while `[backlog] mirror = "github"`.                  |
| `MIRROR DRIFT`        | A mirrored issue's requirements differ from the item's numbered list.                 |
| `MIRROR STALE`        | A mirrored issue's label, open state or blocked links lag the backlog.                |
| `MIRROR SHELVED`      | A shelved body's issue is still open or lacks `drover:deferred`.                      |
| `NO DISPOSITION`      | A merged item's `closes` issue has no disposition.                                    |
| `STALE CONFIG`        | `.drover.toml` lacks a key `drover init` would write today.                           |
| `SUPERVISOR UNSET`    | A project that has driven leaves `supervisor` defaults with no `[supervise]` table.   |
| `BAD TEMPLATE`        | A `[prompts]` template will not read or render.                                       |
| `STALE BLOCK`         | A generated agent-instruction block differs from what this binary and config produce. |
| `STALE POLICY`        | A generated block records a `[supervise]` policy the config does not hold.            |
| `BAD ADDENDUM`        | The `[agents] addendum` file is missing or would end the generated block early.       |
| `NO POINTER`          | The block lives outside `AGENTS.md` and `AGENTS.md` never names its file.             |
| `BAD MARKERS`         | A file's `BEGIN drover` and `END drover` markers are unclosed or inverted.            |

## Issue dispositions

A judgment on one issue the item's `closes` key names, recorded by `resolve`, and what `finish` writes because of it.

| Term        | Meaning                                                                      |
| ----------- | ---------------------------------------------------------------------------- |
| disposition | The ledger row recording what an item's work does to one issue.              |
| `fixed`     | The work fixes the issue; `finish` closes it.                                |
| `partial`   | Some of it shipped; `finish` comments and leaves it open.                    |
| `unrelated` | The item does not address the issue after all.                               |
| `closes`    | Frontmatter key: the issues the item resolves.                               |
| `issue`     | Frontmatter key: the issue the item was taken in from.                       |
| `locking`   | Ledger field: `finish` holds the per-issue lock while it writes the tracker. |
| `unlocked`  | The value that releases that lock.                                           |
| `closed`    | `finish` closed the issue.                                                   |
| `commented` | `finish` commented on the issue.                                             |
| settled     | Two independent passes agree on a disposition.                               |

## Follow-ups and coverage values

Deferred work found in review, and whether anything will carry it.

| Term               | Meaning                                                                                                   |
| ------------------ | --------------------------------------------------------------------------------------------------------- |
| follow-up          | A concrete piece of deferred work found in review, filed as an issue or a row in the follow-up list file. |
| the follow-up list | Every open follow-up; `follow-ups` prints it.                                                             |
| owner              | The item a follow-up belongs to, read from a sub-issue parent or an `Owner:` line.                        |
| `orphaned`         | Every owner has merged, so no run will carry the follow-up.                                               |
| `unowned`          | The follow-up names no owner.                                                                             |
| live               | Neither orphaned nor unowned.                                                                             |
| inherit            | A new item whose `deps:` names an owner picks up that owner's follow-ups.                                 |
| `coverage`         | The `--json` field saying whether anything will carry a filed follow-up.                                  |
| `queued`           | Coverage: an owner is in this drive's queue.                                                              |
| `unqueued`         | Coverage: an owner is ready or implemented, and not queued.                                               |
| `blocked`          | Coverage: every unmerged owner is blocked.                                                                |
| `scaffold`         | Coverage: the owner is a scaffold whose requirements are unwritten.                                       |
| `open`, `closed`   | A follow-up list row's state.                                                                             |
| `dependents`       | `unaudited --json`: how many items depend on this one, directly or not.                                   |
| blast radius       | The prose name for `dependents`.                                                                          |
| finding            | Anything a review or `check` found.                                                                       |

## Labels drover writes

The labels and links drover puts on tracker issues.

| Term                 | Meaning                                                                                      |
| -------------------- | -------------------------------------------------------------------------------------------- |
| `follow-up`          | Default label on a filed follow-up, set by `[github] follow_up_label`.                       |
| `severity:<word>`    | Severity label: `severity:critical`, `high`, `medium` or `low`, from frontmatter C, H, M, L. |
| `drover:draft`       | Mirror label for a scaffold.                                                                 |
| `drover:ready`       | Mirror label for a ready item.                                                               |
| `drover:blocked`     | Mirror label for a blocked item.                                                             |
| `drover:in-progress` | Mirror label for a running item.                                                             |
| `drover:implemented` | Mirror label for an implemented item.                                                        |
| `drover:merged`      | Mirror label for a merged item.                                                              |
| `drover:deferred`    | Mirror label for a shelved item.                                                             |
| `Owner: <id>`        | A line in a follow-up's body naming its owner item.                                          |
| sub-issue link       | A follow-up linked under the owner item's issue, which makes that item its owner.            |

## Supervise actions and policies

The `[supervise]` keys, each an action drive or a supervisor may take, and the policy that decides who takes it.

| Term                          | Meaning                                                                                   |
| ----------------------------- | ----------------------------------------------------------------------------------------- |
| `auto`                        | Policy: drover takes the action itself.                                                   |
| `supervisor`                  | Policy: the supervising session takes it.                                                 |
| `operator`                    | Policy: a person takes it; drover reports and waits.                                      |
| `off`                         | Policy: nobody takes it.                                                                  |
| `update_open_prs`             | After a merge, merge the trunk into every other open drive pull request, verify and push. |
| `retry_moved_trunk`           | Retry, once, a merge gate that refused only because the trunk moved.                      |
| `resolve_conflict`            | Resolve a branch update merge conflict in the item's worktree.                            |
| `merge_drive_pr`              | Merge a pull request drive opened, then finish it.                                        |
| `merge_protected_paths`       | Merge a pull request that changes a `[drive] protected_paths` file.                       |
| `rerun_infra_failure`         | Rerun a check that failed on infrastructure.                                              |
| `report_trunk_red`            | Report a trunk that fails the verify command on its own, and stop dispatching.            |
| `post_finish_check`           | Run `[finish] post_check` after each finish.                                              |
| `rerun_lost_review`           | Rerun, once, a review whose session died or was cut.                                      |
| `relaunch_after_runner_death` | Relaunch an item whose implementing session died.                                         |
| `auth_expired`                | Stop when a runner's credentials expired.                                                 |
| `push_refused_by_hook`        | Stop when a pre-push hook refused a branch.                                               |
| `write_scaffold_requirements` | Write the numbered requirements of a scaffold a review filed.                             |
| `triage_finding`              | Find an owner for a follow-up nothing is scheduled to carry.                              |
| `close_changes`               | Close a sent-back section whose findings a later pass closed.                             |
| `accept`                      | Accept an item's requirements.                                                            |
| `override_review_limit`       | Send an item back past `[review] max_review_rounds`.                                      |
| `move_requirement`            | Move a numbered requirement from one item to another.                                     |
| `signoff`                     | Meet a requirement only a person can meet.                                                |
| `enqueue`                     | Queue the next drive.                                                                     |
| `pause_for_config_change`     | Pause a running drive to take a config change.                                            |
| `cleanup_interrupted_drive`   | Remove the worktrees and branches an interrupted drive left.                              |
| `host_load_admission`         | Hold dispatch while the host is loaded.                                                   |
| `usage_admission`             | Hold the drive at a `[usage]` fail-safe level, on purchased credit, or on a refusal.      |
| `drive_digest`                | Summarize each drive when it exits.                                                       |
| `stale_citations`             | Report cited lines missing from the trunk, before launching.                              |
| `worktree_links_gap`          | Report files the verify command reads that the worktree lacks.                            |
| `deps_lint`                   | Refuse a malformed `deps:` line.                                                          |

## Config sections and keys

The `.drover.toml` sections, with the keys whose names carry drover vocabulary. [Configuration](configuration.md) documents every key.

| Term                         | Meaning                                                                                                                                                                                                 |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `schema_version`             | The config format version.                                                                                                                                                                              |
| `drover_version`             | The release whose defaults the file was last stamped against.                                                                                                                                           |
| `project`                    | The project's name in the per-user registry.                                                                                                                                                            |
| `[paths]`                    | Where the backlog, ledger, shelf, reports, follow-up list, specs, skills and logs live.                                                                                                                 |
| `[paths] reports`            | Where saved self-reports go.                                                                                                                                                                            |
| `[paths] register`           | The follow-up list file, under `[register] backend = "file"`.                                                                                                                                           |
| `[ids]`                      | Namespaces, the id allocator and `planned_max`.                                                                                                                                                         |
| `[frontmatter]`              | The frontmatter keys an item may carry and what each may hold.                                                                                                                                          |
| `[t3]`                       | Where the `t3` bridge finds the T3 Code server, its token file, the project, and T3's provider event logs.                                                                                              |
| `[verify]`                   | The verify command: `default`, plus per-key commands an item can select.                                                                                                                                |
| `[status]`                   | Status columns, timezone and render targets.                                                                                                                                                            |
| `[severity]`                 | The frontmatter key holding severity and its ranks.                                                                                                                                                     |
| `[run]`                      | Branch naming, the trunk, the default runners and `runner_key`.                                                                                                                                         |
| `[run] runner_key`           | The frontmatter key that pins a runner per item; default `runner`.                                                                                                                                      |
| `[runners]`                  | Each agent CLI drover can launch, and its argv.                                                                                                                                                         |
| `[prompts]`                  | The prompt templates: `implement`, `draft` and `review`.                                                                                                                                                |
| `[review]`                   | Review settings: `max_review_rounds`.                                                                                                                                                                   |
| `[review] max_review_rounds` | How many times a review may send work back.                                                                                                                                                             |
| `[intake]`                   | The `stop_marker` line that keeps a scaffold unrunnable.                                                                                                                                                |
| `[agents]`                   | The generated instructions: `principles` and `addendum`.                                                                                                                                                |
| `[git]`                      | Whether drover drives git, `merge_check` and `merge_method`.                                                                                                                                            |
| `[drive]`                    | Drive settings: `merge_gate`, `protected_paths`, `worktree_links`, `worktree_setup` and `max_load`.                                                                                                     |
| `[drive] merge_gate`         | The merge gate: the command that decides whether a branch may merge.                                                                                                                                    |
| `[drive] protected_paths`    | Globs a merge will not carry unattended.                                                                                                                                                                |
| `[drive] worktree_links`     | Paths drive symlinks into each item's worktree.                                                                                                                                                         |
| `[drive] worktree_setup`     | A command drive runs in each new worktree.                                                                                                                                                              |
| `[finish]`                   | `post_check`, the command run after each finish.                                                                                                                                                        |
| `[numbered_files]`           | Globs of numbered files that must not share a number.                                                                                                                                                   |
| `[github]`                   | `repo`, `follow_up_label`, `severity_prefix` and `page_limit`.                                                                                                                                          |
| `[forge]`                    | How drive reads forge checks: `infra_patterns`, `failure_patterns`, `reruns` and `checks_wait`.                                                                                                         |
| `[usage]`                    | How status reads usage rows, and when drive warns and holds: `weekly_budget`, `reset`, `session_max_hours`, `context_warn`, and the tables `[usage.weekly]`, `[usage.five_hour]` and `[usage.overage]`. |
| `"off"`                      | A `[usage]` level written as unset, so a project can remove one the per-user file sets: `pause_remaining = "off"`. Any number, `0` included, is a level.                                                |
| `[register]`                 | Where the follow-up list lives: `backend`.                                                                                                                                                              |
| `[backlog]`                  | `mirror`, which publishes items as tracker issues.                                                                                                                                                      |
| `[supervise]`                | One policy per supervise action, plus `template`.                                                                                                                                                       |
| `[notify]`                   | Where a drive's events go: `coalesce`, `quiet_hours`, and `[notify.destinations.<name>]` tables, each an `ntfy` topic or a `command` routing its own events at their own priorities.                    |

## Concept words

The nouns the docs and the help use throughout.

| Term                          | Meaning                                                                                                                         |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| item                          | One markdown work file.                                                                                                         |
| backlog                       | The directory of items.                                                                                                         |
| shelf                         | The directory of items set aside unmerged.                                                                                      |
| ledger                        | The append-only JSONL history of launches, completions, audits and parks.                                                       |
| head row, body row, stage row | The ledger's row kinds: a completion, an audit or disposition, a launch.                                                        |
| contract                      | An item's numbered requirements.                                                                                                |
| numbered requirement          | One top-level `N.` line in an item body; `report --save` turns each into a checkbox.                                            |
| `[operator]` requirement      | A requirement only a person can meet, met with `signoff`.                                                                       |
| stop marker                   | The line that keeps a scaffold unrunnable until `accept` removes it.                                                            |
| prompt                        | The text drover hands a session: implement, review, draft or supervise.                                                         |
| runner                        | A configured agent CLI.                                                                                                         |
| T3 Code thread                | A stage run by the `t3` bridge: one thread on a T3 Code server, followed to the end of its run.                                 |
| provider event log            | The file T3 Code's Claude driver writes per thread, where the `t3` bridge reads turns, tokens, cost and the rate-limit reading. |
| `usage_unavailable`           | A `t3` log line saying the provider event log yielded no usage, and why.                                                        |
| session                       | One runner process.                                                                                                             |
| run                           | One implement session, and the command that starts it.                                                                          |
| review pass                   | One review session and the result drive read from it.                                                                           |
| self-report                   | A run's own summary of its work; `report` prints it.                                                                            |
| verify command                | The item's own check, from `[verify]`: rendered into the prompt and run after a branch update.                                  |
| merge gate                    | `[drive] merge_gate`: the command whose exit decides whether a branch merges.                                                   |
| update branch                 | Merging the trunk into an item's branch and running its verify command again.                                                   |
| trunk                         | The main branch, `[run] trunk`.                                                                                                 |
| forge                         | The code host, GitHub or similar.                                                                                               |
| mirror                        | Publishing items as tracker issues.                                                                                             |
| park                          | Drive stopping an item for a reason a person reads.                                                                             |
| drive                         | One unattended pass over a queue of items.                                                                                      |
| supervisor                    | The session `drover supervise` launches.                                                                                        |
| operator                      | The person running drover.                                                                                                      |
| session marker                | `.drover/session`: the actor, session id and root a launch leaves in its tree.                                                  |
| `DROVER_ACTOR=operator`       | A person steering from inside a sealed tree; seals nothing, overrides a marker.                                                 |
| worktree                      | The separate checkout drive gives each item.                                                                                    |
| lease                         | The record of which drover command holds a checkout; it expires.                                                                |
| hold                          | A paused item waiting at a stage boundary, or a merge withheld.                                                                 |
| notify level                  | `notify_remaining`: the percent of a window left at which drive sends one `usage_warning`.                                      |
| fail-safe level               | `pause_remaining`: the percent of a window left at which `usage_admission` holds the drive.                                     |
| sent-back section             | The block in an item body that `request-changes` writes, replaced on each round.                                                |
| `## Invariants`               | The item body section `invariant` appends to.                                                                                   |
| planned, intake               | The status sections for ids without a namespace prefix, and ids with one.                                                       |
| stem, slug                    | An item filename's padded id and its title part.                                                                                |
