# Automation

The wire contract for anything that drives drover without a person watching: a CI job, a wrapper script, a coding agent. Exit codes, `--json` formats, and the ledger protocol for a sandbox with no binary.

[Agent guide](agent-guide.md) is the behavioral half, for a coding session: what it may run, what it may not, and the refusals it will hit. [Lifecycle](lifecycle.md) is the same sequence for a human. Ids in the examples (`B1`, `E38`, `F12`) take the bare form described under [Item ids](concepts.md#item-ids).

Two rules cover most of it. Parse `--json`, never a table. Read the exit code before the output.

## The help surface

This page is the reference. `--help` is what a run has in a sandbox with no checkout of these docs, and everything in this section is answerable from the binary alone.

| Read                      | To learn                                                                                                                                                                                                                                         |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `drover --help`           | The lifecycle, grouped in the order an item moves through it. The safety classes, the exit codes, and which commands take `--json`.                                                                                                              |
| `drover <command> --help` | Why the command refuses what it refuses, a worked `Examples:` block, and a `Safety:` line naming what it writes.                                                                                                                                 |
| `drover agents`           | This project's own instructions: its paths, its verify commands, its label vocabulary.                                                                                                                                                           |
| `drover schema`           | The `.drover.toml` contract, for a validator or a generator. It needs no project. `--frontmatter` reads one, and emits what a body there must satisfy.                                                                                           |
| `drover config --diff`    | What this build would write differently in this project's own config: keys it reads that nothing declares, keys declared that nothing reads, and the `schema_version` gap. It validates nothing, so it answers for a config `check` cannot load. |

Every `--json` command carries a sample object in its own `Examples:` block, so the formats documented below are readable from the binary too.

The `Safety:` line resolves against the project it is read in. Inside a tree it names the resolved path; outside one it names the config key the value comes from. `drover finish --help` reads "Appends to .drover/ledger.jsonl" in a project on the defaults and "Appends to `[paths] ledger`" in a tree with no config.

| Class          | Means                                                                                                                                                                |
| -------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| read-only      | Runs anywhere, changes nothing. Safe to call to answer a question.                                                                                                   |
| local writes   | Changes files in this tree, and nothing outside it.                                                                                                                  |
| outward-facing | Reaches past the tree: `intake` relabels a tracker issue, `run` switches the git branch and launches a session, `draft` and `review` launch one when given a runner. |

A flag can escalate a class: `run --dry-run` is read-only, `report --save` writes, `skills --prune` deletes. `draft` and `review` are the inverse: each prints and changes nothing until `--runner`, or `[run] draft_runner` and `[run] review_runner`, gives it a session to launch. The command's own `Safety:` line names which.

`--runner` is one flag across `run`, `draft`, `review`, `finish` and `mark-merged`: it names which runner the invocation is about. It is checked two ways because it does two jobs. On `run`, `draft` and `review` the value selects an executable, so a name `[runners]` does not declare is refused with exit `1`. On `finish` and `mark-merged` it is attribution, so an undeclared name warns on stderr and the row is written as given: an agent that ran the item outside drover has no `[runners]` entry to match, and refusing there would make an accurate record of that run impossible to write.

## Exit codes

<!-- prettier-ignore -->
| Code | Means | Who exits this way |
| --- | --- | --- |
| `0` | Clean. | anything |
| `1` | drover could not answer. Unreadable config, no such item, `gh` unreachable, a git command that failed. | anything |
| `2` | drover ran and the project failed its own validation. | `check` |
| `3` | drover ran and a rule it enforces refused the operation. The tree and the ledger are untouched. | `accept`, `agents`, `amend`, `audit`, `close-changes`, `config`, `draft`, `drive`, `finish`, `init`, `intake`, `invariant`, `link`, `mark-merged`, `new`, `open-pr`, `projects`, `request-changes`, `resolve`, `review`, `follow-up`, `run`, `shelve`, `signoff`, `skills`, `split`, `status`, `t3`, `supervise`, `touch`, `unaccept`, `unmark-merged`, `unshelve`, `watch` |

The distinction that matters to a wrapper is `1` against `2` and `3`. A `1` means the tool did not work and the answer is unknown. A `2` or a `3` means it worked, and what it is telling you is about your project.

Every refusal that exits `3`:

| Command                                                                                                 | Refuses when                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| ------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `run <id>`                                                                                              | The body still carries `[intake] stop_marker`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `draft <id>`                                                                                            | The body's `[intake] stop_marker` is gone, so its requirements are already accepted. The inverse of `run`'s refusal, and the pair is what keeps drafting and accepting apart.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `finish [id]`                                                                                           | The branch does not look merged into the trunk. `--force` overrides.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `audit <id> verified --code-by <who>`                                                                   | The auditor matches an entry: whoever wrote the code cannot certify every requirement against it. The refusal names the entry. A level other than `claimed`, `reviewed` or `verified`, or a missing or malformed `--code-by`, also exits 3.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `resolve <id> <issue> <disposition>`                                                                    | The disposition is not one of `fixed`, `partial`, `unrelated`; `--code-by` is missing or malformed; the item's `closes` key does not name that issue and `--link` was not given, or `--link` was given and the link itself refused; or `finish` already wrote it to the tracker, so the ledger is not where it gets corrected any more.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `link <id> <issue>...`                                                                                  | The project declares no `closes` key, the tracker cannot be read so the number could not be checked, or the issue is owned by a different item.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `drive [id...]`                                                                                         | `--for` and `--until` were both given, the project declares no `[run] review_runner`, the project declares no `[drive] merge_gate` and `--no-merge` was not passed, `[git] enabled` is false, the tree has no `origin` to push to, a merge gate is declared and `gh` cannot be reached, a runner the drive will launch says it is not signed in (see [Park details](#park-details)), or `--review-only` was given other than one id or beside `--enqueue`. A drive with no reviewer would merge a night's work with no review having run; one with no merge gate was asked to merge and given nothing that could say green.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `open-pr [id]`                                                                                          | `[git] enabled` is false, a `run`, `draft` or `review` holds the checkout (a `drive` does not refuse it), the project declares no `[drive] merge_gate`, `gh` cannot be reached, the merge gate did not exit 0, the most recent review did not pass, or a request for changes the cap refused stands with no `amend`, `signoff` or passing review after it, which `--force` does not override; an `--over-limit` send-back answers it only once a later review passes. A stale pass row refuses unless a review stage row follows it. A `findings_open` pass row reads as `passed` once a `drover close-changes` row follows both it and the audit it read, and refuses until then. With no recorded pass row, or a stale one a later review has outdated, the ledger answers the last of those, names the pass it set aside, and refuses a review stage row it cannot find, a `changes` row or a re-run after the latest review, an audit weaker than an independent `reviewed`, an item whose body still carries a sent-back block an earlier round wrote and nobody closed (`findings_open`), an item with no body digest to read the review against (`no_digest`), and one whose body changed outside drover since the last digest drover recorded (`body_edited`). `--force` overrides the merge gate and every reading of the review, and overrides none of the rest: it substitutes your judgment for drover's evidence and cannot conjure a forge, a branch or a free checkout. |
| `split <id> --into <kind> "<title>" -f <file>`                                                          | No `-f` or `--into` was given, the file is empty, the title yields no filename, the kind names no declared namespace, the project declares no `deps` key, or the dependency would close a cycle.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `invariant <id> -f <file>`                                                                              | No `-f` was given, the invariant is empty, or writing it would change the count of numbered requirements.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `close-changes <id>`                                                                                    | The body carries no `## What the review sent back` section, so there is nothing to discharge; the latest review sent work back itself, so that block is current rather than closed; or writing the statement would change the count of numbered requirements.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `request-changes <id> -f <file>`                                                                        | No `-f` was given, the evidence is empty, writing it would change the count of numbered requirements, which are the contract rather than a finding, every finding in the evidence traces to an `[operator]` requirement, which no session can meet, or the item has already been sent back `[review] max_review_rounds` times by a review. At the cap the evidence is kept in `<stem>-<slug>.refused-N.md`, which the `refused` row names as `findings`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `signoff <id> <n> <note>`                                                                               | The number is not one of the item's `[operator]` requirements, the note is empty, or `DROVER_ACTOR` is set.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `amend <id> -f <file> --reason <text>`                                                                  | No `-f` or `--reason`, the file holds no numbered list, the item is a scaffold or merged, prose splits the body's list, or `DROVER_ACTOR` is set.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `touch <id> --reason <text>`                                                                            | No `--reason`, the item is a scaffold or merged, its body matches the recorded digest, or `DROVER_ACTOR` is set.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `request-changes <id> -f <file>` (contract)                                                             | The evidence names a requirement number the body lacks, or says requirements were removed or replaced with no `amend` since the last round. Under `--over-limit` it warns and `--yes` writes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `request-changes <id> --over-limit`                                                                     | The override is the operator's and this is a session drover launched. A refusal naming its own escape is an instruction, and the cap exists to bound the party reading it.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `intake <n>`                                                                                            | The issue carries no label matching a namespace, or a body already exists at that path. A follow-up adds one: it is still owned by an item the ledger does not record as merged, so that run already carries it.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| every project command                                                                                   | `.drover.toml` declares no `drover_version`, or one older by a minor release than the binary, or a root-level file's `drover agents` block differs from what this binary renders. Checked before the command does anything, never inside a session drover launched, and never for agents, config, init, schema, version, help, projects or shell completion. `-I` (`--ignore-stale`) overrides. See [The release stamp](configuration.md#the-release-stamp).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `config --stamp`                                                                                        | The binary carries no release version to write, which is a plain `go build`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `init`                                                                                                  | A `.drover.toml` is already there.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `agents --write`                                                                                        | The target file has the end marker above the begin marker.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `new <kind> <title>`                                                                                    | The kind matches no namespace, the title yields no filename, a body already sits at that path, the project declares no `[intake] stop_marker` to make the scaffold inert, or `--found-in` names an item not recorded merged.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `accept` / `unaccept`                                                                                   | The project declares no `[intake] stop_marker`, so there is no marker to accept or restore. `unaccept` also refuses an item the ledger records as merged. `accept` refuses a scaffold whose body cites a path or a function or type name only the branch of an unmerged item waiting on it has, unless given `--yes`. It also refuses, unless given `--yes`, a body carrying an angle-bracket placeholder or an `[intake] hold_markers` line, naming each line.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `shelve` / `unshelve`                                                                                   | The id is already on the shelf, another item declares it on `deps`, no shelf is configured, or the shelf and the backlog are one directory.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `intake --list`                                                                                         | The `[register]` backend is not `github`, so there is no tracker to take anything from.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `projects --forget <root>`                                                                              | The registry never named that root, so there is no row to tombstone.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `follow-up open` / `follow-up close`                                                                    | The `[register]` backend is not `file`, so the tracker or nothing at all owns the write. `open` also refuses an `--owner` not recorded merged, and a severity outside the [`--sev`](configuration.md#--sev) vocabulary.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `init --user`                                                                                           | A per-user config is already there, or there is no terminal to ask on and no `--defaults`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `skills --from`                                                                                         | The project declares no `[paths] skills`, so there is nowhere to copy to.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `status --sev`                                                                                          | A value outside the [`--sev`](configuration.md#--sev) vocabulary, which adds `none` to what `follow-up open --sev` takes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `status -o <path>`                                                                                      | A file is already at the path and `--force` was not given.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `accept`, `unaccept`, `signoff`, `intake`, `finish`, `mark-merged`, `unmark-merged`, `open-pr`, `drive` | `DROVER_ACTOR` is set, so drover launched this session (`drive` opens to a supervising session under its policy). See below.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `run`, `draft`, `review`, `t3 run`, `t3 thread --new`, `t3 thread --from-event`                         | A launch attempted with `DROVER_ACTOR` set, or from inside a tree a live session marker seals. `t3 run` is exempt only as the session drover itself launched, its parent the drover pid the marker names. The read-only forms, including `run --dry-run`, are unaffected.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `request-changes --over-limit`                                                                          | The flag, not the command. An agent requests changes freely below `[review] max_review_rounds`; deciding a finding is worth exceeding it is the operator's, because the agent is the party the cap bounds.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `supervise [id...]`                                                                                     | `DROVER_ACTOR` is set. A session drover launched, a supervisor included, cannot start a supervisor. `--dry-run` refuses there too, through the same check.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `drive`, `close-changes`, `accept`, `request-changes --over-limit` in a supervising session             | The supervised action the command is (`enqueue`, `close_changes`, `accept`, `override_review_limit`) is at `operator` or `off` in `[supervise]`. The refusal names the action and its policy. See [The supervisor](#the-supervisor).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `drive --events`                                                                                        | `--json` was not given. Events are lines of the `--json` stream.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `drive --stop`                                                                                          | No drive is running in this checkout.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `drive --detach`                                                                                        | The drive it started refused before it held the lease. `--detach` passes on the drive's own exit code and prints its error, so every `drive` refusal above applies.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `watch [drive id]`                                                                                      | The drive exited, or stopped running with no `exit` event, without emitting one of the `--until` events. Not a rule refusing: `3` is the code a wrapper tells from "no such drive" (`1`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `init supervise`                                                                                        | There is no terminal on stdin and no `--defaults`, or `DROVER_ACTOR` is set: the picker decides what a supervisor may do, so no launched session runs it, a supervisor included.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `finish [id]`                                                                                           | A live `run`, `draft` or `review` is holding this checkout, or a live session holds the item's own worktree, which finishing removes. A `drive` holding the checkout is not a refusal. Checked before the merge check, so nothing is fetched and the tree is not switched. See below.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `run <id>` / `drive [id...]` / `draft <id>` / `review <id>`                                             | Another live drover process, a `drive` included, is holding this checkout. These take the lease rather than reading it, so the refusal is the acquire failing; `run --dry-run` and `drive --dry-run` launch nothing and are unaffected.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |

## `DROVER_ACTOR`

drover exports `DROVER_ACTOR=agent` into every session it launches. Two rules key on it, both exiting `3` with nothing written:

- **Twelve commands belong to the operator** and refuse where it is set: `accept`, `unaccept`, `amend`, `touch`, `signoff`, `intake`, `finish`, `mark-merged`, `unmark-merged`, `open-pr`, `drive` and `supervise`. Each certifies work somebody else has to trust, reaches a tracker other people read, or launches sessions. A supervising session may run `drive` under its policy; see [The supervisor](#the-supervisor). `drover --help` prints the reason beside each name, and every one of them carries an `Actor:` line in its own `--help`.
- **A launched session cannot launch another.** Checked before the branch switch and before the `stage` row, so the refusal keeps exit `3`'s promise that the tree and the ledger are untouched.

`audit` is exempt at every level, and that is deliberate rather than an oversight: a review session is independent of the session that implemented the work, drover launched both, and one environment variable cannot tell them apart. A second one can: every launched session also carries its own id in `DROVER_SESSION`, recorded on its stage row, and `--code-by agent` resolves against those rows, so the implement session cannot record `verified` on its own code.

Only drover sets the variable: to `agent` for every session a run, review, draft or drive launches, and to `supervisor` for the one `drover supervise` starts (see [The supervisor](#the-supervisor)). Any non-empty value reads as "launched". `env -u DROVER_ACTOR` defeats both rules, and drover does not pretend otherwise. It is a check against a command being run because it appeared in a code block, not a boundary against one being run on purpose. A wrapper that legitimately needs to finish inside a launched session should strip the variable deliberately and say in its own logs that it did.

CI is unaffected: a job that never went through `drover run` has no `DROVER_ACTOR`, so `finish` there behaves as it does in a terminal.

`DROVER_ACTOR=operator` is the one value that means a person: `accept`, `finish`, `open-pr`, `signoff`, `drive`, `supervise` and the rest run under it, `DROVER_SESSION` is ignored beside it, and it overrides a session marker. drover never writes it. Export it to steer by hand from inside a tree a marker seals, which is a deliberate act in the same way `env -u` is.

### The session marker

The variable reaches only a session drover execs. Every launch (`run`, `review --runner`, `draft --runner`, `drive`, `supervise`) also writes `.drover/session` in the directory the session works in, the item's worktree under `drive` and the checkout otherwise, before the session starts, and removes it when the session exits, a cancel or an interrupt to drover included:

```json
{
  "actor": "agent",
  "session": "3f9c2a1b7d04",
  "root": "/home/sam/src/project",
  "pid": 48213,
  "started": "2026-10-25T14:02:11Z"
}
```

`actor`, `session` and `root` are the values the launch exports as `DROVER_ACTOR`, `DROVER_SESSION` and `DROVER_ROOT`. `pid` is the drover process that wrote the file and will remove it, not the session's own, and `started` is RFC 3339 in UTC.

A command that finds `DROVER_ACTOR` unset walks up from its working directory to the first `.drover/session` or `.drover.toml`, whichever is nearer, and treats a marker found as those three values exported: the operator commands refuse, `audit --code-by agent` resolves the session id, and config lookup takes `root` when `DROVER_ROOT` is unset, refusing with the marker's path when that root holds no `.drover.toml`. A marker with no `actor` reads as `agent`, and one that will not parse seals as `agent` with no session.

A sandbox that starts sessions itself (a T3 Code thread, a hosted agent) and wants the seal to hold writes this file into the session's working directory before it starts and removes it afterward. drover reads nothing else from it, so the sandbox's own process is the right `pid` to name.

A launch into a directory whose marker names a running `pid` refuses with exit `3`, naming the marker and its session, because two sessions in one tree write over each other. A marker whose `pid` is gone is replaced, with a line on stderr saying so; `drover check` reports one as `SESSION MARKER`, as it does a marker git tracks. The launch adds a `.drover/.gitignore` naming the marker and itself when the directory has none, so an agent staging everything does not commit the seal, and `drover init` adds `.drover/` to the project's `.gitignore`. `drover show <id>` prints the marker in the item's worktree and the stage row naming its session.

### The supervisor

`drover supervise` launches one interactive session with `DROVER_ACTOR=supervisor`: the session that watches drives for the operator. It answers to every rule above, with one exception and one addition.

- **It may run `drive`.** `drive` is the `enqueue` action, so a supervisor runs it, `--enqueue` included, while `[supervise] enqueue` is `supervisor` or `auto`, the default being `supervisor`. The sessions that drive launches get `DROVER_ACTOR=agent`, never `supervisor`, so none of them can start a drive or a supervisor. `run`, `draft --runner` and `review --runner` stay refused, and so does `supervise`.
- **A command that is a supervised action answers to its policy.** `drive` (`enqueue`), `close-changes` (`close_changes`), `accept` (`accept`) and `request-changes --over-limit` (`override_review_limit`) run under a supervisor only when the action is at `supervisor` or `auto`, and otherwise exit `3` naming the action, its policy and that the operator owns it. The catalog and each action's ceiling are in [`[supervise]`](configuration.md#supervise).

`accept`, `unaccept`, `touch`, `signoff`, `intake`, `finish`, `mark-merged`, `unmark-merged`, `open-pr` and `supervise` refuse a supervisor whatever `[supervise]` says, and so does `amend`, the `move_requirement` action, whose ceiling is `operator`. The policy is asked first, so where both apply the refusal names the setting.

## `DROVER_ROOT`

drover exports `DROVER_ROOT=<project root>` into every session it launches and into `[drive] merge_gate` and `[drive] worktree_setup`. When it is set, drover still walks up from the working directory, and the variable decides only what the walk would otherwise get wrong.

The reason is `drive`, which runs each item in a worktree of its own. Walking up from a worktree finds the worktree's tracked copy of `.drover.toml` when the project tracks one, and nothing at all when the config lives outside version control. Either way an agent's `drover audit` or `drover request-changes` would write somewhere nobody reads. With the variable set, both write the ledger and the backlog in the checkout the operator launched from.

A session can also leave the project entirely: a review that runs `git init` and `drover init` in a temporary directory to probe a command is standing in a project of its own. When the walk finds a `.drover.toml` in a different git repository from the variable's, or in no repository at all, drover uses the walk's project, so `drover new` there writes into the scratch backlog and not the operator's. The test is `git rev-parse --git-common-dir`: a linked worktree shares its checkout's common git dir and nothing else does, so the comparison separates a worktree's tracked copy of the config from another project's config without drover keeping a list of the worktrees it made.

When the walk finds no `.drover.toml`, or finds one in the variable's repository, a variable naming a directory with no `.drover.toml` is an error, never a fallback to the walk: a session reporting to the wrong project is worse than one that reports nothing.

## The checkout lease

`DROVER_ACTOR` is the environment-scoped half of one rule, and it can only ever see a process drover started: a child inherits the variable, and a second terminal inherits nothing. The lease is the other half, for the fact two unrelated processes have to share.

**The axis is the working tree, not the branch.** The hazard is not two commands wanting one branch; it is one command moving a checkout a second is writing. `drover drive` holds a tree for hours across a branch switch, two agent sessions and a return to the trunk, and a `drover finish` typed in a second terminal switches that same tree to the trunk underneath a live session. Both commands are correct. Neither can see the other. A lease keyed on the branch would have let that finish proceed, because the finish was on the trunk and the drive was on a feature branch.

The lease file lives in the checkout's own git dir, at `$(git rev-parse --absolute-git-dir)/drover.lease`. That path answers per work tree, so a linked worktree reports its own `.git/worktrees/<name>` and two worktrees of one repository never block each other: they are two trees, and the hazard is one tree. Where git answers with nothing, which is a project with `[git] enabled = false` and any other tree git does not recognize, it falls back to `<per-user state>/leases/<hash of the absolute root>.lease`, beside `projects.jsonl`. It is never written under the backlog, the ledger or anything else the project tracks: a lease is one machine's state, and a committed one hands every clone a holder that does not exist.

| Command                                               | What it does with the lease                                                                                                                                                                                                                                                                                                                                      |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `run <id>`                                            | Takes it before the branch switch, so a refusal costs no switch. `--dry-run` takes nothing.                                                                                                                                                                                                                                                                      |
| `drive [id...]`                                       | Takes one on your checkout for the whole queue, which keeps out a second drive and `run`, `draft` and `review`, and one on each item's worktree while that item runs. `--dry-run` takes nothing.                                                                                                                                                                 |
| `draft <id> --runner` / `review <id> --runner`        | Take it before the launch. Neither switches a branch, and both hold an agent inside the tree, which is what is being kept to one command.                                                                                                                                                                                                                        |
| `finish [id]` / `open-pr [id]`                        | Read it and refuse, taking none, unless the holder is a `drive`, whose items run in worktrees of their own. They run in seconds; what they need to know is whether the branch they are about to switch or put through the merge gate belongs to a session in this tree. `finish` also reads the item worktree's lease and refuses while a live session holds it. |
| `drive --enqueue <ids>`                               | Takes none. Appends the ids to the `queue` in the running drive's lease, under `drover.lease.queue`, a lock file beside it that the drive takes for its own queue rewrites too. With no drive holding the checkout it is `drive <ids>` and takes the lease as that does.                                                                                         |
| `drive --stop`                                        | Takes none. Writes a `stop` request into `control` as `--pause` does and returns once the drive answers it, or once the drive is already stopping or gone. See [Detaching from a drive](#detaching-from-a-drive).                                                                                                                                                |
| `drive --detach`                                      | Takes none itself. The drive it starts takes the lease as `drive` does, and `--detach` returns once that lease names the new process and its drive id.                                                                                                                                                                                                           |
| `attach` / `watch`                                    | Read it to learn whether the drive they follow still runs, and take nothing.                                                                                                                                                                                                                                                                                     |
| `drive --pause` / `drive --resume`                    | Take none. Write a request into `control` in the running drive's lease, under the same lock as `--enqueue`, and wait up to two minutes for the drive to echo it back in `answered`. With `--all`, the same in every registered project's lease at once. See [Pausing a drive](#pausing-a-drive).                                                                 |
| `drive` re-executing itself                           | Keeps its lease locked across the `exec`: the descriptor stays open into the new binary, which adopts it rather than acquiring one, so no other drive can take the checkout in between.                                                                                                                                                                          |
| `drive`'s trunk sync / `finish`'s return to the trunk | Take `drover.trunk.lock` in the repository's common git dir around the fast-forward, waiting up to 30 seconds for each other. A wait that runs out is reported, as a failed fetch is, and the command goes on.                                                                                                                                                   |
| `status`                                              | Reports the holder, and a drive's queue, and refuses nothing.                                                                                                                                                                                                                                                                                                    |

**Liveness is proven, not inferred.** Acquire takes an exclusive non-blocking advisory lock (`flock`) and holds the descriptor for the process lifetime, so the kernel releases it however the process dies. A drive killed with `SIGKILL` leaves a lease that is immediately acquirable. A pid read back out of a file would be a guess, and a wrong one once the number is reused.

Nothing ever reports a pid the lock does not stand behind. `Release` empties the account before it releases the lock, and `Acquire` empties the file again under the lock before it names itself, which covers the holder that was killed and ran nothing. Between those two writes a holder is real and unnamed, and drover says so rather than reaching for the last name it saw:

```text
drover: another drover process is holding this checkout; it has not written its account yet.
Retry in a moment, or run drover status to see who is here
```

That is still a refusal, so `finish` exits `3`. The tree is taken and only the name is missing, which is a rule refusing rather than a read that failed, and a `1` there would tell a wrapper the tool did not work. `status` prints the same sentence under `▸ also here:` and `status --json` carries it as `lease_error` with no `lease` object, because there is no holder to describe yet.

The refusal names the holder and the pid, because a refusal reading only "busy" sends the operator to `ps`:

```text
drover: drover run B1 (pid 41234) is holding this checkout since 2026-09-17T09:12:03Z, on b1-first.
Two commands in one tree is how a branch switch lands under a live session.
Wait for it to finish, or stop it: kill 41234
```

**There is no `--force` and no `--anyway`.** The kernel releases the lock on process death, so a lease nobody holds is already acquirable and there is no stale state an override could rescue. The only thing one could do is admit a second command to a tree a live one is writing, which is the collision.

**There is no `finish --wait` either**, and that is a decision rather than an omission. The holder is an agent session measured in hours, so a blocking finish would park a terminal on a condition nothing bounds; and a wait that returned would be returning into a tree the drive had just moved, which needs the merge check re-run from the top anyway. The refusal prints the pid, which is the two answers there are: wait, or stop it. Retrying is one line of shell history.

**On a platform without `flock`** (anything Go does not build with the `unix` tag, which in the release matrix means Windows), nothing is enforced. Every acquire succeeds, every read reports the tree free, no command refuses, and `drover status` names nobody. Two commands can reach one checkout there with nothing to stop them.

### Pausing a drive

A drive reads `.drover.toml` once. `drover drive --pause` tells the drive running in this checkout to dispatch nothing more and to hold each running item at its next stage boundary: before its implement run, before its review, or before its trunk stage. An item already in its trunk stage finishes it. An item whose review passed and that is waiting for its trunk turn behind an item dispatched before it counts as held before `update_branch` for as long as it waits, and leaves the hold when its turn comes or the drive stops. Nothing parks, the drive stays up and its lease stays held, so `--enqueue` still appends to it and `status` still names it. The lease reads `"paused":true`.

`drover drive --resume` checks the config and the prompt and review templates in its own process and refuses with exit `3`, asking the drive nothing, when one will not load or render. Otherwise the drive reloads them itself and runs the launch's preflight under its own flags (the reviewer, the merge gate, the origin, gh and each runner's sign-in), and a failure there is refused with exit `3` as well. Either way the drive stays paused and says why in `refused`. Once a resume takes, each held item adopts the reloaded config at its boundary, and one whose verify command changed reruns it there (see [`resumed`](#drive---events)).

A [usage hold](#drive---events) pauses the drive through this same lease state, `"paused":true`, and `--resume` lifts it the same way; it also lifts on its own at its window's reset when the drive's `--for` or `--until` covers it.

A running drive keeps the `.drover.toml` it started or last resumed with, and an edit made meanwhile does not apply. Before each item is dispatched and at each stage boundary, an unpaused drive compares the file on disk with the bytes it runs under, and reports a difference once per distinct content, without applying it: a stderr line `! config changed on disk at <mtime>: this drive still runs the .drover.toml it started with; drover drive --pause, then --resume, applies it`, naming the tables that differ (or the load error, for a file a resume would refuse), and a [`config_changed`](#drive---events) event. `drover status` says the same under the drive's `▸ also here:` line, from the lease's `config_sha256`. `--pause`, then `--resume`, applies it.

The templates are read from disk at every launch whether or not the drive was paused. What a resume adds is the check before anything launches, and the `resumed` event naming what changed.

A supervisor runs `--pause` and `--resume` only while `[supervise] pause_for_config_change` is `supervisor`, and never edits `.drover.toml` itself. At `operator`, the default, both are the operator's; at `off`, `--pause` refuses for everybody.

#### Every project at once

`drover drive --pause --all` pauses the drive in every project in the [per-user registry](#projects---json) through each one's lease, all at once, and needs no project to run from. A registered root that is gone or holds no `.drover.toml` has no drive and is skipped without a line. A git worktree of a registered project is paused under that project's name and its `pause_for_config_change` policy, through the worktree's own lease, so a drive running there takes the pause too. It prints one line per project:

```text
alpha  paused drive D3 (pid 41234): it dispatches nothing more, and 7 9 hold at the next stage boundary each reaches
beta   no drive running
gamma  could not be paused: [supervise] pause_for_config_change is off there
```

It exits `0` when every live drive took the pause and `3` otherwise; a project with no drive is not a failure. Each project is judged by its own `pause_for_config_change`: `off` refuses everybody there, and a supervisor reaches only the projects at `supervisor` or above. The project a supervisor stands in is judged by that policy first, as for `--pause` alone.

`--wait` returns only once no paused drive has an item in a session or a trunk stage: every `running` id appears in the lease's `holds`. It prints what it is still waiting on at most once a minute, and gives up after `--timeout` (default `2h`) with exit `3`, naming each drive still busy and the ids inside a stage. An interrupt leaves every drive paused, since nothing resumes them but `--resume`. `--wait` works on `--pause` alone too, over this checkout's drive.

`drover drive --resume --all` resumes every paused drive in the registry. Each project's config and templates are checked first, as `--resume` checks this one's, and the drive then checks again under its own flags. A project that fails either check stays paused and is named, and the rest resume; any such project makes the exit `3`.

#### Resuming onto a new binary

A drive runs the binary it was launched from, so a config key a newer drover added reads as unknown to a drive started before the install, and no new feature reaches it. On `--resume`, the drive compares the file now at the path it was started from (its `argv[0]`, resolved through `PATH`) with the one it started from, by inode, size and modification time. When an install replaced it:

1. The drive runs the new binary over its own flags with `--resume-check`, which loads the config, the templates and the launch's preflight. A failure refuses the resume, and the drive stays paused on the binary it has.
2. Otherwise it answers the resume with `reexec` naming the new binary, stays paused until every running item holds at a boundary, and then replaces itself with `exec`, passing its original arguments plus `--takeover <id>` and the descriptor its lease is open on. An item waiting for its trunk turn is held, so a re-exec never waits on it.
3. The new process adopts that lease without releasing it, reads the held items, the queue, the deadline, the count `--max-items` bounds and the outcomes so far out of the lease's `handoff`, writes a second start row for the same drive id naming both binaries, emits `resumed` with `changed` `binary`, and carries on. Each held item goes on from its boundary: its implement run, its review on the branch as it stands, or its trunk stage, with its verify command rerun first when the new config gives it another.

An item inside a session, a verify command or its trunk stage holds only once that ends, so a re-exec can wait a long time. Once it has waited more than a minute on running items that do not hold, the drive emits [`reexec_waiting`](#drive---events) naming each of them and the stage it was last seen in, at most once a minute while the wait lasts, and writes the same line into the lease's `reexec_waiting`. `drover status` prints the drive as `pausing to re-exec` with that line under it, in place of a bare `pausing`. `drive --stop` ends the wait, parking each item that has not finished.

`--resume --keep-binary` skips all of this and resumes on the binary the drive has. A re-exec is unix-only; elsewhere the `exec` fails, says so, and the drive stays paused for a `--keep-binary` resume.

### Cleaning up after a drive

`drover drive --cleanup` lists what interrupted drives left in this project and writes nothing:

- every worktree under the drive tree directory for this checkout, with its item, its branch, whether it holds uncommitted work and whether a live process holds its lease;
- every local branch whose last path part is an item's slug, alone or after a hyphen, and that `[run] branch` does not render for any item, with whether it carries commits past the trunk;
- every saved report that is the stub a run log with no result event leaves.

`--cleanup --apply` removes the clean worktrees nobody holds, with `git worktree remove` and never `--force`, and deletes the orphan branches holding no commit past the trunk. Uncommitted work, a held tree and a branch with commits are kept and named. Report stubs are listed and never deleted. A supervisor runs either while `[supervise] cleanup_interrupted_drive` is `supervisor` or `auto`; at `auto` every drive also applies it before reading its queue, and at `off` `--apply` refuses.

The branch match is by slug because a branch template since changed cannot be rendered again. A template that renders no slug leaves its old branches unlisted.

### Detaching from a drive

A drive is a child of whatever launched it. Started from an agent session, it lives under that session's process, so restarting the agent (to load a new version, or after a crash) orphans the drive or kills it. `drover drive --detach` starts the same drive with the same flags in a session of its own, and three verbs reach it from any process: `attach` and `watch` read it, and `drive --stop` ends it.

**The event file.** Every drive, detached or not, appends each event to `$XDG_STATE_HOME/drover/drives/<project>/<drive id>.jsonl` (default `~/.local/state/drover/drives/...`), one [`drive --events`](#drive---events) object per line, written and flushed as it is emitted, whether or not `--events` was given. Each event's `seq` is its line in the file, from 1. The drive empties the file when it takes its id, so a ledger reset that reuses `D1` does not hand a reader the old drive's `exit`; a drive that re-executed onto a new binary keeps its id and appends, numbering on from the lines the file holds and first ending a line the old process left cut short. The last line is the `exit` event, after which the drive closes the file. A write that fails is reported once on stderr, the drive stops writing the file, and nothing else changes: the queue runs to the end and `--events` still streams.

**`drover drive --detach [ids] [flags]`** re-runs the same command line less `--detach` from the binary at the path it was started from, in a new session (`setsid`) with no controlling terminal, stdin on `/dev/null` and stdout and stderr in a log file beside the event file. Once the new process holds the lease and has its id, it renames the log to `<drive id>.log`, prints both paths and exits `0`:

```text
detached drive D3 (pid 41234)
  log:    /home/me/.local/state/drover/drives/webapp/D3.log
  events: /home/me/.local/state/drover/drives/webapp/D3.jsonl
Follow it with drover attach D3, wait on it with drover watch D3 --until <event,...>, stop it with drover drive --stop
```

A drive that exits before it holds the lease (a refusal, a preflight that fails) has its log printed and its exit code passed on: `3` for a refusal, `1` otherwise, and `0` for one with nothing to do. Every other `drive` flag means what it does without `--detach`; `--json` and `--events` write into the log. `--detach` refuses inside a session drover launched, as `drive` does, and a supervisor may run it while `[supervise] enqueue` allows `drive`. It waits up to five minutes for the lease, the time the preflight's sign-in checks can take, and otherwise exits `1` naming the pid and the log, the drive still running. It needs a unix session: elsewhere it fails before it starts anything.

**`drover attach [drive id]`** prints the drive's event file from the first line and follows it until the `exit` event, then exits `0`. With no id it follows the drive running in this checkout, else the project's latest from the ledger. Each event is one readable line by default and the drive's own JSON line under `--json`; `--since <ts>` (RFC 3339) skips events stamped before it. An interrupt stops following and leaves the drive running, since `attach` never signals the drive or writes anything it reads. An id the ledger and the lease do not name exits `1`, and so does a drive that stopped running with no `exit` event in its file, or with no file at all (one that ran before drives kept one).

```text
2026-10-18T09:12:02Z  started        queue=4 jobs=1 seq=1
2026-10-18T09:12:03Z  dispatched     4 seq=2
2026-10-18T09:12:03Z  stage          4 stage=implement log=~/.cache/drover/webapp/4-retry.20261018-021203.123456789.jsonl seq=3
2026-10-18T09:40:51Z  parked         4 park=no_record detail=unknown seq=4 reason="the review recorded nothing"
2026-10-18T09:40:52Z  exit           code=0 ended=drained merged=0 merge_queued=0 parked=1 skipped=0 held=0 seq=5
```

`--session` prints, beneath each `stage` event that carries a [`log`](#drive---events), what that session is doing as its log grows: one line per tool call or block of text, indented under the item and prefixed with its id, so items running at once under `--jobs` stay apart. `attach` follows each log by offset as it follows the event file, opened for reading only, and stops at the same item's next event, reading the log once more to its last whole line first. Replaying a drive already under way, it reads the logs of sessions still running when it started and of every session launched after; a session that finished before `attach` started keeps its `stage` line alone, so attaching to a long drive does not print hours of old logs. A log not written yet is waited for, and one that will not read is named once on stderr with the error while `attach` goes on with the events. What a line says depends on the runner: a Claude tool call is its name and the first of its `command`, `file_path`, `pattern` or `url` inputs, a codex one is a `command_execution` or `file_change` item, an agy one is the tool step it opens; Claude and codex text is each message the session writes, and agy, which streams text in fragments, yields none. Each summary is one line, cut at 160 characters with a trailing `...`.

```text
2026-10-24T22:40:11Z  stage          F14 stage=implement log=~/.cache/drover/webapp/F14-ntfy.20261024-154011.123456789.jsonl seq=7
                                     F14 > Read internal/cmd/events.go
                                     F14 > Bash make build && make check
                                     F14 · The verify command passes; committing.
```

Under `--json --session` each activity is one object keyed `activity`, never `event`, so a reader filtering on `event` sees the drive's stream unchanged. `activity` is `tool` or `text`, `tool` is the tool's name on a `tool` entry alone, and `text` is the summary above. `ts` is the time `attach` read the line, not the time the session wrote it: codex and agy stamp none of their lines, and one clock across runners keeps the entries of items running at once in the order they were read.

```json
{ "activity": "tool", "ts": "2026-10-24T22:41:02Z", "drive": "D8", "id": "F14", "stage": "implement", "tool": "Bash", "text": "Bash make build && make check" }
```

`--item <id>` limits `attach`, with or without `--session`, to one item's events and the `exit`. An id the drive has not dispatched or queued is not an error, since it may be enqueued later: `attach` prints nothing for it until it appears.

**`drover watch [drive id] --until <event,...>`** blocks until the drive emits one of the named events, prints that event's JSON line and exits `0`. It exits `3` when the drive exits, or stops running, without emitting one, and `1` when there is no such drive or an `--until` name is no event. Without `--after` or `--since`, only events written after `watch` starts count, so an event written between two calls is lost. `--after <seq>` counts only events whose `seq` is past it, whenever they were written: a loop passing the `seq` of the last event it handled gets each event once and misses none. `--since <ts>` counts events stamped at or after `ts`, for a person reading by time; `ts` is one second wide and two events often share one, so a loop on `--since` returns the event it handled again, and strict "after" on `ts` would drop the event beside it. The `exit` event ends a watch whenever it was written, so `watch` on a drive that already exited returns at once: `0` with that event when `--until` names `exit`, `3` otherwise. An interrupt exits `1` and leaves the drive running.

```sh
drover watch --until parked,needs_operator,merged,exit --after 7
```

```json
{
  "event": "parked",
  "ts": "2026-10-18T09:40:51Z",
  "seq": 8,
  "drive": "D3",
  "id": "4",
  "park": "no_record",
  "reason": "the review recorded nothing"
}
```

This is the loop a supervising session would otherwise write around `sleep 30` and the ledger, the lease or a JSON report, and the [supervisor prompt](#the-supervisor) tells the session to use it.

**`drover drive --stop`** writes a `stop` request into the running drive's lease, under the lock `--pause` uses, and returns once the drive answers. The drive drains as an interrupt makes it, sending and receiving no signal: it dispatches nothing more and closes its queue to `--enqueue`, each running item finishes the stage it is in and parks `stopped`, and the drive exits once they have, with the usual end row and `exit` event. A paused drive stops the same way: its held items let go and park at their boundary. A drive already stopping, or one that exits before it answers, counts as stopped. With no drive running, `--stop` exits `3`. It takes no ids and joins `--pause`, `--resume`, `--enqueue` and `--cleanup` as the verbs that act on a drive rather than run one; a supervisor runs it under `[supervise] enqueue`, as it runs `drive`.

**After the launching agent restarts**, the drive is still running and its events are still on disk. Recover in three steps:

1. `drover drives` names the running drive (`running`) and its id.
2. `drover attach <id>` replays what it did while nobody was reading; interrupt it once it is current.
3. `drover watch <id> --until parked,needs_operator,merged,exit --after <seq of the last event you handled>` picks up from there, and each call after passes the `seq` of the event the one before returned.

**There is no `droverd`.** A resident daemon owning every drive was the alternative, and it lost on three counts: it is one more process somebody has to supervise, which moves the problem rather than removing it; one daemon across every project is a single failure point for all of them; and it would need a protocol of its own to take requests, where the lease already carries `--enqueue`, `--pause`, `--resume` and `--stop`. A detached drive is a plain process the kernel tracks, its lease says whether it lives, and its event file outlives it.

## `--json`

On `status`, `show`, `unaudited`, `drive`, `drives`, `attach`, `check`, `follow-ups`, `intake --list`, `projects`, `skills`, `t3 discover` and `report`. A list command emits an array, `[]` when empty and never `null`, with two declared exceptions below: `status` wraps its array in an object and `drive` emits one object per line. A field absent from the output is a field with no value; nothing emits `null` for a string.

**stdout carries the document and nothing else.** Every progress and side-effect report goes to stderr: what `skills --from` vendored, what `skills --prune` deleted. So `drover skills --json | jq` parses with nothing in front of it, and `2>&1` is what breaks it. Capture the two streams separately and a wrapper gets both the document and the account of what changed under it.

### `status --json`

An object. `items` is one entry per item in backlog order; merged rows are absent by default, the same scope the table shows, and `--all` puts every row in it.

```json
{
  "items": [
    {
      "id": "1",
      "title": "Test suite",
      "state": "merged",
      "column": "1",
      "level": "verified",
      "by": "agent",
      "independent": true,
      "reviewer": "sam",
      "implementer": "claude",
      "review_runner": "codex",
      "runs": 3,
      "audits": 2,
      "planned": true
    },
    {
      "id": "3",
      "title": "Resolver",
      "state": "blocked",
      "column": "1",
      "missing": ["2"],
      "planned": true
    },
    {
      "id": "4",
      "title": "Cache",
      "state": "implemented",
      "column": "1",
      "implementer": "claude",
      "runs": 1,
      "planned": true,
      "drive": "D2",
      "origin": {
        "unrecorded": false,
        "text": "operator intake #44",
        "cmd": "intake",
        "actor": "operator",
        "issue": 44
      },
      "created": { "unrecorded": false, "at": "2026-10-27T09:21:04-07:00", "source": "origin" },
      "edited": {
        "unrecorded": false,
        "at": "2026-10-27T10:02:11-07:00",
        "source": "body",
        "cmd": "request-changes",
        "actor": "operator",
        "drive": "D2"
      }
    }
  ],
  "lease": {
    "command": "drive",
    "pid": 41234,
    "branch": "b1-first",
    "root": "/src/webapp",
    "host": "boxcar",
    "started": "2026-09-17T09:12:03Z",
    "drive": "D2",
    "label": "night run",
    "queue": ["5"],
    "running": ["4"],
    "jobs": 1,
    "paused": true,
    "holds": [{ "id": "4", "at": "review", "verify": "make check" }]
  },
  "drives": [
    {
      "id": "D2",
      "label": "night run",
      "started": "2026-09-17T09:12:05-07:00",
      "queued": ["4"],
      "items": [{ "id": "4", "outcome": "" }],
      "running": true,
      "paused": true
    },
    {
      "id": "D1",
      "started": "2026-09-16T22:00:00-07:00",
      "queued": ["1"],
      "ended": "2026-09-16T23:10:00-07:00",
      "exit": 0,
      "items": [{ "id": "1", "outcome": "ready_to_merge" }]
    }
  ]
}
```

`origin`, `created` and `edited` are on every item, an unreadable one included, read under the id its filename names from the ledger and the file's mtime; they are the table's second line as data. Each carries `unrecorded`, true for what the ledger never wrote down, so a consumer tells an item made before drover recorded origins from one made by the operator.

- `origin` is the item's [origin row](#writing-to-the-ledger-without-the-binary): `cmd`, `actor`, `agent`, `session`, `found_in`, `issue` and `split_from`, each absent when empty. `drive`, `stage` and `of` are the stage row the origin's session resolves to, absent when it resolves to none. `text` is the phrase the table prints (`operator new`, `claude new`, `operator intake #44`, `operator split of E84`, `D20 review of B104`, `supervisor new`, `agent new`, a trailing `← B104` for a `found_in` that is not the resolved item), and `unrecorded` alone with `text` is all an item with no origin row carries.
- `created.at` is RFC 3339, or a bare date for a row that carries no `ts`. `source` is `origin`, or `ledger` for an unrecorded item: the earliest ledger row naming it, which bounds the creation from above and prints as `≤` in the table. An unrecorded item no row names has no `at`.
- `edited.source` is `body` (the latest row carrying the body's digest, which still matches the file, with its `cmd`, `actor`, `agent` and `drive`; `unrecorded` when the row predates attribution), `origin` (no digest row yet, so the creation is the last write drover saw), `hand` (the file no longer matches the latest digest: `by_hand` is true and `at` is the file's mtime) or `mtime` (no row at all: `unrecorded`, `at` the mtime).

`--since` keeps the same items in `items` that it keeps in the table: those whose `created.at` or `edited.at` is at or after it. An unreadable item is held to the same cut rather than kept unconditionally, as `--sev` keeps it.

`lease` is [the checkout lease](#the-checkout-lease), present only while another drover command holds this tree, and `lease_error` only when drover could not name the holder: a degraded read says what it could not ask rather than reporting nobody. That covers a lease drover could not read at all and a holder that has the tree and has not written its account yet, which is why the two never appear together: one of them is always a holder drover cannot describe. `item` is absent on a lease taken over a queue rather than for one id, which is every `drive`. A `drive` names its queue instead: `queue` is the ids it holds waiting, in dispatch order, `running` the ids it has dispatched and not yet heard back from, and `jobs` its `--jobs`. `waiting` is each queued id it holds for a dep that parked for the operator, as `{ "id": "5", "on": "4" }`, and `skipped` each id it took off its queue without attempting, as `{ "id": "6", "reason": "blocked by 4" }`; both are absent while empty. It rewrites all five in the lease file as items are dispatched and finish, so another process reads them without asking the drive. A stopped drive dispatches nothing more and publishes an empty `queue`. `paused` is a drive holding its items for a resume, and `holds` each running item it holds at a stage boundary: the item, the stage it goes on to (`at`) and the verify command the stage before was told to pass. A `running` id with no hold is still inside a session, a verify command or the trunk stage. `reexec` names the binary a resumed drive re-executes onto once every running item holds, `reexec_waiting` is the line naming the running items it has waited on for more than a minute, and `handoff` is what it leaves there for the new process, present only across the `exec`. `control`, `answered` and `refused` carry [`--pause` and `--resume`](#pausing-a-drive) between processes. `config_sha256` is the SHA-256 of the `.drover.toml` the drive runs under, written when it takes the lease and again by each resume that takes, and `config_resumed` says a resume wrote it; when the file on disk no longer matches, the table prints `config changed since this drive started; --pause, then --resume, applies it` (`resumed`, after a resume) under the `▸ also here:` line, and the running drive in `drives` carries `"config_changed": true`. A lease an older binary wrote has no `config_sha256` and reports nothing. `closed` is a drive that takes no more ids: it stopped, or its queue ran out and it is exiting. `drive --enqueue` writes into `queue` while it is absent, and the drive takes what was appended at its next dispatch. `drive` is the running drive's id and `label` its `--label`, both written when the drive takes the lease, before reconciliation, and absent on every other holder. The table prints them after the command on the `▸ also here:` line: `drover drive D2 "night run"`, followed by `paused` while it is (`pausing` while an item is still inside a stage, `pausing to re-exec` with a `waiting:` line carrying `reexec_waiting` once that is set), and a `held:` line under it naming each held item and the stage it holds before.

`drives` is every drive the ledger records, newest first, in the form [`drives --json`](#drives---json) prints, and absent before the first. The one the lease names carries `"running": true`. A row's `drive` is the drive that wrote the item's latest row carrying one, absent when no drive has touched the item, so grouping `items` by `drive` answers which items one drive ran without reading the dependency graph.

This is the one list command that emits an object, and the lease is why. It belongs to the tree rather than to any item, so there is no row it could ride on, and a holder named in the table but missing from the document would make the two outputs of one command disagree.

`findings_needing_triage` is every open follow-up whose [coverage](configuration.md#supervise) is `blocked`, `orphaned` or `unowned`, and every scaffold, judged at this read from the follow-up list, the ledger, the backlog and the queue of the drive holding the checkout. It is `[]` when there are none. `findings_error` is present whenever the list holds no follow-ups because none were read: the follow-up list would not read, `--no-tracker` on a GitHub backend left it unasked, or a drive's render came before that drive's first read. Each of those still lists the scaffolds. The [`[status] render`](configuration.md#status) template data carries both. A drive's own render on a GitHub backend does not ask the forge; it judges the follow-up list as the drive last read it, at the latest dispatch or result, against the ledger and backlog as they are when it renders.

`severity` is the frontmatter value verbatim, absent when the item declares none. Deliberately the raw string rather than a rank, so a project writing a scale drover does not recognize still gets its value out. [`--sev`](configuration.md#--sev) and `--by-sev` filter and order the array the same way they do the table.

`state` is one of `merged`, `implemented`, `ready`, `blocked`, `scaffold`, `broken`. `scaffold` means the body still carries the stop marker, so `run` will refuse it. `requirements` is set on a scaffold row only: how many numbered requirements already carry text. Absent means none are written and the item needs drafting; present means the list waits on `drover accept`. It stays one state rather than two because `drive` and every consumer keyed on `scaffold` read the same answer to "will run refuse this". `missing` names the unmerged dependencies behind a `blocked`. `deps` is every id the item declares on its `deps:` line, merged or not, and `blocks` every id whose own `deps:` line names this one, so a view can link an item to what it waits on and to what waits on it without reading a body. Both are absent when empty, and `blocks` is read from the whole backlog whatever `--all` or `--sev` print.

```json
{"id":"3","title":"Resolver","state":"blocked","missing":["2"],"deps":["1","2"],"planned":true},
{"id":"2","title":"Parser","state":"ready","blocks":["3"],"planned":true}
```

`level` and `by` are absent when nobody has audited it, and `by` alone is absent on a row that recorded no authorship, which `status` renders as `<level> (no authorship)`. drover does not fill one in: assigning a value would fabricate provenance. `by` is the `--code-by` list as typed, and `independent` is true when the auditor differed from every entry, read when the row was written.

`reviewer` is who the operator said looked, from the audit row's `reviewer`, and it is the only place a human reviewer appears: an agent reviewer leaves a `stage: review` row and a person leaves none. It is absent when `audit` ran without `--reviewer`, and unlike a completion row's `agent` it is never defaulted, so the absence means the row named nobody rather than the project default.

`implementer` and `review_runner` are the runners `run` and `review` recorded when they launched. Either is absent when no run of that stage was recorded, and an absent key means **unknown**, never the project default: a row with no stage recorded, or an item run outside drover, is a different answer from one drover watched a runner execute.

`runs` and `audits` are how many implement launches and how many audit rows the item carries, absent at zero under the same rule. They count rows written by two different commands and are not interchangeable: `runs` is what `drover run` recorded, `audits` is what `drover audit` recorded, and a review loop that keeps going shows in both. There is deliberately **no** `reviews` key. A review launch is recorded only when `review` resolved a runner, so a project that pipes `drover review` into something else would read zero there forever; `show` and the review prompt print that count in prose that can say what it counts.

The checked-out item carries `"current": true`.

`branch` is the branch the latest ledger row recorded for the item's work, from an implement or review launch, a pushed head or a pull request, whole however long. It is absent for an item nothing has run, and it is never the name `[run] branch` would render today.

`pr` is the item's pull request, absent when the forge lists none from its branch, the project has no branches, or no remote names a GitHub host. `state` is `open`, `draft`, `merged`, `closed` or `unknown`. `unknown` comes with `error`, which names the read that did not complete (`gh missing`, `not authenticated`, `forge unreachable`, `gh failed`, `no GitHub remote` on a number the ledger recorded, `--no-tracker`) followed by what gh said, so a degraded read says what it could not ask rather than reporting no pull request. `number` is absent only when the read failed and the ledger recorded none. A merged item whose number `open-pr` recorded carries `{"number":236,"state":"merged"}` without the forge being asked. One `gh pr list` call serves the whole array.

```json
{
  "id": "B2",
  "title": "drive parks twice",
  "state": "implemented",
  "planned": false,
  "pr": { "number": 240, "state": "unknown", "error": "not authenticated: exit status 4: ..." }
}
```

`issue` is the tracker issue the item's frontmatter `issue` key names, usually the one [`[backlog] mirror`](configuration.md#backlog) published it as. It is absent when the key is unset, unparsable or not a positive number, so a consumer building a link from it never sees `#0`. Unlike `pr` it asks no forge: the number is whatever the body records, open or closed. [`issue`](#template-functions) turns it into an address.

```json
{
  "id": "E56",
  "title": "Export the report as CSV",
  "state": "ready",
  "issue": 312,
  "planned": true
}
```

`started`, `stage`, `stage_started` and `finished` are when the item's latest run moved, read from the ledger's [`ts`](#writing-to-the-ledger-without-the-binary). `started` is the first stage row of the latest run, `stage` and `stage_started` the latest stage row, and `finished` the completion row that closed the run. A run opens at the first stage row after a completion or a tombstone, so an item reopened by `unmark-merged` reports when the work resumed. Each time is RFC 3339, converted to [`[status] timezone`](configuration.md#status) (UTC unless the project sets it, or `--tz` for one call) with that zone's offset, and each is absent when its row carries no `ts`: a row written before `ts` existed has a day and no time of day, and drover does not report midnight for it. `stage` is absent with `stage_started`.

`queued` marks a pending item a running `drive` holds in its `queue`, with that drive's `pid` and the item's 1-based `position`. `waits_for` is present when the drive holds the item for a dep that parked for the operator, and names that dep; see [Running items side by side](#running-items-side-by-side). The table carries it as `queued <n>` beside the state, with `, waits for <dep>'s merge` after it when `waits_for` is set, and names the queue under `▸ also here`.

`eta` is an **estimate**, present only for an item a running drive has dispatched or queued: when its review stage should end. It is the median duration of each stage the item has left, over every row in this project that carries `ts`, summed from now. A stage lasts from its stage row to the next stage, pass or completion row for the same item. A running item counts what is left of the stage it is in, never below zero; a queued item waits for the earliest of the drive's `jobs` slots to free, behind the running items and every queued item ahead of it. Fewer than 5 timed runs of either stage leaves `eta` absent rather than guessed. It covers implement and review only: the merge gate, the pull request and the merge open no stage row, so their time is not measured.

```json
{
  "id": "E35",
  "title": "Cache the session lookup",
  "state": "ready",
  "planned": false,
  "started": "2026-10-16T09:02:11-07:00",
  "stage": "implement",
  "stage_started": "2026-10-16T09:02:11-07:00",
  "queued": { "pid": 41234, "position": 2, "waits_for": "E34" },
  "eta": "2026-10-16T11:40:00-07:00"
}
```

#### trends

`trends` is what the ledger records over the last `--window` days (14 unless given, from 1 to 366), day by day and in total. It is always present, in `--json` and in a template's data, and is read from the ledger already loaded for the rest of the report, in one pass.

```json
"trends":{"window":14,"from":"2026-10-04","to":"2026-10-17",
 "days":[
  {"date":"2026-10-04","finished":null,"cycle_time":null,"review_passes":null,"changes_rate":null,"parks":null,"review_to_merge":null,"verify":null},
  {"date":"2026-10-16","finished":{"value":3,"n":3},"cycle_time":{"median":36000,"p90":86400,"n":3},
   "review_passes":{"value":1.5,"n":2},"changes_rate":{"value":0.333,"n":3},
   "parks":{"value":0,"by_reason":{},"n":0},"review_to_merge":{"median":38700,"p90":75600,"n":2},
   "verify":{"median":900,"p90":1200,"n":2}}],
 "total":{"finished":{"value":4,"n":4},"cycle_time":{"median":23400,"p90":86400,"n":4},
  "review_passes":{"value":1.333,"n":3},"changes_rate":{"value":0.25,"n":4},
  "parks":{"value":2,"by_reason":{"no_commits":1,"trunk_red":1},"n":1},
  "review_to_merge":{"median":3600,"p90":75600,"n":3},"verify":{"median":600,"p90":1200,"n":3}}}
```

`window` is the number of days, and `from` and `to` the first and last of them as `YYYY-MM-DD`, `to` being today. Days are calendar days in [`[status] timezone`](configuration.md#status) or `--tz`, and `days` holds one entry per day, oldest first, each carrying the same metrics as `total` beside its `date`.

| Field             | Unit                        | What it measures                                                                                                                                     | Day it belongs to            | `n`                                               |
| ----------------- | --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------- | ------------------------------------------------- |
| `finished`        | items                       | Completion rows (`"merged":true`)                                                                                                                    | The completion row's         | Items finished, equal to `value`                  |
| `cycle_time`      | seconds, `median` and `p90` | From the item's first implement stage row to its completion row                                                                                      | The completion row's         | Finished items with an implement stage row        |
| `review_passes`   | review launches per item    | Review stage rows between the item's previous finish and this one, averaged                                                                          | The completion row's         | Finished items with at least one review stage row |
| `changes_rate`    | share, `0` to `1`           | Finished items sent back by a review at least once, over all finished items                                                                          | The completion row's         | Finished items                                    |
| `parks`           | parks                       | Park rows; `by_reason` counts them by `park`                                                                                                         | The park row's               | Items parked                                      |
| `review_to_merge` | seconds, `median` and `p90` | From the item's last pass row reading `passed` to its completion row: mostly the wait on CI and the merge                                            | The completion row's         | Finished items with a `passed` pass row           |
| `verify`          | seconds, `median` and `p90` | A [`[verify]`](configuration.md#verify) command's wall time as drive ran it, from the row's [`verify_ms`](#writing-to-the-ledger-without-the-binary) | The row carrying `verify_ms` | Verify command runs                               |

A metric with nothing to read in a period is `null`, never `0`. For `finished` and `parks` that is a period holding no timed row at all: a day the ledger recorded work on and nothing finished is `{"value":0,"n":0}`, and a day it recorded nothing on is `null`. Every other metric is `null` when no item in the period has what it is computed from. The median of an even count is the mean of the middle two, and `p90` is the nearest-rank 90th percentile, so it is always a time some item took: over three items it is the slowest. Durations carry millisecond precision.

A row without `ts` is left out of every trend, since its time is a day in the writer's zone and placing it on a day of another zone would guess. Drive's own rows (`"id":"drive"`) are left out too. A tombstone (`"merged":false`) withdraws the finish before it. `review_passes` counts review launches drover resolved a runner for, so a project that pipes `drover review` reads `null` there rather than a count of zero. `verify` covers the runs drive makes itself, on an updated tree and after a resume that changed `[verify]`; the verify command a session runs inside its own run writes no row drover can time.

#### usage

`usage` on an item and on the report, and `long_sessions` on the report, read the ledger's [usage rows](#writing-to-the-ledger-without-the-binary). The counts are tokens as the API reported them, not the subscription's quota accounting, and `cost_usd` is list price, not what a subscription charged. `weighted` is `input + output + cache_create + 0.1 * cache_read`, computed when status reads the rows and never stored: it ranks which item, stage or session consumed the most, and it is not how the service counts against a limit.

```json
{
  "items": [
    {
      "id": "F193",
      "usage": {
        "runs": 7,
        "tokens": {
          "input": 2100,
          "output": 310000,
          "cache_create": 2400000,
          "cache_read": 98000000
        },
        "weighted": 12512100,
        "cost_usd": 61.4
      }
    }
  ],
  "usage": {
    "window": "2026-10-17T20:00:00-07:00",
    "runs": 9,
    "tokens": {
      "input": 3000,
      "output": 540000,
      "cache_create": 4100000,
      "cache_read": 170000000
    },
    "weighted": 21643000,
    "cost_usd": 104.2,
    "by_stage": {
      "implement": {
        "runs": 5,
        "tokens": {
          "input": 2100,
          "output": 310000,
          "cache_create": 2400000,
          "cache_read": 98000000
        },
        "weighted": 12512100,
        "cost_usd": 61.4
      },
      "interactive": {
        "runs": 4,
        "tokens": {
          "input": 900,
          "output": 230000,
          "cache_create": 1700000,
          "cache_read": 72000000
        },
        "weighted": 9130900,
        "cost_usd": 42.8
      }
    },
    "budget": 600000000,
    "budget_used": 0.62,
    "budget_unread": [
      "capy (/src/capy): open /src/capy/docs-internal/ledger.jsonl: permission denied"
    ]
  },
  "long_sessions": [
    {
      "session_id": "5f1c0e2a",
      "source": "interactive",
      "id": "",
      "hours": 424.3,
      "context_max": 965000,
      "weighted": 48210000,
      "reason": "open 17d 16h, past 24h; context reached 965K, past 400K",
      "started": "2026-10-01T09:00:00Z",
      "last_seen": "2026-10-18T01:18:00Z",
      "state": "last_seen"
    }
  ]
}
```

An item's `usage` sums every usage row whose `id` names it, all time: `runs` is how many rows, `tokens` the four counts added up, `weighted` the formula over those sums, rounded, and `cost_usd` the rows' list prices added up. An item no usage row names has no `usage` key, never a zero object, because nothing measured is a different answer from nothing spent.

The report's `usage` holds the same four totals over every usage row of this project whose `ts` falls in the window, the rows naming no item included, which is where an interactive session lands. `window` is `"7d"`, the 7 days before the report's `now`, or with [`[usage] reset`](configuration.md#usage) set, the RFC 3339 time of the last reset, which the window runs from to `now`. `by_stage` holds the totals per stage (`implement`, `review`, `draft`), with a row no stage launched under `interactive`. `budget` and `budget_used` are present only when [`[usage] weekly_budget`](configuration.md#usage) is set: `budget` is that figure and `budget_used` a fraction, the weighted total of every usage row in the window across every project in the [per-user registry](#projects---json), over it. The limit is per account, so this project's own `weighted` is not the number measured against it. Each ledger is counted once, however many registered roots reach it. `budget_unread` names each registered project whose config or ledger would not read, which the fraction then leaves out rather than counting as zero; it is absent when every one read. A ledger with no usage row has no `usage` key.

`long_sessions` is every session with a usage row in the window whose rows, taken together, span more than [`[usage] session_max_hours`](configuration.md#usage) (24 unless set) or reach a `context_max` past [`[usage] context_warn`](configuration.md#usage) (400000 unless set). The span runs from the earliest of the rows' `started`, `first_ts` and `ts` minus `duration_ms`, to the latest `ts`. `hours` is that span to one decimal, `context_max` the largest of the rows' and `weighted` the session's whole total, all of its rows, not only the window's; `id` is the item a row names, `""` for an interactive session. `reason` says which threshold it passed, in words a view prints as they are. `started` and `last_seen` are the two ends of that span, RFC 3339. `state` is `ended` when every one of the session's rows has `source` `headless`, and `last_seen` otherwise. It cannot say `open`, because no usage row is written while a session runs: a `headless` row is written when the session exits, so a session with only those has ended, and an `interactive` row is written per finished local day, so the day a session is in has no row until it ends and `last_seen` is the latest instant the ledger knows of, not proof it stopped. A running drive's sessions are in [`lease`](#the-checkout-lease) and `drives`, not here. The list is sorted by `weighted`, largest first. It is `[]` when the ledger holds usage rows and no session qualifies, and absent with no usage row at all.

### `status --template`

`drover status --template <file>` renders the object `status --json` emits through a Go [`text/template`](https://pkg.go.dev/text/template) instead of the table. The template reads the same field names `--json` does, lowercase and underscored (`.items`, `.id`, `.stage_started`, `.pr.number`, `.deps`, `.blocks`, `.drive`, `.drives`, `.trends`), plus `now`, the time of the render, [`attention`](#attention), `zone` (the zone's name, such as `America/Los_Angeles`), `project`, `version` (the drover binary's) and `refresh` ([`[status] refresh`](configuration.md#status)) and `index` ([`[status] index`](configuration.md#status), an empty string when unset). Numbers are JSON numbers and every time is an RFC 3339 string in [`[status] timezone`](configuration.md#status), `now` included, or in the zone `--tz` names. A field the JSON omits is absent from the template's data too: test it with `with` or `or` rather than printing it, or the output says `<no value>`. `--all`, `--sev` and `--by-sev` scope the items as they do for `--json`, and `--template` is refused together with `--json`. `--template builtin:<name>` renders a template drover ships rather than a file: see [Built-in templates](#built-in-templates).

A template that does not parse or does not execute exits `1`, and the message names the file and the line, as `text/template` reports them:

```bash
drover: template: docs/status.tmpl:4:12: executing "docs/status.tmpl" at <.pr.number>: ...
```

This renders a markdown table of every item, with the finish time where the run closed and the estimate where a drive has it:

```text
| ID | PR | State | Started | Finished or ETA | Title |
| --- | --- | --- | --- | --- | --- |
{{- range .items}}
| {{.id}} | {{with .pr}}{{with .number}}#{{.}}{{end}}{{end}} | {{.state}}{{with .queued}} (queued {{.position}}){{end}} | {{or .started "-"}} | {{with .finished}}{{.}}{{else}}{{with .eta}}~{{.}}{{else}}-{{end}}{{end}} | {{.title}} |
{{- end}}

Rendered {{.now}}.
```

#### Template functions

Each function takes the field as `--json` carries it and returns a string. An absent or unreadable argument gives `""` rather than failing the render, so `{{ago .finished}}` on an item that never finished prints nothing.

| Function | Example                                              | Renders                                                      |
| -------- | ---------------------------------------------------- | ------------------------------------------------------------ |
| `class`  | `{{class .state}}` on an `implemented` item          | `running`                                                    |
| `ago`    | `{{ago .stage_started}}`, 14 minutes before `now`    | `14m ago`                                                    |
| `dur`    | `{{dur .started .finished}}`, 72 minutes apart       | `1h 12m`                                                     |
| `time`   | `{{time "Jan 2 15:04 MST" .started}}`                | `Oct 17 09:30 PDT`                                           |
| `anchor` | `{{anchor .id}}` on `E37`                            | `e37`                                                        |
| `pr`     | `{{pr .pr.number}}` on 236                           | `https://github.com/acme/widgets/pull/236`                   |
| `issue`  | `{{issue 12}}`                                       | `https://github.com/acme/widgets/issues/12`                  |
| `md`     | `{{md .title}}` on a title holding a pipe            | the pipe, escaped with a backslash                           |
| `html`   | `{{html .title}}` on `<b>&`                          | `&lt;b&gt;&amp;`                                             |
| `esc`    | `{{esc .title}}` on `it's <b>`                       | `it's &lt;b&gt;`                                             |
| `groups` | `{{range groups .items .drives .lease}}`             | the open items by drive, then by state                       |
| `done`   | `{{range done .items}}`                              | the merged items, most recently finished first               |
| `open`   | `{{len (open .items)}}`                              | the number of items not merged                               |
| `spark`  | `{{spark .trends.days "verify" "median"}}`           | an inline SVG sparkline of the verify command's daily median |
| `secs`   | `{{secs .trends.total.cycle_time.median}}` on 23400  | `6h 30m`                                                     |
| `pct`    | `{{pct .trends.total.changes_rate.value}}` on 0.25   | `25%`                                                        |
| `num`    | `{{num .trends.total.review_passes.value}}` on 1.333 | `1.333`                                                      |
| `tokens` | `{{tokens .usage.weighted}}` on 12512100             | `12.5M`                                                      |
| `usd`    | `{{usd .usage.cost_usd}}` on 1234.56                 | `$1,235`                                                     |
| `budget` | `{{budget .usage.budget_used}}` on 0.85              | `warning`                                                    |

`class` maps a state, a park reason or an audit level to one of nine names a stylesheet or a callout can key on: `ready`, `running`, `blocked`, `parked`, `scaffold`, `done`, `broken`, `needs-operator`, `unaudited`. `implemented` is `running`, since the work is in flight and not merged. The levels `claimed`, `reviewed` and `verified` are `done`, and `UNAUDITED` is `unaudited`. Any other value is a park reason: `needs-operator` when the [supervise](configuration.md#supervise) action that owns it is at `operator` policy (`trunk_red` by default), and `parked` otherwise. An absent value is `""`, so `{{class .park}}` on an item nothing parked adds no class. Pass `.stage` to nothing here: a stage is not one of these vocabularies, and `class` reads it as a park reason. An [`attention`](#attention) `kind` reads as its own class, with `needs_operator` as `needs-operator`; `follow-up` has none.

`ago` reads a time against `now`: whole minutes under an hour, whole hours under a day, whole days after (`14m ago`, `2h ago`, `3d ago`). A time after `now`, such as an `eta`, reads `in 30m`. `dur` is the span between two times in either order: `12m`, `1h 12m`, or `2d 3h` past a day. `time` formats with a Go [layout](https://pkg.go.dev/time#Layout) in the configured zone.

`anchor` lowercases an id and turns anything outside `a-z`, `0-9` and `-` into `-`, so `[E37](#{{anchor .id}})` and a heading for the same item agree.

`pr` and `issue` build the forge's address for a number, from the repository `gh repo view` names, asked at most once per render and only when a template calls one. Both give `""` when the forge is unknown: `gh` could not say, `--no-tracker` was given, or the render is one a drive makes, which asks the forge nothing. `issue` gives `""` as well when the follow-up list is not GitHub's, because a file register's numbers have no page there.

`groups` and `done` return lists rather than strings, for the two views a template cannot build with `range` and `if` alone. `groups` drops merged items and returns one entry per drive, in the order `drives` lists them, then one with an empty `drive` for the items no listed drive ran. Each entry carries `drive`, `label` and `states`, and each of `states` carries a `state`, its `class` and the `items` in it, in backlog order. `state` is the word to print and `class` the name to style it with, and the two differ: an `implemented` group has state `implemented` and class `running`. An item whose id is in the lease's `running` list has state `running`, whatever its backlog state or park, so the Open list agrees with the running drive. Pass `.lease` as the third argument; with no lease, every item keeps its backlog state. The states come in a fixed order, what waits on a person first: `needs-operator`, `parked`, `broken`, `running`, `implemented`, `ready`, `blocked`, `scaffold`. An item a park stands on takes the park's class as its state, `parked` or `needs-operator`, so it sits with the other parks rather than among the ready items. `done` is every merged item, newest `finished` first, and an item with no `finished` last. `open` is every item that has not merged, in backlog order, ungrouped, which is what a count wants.

`spark` draws one [`trends`](#trends) metric across `.trends.days` as an inline `<svg>` of 120 by 24 with the class `spark`: the second argument names the metric and the third the value inside it (`value`, `median`, `p90` or `n`). A day the metric is `null` on is a gap in the line rather than a point at zero, and a value standing alone between gaps is a dot. The markup holds numbers and the two names alone, uses no script and fetches nothing, and `html/template` places it unescaped. `secs` prints seconds in their two largest units (`42s`, `14m 5s`, `6h 30m`, `3d 2h`), `pct` a share as a whole percentage, and `num` a number with no trailing zeros. All three give `""` for a `null`, so test a metric with `with` and print `n/a` in the `else`.

`tokens` prints a count in its largest unit with one decimal at most, trailing `.0` dropped: `840`, `1.3K`, `840K`, `12.5M`, `1.2B`. A value that rounds to the next unit's 1000 is written in that unit, so 999,950 is `1M`, and nothing goes past `B`. `usd` prints a figure as whole dollars, rounded, with thousands separators: `$0`, `$999`, `$1,235`. Both read counts and list prices, so the caveat on [`usage`](#usage) holds for what they print: tokens, not the subscription's quota, and list price, not what a subscription charged. `budget` is the class a `budget_used` fraction is styled with, `warning` from 0.8, `critical` from 1.0 and `""` below, which the built-in templates and `drover status --print-css obsidian` style in the warning and critical colors.

`md` escapes a string for a Markdown table cell: a backslash doubles, `|` becomes `\|`, and a line break becomes a space. `html` escapes `<`, `>`, `&`, `'` and `"`. `esc` escapes only `<`, `>` and `&`, which is what Markdown needs to keep a value from opening a tag or an entity: Obsidian shows `html`'s `&#39;` as typed, so a title reading "place's" would read "place&#39;s".

This renders an Obsidian table that colors each row, says how long ago it moved, and links a blocked item to what holds it:

```text
| ID | State | Moved | Waiting on |
| --- | --- | --- | --- |
{{- range .items}}
| [{{.id}}](#{{anchor .id}}) | <span class="{{class .state}}">{{.state}}</span> | {{ago .stage_started}} | {{range .missing}}[{{.}}](#{{anchor .}}) {{end}}|
{{- end}}
```

#### attention

`attention` is the list a person reads first: one entry per thing waiting on someone, each with the command that answers it.

```json
{
  "attention": [
    {
      "kind": "needs_operator",
      "id": "E38",
      "reason": "trunk_red: unknown",
      "since": "2026-10-16T22:41:00-07:00",
      "command": "fix the trunk, then drover drive E38"
    },
    {
      "kind": "parked",
      "id": "E39",
      "reason": "no_commits: runner_exit",
      "since": "2026-10-16T23:05:00-07:00",
      "command": "drover drive E39"
    },
    {
      "kind": "scaffold",
      "id": "B4",
      "reason": "requirements unwritten",
      "command": "write B4's requirements, then drover accept B4"
    },
    {
      "kind": "unaudited",
      "id": "E31",
      "reason": "merged, and nobody has audited it",
      "since": "2026-10-15T18:02:00-07:00",
      "command": "drover audit E31 verified --code-by agent \"<what you re-checked>\""
    },
    {
      "kind": "follow-up",
      "id": "12",
      "reason": "the contract checks assert format, not behavior (unowned: no item owns it)",
      "command": "make #12 a sub-issue of the owning item's issue or add an Owner: <id> line, or close it"
    }
  ]
}
```

`kind` is one of these:

| `kind`           | Read from                                                                                                                                       | `since`                                              |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `needs_operator` | A park whose [supervise](configuration.md#supervise) action is at `operator` policy                                                             | The park row                                         |
| `parked`         | Any other park standing on the item                                                                                                             | The park row                                         |
| `scaffold`       | An item still carrying the intake stop marker; `reason` says whether its requirements are unwritten or drafted and waiting on `accept`          | Absent: no row records it                            |
| `unaudited`      | A merged item with no audit, the rows [`unaudited`](#unaudited---json) ranks                                                                    | `finished`                                           |
| `follow-up`      | A follow-up that is unowned, or orphaned because every owner merged, as [`follow-ups`](#follow-ups---json) reads them; `id` is the issue number | Absent: drover reads no date from the follow-up list |

`command` is the supervise catalog's sentence for the action that owns the park, with the id filled in, or `drover drive <id>` for a park no action owns. Items come in backlog order, then the follow-ups. `attention` covers the whole backlog whatever `--all` or `--sev` print, since a merged item nobody audited waits on someone whether or not the view shows it.

A source that could not be read is an entry rather than an absence. A follow-up list `gh` could not reach, or one nobody asked because of `--no-tracker` or because a drive is rendering, is one `follow-up` entry with no `id` whose `reason` names why and whose `command` is `drover follow-ups`. An empty `attention` therefore means nothing waits on anyone.

`attention` is in the template's data and not in `status --json`, because it reads the follow-up list, which `--json` never asks.

#### Drive grouping

`drive` on each item and `drives` on the report reach the template as [`status --json`](#status---json) carries them, so a view can group what one drive ran:

```text
{{- range .drives}}
## {{.id}}{{with .label}} {{.}}{{end}}{{if .running}} (running){{end}}
{{- $d := .id}}{{range $.items}}{{if eq (or .drive "") $d}}
- {{.id}} {{.title}}{{end}}{{end}}
{{- end}}
```

#### Built-in templates

drover ships two templates in the binary, so a dashboard can start from one that works:

| Template           | Writes                                                                             |
| ------------------ | ---------------------------------------------------------------------------------- |
| `builtin:obsidian` | Markdown for an [Obsidian](https://obsidian.md) vault                              |
| `builtin:html`     | One self-contained HTML5 page; `--html` is shorthand for `--template builtin:html` |

Name one with `--template builtin:<name>` or in a [`[[status.render]]`](configuration.md#status) `template`. An unknown name is refused with the list of built-in ones, on the command line and when the config loads. `drover status --print-template builtin:<name>` prints a template to copy and change, and `drover status --print-css obsidian` prints the CSS snippet the Obsidian one expects. Neither reads a project.

Both open on a contents line linking each section below by name, with a count beside Needs you (the [`attention`](#attention) rows), Open (`open`) and Done (`done`). Both render, in this order:

1. **Needs you:** the [`attention`](#attention) list, or a line saying nothing is waiting, then [`long_sessions`](#usage) under Long sessions, each with its `state`: `ended`, or `last seen` and how long ago.
2. **Running drive:** the drive the lease names, with each running item's stage, the time since that stage started and its `eta`, and the queue.
3. **Open:** [`groups`](#template-functions), each item linking its `deps` and `blocks` to their rows.
4. **Done:** [`done`](#template-functions), each item with its audit level, or `unaudited`, and its pull request.
5. **Trends:** every [`trends`](#trends) metric over the window with its `n`, `n/a` where it is `null`. `builtin:html` draws each across the days as a [`spark`](#template-functions) line; `builtin:obsidian` writes a table with a row per day and a last row for the window, then the parks by reason.
6. **Key:** what each state's color means.
7. A footer naming the render time and zone, the project and the drover version.

Both also render the data [`status --all-projects`](#status---all-projects) hands them, as an index of the registered projects: one row each, with the running drive, its items, its queue, the attention count, the items finished in the window and the last finish, or the project's error. A project whose `[[status.render]]` names a dashboard of the template's own format (a path ending `.html` for `builtin:html`, `.md` for `builtin:obsidian`) is linked to it. A template tells the two kinds of data apart by whether `.projects` is present, which the built-in ones test as `{{if ne (printf "%T" .projects) "<nil>"}}`.

A built-in template carries every item, as a `[status] render` does, because its done list is part of the view; `--sev` and `--by-sev` beside one are refused. Copy it with `--print-template` and filter in the copy.

Every state is written as a word and carries its [`class`](#template-functions) as `drover-<class>`, so the color adds to the text and never replaces it. The stylesheet colors `done` and `ready` green, `running` yellow, `blocked`, `scaffold` and `unaudited` orange, and `parked`, `broken` and `needs-operator` red, each with a light and a dark value.

`builtin:obsidian` writes each state as `<span class="drover-<class>">`, and each item's row ends in a block id (`^b4`), which every link to it names: `[[#^b4|B4]]`. Its first line is an Obsidian comment saying where it came from. Save the output of `--print-css obsidian` as `.obsidian/snippets/drover.css` and enable it under Settings, Appearance, CSS snippets. Titles, reasons and labels pass through `esc`, since Obsidian renders HTML inside Markdown but shows an entity for a quote literally. Each section is a `##` heading, which the contents list links as `[[#Open|Open (12)]]`, over a collapsible callout holding its content: `> [!note]+` for Needs you, open by default, and `> [!note]-`, folded, for the rest.

`builtin:html` executes through [`html/template`](https://pkg.go.dev/html/template), so every value is escaped for where it ends up: a title holding `<script>` arrives as text. The CSS is inline and the page fetches nothing: no web fonts, external scripts, images or CDN. The only addresses in it are forge links to pull requests and issues.

Each section is a `<details open>` with its heading as the `<summary>`, its content in a bordered block on the stripe color, and Needs you's border red while it holds an attention row. One inline script keeps which sections are open across the reload: it saves their state to `localStorage` under `drover-sections:<project>` on every toggle and restores it on load, and following an in-page link opens the section the target sits in. Every storage read and write is inside `try`, so a browser that refuses storage, or one that runs no script, shows every section open. It follows `prefers-color-scheme`, fits a phone's width, and reloads itself every 60 seconds through `<meta http-equiv="refresh">`; [`[status] refresh`](configuration.md#status) changes the interval, and `0` removes the tag.

Needs you, Running drive, each Open state and Done are tables with a header row and one row per item, so the fields line up down a column:

| Section       | Columns                                                            |
| ------------- | ------------------------------------------------------------------ |
| Needs you     | state, item, reason, since, command                                |
| Running drive | item, title, stage, elapsed, ETA                                   |
| Open          | item, title, severity, stage or park reason, deps, blocks, PR, ETA |
| Done          | item, title, level, PR, merged                                     |

A cell with no value shows `-`. Each row keeps its state word as a badge (Done's is the level), so nothing relies on color. Each Open and Done row has an `id` (`item-b4`), which every deps, blocks, Needs you and queued link points at, and a row reached that way is outlined. In those rows the item id links to its [`issue`](#status---json) when it names one and is plain text otherwise; the PR cell links the pull request. Each table scrolls sideways inside its own box at phone width rather than widening the page, and down inside a box of 80% of the window, under a header row that stays put. Rows stripe and highlight on hover in both palettes.

##### Exposure

The HTML page lists item titles, follow-up notes, park reasons and the commands that answer them. Serve it from a host only the team can reach, never from a public site. drover runs no server: it writes a file, and any static web server can serve the one a `[[status.render]]` pair keeps current:

```toml
[[status.render]]
template = "builtin:html"
path = "/srv/www/drover/index.html"
```

#### Output

`-o <path>` writes the rendered template, or `--json`, to that path instead of stdout. The write goes to a temp file in the same directory and is renamed into place, so a reader of the path sees the old file or the new one and never half of either. A file already at the path is refused with exit `3` unless `--force` is given. A leading `~/` resolves against `$HOME`. `-o` without `--template`, `--html` or `--json` is an error: the table is not a document.

`--render` renders every [`[status] render`](configuration.md#status) pair once, which is what `drive` does after each ledger row it appends and each change to its queue. Those renders carry every item, whatever `--all` or `--sev` say, so a file kept current over a drive shows the items it finishes. A drive's renders ask the forge nothing, and a pull request number comes from the ledger or not at all; `status --render` asks unless `--no-tracker` is given. A render that fails during a drive is reported once on stderr and never parks, stops or fails the drive.

### `status --all-projects`

`drover status --all-projects` reads every project in the per-user registry, as [`drives --all`](#drives---json) does, and prints one row per project in place of one project's items. It needs no project, so it answers from anywhere.

<!-- markdownlint-disable MD040 -->

```
PROJECT  DRIVE           ITEM  QUEUE  NEEDS YOU  FINISHED  LAST FINISHED
webapp   D7 "night run"  F12   3      2          9         2026-10-17 04:12
api      -               -     -      5          12        2026-10-17 11:40
  worktree /src/api-b80
old      unreadable: /src/old/.drover.toml: toml: line 3: expected '='
2 registered roots left out: gone or no .drover.toml; 3 scratch projects under /tmp left out (drover projects lists them, drover projects --forget <root> forgets one)
```

<!-- markdownlint-enable MD040 -->

A registered root that is gone or holds no `.drover.toml` is a record of where drover was once pointed, usually a scratch tree, not a project, so it gets no row. A project under the OS temp directory (`os.TempDir()`, which is `$TMPDIR`) whose config loads is a scratch project: real, but a reproduction rather than something the host drives, so it gets no row either. The one footer line counts the two kinds apart and names the command that lists them and the one that [forgets](#projects---json) them. Under `--json` the counts are `skipped` and `scratch`, each absent at zero, and `temp_dir` names the directory beside a `scratch` count; with `--template` or `--html` the footer goes to stderr so the page carries none of it. A root whose `.drover.toml` exists and will not load still gets its row, wherever it is: that is a broken project.

A registered root that is a git worktree of another registered root (its `git rev-parse --git-common-dir` is the other's git dir, at the same path inside the repository) is that project's child. It is shown under the parent's row as `worktree <path>` and never as a row of its own, and under `--json` it is a path in the parent's `worktrees`. The built-in templates render it the same way. Two projects in different directories of one repository share a git dir without being worktrees of each other, so neither folds into the other.

`--json` emits an object, and `--template` renders the same object, whose data is `projects`, `now`, `skipped`, `scratch` and `temp_dir` and nothing else:

```json
{
  "projects": [
    {
      "project": "webapp",
      "root": "/src/webapp",
      "drive": "D7",
      "label": "night run",
      "running": ["F12"],
      "queue": 3,
      "paused": true,
      "attention": 2,
      "finished": { "value": 9, "n": 9 },
      "last_finished": "2026-10-17T04:12:00Z",
      "renders": [
        {
          "template": "builtin:html",
          "path": "/srv/www/webapp/index.html",
          "href": "webapp/index.html",
          "format": "html"
        }
      ]
    },
    {
      "project": "api",
      "root": "/src/api",
      "attention": 5,
      "finished": { "value": 12, "n": 12 },
      "last_finished": "2026-10-17T11:40:00Z",
      "worktrees": ["/src/api-b80"]
    },
    {
      "project": "old",
      "root": "/src/old",
      "error": "/src/old/.drover.toml: toml: line 3: expected '='"
    }
  ],
  "now": "2026-10-17T18:00:00Z",
  "skipped": 2,
  "scratch": 3,
  "temp_dir": "/tmp"
}
```

`project` is the name the registry recorded and `root` its checkout. `drive` and `label` are the drive holding that checkout, `running` the items it has in a stage now, `queue` how many wait behind them and `paused` whether [`drive --pause`](#pausing-a-drive) holds it, all five absent when no drive runs there. `attention` is how many entries the project's [`attention`](#attention) list holds. `finished` is the window's [`trends`](#trends) `total.finished`, `null` when the window holds no timed row, and `last_finished` the newest completion row the ledger holds, window or not. `renders` is each [`[[status.render]]`](configuration.md#status) pair, with `path` absolute, `href` the same file relative to the directory `-o` writes into (or `path` again without `-o`) and `format` the path's extension. `lease_error` is present when the checkout's holder could not be read. `worktrees` is the root of each registered worktree of the project, absent when there is none.

A project that could not be read is listed with `error` and nothing beside its name and root: a config that will not load, a backlog or a ledger that will not open. A project left out would read as one that does not exist.

Each project's `attention` asks the forge for its follow-ups as `status --template` does, and `--no-tracker` asks nothing, which makes each one count the entry saying the follow-up list was not read. Times are in `--tz` or UTC, since no one project's `[status] timezone` speaks for the rest. `--window` sizes `finished`. `--all`, `--sev` and `--by-sev` choose items and are refused beside it, and so is `--render`.

### `drive --json`

**One object per line, not an array**, flushed as each item ends. It is the one exception to the rule above, and it earns it: every other `--json` answers a question about a state that already exists and returns in milliseconds, while this document is produced over hours and its most valuable reader is somebody reading a drive that died. An array is only writable by a process that finished.

```json
{"id":"B1","title":"First","branch":"b1-first","outcome":"ready_to_merge","rounds":1,"reviews":2,"changes_requested":1,"audits":2,"implementer":"claude","review_runner":"agy","started":"2026-09-13T22:00:00Z","ended":"2026-09-13T22:41:00Z"}
{"id":"B4","title":"Fourth","branch":"b4-fourth","outcome":"parked","park":"needs_decision","reason":"the review wanted to send work back, and the item is at [review] max_review_rounds (4), so the findings went to the operator rather than the branch: .drover/reports/04-fourth.refused-1.md","rounds":4,"reviews":5,"changes_requested":4,"audits":5,"started":"2026-09-13T22:41:00Z","ended":"2026-09-14T00:12:00Z"}
```

`started` and `ended` are RFC 3339 times, and both are present on every row whatever its outcome or park: `ended` is when drive finished with the item, and is never earlier than `started`.

`tree` is the item's worktree, present only while one is left standing for you: an item parked on `uncommitted_work` keeps its worktree and names it here, and a worktree that ended clean is removed and the field omitted.

`detail`, `evidence` and `log` are on every parked row: why the session or the push under the park failed, the line that says so, and the log it came from. See [Park details](#park-details).

Objects arrive in the order items finish. At `--jobs 1`, the default, that is the order they were dispatched. Above 1 each object carries `slot`, the slot the item ran in, numbered from 1; at `--jobs 1` the field is absent.

`findings` is every finding attributed to the item's sessions, absent when there are none: `{ref, kind, title, owners, coverage, blocked_on, ambiguous, filed_by}`, as the [`filed` event](#drive---events) carries it. `register_error` is why the follow-up list would not read at the item's dispatch or result; it changes nothing else about the item.

```json
"findings":[{"ref":"851","kind":"follow-up","title":"the retry budget is per process","owners":["F126"],"coverage":"blocked","blocked_on":{"F126":["F120","F124"]},"filed_by":["F119"]}]
```

The last object, after every item, is the drive's totals, with or without `--events`: each coverage value's count, and every finding nothing is scheduled to carry (a follow-up at `blocked`, `orphaned` or `unowned`, and every scaffold). It carries neither `id` nor `event`. The stderr summary prints the same counts and lists those findings when there is any.

```json
{
  "findings_by_coverage": {
    "blocked": 1,
    "orphaned": 0,
    "queued": 0,
    "scaffold": 0,
    "unowned": 0,
    "unqueued": 0
  },
  "findings_needing_triage": [
    {
      "ref": "851",
      "kind": "follow-up",
      "owners": ["F126"],
      "coverage": "blocked",
      "blocked_on": { "F126": ["F120", "F124"] },
      "filed_by": ["F119"]
    }
  ]
}
```

#### Running items side by side

`--jobs N` runs up to N items at once, and splits each item in two:

| Stage                | Runs                                            | What is in it                                                                                                                                                      |
| -------------------- | ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Implement and review | Up to N items at once, each in its own worktree | The worktree, the implement run, the push of what it committed, the review, and every round a review sends back                                                    |
| Trunk                | One item at a time, in dispatch order           | The push after the review, the branch-update merge of origin's trunk, the item's `[verify]` run on that tree, `[drive] merge_gate`, the pull request and the merge |

The trunk stage is serialized because a branch update merges whatever origin's trunk gained and the merge gate then reads that tree: two items updating their branches at once would each be gated against a trunk the other had already moved. A new worktree's fast-forward of the local trunk also waits for any item inside the trunk stage. An item whose review passed waits for every item dispatched before it to finish the stage or end without needing it, so a fast item can sit behind a slow one.

Two items whose bodies cite an overlapping path never run at once: the later one is not dispatched until the earlier finishes or parks, and a later item never overtakes an earlier one it overlaps, unless that one waits for an operator's merge as described below. A path is a slash-separated name whose last part carries an extension (`internal/cmd/drive.go`, with any `:120-145` dropped), or any slash-separated name in backticks (`` `internal/lease` ``); a directory overlaps every path below it. An item citing no path overlaps nothing. An item never runs beside one of its deps that is running or earlier in the queue either: it waits until the dep finishes or parks, then starts and reads the dep's state as it would at `--jobs 1`, skipping as `blocked` if the dep parked for any cause the operator does not resolve. A dep that parks with a `needs_operator` event (`protected_paths` at the default `operator` policy, for one) holds its dependents instead, at every `--jobs`: each stays pending at its place in the queue, later items that do not depend on it overtake it, including one that only cites a path it cites (a held item is not running, so nothing runs beside it, and once its dep merges it waits for any running item it overlaps), and `status` lists it as `queued <n>, waits for <dep>'s merge`. Before each dispatch decision the drive rereads the ledger, and once it records the dep merged (a `finish` or `mark-merged` the operator ran while the drive runs) the dependent dispatches ahead of every item queued after it. The wait lasts only while the drive has other work: when nothing is running and every pending item waits on such a dep, directly or behind one that does, each one waiting directly skips with a `skipped` event whose reason reads `blocked by <dep>, which parked <park> for the operator`, the items behind it then start and read their own state, and the drive exits as a drained drive does. A dep named after the item in the same drive does not hold it, so the item skips as `blocked` at its turn whatever `--jobs` is. `drive --dry-run --jobs N` prints the waves it would dispatch, each assuming the one before it finished, and names every pair it holds apart with the path or the dependency that caused it. At any `--jobs`, the preview lists an item blocked only by deps named earlier in the same drive rather than skipping it, and says it runs once they finish.

A stop (`trunk_red`, `runner_fault`, `forge_down`, a merge gate refusing a head the forge already merged) or an interrupt dispatches nothing more. Each running item finishes the stage it is in, launches nothing after it, and parks `stopped`; an item waiting for its turn at the trunk stage parks `stopped` without updating its branch. The drive then exits as a stop always does, with the first stop's reason. An interrupt also ends the sessions in flight, as it does at `--jobs 1`. `--max-items` and the deadline count dispatched items, so an item already running when the clock runs out finishes. `--follow` adds each item a finish unblocked to the items waiting for a slot as the finish is reported, and it is dispatched into the next slot nothing earlier in the queue can take.

Stdout carries the document and nothing else. Every count, warning and summary goes to stderr, so `drover drive --json > night.jsonl` keeps a file `jq` can read while the operator still sees the run.

Each park is said on stderr the moment it happens, before the next item starts, as an indented `! <id> parked <park> at <head>: <first line of the reason>`. `<head>` is the branch's commit, shortened, or `no head` where the branch has none. The summary after the queue drains repeats every park; the line is what an operator watching `--follow` sees in time to act on it.

`outcome` is one of `ready_to_merge`, `merged_by_drover`, `parked`, `skipped` or `held`. `held` is an item a [usage hold](#drive---events) kept from starting or finishing when the drive's `--for` or `--until` arrived first: the drive exits 0, `reason` names the hold, nothing parked and no park row is written, and the next drive picks the item up. `merged` says the forge reported the pull request merged; it is absent on an item nothing merged: a park, a branch `protected_paths` held, or a run under `--no-merge`.

`merged_by_drover` is the merge drive made itself, on `[drive] merge_gate`'s word: `gh pr merge --match-head-commit` on the head the merge gate passed, never `--auto`. drive then asks the forge whether the pull request merged. Such an item carries `merged`, `pr`, and `finished` when drive also recorded it in the same drive (ledger row, saved report and tracker closes, as `finish` does). `finished` is absent where the operator's checkout could not take the records cleanly, and the reason says the next drive finishes it. A pull request the forge does not report merged after the call, because a required merge queue holds it or the read failed, leaves the item `ready_to_merge` with a reason saying the next drive's reconciliation records it once it lands. See [The merge](lifecycle.md#the-merge).

`merged_immediately` and `merge_queued` appear only on end rows written by a drover that still asked for `--auto`: `merged_immediately` was an `--auto` request the forge merged on the spot, and `merge_queued` one it accepted and had not merged. Reconciliation still finishes an item whose ledger holds such a queue.

```json
{
  "id": "F13",
  "title": "Thirteenth",
  "branch": "f13-merge",
  "outcome": "merged_by_drover",
  "reason": "[drive] merge_gate passed 3f2a9c1e0b7d and drover merged it",
  "rounds": 0,
  "reviews": 1,
  "changes_requested": 0,
  "audits": 1,
  "pr": 212,
  "merged": true,
  "finished": true,
  "started": "2026-10-05T22:00:00Z",
  "ended": "2026-10-05T22:40:00Z"
}
```

The summary on stderr counts these on their own (`· 2 merged by drover`, `· 1 merged immediately`), so a reader auditing what reached the trunk unattended can tell them from the merges branch protection decided.

Before the merge gate reads a branch, drive merges origin's trunk into it when the branch does not already contain it, runs the item's own verify command on the merged tree, then pushes the merge commit and waits for that head's checks. A row whose branch update merged something carries `update_branch`, and the field is absent when the branch already held the trunk. A branch can be updated more than once in a drive, before the pull request opens and again before a `merged_by_drover` merge; the row reports the last branch update that merged something, so a later one that found the branch up to date leaves the earlier one in place:

```json
"update_branch":{"from":"9b1e…","to":"3f2a…","verify":"passed"}
```

`from` is the trunk commit the branch last held and `to` the `origin/<trunk>` commit merged in. `verify` is `passed`; `failed` (the merged tree failed and the trunk alone passed, so the item was sent back and parked `gate_refused`); `trunk_red` (the trunk failed the verify command alone); `skipped` (no verify command applies to the item, so nothing checked the merged tree); or `unrunnable` (the trunk's worktree would not set up, or an interrupt stopped the verify command). The summary on stderr adds the same fact to the item's outcome, as in `updated to main@3f2a9c1e0b7d; verify command passed`. The branch update runs whenever `[drive] merge_gate` is declared, under `--no-merge` too, because the merge gate reads the branch either way. `protected_paths` and the review's diff are measured from where the branch left the trunk, counting origin's, so a file the trunk gained never reads as the item's. See [Keeping each branch level with the trunk](lifecycle.md#keeping-each-branch-level-with-the-trunk); a conflict is the `trunk_conflict` park below.

`--follow` adds, after each finish, the ready items that list the finished one in `deps` to the items waiting for a slot, so their rows follow in the same stream. Only a finish in this drive counts, which means a `merged_by_drover` or `merged_immediately` row with `finished`. An item named later in the same drive that depends on one of those runs at its turn rather than being skipped as blocked.

Reconciliation runs before the queue is read, only over a clean tree, and finishes only items drover launched, which a stage row in the ledger is the evidence for. A merged branch alone does not say a drive produced the work: an item implemented in the operator's own session leaves the same branch and the same merge, and adopting it would have drive record a completion for work it never drove and never reviewed. Those are named and left alone, with the `drover finish` that records them. Reconciliation finishes under `--no-tracker`, so no issue is closed or commented on before the first launch of the night.

Reconciliation refuses a finish that would leave the tree dirty, before `finish` writes anything, and the refusal ends the drive with exit 3 whether or not any item is drivable: no completion row is written, nothing switches, and the `reconciled:` line counts the items it finished before the stop. It refuses when `git status --porcelain` reports any path before the finish, and when the ledger row or the report `finish` is about to write is a path git does not ignore inside the Git worktree the checkout changes (not the directory holding `.drover.toml`). A path outside that worktree, or one git ignores, is exempt. Which path counts depends on how `finish` writes it. The ledger is appended to, so every symlink resolves, a dangling one included, because opening it creates the file it names. The report is written to a temporary file and renamed over its path, so its directories resolve and the final entry does not: a tracked report symlink is replaced where it stands, however far outside the worktree it pointed. A failed `git status`, `git check-ignore` or symlink resolution stops the drive with exit 1 rather than reading as clean. Before asking the forge about an item, reconciliation lists it as running in the drive's lease; a lease rewrite that fails stops the drive with exit 1 there, before the forge is asked or anything is written, because an item missing from that list is one `finish` or `mark-merged` from another process would not refuse.

`park` is the machine-readable cause and `reason` the sentence for a person. Two fields because a wrapper keying on prose breaks the day somebody rewords an error. The causes are `died`, `runner_fault`, `unreadable`, `no_record`, `findings_open`, `no_digest`, `body_edited`, `repeating`, `needs_decision`, `implement_died`, `implement_unreadable`, `uncommitted_work`, `worktree_failed`, `no_commits`, `gate_refused`, `gate_unrunnable`, `forge_down`, `trunk_conflict`, `trunk_red`, `infra_failure`, `protected_paths`, `deny_unreadable`, `awaiting_signoff`, `push_failed`, `open_pr_failed`, `blocked_on_deps`, `hand_edited`, `ledger_unwritable`, `merge_failed`, `branch_failed`, `interrupted` and `stopped`; a `skipped` item carries the state that skipped it instead, one of `merged`, `scaffold`, `blocked` or `broken`.

`body_edited` compares the item file's exact bytes, with no whitespace normalization. A formatter, an editor's format-on-save or a snapshot hook that rewrites the body during a review counts as an edit and parks the pass `body_edited`: a normalization rule would be a set of edits drover chose to trust without seeing them.

`pr` is the pull request drover opened for the item, and it is absent wherever none exists. Only a green `[drive] merge_gate` on a review that passed opens one, so every park before that point reports none; `blocked_on_deps`, `awaiting_signoff` and `protected_paths` report one, because each is a branch a human merges and a human needs something to merge. A zero on an item that reached the forge means `gh` printed a URL drover could not read a number out of, which is not a failure: the pull request is open either way.

`gate_refused` with no `pr` is the merge gate blaming the diff before any pull request opened, and its output goes back to the branch. While [`fix_gate_refusal`](configuration.md#supervise) is `auto`, that send-back and the verify command's below do not park the first time in a drive: the item goes back to the head of the queue for another implement session, with a `queued` event carrying `action` `fix_gate_refusal`, `stage` `implement` and `position` 1, and parks `gate_refused` only on a second refusal in the same drive, its reason ending `this drive already sent it back once`. Where a red check drive read as `code` sits under that refusal, the reason reads `<check> failed: <first line>`, the `detail` is `code`, `evidence` holds up to 5 lines its job log says failed (see [`[forge]`](configuration.md#forge)), and the body sent back adds those lines and the check's URL under the merge gate's output, so the next run starts from the failure rather than a check name; with a `pr` it is the merge gate refusing at the merge, after the head or the trunk moved, and the reason carries the merge gate's last lines. `gate_refused` with `update_branch.verify` `failed` is the item's own verify command failing on the branch merged with the trunk while the trunk alone passes: the merge commit is pushed, the send-back opens with the trunk range drive merged and carries the verify command's output; its `changes` row names `verify` as its source, so it is not a round under `max_review_rounds`. `trunk_conflict` is a trunk that moved since the branch was pushed and would not merge into it: the reason names the conflicted files, the merge is aborted and nothing is pushed. `trunk_red` is a trunk that fails the item's verify command on its own, which no item can fix: the reason names the trunk commit and the verify command's last lines, nothing is sent back or pushed, the local branch drops the merge, and the drive stops. A pull request already open stays open. `trunk_red` is also a red check on the head that concluded failure on the trunk's latest completed run of the same check: the reason names the check and that run's URL, the merge gate does not run, nothing is sent back, and the drive stops. `infra_failure` is a check that failed on the forge's infrastructure while `rerun_infra_failure` is below `auto`, so drive did not rerun it: the reason names each check and the log line that marked it; the queue goes on. Once the queue drains, and only when the drive is not stopping, drive reads the checks on that head once more. Where every one completed `success`, `neutral` or `skipped` and the branch still points there, it records a `green` check row and sends the item through the merge gate and the merge it would have taken, and the item is reported a second time with its new outcome; otherwise stderr says `checks on <head> still red at the end of the drive; left parked`. `forge_down` on a head no workflow reported on within `[forge] checks_wait` opens its reason with `no workflow ran on this head`, because the fix is CI that fires on a branch push rather than a failing check. See [`[forge]`](configuration.md#forge) for how each red check is classified. `merge_failed` is any refusal from `gh pr merge`: a conflict, a branch protection rule, a review requirement, or a head that moved past the one `--match-head-commit` names.

`push_failed` is a rejected `git push`, which drover never forces past and never retries with `--no-verify`: a branch can carry a human's commits, and a pre-push hook is the project's own check on what leaves the machine. `blocked_on_deps` is a `SPLIT-OUT` the review filed during the drive, since `drover split` rewrites the `deps` line and leaves the item waiting on work nobody has accepted. `ledger_unwritable` is a ledger that refused the pass row, which parks rather than merging on a result nothing can read back.

`uncommitted_work`, `worktree_failed` and `no_commits` are the item's worktree, not the review. Each item runs in a worktree of its own branch (see [Where each item runs](playbook.md#where-each-item-runs)), and `drive` asks `git status --porcelain` there after the implement stage, again after the review, before each push, and wherever it is done with the item, whatever parked it. So a runner that wrote files and then ran out of budget parks `uncommitted_work`, with the fault and its log named in the reason. Every path git does not ignore counts, untracked files included even where `status.showUntrackedFiles` is `no`. The operator's checkout is never asked on an item's behalf, because no item runs in it: work in one item's worktree cannot become part of another item's diff, which is what a single shared checkout did with no error anywhere.

drover and the sessions it launches write the ledger, saved reports, register file and logs into the operator's checkout, through `DROVER_ROOT`, and never into an item's worktree. A project that tracks any of them leaves that checkout dirty after a drive, and the next drive skips reconciliation over it. Ignoring them or keeping them outside the host repo keeps reconciliation running.

- `uncommitted_work` is work the item's own sessions left uncommitted in its worktree. After a run that finished, the report is still captured, because it is the only record of what the session said it did; after a runner fault it is not, and the reason carries the fault and the log instead. The worktree is kept, its path is in the reason and in the `tree` field, and the drive moves on to the next item.
- `worktree_failed` is an item whose worktree would not open: git refused the branch (most often because the operator has it checked out), a `[drive] worktree_links` entry is not ignored on the branch, or `[drive] worktree_setup` exited non-zero. Nothing was launched for it, and the rest of the queue runs.
- `no_commits` is a clean worktree on a branch holding nothing past the trunk: the session did nothing that survived, so the branch is not pushed, no review is paid for, and `drover drive <id>` runs it again from the start. Its own cause because an empty branch is an ancestor of the trunk, which `git branch --merged` lists the same way it lists one whose work merged.

Nothing is stashed, reset or cleaned on the operator's behalf: only they know which branch the work belongs to. The one reset `drive` makes undoes its own branch-update merge when the merged tree does not pass, with `git reset --keep`, which refuses rather than discard uncommitted work.

`implement_died` and `implement_unreadable` are the same two readings of the implement run, taken before the reviewer launches. They carry a stage prefix rather than the bare tokens because `park` is what a wrapper keys on, and one `died` for both stages would have a wrapper answer an implement failure by re-running a review against a branch holding no work. The branch is still pushed: a run killed mid-flight leaves commits and an unfinished log both. What a dead run does not do is buy a review pass, since the pass that follows reads the implementer's silence as the reviewer's and sends the operator to the wrong log.

`died` and `unreadable` split a review that left no terminal event by whose stream it left. `died` is a session that stopped: an empty log, or a log of events drover recognizes with no terminal one among them. `unreadable` is a log full of events no schema drover reads recognizes at all, which is a reviewer that executed correctly and a runner drover cannot read back (see [`[runners]`](configuration.md#runners)). Both parks name the runner and the log path, because the cause lives in `[runners]` and the evidence lives in the log.

`runner_fault` is the runner reporting about itself rather than about the work: the service was unreachable, the account is out of budget, or the runner holds no credentials the service accepts. It is read off the terminal event, structurally where a runner offers structure and out of the failure's own message otherwise, and the three are answered differently. A transient failure is retried twice, backing off 45 seconds then 90. A quota is waited out only when the runner named the instant it serves again _and_ that instant falls inside the drive's own `--for` or `--until`: those flags are the operator saying how long this drive may own the terminal, and an unbounded drive has no such grant to spend. An unauthenticated runner is never waited on, because no reset repairs it. A [`t3` log](#the-t3-log) whose result is an error is the fourth kind: the T3 Code run ended interrupted, failed or with its thread gone, or the bridge stopped before a launch, and it parks at once, with no retry, unless its message names credentials, a quota or the service. Everything left parks, quoting what the runner said and naming the log. Both stages check it, so a run that faulted before it touched the work never reaches a push, a review or a captured report.

`findings_open`, `body_edited` and `no_digest` are the three ways an audit with nothing sent back fails to read as a clean pass. A review that records its audit and then writes its findings into the item body by hand leaves an audit row and no `changes` row, which reads the same as a review that found nothing. The body tells them apart. The review's stage row carries a 12-hex digest of the item file as the session launched, frontmatter included, and every drover command that writes an item body appends a body row carrying the digest of what it wrote: `request-changes`, `close-changes`, `invariant`, `link`, `resolve --link`, `split` (for the item it splits, not the scaffold), `accept`, `unaccept`, `amend` and `touch`. When the review ends, `drive` digests the file again and compares it with the latest recorded digest. The digest ignores spaces, tabs, carriage returns and newlines at the end of the file, because a final newline an editor or a snapshot hook adds is nobody's edit; a rewrapped paragraph or one changed word still moves it. A row recorded before that rule holds the digest of the exact bytes, and either form matches. A match is `passed` and goes on to the merge gate, on the first review as on any later one. A difference is `body_edited`: the body changed outside drover after the review launched, so findings written into it by hand cannot be told from a clean pass, and the park names both digests. Diff the body against its history, then `drover request-changes <id> -f <file>` the findings properly, or `drover open-pr <id> --force` if the change was not a finding. An edit made before the drive is caught earlier: `drover check` reports it `HAND EDITED` and `drive` parks the item `hand_edited` before any session, until `drover amend` or `drover touch` records it. A block deleted by hand reads the same as findings written by hand: `drover close-changes <id>` is how a closed block is discharged without moving the digest. An item sent back before is no exception: an earlier send-back says nothing about whether this pass wrote its findings down.

`findings_open` is read before the other two. A review that finds the block an earlier round wrote under `## What the review sent back` still open, and records its audit without sending anything back, leaves the block in place and the digest where drover left it: the rows of a clean pass over a body that says the opposite. Any block that is not the statement `drover close-changes` writes parks the pass as `findings_open`, whether or not a digest exists. It parks rather than running the item again, because no `changes` row was written and `[review] max_review_rounds` would never count the round. `drover request-changes <id> -f <file>` if the findings are still open, `drover close-changes <id>` if they are closed.

A review over a body carrying an open block has three exits, and the prompt `drover review` prints ends on drover's own instruction naming them, appended after the project's `review.tmpl` so no template can drop or reword it:

| The review                        | What it runs                                                                           | What `drive` reads                                                                                                                                      |
| --------------------------------- | -------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Finds every finding closed        | `drover close-changes <id>` (with `-f <file>` per finding traced), then `drover audit` | `passed`: the close-changes body row is drover's own write and the block it leaves is closed, so the pass goes on to `[drive] merge_gate` and the merge |
| Finds a finding still open        | `drover request-changes <id> -f <file>`                                                | `changes_requested`: the item runs again                                                                                                                |
| Records an audit and does neither | `drover audit` alone                                                                   | `findings_open`: parks                                                                                                                                  |

The `findings_open` park reason names the follow-up for that item, chosen from the audit note: `drover close-changes <id> && drover open-pr <id>` when the note reads as closed, `drover request-changes <id> -f <file>` when it names a finding as unmet or open, and both when it says neither. A note reads as closed only when every clause in it asserts the whole block closed, a closed word (closed, fixed, resolved, addressed, met) after `all`, `every` or `each`, and every word in it belongs to that assertion: the quantifier, the closed word, or one of `finding`, `findings`, `requirement`, `requirements`, `the`, `in`, `of`, `this`, `block`, `are`, `is`, `were` and `now`. Words split on whitespace alone, so a numeral, a percent sign, a non-ASCII word or an id such as `R4` is a word outside the list and names both commands: `all 3 of 4 findings fixed` and `each of 3/4 fixed` say neither. `every finding in the block closed` and `All met; each finding resolved.` read as closed. Any other word names both commands, because the note cannot show what else the block holds: `R2 fixed in 1a2b3c` speaks for one finding, `all of R1-R3 fixed` for three, and `All findings closed and R4 needs a test` joins an open finding with no punctuation between. A negated closed word reads as open: `R4 not closed`, `R2 remains unfixed` and `R1 fixed, R4 still fails` name `request-changes`. It also says the drive did not merge the item and that every later item depending on it is blocked until it finishes. After `drover close-changes <id>`, `drover open-pr <id>` reads the recorded `findings_open` pass as `passed` and runs the merge gate as usual, because the close-changes row follows the pass and the audit it read. A `findings_open` pass with no later close-changes row still refuses, and one recorded before the pass does not count.

`no_digest` applies only where no body digest exists for the item at all, which is every review launched by a drover that did not record one. There is nothing to compare, so the pass parks rather than merging on the reviewer's word. Read the audit with `drover show <id>` and, if it holds, `drover open-pr <id> --force`. A parked item keeps its branch, its commits and its audit row, and `drover drive B7` picks it up by name.

#### Park details

`park` says what drive stopped on. `detail` says why the session or the push under it failed, so nobody has to read the session log to find out. Every park carries one, the ledger records it beside the park (see [The ledger](concepts.md#the-ledger)), and `evidence` is the line it was read from:

| `detail`                  | Read from                                                                                                                       | `evidence`                                                                      |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `auth`                    | The runner's terminal event reporting no credentials the service accepts                                                        | The runner's own line, such as `Not logged in · Please run /login`              |
| `oom`                     | Exit `137`, or the runner killed by `SIGKILL`                                                                                   | The exit or the signal                                                          |
| `stream_cut`              | The terminal event or the log's last lines naming a cut stream, such as agy's `The stream was interrupted`                      | That line                                                                       |
| `background_left_running` | A process in the session's own process group still alive when the session exited                                                | Each process, `pid N: command`. drover kills them                               |
| `runner_exit`             | Any other non-zero exit, or a signal                                                                                            | The exit code or the signal, or why no session started                          |
| `tool_denied`             | The terminal event's denials (claude's `permission_denials`, agy's `denied_actions`)                                            | `the runner denied <tools>`                                                     |
| `push_hook`               | A refused `git push` in a tree whose pre-push hook would have run, with no rejection from the remote                            | The hook (its config name, or its path) and the first line of the push's output |
| `push_network`            | A `git push` whose output names the transport failing (`closed by remote host`, `Could not resolve host`, `Connection refused`) | That line                                                                       |
| `code`                    | A `gate_refused` park whose merge gate refused over a red check drive read as `code`, rather than any session log               | Up to 5 lines the check's job log says failed                                   |
| `unknown`                 | None of the above                                                                                                               | The log's last 5 lines, or the push's                                           |
| `unrecorded`              | A park no session caused, whose cause named no detail of its own                                                                | `<park>: <reason>`, never a session log                                         |

The order of the rows is the order they are read in. Only a park a session caused reads the session's log: `runner_fault`, `implement_died`, `implement_unreadable`, `no_commits`, `uncommitted_work` and the review pass causes (`died`, `unreadable`, `no_record`, `repeating`, `needs_decision`, `findings_open`, `no_digest`, `body_edited`). One of those that matches no other row reads `unknown` with the last lines of the last log the item wrote, and one before any launch says no session ran. Every other park (`gate_refused` with no `code` check under it, `protected_paths`, `trunk_red` and the rest) reads `unrecorded`, with the cause and the reason as its evidence and no `log`, because the last session before it finished cleanly and its log says nothing about why drive stopped.

A `repeating` park's `detail` says what repeated instead of a token: `R3 still open after rounds 4 and 5` for a finding the reviewer marked still open twice, and `identical evidence in rounds 4 and 5` for an equal digest. Its `evidence` is the first paragraph of the last request for changes drover kept, and its `log` that kept round's path, since the session finished cleanly and its log explains nothing. A `needs_decision` park's `detail` names the requirements (`R3 needs an operator decision`), and its `evidence` is one line per requirement, `R3: ` and the first sentence of the reason the evidence gave under it. Its `needs_operator` carries the same requirements and sentences in `reason`, and in `run` the commands that resolve it: `drover amend <id> -f <file> --reason "<why>"` to change the contract, or, where a person can meet the requirement, an amend tagging it `[operator]` and `drover signoff <id> <n>`. A `needs_decision` park the cap caused, a review that wanted to send work back past `[review] max_review_rounds`, says so instead: its `detail` is `max_review_rounds 3 reached; the refused request named R2, its findings kept in .drover/reports/04-fourth.refused-1.md`, ending `and no file kept its findings` where the file would not write, and its `log` is that file. Its `run` names the operator's four options: `drover amend`, `drover signoff`, `drover shelve`, or `drover request-changes <id> -f <file> --over-limit`. No filing command is among them, because the findings are the unmerged item's own.

A `trunk_conflict` park's `detail` is the conflicted files instead of a token, comma-separated and relative to the repository root, so `drover status` prints `parked: trunk_conflict (docs/usage.md)`. Its `evidence` is the last 5 lines of `git merge-tree --write-tree --name-only --messages <branch> origin/<trunk>`, which name each file and the kind of conflict, and it carries no `log`.

A session that leaves a process it started running is not counted as clean, whatever its terminal event says: the implement run parks `implement_died` and a review reads `died`. The prompt tells every session to run its verify command in the foreground.

A review that `died`, or that recorded nothing and ended on `stream_cut`, `oom` or `runner_exit`, is rerun once on the same head with no implement run before it, when [`[supervise] rerun_lost_review`](configuration.md#supervise) is `auto` (the default). A second loss in the same drive parks. `drover drive --review-only <id>` is the same rerun by hand: it skips that item's implement run, reviews its branch as it stands, and carries a clean pass through the merge gate, the pull request and the merge as any other. It takes one id, and refuses `--enqueue`.

An `auth` detail parks `runner_fault`, stops dispatching, and parks every running item `stopped` at its next stage boundary. Its `needs_operator` event carries `auth_expired` and names the command that signs the runner in (`claude auth login`, `codex login`). Before the first launch, drive asks the implementing runner it resolved and the reviewer whether each is signed in, where the binary offers that question (`claude auth status`, `codex login status`), and refuses with exit `3` when one says no. An item naming its own runner meets that runner at launch.

A `push_failed` park with `push_hook` belongs to `push_refused_by_hook`, which stops for the operator with the hook's output in `evidence`.

`rounds` counts implement launches past the first and `reviews` counts review launches. `changes_requested` and `audits` are the rows _this drive's_ reviews added, not the item's totals; `drover show` carries those. `same_runner` marks an item implemented and reviewed by one runner, which drive warns about and drives on.

`forge_down` and `runner_fault` are the two parks that end the drive rather than the item, and `drive` then exits 1. A trunk that did not receive this merge is the trunk every later item builds on; a service that is down and an account with nothing left in it are true of the next item as well, and walking the queue anyway spends a branch, a push and two dead sessions per item to rediscover it.

#### rate_limit

`rate_limit` is the newest reading Claude Code left on a usage row of any project in the [per-user registry](#projects---json), this one included, because the limit is the account's. It is absent when no usage row carries a reading, which is every ledger written only by codex or agy.

```json
{
  "rate_limit": {
    "reading": {
      "status": "allowed_warning",
      "rate_limit_type": "seven_day",
      "resets_at": 1792983600,
      "five_hour": { "utilization": 0.09, "resets_at": 1792747800 },
      "seven_day": { "utilization": 0.79, "resets_at": 1792983600 },
      "overage_status": "rejected",
      "overage_disabled_reason": "org_level_disabled"
    },
    "project": "drover",
    "ts": "2026-10-22T14:03:11-07:00",
    "remaining": { "weekly": 21, "five_hour": 91 },
    "overage": false
  }
}
```

`reading` is the usage row's [`rate_limit`](#writing-to-the-ledger-without-the-binary) as written, `project` the registered project whose row it is and `ts` that row's time, in the report's zone. `remaining` is the percent of each window left now, `100 - utilization * 100`: the reading's own figure until that window's `resets_at` passes, and 100 after it, since the account has started over. `overage` is true while sessions draw on purchased credit (the weekly window `rejected` with `is_using_overage`). `unread` names each registered project whose ledger would not read, when one would not, so a reading older than one in that ledger is not mistaken for the newest. `status` without `--json` prints the same reading as one footer line:

```
account: weekly 21% left, five-hour 91% left, not on purchased credit, week resets Sun Oct 25 20:00 PDT (read Oct 22 14:03 in drover)
```

### `drive --events`

`drive --json --events` writes, beside each item's report, one event object per line as things happen. Every event carries `event`, which no item report has, so a reader splits the stream on that key. Every event also carries `ts` (RFC 3339, UTC), `drive` (the drive's id, taken with the lease; absent only on the `exit` of a drive whose id would not allocate because the ledger would not open), `seq` (the event's line in the drive's event file, from 1, and the cursor [`watch --after`](#detaching-from-a-drive) takes; absent where `drive` is), `project` (the project's name as [`drover projects`](#projects---json) lists it, so a notifier serving several projects reads it rather than working it out from its directory), `id` where one item is meant, and `action` where a [`[supervise]`](configuration.md#supervise) catalog action produced it. `--events` without `--json` refuses with exit `3`. The same objects go to the drive's event file whatever the flags; see [Detaching from a drive](#detaching-from-a-drive).

`started`: the drive holds its lease and has read its queue, before anything is dispatched. `queue` is the ids it will run, in order, `jobs` its `--jobs`, and `deadline` the instant `--for` or `--until` bounds it at, in local time; `deadline` is absent on an unbounded drive. A drive whose queue is empty, and one [re-executed onto a new binary](#resuming-onto-a-new-binary), emits none.

```json
{
  "event": "started",
  "ts": "2026-10-24T17:31:04Z",
  "seq": 1,
  "drive": "D10",
  "project": "drover",
  "queue": ["B89", "E71", "F14", "E72"],
  "jobs": 2,
  "deadline": "2026-10-24T19:31:04-07:00"
}
```

`queued`: an id `drive --enqueue` appended to this drive was taken into its queue. `position` is its place among the items waiting, from 1. `action` is `enqueue`.

```json
{ "event": "queued", "ts": "2026-10-16T22:20:00Z", "id": "E37", "action": "enqueue", "position": 2 }
```

`dispatched`: an item left the queue for a slot. `slot` appears at `--jobs` above 1.

```json
{ "event": "dispatched", "ts": "2026-10-16T22:00:00Z", "id": "E36" }
```

`skipped`: an item left the queue without being attempted, at any `--jobs`. `state` is the work state the drive read it in (`blocked`, `merged`, `scaffold` and so on), and `reason` says why where there is more to say: `blocked by E34` for an unmerged dep, and `blocked by E34, which parked protected_paths for the operator` for a dep the drive held it for until nothing else was left to run. `slot` appears at `--jobs` above 1 for an item skipped in one; an item skipped while it waited for a dep's merge had none.

```json
{ "event": "skipped", "ts": "2026-10-16T22:40:00Z", "id": "E37", "state": "blocked", "reason": "blocked by E34, which parked protected_paths for the operator" }
```

`stage`: an item reached a step. `stage` is one of `implement`, `review`, `update_branch`, `gate`, `pr`, `merge` or `finish`. The trunk steps (`update_branch` through `finish`) can repeat for one item, because the merge fallback updates the branch and runs the merge gate again at the head it merges.

```json
{ "event": "stage", "ts": "2026-10-16T22:00:01Z", "id": "E36", "stage": "implement", "log": "~/.cache/drover/webapp/E36-retry.20261016-150001.123456789.jsonl" }
```

An `implement` or `review` stage launches a session and carries `log`: the session log's path as the launch banner prints it, relative to the project root or under `~/`, chosen before the event and the file the runner writes. A relaunch after a transient fault (an overloaded service, an exhausted account) writes a log of its own and emits a `stage` of its own. The trunk steps launch no session and carry no `log`. [`attach --session`](#detaching-from-a-drive) follows it.

An `update_branch` for another item's open pull request, after a merge or under `--update-branches`, arrives once its branch update is done and carries `result`, `branch` and `pr`. `result` is an `update_branch.verify` word (`passed`, `failed`, `sequence`, `trunk_red`, `skipped`, `unrunnable`), or `up_to_date` (the branch already held the trunk), `conflict`, or `kept` (not attempted, `reason` says why).

```json
{
  "event": "stage",
  "ts": "2026-10-16T22:45:02Z",
  "id": "E39",
  "stage": "update_branch",
  "reason": "merged origin/main (9b1e…..3f2a…), verify passed, pushed",
  "pr": 254,
  "result": "passed",
  "branch": "enhancement/e39-ref-drift"
}
```

The branch update drive makes before an item's own implement session, or its `--review-only` review, also carries `result`, with `branch` and `tree`: `up_to_date` (the branch already held the trunk), `merged` (the merge was committed for the session to start on), `conflict` (the session resolves it, at `resolve_conflict = "auto"`) or `parked` (the merge was aborted and the item parks `trunk_conflict`, or the worktree held uncommitted work or an unconcluded merge, nothing was merged, and the item parks `uncommitted_work`). `conflict` and `parked` carry the conflicted files in `paths`. The branch updates of the trunk stage itself emit a bare `update_branch`, with no `result`. The ledger's `implement` stage row for a session that started on a conflict carries `"conflict": true`.

```json
{
  "event": "stage",
  "ts": "2026-10-19T09:12:40Z",
  "id": "E53",
  "stage": "update_branch",
  "reason": "merging origin/main at 3f2a… into enhancement/e53-conflict conflicts on internal/cmd/drive.go; the session resolves it",
  "result": "conflict",
  "branch": "enhancement/e53-conflict",
  "tree": "/home/op/.local/state/drover/worktrees/5c1d…/e53",
  "paths": ["internal/cmd/drive.go"]
}
```

`parked`: an item stopped. `park`, `reason`, `detail` and `evidence` are the fields the item's report carries; key on `park`. For `gate_refused` over a `code` check, `evidence` is the lines its job log says failed, one per line, the same lines `drover show` prints under the park. Where a catalog action owns the park, `action` names it and `policy` says who handles it in this project.

```json
{
  "event": "parked",
  "ts": "2026-10-16T22:41:00Z",
  "id": "E38",
  "action": "merge_protected_paths",
  "park": "protected_paths",
  "reason": "this branch changes Makefile, which decides what every later item tonight is measured by; merge it by hand",
  "policy": "operator",
  "pr": 7
}
```

```json
{
  "event": "parked",
  "ts": "2026-10-25T00:54:02Z",
  "id": "F14",
  "park": "gate_refused",
  "reason": "Gate failed: internal/cmd/notify.go:28:16: var-declaration: should omit type http.RoundTripper from declaration of var ntfyTransport; it will be inferred from the right-hand side (revive); sent back",
  "detail": "code",
  "evidence": "internal/cmd/notify.go:28:16: var-declaration: should omit type http.RoundTripper from declaration of var ntfyTransport; it will be inferred from the right-hand side (revive)"
}
```

| `park`                                | `action`                      |
| ------------------------------------- | ----------------------------- |
| `trunk_red`                           | `report_trunk_red`            |
| `infra_failure`                       | `rerun_infra_failure`         |
| `trunk_conflict`                      | `resolve_conflict`            |
| `protected_paths`                     | `merge_protected_paths`       |
| `awaiting_signoff`                    | `signoff`                     |
| `blocked_on_deps`                     | `write_scaffold_requirements` |
| `implement_died`                      | `relaunch_after_runner_death` |
| `died`                                | `rerun_lost_review`           |
| `runner_fault` with detail `auth`     | `auth_expired`                |
| `push_failed` with detail `push_hook` | `push_refused_by_hook`        |
| `needs_decision`                      | `decide_requirement`          |
| `gate_refused` with detail `code`     | `fix_gate_refusal`            |
| `stopped`, `interrupted`              | `cleanup_interrupted_drive`   |
| `stale_citations`                     | `stale_citations`             |
| the `held` outcome, which is no park  | `usage_admission`             |

`preflight`: a check drive ran before spending a session. `action` is the check's key (`stale_citations`, `deps_lint`, `worktree_links_gap` or `host_load_admission`; `usage_admission` reports through `usage_warning`, `paused` and `needs_operator` instead) and `policy` its policy here. One event per finding, with `reason` the sentence the drive printed, or one with `reason` `clean` for a check that found nothing. `stale_citations` carries `id`. `host_load_admission` emits on a change of state rather than per dispatch: `clean` on the first admission and again as each hold ends, the hold's reason as it starts, and the error at the first load it cannot read. A `deps_lint` refusal emits its event before the drive exits `3`, with no `exit` event after it.

```json
{
  "event": "preflight",
  "ts": "2026-10-17T22:00:00Z",
  "id": "F37",
  "action": "stale_citations",
  "reason": "internal/cmd/jobs.go:203-224 runs past the end of internal/cmd/jobs.go, which has 198 lines on origin/main",
  "policy": "auto"
}
```

`filed`: a session filed a finding while the drive ran, noticed after the filing item's result. `kind` is `scaffold` (the backlog gained an item whose requirements are unwritten) or `follow-up` (a follow-up that was not open when the item was dispatched). `ref` is the item id or the issue number, `filed_by` the items it is attributed to, and `owners`, `coverage`, `blocked_on` and `ambiguous` its [coverage](configuration.md#supervise). A scaffold carries its own id as `id` and `action` `write_scaffold_requirements`, and is attributed to the item its `Found while working <id>` line names, or to none. A follow-up carries the filing item as `id` when only one item was running; a row that appeared while several ran under `--jobs` names them all in `filed_by` with `ambiguous` and carries no `id`. A row whose coverage is `blocked`, `orphaned` or `unowned` goes on to `triage_finding`. An item the backlog gained with its requirements written emits `filed` with its `id` and nothing else.

```json
{"event":"filed","ts":"2026-10-16T22:30:00Z","id":"E44","action":"write_scaffold_requirements","policy":"supervisor","kind":"scaffold","ref":"E44","filed_by":["E43"],"coverage":"scaffold"}
{"event":"filed","ts":"2026-10-16T22:31:00Z","id":"E43","kind":"follow-up","ref":"846","filed_by":["E43"],"owners":["E35"],"coverage":"orphaned"}
{"event":"needs_operator","ts":"2026-10-16T22:31:00Z","id":"E43","action":"triage_finding","reason":"follow-up #846, filed by E43, is orphaned: every owner has merged, so no run is left to carry it","policy":"operator","run":"read follow-up #846 (drover follow-ups --coverage), then give it an owner a run will reach, or close it","kind":"follow-up","ref":"846","filed_by":["E43"],"owners":["E35"],"coverage":"orphaned"}
```

`needs_operator`: an action at `operator` policy stopped, after its `parked` or `filed` event. `run` is what the operator has to do. Each [`[notify]`](configuration.md#notify) destination that routes `needs_operator` is sent it, with or without `--events`; see [Notification payloads](#notification-payloads).

```json
{
  "event": "needs_operator",
  "ts": "2026-10-16T22:41:00Z",
  "project": "example.app",
  "id": "E38",
  "action": "merge_protected_paths",
  "reason": "this branch changes Makefile, which decides what every later item tonight is measured by; merge it by hand",
  "policy": "operator",
  "run": "review E38's pull request, merge it by hand, then drover finish E38"
}
```

`needs_supervisor`: an action at `supervisor` policy that drive met and left for the supervising session. `run` is what that session does, and a conflict carries `branch`, `tree` (the worktree left standing) and `paths`.

```json
{
  "event": "needs_supervisor",
  "ts": "2026-10-16T22:45:03Z",
  "id": "E40",
  "action": "resolve_conflict",
  "reason": "merging origin/main into enhancement/e40-tree-links conflicts on internal/cmd/drive.go; nothing was merged or pushed",
  "policy": "supervisor",
  "run": "Resolve the conflict in the worktree the park names, run the item's verify command, commit and push without force, then `drover drive E40`. Report the files and the verify command's exit.",
  "pr": 255,
  "branch": "enhancement/e40-tree-links",
  "tree": "/home/op/.local/state/drover/worktrees/5c1d…/e40",
  "paths": ["internal/cmd/drive.go"]
}
```

`check`: drive classified a red check on an item's pushed head before the merge gate read it, or found no check on it at all. `head` is the commit, `check` the check's name, `class` one of `infra`, `trunk`, `code` or `none` (no check ran, and `check` is absent), or `green` (an `infra_failure` head whose checks passed when drive read them again at the end of the drive, and `check` is absent), and `evidence` the line that decided it. `action` is `rerun_infra_failure` on an `infra` check drive reran or parked, and `report_trunk_red` on a `trunk` one. The same four fields are recorded in the ledger as one row per event.

```json
{
  "event": "check",
  "ts": "2026-10-16T22:38:00Z",
  "id": "E36",
  "action": "rerun_infra_failure",
  "head": "3f2a9c1e0b7d4a5e6f708192a3b4c5d6e7f80912",
  "check": "build",
  "class": "infra",
  "evidence": "Error response from daemon: toomanyrequests: You have reached your pull rate limit."
}
```

`merged`: an item's pull request merged, by drover on the merge gate's word, or outside an earlier drive (by hand, or by a required merge queue). `pr` is its number and `title` its title. A merge drive did not see land emits `merged` from the drive whose reconciliation finishes it, carrying that drive's id, after the completion row and while the item is still listed in its lease's `running`.

```json
{
  "event": "merged",
  "ts": "2026-10-16T22:45:00Z",
  "id": "E36",
  "pr": 252,
  "title": "E36: retry a flaky gate once"
}
```

`merge_queued`: no current drive emits it. A drover that asked the forge for `gh pr merge --auto` emitted it when the forge accepted the request and had not merged it. The name stays valid in `[notify]` routing so existing configs load.

```json
{
  "event": "merge_queued",
  "ts": "2026-10-26T09:50:44Z",
  "id": "B173",
  "pr": 238,
  "title": "B173: drive reads the trunk once"
}
```

`paused`: `drive --pause` reached the drive, or [`usage_admission`](configuration.md#supervise) held it. It dispatches nothing more, and each running item holds at its next stage boundary. A usage hold carries `action` `usage_admission`, `policy`, and `window`, `remaining` and `resets_at` as `usage_warning` does, with `reason` naming the level crossed: `the weekly window has 4% left, at or below [usage.weekly] pause_remaining 5; it resets Sun Oct 25 20:00 PDT`, a window entered on purchased credit under `[usage.overage] mode = "pause"`, a list-price sum at `max_usd`, or a refusal, `impl's review session was refused: the weekly window is spent and so is the purchased credit (out_of_credits), so the service refuses sessions; it resets …`. It also carries `holds`, every usage hold the same reading crosses that no resume released, each with `window`, `reason` and `resets_at`, the one taken first: `window` and `reason` stay those of that hold, and each `drover drive --resume` releases one entry. A `needs_operator` with the same fields follows it at every policy, so a destination routing `needs_operator` reaches the operator. Its `run` is `drover drive --resume` for one hold, and with more says what each resume releases: `drover drive --resume (releases weekly; overage still holds and takes a second --resume)`.

```json
{ "event": "paused", "ts": "2026-10-16T22:50:00Z" }
{
  "event": "paused",
  "ts": "2026-10-24T09:12:40Z",
  "action": "usage_admission",
  "policy": "auto",
  "reason": "the weekly window has 4% left, at or below [usage.weekly] pause_remaining 5; it resets Sun Oct 25 20:00 PDT",
  "window": "weekly",
  "remaining": 4,
  "resets_at": "2026-10-25T20:00:00-07:00",
  "holds": [
    {
      "window": "weekly",
      "reason": "the weekly window has 4% left, at or below [usage.weekly] pause_remaining 5; it resets Sun Oct 25 20:00 PDT",
      "resets_at": "2026-10-25T20:00:00-07:00"
    }
  ]
}
```

A session the service refused for quota (its reading `rejected` with no overage, or `out_of_credits`) is held rather than parked: its item goes back to the head of the queue at the stage that was refused, with a `queued` event carrying `action` `usage_admission`, `stage` and `position` 1, and the refused launch counts against neither `rerun_lost_review` nor `max_review_rounds`. A quota fault with no reading, codex's or agy's, waits or parks `runner_fault` as before, and so does every quota fault at `usage_admission = "off"`.

`usage_warning`: the newest reading crossed a [`[usage]` notify level](configuration.md#usageweekly-and-usagefive_hour). `window` is `weekly` or `five_hour` with `remaining` the percent of it left, `overage` (the weekly allotment spent and sessions drawing on purchased credit, sent when `[usage.weekly] notify_remaining` is set) or `overage_usd` (the list-price sum at `[usage.overage] notify_usd`); neither overage form carries `remaining`, since the service reports no credit figure. `resets_at` is the window's reset in local time and `reason` the sentence. It pauses nothing. Each is sent once per window per reset: drive appends a ledger row naming both first, and sends nothing a row in any registered project already names. Each [`[notify]`](configuration.md#notify) destination routing `usage_warning` is sent it.

```json
{
  "event": "usage_warning",
  "ts": "2026-10-23T18:40:02Z",
  "action": "usage_admission",
  "reason": "the weekly window has 18% left, at or below [usage.weekly] notify_remaining 20; it resets Sun Oct 25 20:00 PDT",
  "window": "weekly",
  "remaining": 18,
  "resets_at": "2026-10-25T20:00:00-07:00"
}
```

`thread_opened`: a [`command` destination](configuration.md#notify) running `drover t3 thread --from-event` opened a T3 Code thread for an item's `needs_operator` or `needs_supervisor`, and the drive appended what it printed. `thread` is the thread's id and `url` the `t3-thread://` link T3 Code opens it at. It is the drive's record of which thread answers the item: the next stop for that item reaches the bridge carrying `thread` and `url`, and the bridge sends to that thread while it stays open instead of opening another, so a reused thread emits no second `thread_opened`. A thread stopped or deleted since is replaced, with a new `thread_opened`. Until the item has a pull request, an ntfy message about it sets `Click` to `url`. One the destination answers after the drive's `exit` is not written.

```json
{
  "event": "thread_opened",
  "ts": "2026-10-26T03:12:40Z",
  "seq": 41,
  "drive": "D12",
  "project": "drover",
  "id": "B12",
  "thread": "mcp:68eab9c9-e102-4e85-8e5b-4563f82da101",
  "url": "t3-thread://v1/mcp:68eab9c9-e102-4e85-8e5b-4563f82da101"
}
```

`resumed`: `drive --resume` reloaded the drive's inputs. The event without `id` is the drive's own; one with `id` is an item leaving the boundary it was held at, and `stage` is where it goes next (`implement`, `review` or `update_branch`). `changed` names which inputs differ from what the drive, or that item, ran under before: `config` (the `.drover.toml` bytes), `prompt`, `review` (the template text) and `verify` (the item's own verify command, or for the drive's event the whole `[verify]` table). An item whose `verify` changed runs the new command on its tree before it goes on, with a `stage` event for `gate`, and a refusal is sent back to it as a merge gate's would be. A drive that [re-executed onto a new binary](#resuming-onto-a-new-binary) emits one `resumed` with `changed` `binary` from the new process. A usage hold lifting on its own at its window's reset (plus a minute), which it does when the reset falls inside `--for` or `--until`, emits `resumed` with `action` `usage_admission`, the hold's `window`, `remaining` and `resets_at`, and `reason` `the weekly hold reached its reset at Sun Oct 25 20:00 PDT, so the drive dispatches again`; a destination routing `resumed` is sent that one as it is every other. `drive --resume` lifts a usage hold at any time and emits the ordinary `resumed`, and the same reading does not hold the drive again before its window resets.

```json
{
  "event": "resumed",
  "ts": "2026-10-16T23:02:00Z",
  "id": "E36",
  "stage": "review",
  "changed": ["config", "verify"]
}
```

`config_changed`: the `.drover.toml` on disk is no longer the one the drive runs under, the one it started or [last resumed](#pausing-a-drive) with. The drive applies nothing; `--pause`, then `--resume`, does. It is emitted once per distinct content, at the dispatch or stage boundary that first sees it, with the stderr line saying the same: an edit reverted to the bytes the drive runs under emits nothing, and a later edit emits again. `tables` names each top-level table that differs, bracketed (`[drive]`), and each top-level key outside a table, and is absent when only comments or layout changed. `error` replaces it when the new file will not load, which a resume would refuse, or will not read.

```json
{
  "event": "config_changed",
  "ts": "2026-10-24T11:12:40Z",
  "drive": "D7",
  "project": "capy",
  "tables": ["[drive]"]
}
```

`reexec_waiting`: a resume onto a [replaced binary](#resuming-onto-a-new-binary) has waited more than a minute for every running item to hold, and these have not. `waiting` names each, in dispatch order, with the `stage` its last `stage` event named, absent when it has emitted none since its dispatch. `reason` is the line `status` prints. Repeated at most once a minute while the wait lasts.

```json
{
  "event": "reexec_waiting",
  "ts": "2026-10-16T23:05:00Z",
  "drive": "D7",
  "project": "capy",
  "reason": "the re-exec onto v0.1.2 (built 2026-10-16T22:58:00Z) has waited 3m0s for E37 (in review) to hold at a stage boundary; it goes on once each does, and drive --stop parks them instead",
  "waiting": [{ "id": "E37", "stage": "review" }]
}
```

`exit`: the drive ended, last on the stream. `code` is the exit status the process returns, and `reason` the stop when there was one. A drive that walked its queue also carries `findings_by_coverage` and `findings_needing_triage`, as the [totals object](#drive---json) does.

Every `exit` carries `summary`, what the drive left done and undone:

| Field            | Meaning                                                                                                                                                                                                                                                                                                                                                  |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `merged`         | Items whose pull request merged.                                                                                                                                                                                                                                                                                                                         |
| `ready_to_merge` | Items reviewed and green that nothing merged, such as under `--no-merge` or a `protected_paths` hold.                                                                                                                                                                                                                                                    |
| `merge_queued`   | Always `0` from a current drive, which never asks for `--auto`. Kept so readers of older exit events parse. Counted under `ready_to_merge` too, and never under `merged`.                                                                                                                                                                                         |
| `parked`         | Items parked.                                                                                                                                                                                                                                                                                                                                            |
| `skipped`        | Items not attempted: done, scaffolded, blocked or broken.                                                                                                                                                                                                                                                                                                |
| `held`           | Items a usage hold kept from starting or finishing.                                                                                                                                                                                                                                                                                                      |
| `undone`         | Each item that did not merge, in the order it finished: `id`, `outcome`, and `park`, `detail`, `reason` and `pr` where it has them. A skipped item blocked by its dependencies carries `blocked_by`, each unmerged dependency's `id` with its `state`: its outcome in this drive (`parked gate_refused`) where the drive ran it, else its backlog state. |
| `weighted`       | The weighted tokens of the usage rows the drive's sessions wrote, as [`usage`](#usage) weighs them.                                                                                                                                                                                                                                                      |
| `ended`          | Why the drive ended: `drained` (the queue ran out), `deadline`, `max_items`, `stopped` (`drive --stop`), `interrupted` or `fault`.                                                                                                                                                                                                                       |

The summary counts each item once, by its latest outcome, so an item parked and then merged from `--enqueue` in the same drive counts as merged and is not in `undone`. An exit whose summary counts any item parked, skipped or held is what a [`[notify]`](configuration.md#notify) destination routes at `exit_undone`'s priority. A drive re-executed onto a new binary counts the items its predecessor finished by their outcome alone.

```json
{
  "event": "exit",
  "ts": "2026-10-24T18:59:12Z",
  "drive": "D10",
  "code": 0,
  "summary": {
    "merged": 2,
    "merge_queued": 0,
    "ready_to_merge": 0,
    "parked": 1,
    "skipped": 1,
    "held": 0,
    "undone": [
      {
        "id": "F14",
        "outcome": "parked",
        "park": "gate_refused",
        "detail": "code",
        "reason": "Gate failed: internal/cmd/notify.go:28:16: var-declaration (revive); sent back"
      },
      {
        "id": "E72",
        "outcome": "skipped",
        "park": "blocked",
        "reason": "blocked by F14",
        "blocked_by": [{ "id": "F14", "state": "parked gate_refused" }]
      }
    ],
    "weighted": 48210000,
    "ended": "drained"
  }
}
```

```json
{
  "event": "exit",
  "ts": "2026-10-16T23:10:00Z",
  "code": 0,
  "findings_by_coverage": {
    "blocked": 0,
    "orphaned": 1,
    "queued": 2,
    "scaffold": 0,
    "unowned": 0,
    "unqueued": 0
  },
  "findings_needing_triage": [
    {
      "ref": "846",
      "kind": "follow-up",
      "title": "the cache key ignores the locale",
      "owners": ["E35"],
      "coverage": "orphaned",
      "filed_by": ["E43"]
    }
  ]
}
```

#### Notification payloads

A [`[notify]`](configuration.md#notify) `command` destination reads each message as one JSON line on stdin: the event as above, plus `priority`, the ntfy priority the destination routed it at, and `message`, the body an ntfy destination would send. A message standing for several events, a coalesced kind or what quiet hours held, carries them in `folded`; a coalesced one takes the last event's fields with `id` and `pr` cleared, and what quiet hours held arrives when the window ends as `event` `held`, which no drive emits and only a destination's stdin carries. An exit inside quiet hours folds what was held into the `exit`. A destination's [reminder](configuration.md#reminders) of an unanswered `needs_operator` repeats that event with `reminder` added, such as `"reminder": "(reminder 2 of 6)"`, and the message calling the reminders off arrives as `event` `answered`, carrying the item's `id` and `action` and the `message` `answered: <action> <id>`, which likewise only a destination's stdin carries.

```json
{ "event": "parked", "ts": "2026-10-24T18:12:40Z", "seq": 31, "drive": "D10", "project": "drover", "id": "F14", "park": "gate_refused", "reason": "Gate failed: internal/cmd/notify.go:28:16: var-declaration (revive); sent back", "detail": "code", "priority": "default", "message": "parked gate_refused (code): Gate failed: internal/cmd/notify.go:28:16: var-declaration (revive); sent back" }
{ "event": "held", "ts": "2026-10-25T14:00:00Z", "drive": "D11", "project": "drover", "priority": "default", "message": "held during quiet hours (2):\nB93 merged #19: B93: …\nB94 merged #20: B94: …", "folded": [{ "event": "merged", "id": "B93", "pr": 19 }, { "event": "merged", "id": "B94", "pr": 20 }] }
```

### `check --json`

One object, not an array, because the counts are part of the answer.

```json
{
  "ok": false,
  "bodies": 14,
  "ledger_rows": 3,
  "findings": [
    { "kind": "MISSING DEP", "message": "7 depends on \"6b\", which has no body", "id": "7" }
  ]
}
```

`kind` is the stable token to key on: `NO BACKLOG`, `NO SHELF`, `SHELF IS BACKLOG`, `NO VERIFY`, `UNDECLARED KEY`, `NO REQUIREMENTS`, `UNREADABLE`, `BAD VALUE`, `BAD RUNNER`, `BRIDGE SIGNED OUT`, `BRIDGE UNREACHABLE`, `BRIDGE VERSION`, `BAD LEDGER`, `ORPHAN ROW`, `SHELF CLASH`, `MISSING DEP`, `BAD DEPS`, `BRANCH CYCLE`, `MISSING REPORT`, `BAD REGISTER`, `NO LABEL`, `NO REGISTER`, `DUPLICATE ISSUE`, `ORPHANED FOLLOW-UP`, `NO CLOSES KEY`, `UNLINKED`, `NO DISPOSITION`, `UNMIRRORED`, `MIRROR DRIFT`, `MIRROR STALE`, `MIRROR SHELVED`, `STALE CONFIG`, `SUPERVISOR UNSET`, `BAD TEMPLATE`, `STALE BLOCK`, `STALE POLICY`, `BAD ADDENDUM`, `NO POINTER`, `BAD MARKERS`, `PROTECTED UNMATCHED`, `SEQUENCE CLASH`, `SESSION MARKER`, `PLACEHOLDER`, `HOLD MARKER`, `LIVE REQUIREMENT`, `BROAD CITATION` and `HAND EDITED`. `message` is prose and will change wording. `path`, `line` and `id` are present where the finding has one.

`register_state` is what the follow-up read amounted to, as a token rather than as prose. Only `read` says the count of orphaned rows is the whole answer:

| State         | Means                                                                                                                     |
| ------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `read`        | The follow-up list answered whole. A zero is a zero.                                                                      |
| `truncated`   | The read stopped at `[github] page_limit`, so every count taken from it is a floor.                                       |
| `unreachable` | Nothing was read, so the findings are a floor and `ORPHANED FOLLOW-UP` could not be computed.                             |
| `unresolved`  | The read succeeded, returned nothing, and named a label or a path that resolves to nothing, so no row could have matched. |
| `off`         | `[register] backend = "none"`. Nothing was read and nothing failed.                                                       |

Switch on it rather than on `register_note`, which is prose and only ever elaborates. Under `read` the note is present only when the label set itself could not be reached, since drover then has no second fact to judge the zero by.

`mirror_note` is the same kind of prose for the [mirror](concepts.md#the-mirror): present only under `[backlog] mirror = "github"`, when the mirrored issues could not be read or the read stopped at the page limit. `MIRROR DRIFT`, `MIRROR STALE` and `MIRROR SHELVED` are then a floor; `UNMIRRORED` reads no tracker and is always whole.

`register_read` answers one question: did the read happen. It equals `register_state != "unreachable"`, and it cannot separate a whole read from a truncated one, or either from a project with no follow-up list at all.

`stopped` is present only when the report is less than the whole validation, and it is prose: switch on the findings, never on this. One case reaches it today. A config declaring a `schema_version` this build does not read, or declaring none at all, is refused before any body, ledger row or path is opened, so `check` reports the `STALE CONFIG` finding through the inspection path and `stopped` says that `bodies` and `ledger_rows` are unread rather than zero. The order is what makes that statement true: `check` loads the config, decides the version, and opens the backlog, ledger and shelf only after. A project whose own file declares no version loads when the per-user file declares the current one, so a check that opened all four first answered about whichever path failed soonest and never reached the version at all. An unrelated config error still comes first, and exits 1 naming the file that carries it. That finding's `path` is whichever file declared the version, which is the project's own except where it inherits the key from the per-user config. Treat a report carrying `stopped` as a floor.

`ok` is redundant with the exit code on purpose, so a consumer that captured stdout and dropped the status still has the answer.

### `show --json`

One object: the item's frontmatter verbatim under `meta`, its dependencies with `state`, `done` and `weak` per dep, the count of numbered requirements, the selected verify command, the runners under `implementer` and `review_runner`, and the full audit history under `audits`.

A dep's `state` is what drover found behind the id, and it is the field to read before any of the others: `readable` (a body that parsed), `unparsed` (a body on disk whose frontmatter does not), `shelved`, `absent` (no body in either directory) or `unresolved` (not an id this project can parse). Those are the same four unreadable states `check` reports as `MISSING DEP` and the prompt prints under deps it could not read.

`splits` lists every split the item took part in, found by the line `split` writes into the new item's body: `from` is the item split, `into` the item it wrote, and `direction` is `--after` when `from` waits on `into` or `--before` when `into` waits on `from`. `direction` is read from the two deps lines, which are what `status` and `drive` obey; `recorded` is the direction the split wrote, and differs from `direction` once somebody reverses the deps by hand. When neither deps line names the other, or both do, `direction` is `recorded` and the table says which. Absent for an item no split touched.

`weak` is scoped to that state and needs nothing remembered: it is `false` on any dep whose `state` is not `readable`, because a body drover could not read is not a weak foundation, it is not a foundation. It is the same statement the table makes with its `⚠`. `done`, `level` and `by` are unscoped on purpose: they echo ledger rows rather than judging a foundation, and an id whose body was shelved after it merged still merged.

`rounds` is how many times a review sent this item back, and it is the one count present at zero: omitting it would leave a consumer unable to tell "never sent back" from a field nobody wrote. It does not decide whether a clean-looking review merges: the body digest does, and a `body_edited` or `no_digest` park is explained by the ledger's body rows rather than by `rounds` at zero. `max_review_rounds` is the project's limit, absent when it set none, and `at_limit` says the next `drover request-changes` would refuse. `status --json` carries `rounds` and `at_limit` too, both omitted at zero, because it is an array over the whole backlog. `verify_send_backs`, `conflict_send_backs` and `numbered_send_backs` count the times drive sent the branch back on its own evidence, by the `changes_source` of the row, each absent at zero; none of them is in `rounds`, because none is a review round.

`review_result` is what the most recent `drive` read from the item's last review, one of the park tokens above or `passed`, and `review_result_stale` says the item has run since, which leaves the result a record of that pass rather than a statement about the branch as it stands. Both are on `status --json` and `show --json`. An absent `review_result` is not a review that found nothing: it says no drive ever computed one, which is the manual lifecycle, and `drover open-pr` reads the audit, `changes` and body digest rows instead and names which authority answered. A stale `review_result` with a review recorded after it gets the same treatment: `open-pr` sets it aside, names its token and date, and hands the answer to the ledger reading of that later review. A stale `review_result` with no review after it refuses, because nothing has looked at the branch since. Only `passed`, unstale, opens a pull request on the strength of a `drive` pass alone.

`park` and `detail` on `status --json` are the drive park standing on the item: its cause and its [detail](#park-details), read from the ledger's park row while nothing has launched the item since. An `implemented` item carrying them is waiting on whatever parked it, and the table prints it as `parked: <park> (<detail>)` instead of `implemented`. `show --json` carries the same row as `parked`, an object of `park`, `detail`, `evidence`, `log` and `date`, and `show` prints the park, the evidence line under it and the log path. A launch of any kind, or a completion, ends the park, and both fields go absent.

`dispositions` is one object per issue the item's `closes` key names: the recorded `disposition`, `by`, `note` and `date`, plus `applied` once `finish` wrote to the tracker and `settled` once two independent passes agreed, or `amended`, the date of an `amend` recorded after the latest disposition, which keeps it unsettled however many passes agreed. An issue nobody has judged appears with its `issue` number and nothing else, which is the same list `resolve`, `finish` and `check` call undisposed. It is read from the ledger alone, so no entry carries an issue title or the tracker's own state.

`runs` is how many times `drover run` launched this item, absent at zero. There is no `audits` count beside it because the `audits` array already is one, and `reviews` is absent for the reason `status --json` gives. `recent` is the last three commits on the item's branch, each `{"abbrev","lines"}`, and `recent_unknown` is why there are none: a branch that merged and was deleted, a project with `[git] enabled = false`, or a tree git could not read. One of the pair is always present, never both, the same contract `created` and `created_unknown` keep.

`implementer` and `review_runner` follow the rule they follow on `status --json`: absent means unknown. `meta.runner` is what the item asked for and answers a different question: an item pinning `runner: claude` run with `--runner codex` carries both, and only `implementer` says who ran.

`pr` is the pull request from the item's branch, with `number`, `state` and `error` as `status --json` carries them, plus `url` and `checks`, the head's check conclusion: `"success"`, `"failure"`, `"pending"` or `"none"`. A check re-run on the same head counts once, at its latest run. A merged item keeps its recorded `number` and `"state":"merged"` through a failed read, and `error` then says the `url` and `checks` went unread. `drover status <id>` prints this same document.

`body` is present only under `drover show <id> detail`: the item's text below the frontmatter fence, as it sits on disk. Absent means nobody asked for it, not an empty prompt, so a consumer wanting the prompt passes `detail` rather than reading `path`.

`marker` is the [session marker](#the-session-marker) in the item's drive worktree, present only while the tree holds one: `path`, `session`, `actor`, `pid`, `alive` for whether that pid is running, `started`, and `stage` and `stage_date` from the stage row recording the same session, absent when no stage row names it. A park on an item whose marker is `alive` is a launch still in flight.

`requirements` counts the top-level `N.` lines, which is what `report --save` turns into checkboxes. A zero there on an item that reads as fully specified means the requirements were written as prose and nothing will ever confirm them.

`modified` is the body file's mtime as a `YYYY-MM-DD` date. The file system always knows it, so the key is always there; drover keeps no `modified:` frontmatter, which a hand edit would silently leave stale.

`origin`, `created` and `edited` are the objects [`status --json`](#status---json) carries, with one difference in `created`: an item with no origin row falls back to the date of the commit that added the body, `source` `git`, rather than to the earliest ledger row. `created` is `{"unrecorded": true}`, with no `at` and no `source`, when there is neither, on a project that keeps its bodies outside git or declares `[git] enabled = false`, and `created_unknown` then carries the reason, so a consumer reads a missing date as a question nobody could ask rather than as a body with no history. A backlog under a gitignored `.drover/` is that case: `drover show B9 --json` there reports the reason rather than a zero date.

```json
{
  "modified": "2026-08-27",
  "origin": { "unrecorded": true, "text": "unrecorded" },
  "created": { "unrecorded": true },
  "created_unknown": ".drover/backlog/B09-login-redirect-loops.md is not tracked in the work tree",
  "edited": { "unrecorded": true, "at": "2026-08-27T14:02:11-07:00", "source": "mtime" }
}
```

`show` prints the same answers beside its dates: `modified   2026-08-27  edited accept · operator` and `created    2026-08-26  origin operator new`.

### `unaudited --json`

An array, worst blast radius first. `dependents` is how many items transitively depend on this one, which is how much rework a defect there would cost.

`critical`, `high`, `medium` and `low` bucket the item's open rows by the rank on their `severity_prefix` label. They sum to less than `follow_ups` when a row carries no rank, which is not an error: the follow-up is open whether or not anyone ranked it.

Read `follow_ups_known` before `follow_ups`. It is `false` when the follow-up list could not be reached, and the counts are then zero because nobody asked, not because nobody filed. That is the JSON form of the `?` the table prints, and the two rank the same item very differently.

### `intake --list --json`

An array, one object per open tracker issue carrying a label that matches an id namespace and not yet named by a body.

```json
[
  {
    "number": 324,
    "title": "deleting a group leaves a solicitation",
    "kind": "bug",
    "severity": "medium",
    "owners": ["2b"]
  }
]
```

`kind` is the namespace `drover intake <n>` would allocate against, resolved by the same rule intake uses: where an issue carries several declared kinds the one that sorts first wins, since a bug carries a stricter definition of done than an enhancement. `severity` is the tracker's own rank from the `severity_prefix` label, which is what intake seeds the scaffold's frontmatter from; absent means the issue carries no rank drover recognizes.

`owners` is present because an owned issue is the ordinary case rather than the exception: an audit files a defect against the item that owns the gap, and it sits here until somebody decides it is worth its own item. An owner link says who owns it, never that anybody scheduled it.

This is not the follow-up list. `intake --list` reads every open issue; `follow-ups` reads only those carrying the membership label. An issue in neither is the tracker's business and drover does not report it.

### `follow-ups --json`

One object. `rows` holds the tracker issues as drover reads them.

```json
{
  "truncated": false,
  "page_limit": 300,
  "rows": [
    {
      "number": 12,
      "title": "the contract checks assert format, not behavior",
      "severity": "medium",
      "owners": ["4"],
      "unowned": false,
      "orphaned": false
    }
  ]
}
```

`unowned` is `true` when neither a sub-issue parent nor an `Owner:` line names an owner. Such an issue is deliberately inert and reaches no run, which is the right state for a gap no item owns and also what a typo looks like.

`orphaned` is `true` when the row has an owner and the ledger records every one of them merged. The owner link still stands and the row still travels: the next item declaring that id on its `deps:` line inherits it, one hop. What it has lost is a run of its own, so `unowned` and `orphaned` answer different questions and a row is never both. Filter on `orphaned` to find the rows nothing scheduled will carry; do not close them on the strength of it, because whether an orphaned row should be chased, closed or given a new owner is a policy question drover does not answer. `--live` applies the same filter before the read is rendered, so a wrapper that wants only reachable rows can take them either way.

`--coverage` keeps only the rows nothing is scheduled to carry, and gives each `coverage` (`blocked`, `orphaned` or `unowned`) and, on a blocked row, `blocked_on`. Both fields are absent without it.

Read `truncated` before `rows`. It is `true` when the read hit `page_limit`, which makes `rows` a floor rather than the follow-up list: an empty `rows` beside it is a page nobody finished reading, not a project nobody has filed against. The rows are filtered to one kind and the limit applies to the whole read, so a truncated read can return no rows and still be hiding some.

A project declaring [`[register] backend = "none"`](configuration.md#register) returns an empty `rows` with `truncated` false, because there is no follow-up list and that is the whole answer.

### `projects --json`

An array, one object per project drover has been pointed at on this machine, sorted by `project` then `root`.

Any project command registers the project it runs in the first time it runs there: `open`, which every command reading a backlog passes through, appends a row when no live row names the root, and appends nothing on every later run. `drover init` registers the same way, and any project not yet listed appears after its next `status`. A command run with `DROVER_ACTOR` set never registers: it is a session drover launched, in a drive worktree of a project the operator's own command already recorded. A root under the OS temp directory registers like any other and is listed here; it is [`status --all-projects`](#status---all-projects) that counts it as scratch rather than giving it a row, because it is the reader, not the writer, that knows whether a root is still worth showing. A registry that cannot be written is a warning on stderr and never fails the command.

A root that is a git worktree of a registered project is written with `"parent"`, that project's root. The host-wide views run the same check when they read, so a worktree registered before its parent, or before rows carried the field, folds into its parent too: `status --all-projects` shows it under the parent's row, `drives --all` reads the parent's ledger alone, and `drive --pause --all` reaches the worktree's own lease under the parent's name and policy.

`drover projects --forget <root>` appends a tombstone, a row with `"forgotten":true` and today's date as `registered`, and the root leaves every list: `projects`, `status --all-projects`, `drives --all` and `drive --pause --all`. Nothing is deleted. The earlier rows stay, `drover projects --all` shows the root marked forgotten (`"forgotten":true` under `--json`), and the next command run in a forgotten project that still exists registers it again. `--forget` with a root the registry never named exits `3`.

```json
[
  {
    "project": "webapp",
    "root": "/home/me/src/webapp",
    "repo": "me/webapp",
    "registered": "2026-08-28",
    "present": true
  }
]
```

`repo` is absent when the project declares no tracker. `registered` is the date the row was written, not the date the tree was created.

`present` is the only field read from the filesystem rather than from the registry, and it is `false` when the root is gone. The row stays: the file records where drover was pointed, and a tree that moved still was. So a wrapper counting live projects filters on `present`, and one migrating a config vocabulary reads every row, because the trees it has to find are the ones nobody has visited lately.

This command needs no project and answers from anywhere, which makes it the one `--json` document a wrapper can read before it knows whether it is standing in a drover repo. An empty array means the registry is empty or absent, or that no drover command has run in a project here yet.

### `drives --json`

An array, one object per drive this project's ledger records, newest first.

```json
[
  {
    "id": "D2",
    "label": "night run",
    "started": "2026-10-17T01:00:00-07:00",
    "queued": ["4", "5"],
    "jobs": 2,
    "items": [
      { "id": "4", "outcome": "" },
      {
        "id": "5",
        "outcome": "skipped",
        "reason": "blocked by 4, which parked protected_paths for the operator"
      }
    ],
    "running": true
  },
  {
    "id": "D1",
    "started": "2026-10-16T22:00:00-07:00",
    "queued": ["1", "2"],
    "ended": "2026-10-16T23:10:00-07:00",
    "exit": 0,
    "items": [
      { "id": "1", "outcome": "ready_to_merge" },
      { "id": "2", "outcome": "parked" }
    ]
  }
]
```

`id` is `D1`, `D2` and so on: each drive that launches takes the next one in its project when it takes the lease, allocated under the ledger's lock from the highest any row records, so two drives sharing a ledger never share an id. A `--dry-run` takes none. `label` is its `--label`, absent when none was given. `started` is when the drive read its queue, and `queued` the ids it held then; a drive whose queue came up empty, or whose reconciliation failed, writes its start row as it exits, with `queued` empty, and one refused with exit `3` before reading its queue writes no drive row unless reconciliation had already written one carrying its id; `jobs` is its `--jobs`. `ended` and `exit` are absent until the drive wrote its end row, and `items` is then each item's `outcome` as its [`drive --json`](#drive---json) report gave it, one per item, in the order they first finished. An item the drive parked and then took again from `--enqueue` reads its latest outcome, at the place of its first. Until then `items` is the ids queued at launch with an empty `outcome`, except an item the running drive already skipped, which its lease publishes: that one reads `skipped` with its `reason`, and one appended after launch is added to the end. `reason` is present on a `skipped` outcome alone, in the end row too. `running` is true while this checkout's lease names the drive, and `paused` while that lease reads paused; a drive with no end row and no `running` was killed before it could write one.

`drives --all --json` reads every project in the [per-user registry](#projects---json) instead, needs no project, and emits an array of projects, each with its own `drives`. A registered root that is gone or holds no `.drover.toml` is skipped, and so is a git worktree of another registered project, whose ledger is a copy of its parent's or none:

```json
[
  {
    "project": "webapp",
    "root": "/home/me/src/webapp",
    "drives": [
      { "id": "D1", "started": "...", "queued": ["1"], "items": [{ "id": "1", "outcome": "" }] }
    ]
  },
  {
    "project": "api",
    "root": "/home/me/src/api",
    "drives": [],
    "error": "/home/me/src/api/.drover.toml: toml: line 3: expected '='"
  }
]
```

`error` is why one project's drives could not be read, present only then; the rest of the registry still answers.

### `skills --json`

An array, most-referenced first. `state` is `vendored`, `missing` or `unreferenced`. Fail your pipeline on `missing`: an unattended run cannot read a guide that is not in the tree.

`--from` and `--prune` are the two flags that write while the array is produced. Both run before it is encoded, and both report on stderr, so stdout stays the array alone either way.

`--from <dir>` copies every referenced guide into `[paths] skills` and reports `vendored N guides from <dir>` plus any name the source did not have. The array is recomputed after the copy, so a guide that arrived reads `vendored` rather than `missing`.

`--prune` deletes every `unreferenced` guide from the tree and reports `pruned N vendored but unreferenced: <names>`. The array is _not_ recomputed, so those rows are still in it and still read `unreferenced`: that is how the document names what went, and it is the state the guides were found in rather than the state they are in now.

### `report --json`

One object. `unfinished` is `true` when the log exists but carries no result event, which means the run is still in flight or it died. That is a different answer from "never ran" (exit 1, no log), and a wrapper deciding whether to review needs the difference.

`log_path` is absolute, and stays absolute whatever the terminal output does with it: `os.Open` and every shell-free reader expand no `~`, so a path shortened for a person to read is one a wrapper cannot open. `report_path` stays relative to the project root, because the file it names is inside the tree and the wrapper is standing in it.

### The `t3` log

The `drover t3 run` bridge writes a stream-json log like any runner's, so `report`, `attach --session`, `status` and the usage rows read it through a `t3` schema. Its own lines name their kind under the key `t3`, never under `type`, which `claude` and `codex` both key on. Between them it passes the Claude Agent SDK's `rate_limit_event` and `result` events through unchanged from T3 Code's provider event log, so the [`claude` schema](configuration.md#what-drover-reads-back) reads the window readings, turns, tokens and cost from them exactly as from a direct `claude` log.

| `t3`                | Written                                                          | Fields                                                                                                                                 |
| ------------------- | ---------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `thread.started`    | First, once the launch returns                                   | `thread_id`, `run_id`, `project`, `provider`, `model`, `runtime_mode`, `session` (the drover launch id), `url` (`t3-thread://v1/<id>`) |
| `message`           | Each user or assistant message item                              | `role` (`user` or `assistant`), `item_id`, `position`, `status`, `text`, `text_truncated`                                              |
| `reasoning`         | Each thinking item                                               | `item_id`, `position`, `title`, `text`                                                                                                 |
| `tool`              | Each other item, `command_execution` included                    | `item_type` (T3's own type), `item_id`, `position`, `title`, `text`                                                                    |
| `approval`          | A run entering `waiting`, an approval item, or a pending request | `status` (`waiting` or `pending`), `request_id` and the raw `request` where the server listed one. drover answers none.                |
| `checkpoint`        | Each checkpoint item, one per run                                | `item_id` (which carries the checkpoint scope and name), `text`                                                                        |
| `usage_unavailable` | Before the result, when the provider log yielded no SDK `result` | `path`, `reason`                                                                                                                       |
| `result`            | Last, always                                                     | See below                                                                                                                              |

An item is printed once, when its status says it is finished, because `afterPosition` is exclusive and an item read half-written would never be read again; at the end of the run every item is printed as it stands. The terminal line:

```json
{
  "t3": "result",
  "thread_id": "mcp:68eab9c9-…",
  "run_id": "run:thread:mcp%3A68eab9c9-…:ordinal:1",
  "provider": "claude",
  "model": "claude-haiku-5-5",
  "subtype": "success",
  "result": "ok",
  "num_turns": 1,
  "usage": {
    "input_tokens": 2,
    "output_tokens": 48,
    "cache_creation_input_tokens": 60365,
    "cache_read_input_tokens": 12817
  },
  "cost_usd": 0.01235007,
  "checkpoints": ["turn-item:provider:claudeAgent:native-item:checkpoint%3A…"],
  "session_id": "5b1c8f0e-…",
  "duration_ms": 4844
}
```

- `is_error` is on every `result` line, `false` for `success`, and on no other kind. `subtype` is `success` for a run T3 reports `completed`. Anything else sets `is_error` and names the run's own status (`interrupted`, `failed`, `cancelled`), the server's refusal code (`thread_not_found`, `capability_denied`), or the bridge's (`unauthorized`, `wait_failed`, `refused` for a project, provider or model that stopped the stage before a launch). `result` then carries why, followed by the run's last assistant message where it wrote one. The bridge exits 1 for all of them.
- `num_turns`, `usage`, `cost_usd` and `session_id` come from the SDK `result` in the provider log. A stopped bridge interrupts its run and waits up to 8 seconds for the SDK `result` the interrupt writes, inside the 10 seconds `runner.Run` allows after `SIGTERM`. Where there was none, all four are absent rather than zero, and `usage_unavailable` says the counts were not reported and why. A usage row is written only from a result that carries `usage`.
- `checkpoints` is the item id of every `checkpoint` item of the run, empty when there were none.
- `session_id` is the SDK's own session id, which also names the Claude Code transcript the SDK writes, so `usage --sync` does not count a T3 thread twice.

## Config as data

```bash
drover schema
```

Prints the JSON Schema for `.drover.toml`. It needs no project, so it answers from anywhere, including a tree with no config yet. Use it to validate a config before drover runs, to complete keys in an editor, or to generate one.

The schema is embedded in the binary and a test walks the Go types against it, so a build cannot ship a binary that disagrees with the contract it publishes.

```bash
drover schema --frontmatter
```

```json
{
  "required": ["title", "deps"],
  "optional": ["kind", "issue", "severity", "skills", "runner"],
  "shape": { "issue": "int", "severity": "enum:C,H,M,L" }
}
```

The other direction: what a body in _this_ project must satisfy, so an agent drafting one reads the target rather than imitating a neighboring file. `shape` is what `check` enforces as `BAD VALUE`; a key with no entry there may hold anything, and an empty value passes every type. See [`[frontmatter] shape`](configuration.md#frontmatter-shape).

```bash
drover schema --ledger
```

Every key a ledger row may carry, with the Go type it holds, read off the row type drover writes: `{"changes":"string","changes_source":"string","findings":"string",...}`. Use it before appending a row by hand. It needs no project.

**It does not cover the cross-field rules.** A schema-valid config can still be rejected by `Load`: a frontmatter key listed as both required and optional, a `root_marker` naming a file that is not on disk, a `planned_max` that excludes an id the backlog already uses. Validate with the schema to catch the typo, then run `drover check` to catch the rest.

## Instructions as output

```bash
drover agents                       # to stdout
drover agents --write DROVER.md     # replace the marked block in place
drover agents --pointer DROVER.md   # the stanza AGENTS.md needs to reach it
```

Generates this project's agent instructions from its own config: the paths, the verify commands in precedence order, the label vocabulary, the frontmatter schema, the command-safety classes and the exit codes above.

The safety table it renders is the same one each command's `--help` prints its line from, read at run time rather than copied. Two hand-kept lists of what writes will disagree eventually, and neither of them will report it.

`--write` replaces the text between `<!-- BEGIN drover -->` and `<!-- END drover -->`, appends the block if the markers are absent, and touches nothing outside them. Anything hand-written in the same file survives.

Regenerate it in CI and fail on a diff. A hand-filled copy goes stale the day the binary gains or changes a command, keeps giving agents the old instructions, and nothing reports the conflict.

`[agents] principles` is the one thing that cannot be read off the rest of the config. It lives there rather than as a hand-edit in the generated file so that regenerating cannot lose it.

## Degradation

Every follow-up read degrades rather than fails.

| Command                 | Without `gh`                                                                                                                                     |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `status`                | Full table; the follow-up counter is omitted.                                                                                                    |
| `unaudited`             | Full ranking; `follow_ups_known` is `false` and the table prints `?`, never `0`.                                                                 |
| `check`                 | Every file finding; no `ORPHANED FOLLOW-UP`, `register_state` is `unreachable`, and a note says the finding set is a floor. Exit code unchanged. |
| `run`                   | Assembles and runs; carries no inherited-work note.                                                                                              |
| `finish`                | Every step, exit code unchanged; the orphaned follow-up line is dropped and a reason printed in its place.                                       |
| `follow-ups` / `intake` | Exit 1 with a message.                                                                                                                           |

Completion, audit levels and the dependency graph are local reads and stay exact with no network at all.

## A CI job

```yaml
- run: drover check --json > check.json || true
- run: |
    jq -e '.ok' check.json || {
      jq -r '.findings[] | "\(.kind)\t\(.message)"' check.json
      exit 1
    }
- run: drover skills --json | jq -e 'all(.state != "missing")'
- run: |
    drover agents --write DROVER.md
    git diff --exit-code DROVER.md
```

The `|| true` on the first line is deliberate: `check` exits 2 on findings, and the job wants to print them before it fails.

## Writing to the ledger without the binary

An agent that ran an item in a sandbox with no drover and no network records the fact by appending one line:

```json
{"id":"16","stage":"implement","runner":"claude","date":"2026-07-14"}
{"id":"16","merged":true,"implementer":"claude","date":"2026-07-14","report":".drover/reports/16-x.md"}
{"id":"16","level":"claimed","code_by":"agent","date":"2026-07-14","note":"…"}
{"id":"16","level":"reviewed","reviewer":"sam","code_by":"agent","independent":true,"date":"2026-07-15","note":"…"}
```

End each line with a newline. Before it appends, drover reads the file's last byte and writes a newline first when the last line has none, so a row written by `printf` or an editor that left it unterminated stays a row of its own. That repair happens only at the next drover append: until then the file ends mid-line, and any other reader that splits on newlines, or a second hand-written line, sees the two run together.

Every row drover writes carries `ts` beside `date`: the instant it was written, RFC 3339 with the writer's offset (`"ts":"2026-07-14T09:12:03-07:00"`). `date` stays, so a reader keyed on it and every row written before `ts` existed still parse. A row without `ts` is day-only: anything drover derives from times, `status`'s `started`, `stage_started`, `finished` and `eta`, reads it as absent rather than as midnight. A line appended by hand may carry either.

`reviewer` on an audit row is who looked, as the operator attributes it, and it is how a human reviewer is recorded at all. Omit it and the row says nobody was named; drover never fills it in, because a reviewer the tool invented is one nobody can go and ask. `agent` on a completion row means who the record credits with the work, which `mark-merged` and `finish` do default. `level` is the only key drover reads as an audit level; a row carrying an `audit` key instead is not read as an audit row. `independent` is an assertion when written by hand, as `--code-by` is when typed.

A `changes` row is one `drover request-changes`: `changes` is a digest of the evidence, and `reqs` the requirement numbers its findings name, sorted, absent when none does. A finding is a heading outside a code fence, or the whole evidence when it has none, and a number counts only when the body's list holds it. `still_open` is the requirement numbers the reviewer passed to `--still-open`: those whose finding from the previous request it reproduced as still open, a subset of that row's `reqs`, absent when empty. `changes_source` names what wrote the row: `review` for `drover request-changes` run by a review or a person, `verify` for drive sending a branch back on a verify command or merge gate it failed, `trunk_conflict` for a trunk merge that conflicted and a session resolves, and `numbered_files` for a `[numbered_files]` clash. Only `review` rows, and rows with no `changes_source` (written before the field existed), are rounds under `[review] max_review_rounds`; `drover show` prints drive's send-backs on a line of their own. A `refused` row carries `reqs` too, naming what the request the cap turned away named, and `findings`, the path from the project root of the file holding the evidence it turned away, `<stem>-<slug>.refused-N.md` beside the kept rounds. `operator` is the requirement numbers the reviewer passed to `--operator`: those whose finding no implementing session can meet as written, each named by a finding in the evidence that carries the reason, absent when empty. `drive` parks a pass whose latest `changes` row carries `operator` as `needs_decision` before it asks whether the round repeated, and launches no implementing session for the item after it in that drive.

`drive` parks the item `repeating` on two consecutive rows with the same `changes`, or on two consecutive rows each carrying `still_open` where the two lists share a number: two runs were sent the same finding and neither closed it. Equal `reqs` alone is not a repeat, because one requirement can yield a different defect every round while the branch improves, and `[review] max_review_rounds` is the bound on that.

```json
{"id":"16","changes":"9c41e07b2d3a","reqs":[4,7],"still_open":[4],"date":"2026-10-26","ts":"2026-10-26T10:02:11-07:00"}
```

A body row records that drover wrote an item's body: `body` is a 12-hex digest of the file, and `cmd`, `actor` and `agent` say who wrote it. `cmd` is the drover command (`accept`, `unaccept`, `amend`, `invariant`, `link`, `split`, `request-changes`, `close-changes`, `touch`); `request-changes` keeps that name when `drive` sends work back through it. `actor` is `operator`, `agent` for a session drover launched, or `supervisor` for the one `drover supervise` started. `agent` names the interactive agent driving the command: in a session drover launched it is the runner that session's stage row recorded, and in a terminal drover did not start it is `claude` when `CLAUDECODE=1` is set, `codex` when `CODEX_THREAD_ID` is, and `""` otherwise. The `agent` key is always present on a body row, so an empty value means drover saw no agent rather than that nobody recorded one. drover refuses to append a row carrying `body` without `cmd` and `actor`. `touch` always records `operator`, since a person's hand edit is what it certifies. A review stage row carries the same four keys beside its `body`, with `cmd` the command that launched the session, `review` or `drive`. A file whose digest no longer matches the latest body row was edited by something drover did not run. A body row written before these keys existed carries none of them, and a reader reports its author as unrecorded rather than as the operator.

```json
{"id":"16","body":"a1b2c3d4e5f6","cmd":"accept","actor":"operator","agent":"claude","date":"2026-10-27","ts":"2026-10-27T09:12:03-07:00"}
```

An origin row records how an item came to exist, and a set `origin` object is what makes a row one. `new`, `intake` and `split` each write one when they create a body, and nothing else does. `origin` holds every key whether or not it applies: `cmd` (`new`, `intake` or `split`), `actor` and `agent` as on a body row, `session` (the launch id of the session drover started, empty for the operator), `found_in` (`new --found-in`), `issue` (the tracker issue `intake` read, 0 otherwise) and `split_from` (the item `split` took the work from). Where `session` matches a stage row's, that row's `drive`, `stage` and `id` say which drive's session filed it and while working which item. An item created before origin rows existed has none, and drover never writes one after the fact: a row it did not witness would be a record nobody made, so a reader reports the origin as unrecorded. The `Found while working <id>, <title>.` line `new --found-in` writes into the body stays, because `draft` reads it back.

```json
{"id":"B105","origin":{"cmd":"new","actor":"agent","agent":"codex","session":"5e55a0c1d2e3","found_in":"B104","issue":0,"split_from":""},"date":"2026-10-27","ts":"2026-10-27T09:14:40-07:00"}
```

The stage row is what `run` and `review` write themselves, and writing it is what lets `mark-merged` and `finish` attribute the work without being told. An agent recording a run by hand should write one too; omitting it costs the attribution, not the completion.

A row `drive` appends carries `drive`, the drive's id, and a row written outside a drive leaves it out. Each drive also writes two rows of its own under `"id":"drive"`, an id no namespace can mint: a start row once its queue is read, and an end row when it exits, the one carrying `exit`. The id itself is taken with the lease, before reconciliation, so the rows reconciliation's `finish` appends carry it too; a drive that exits before reading its queue writes its start row, with nothing `queued`, just ahead of its end row, unless it exits `3` with no row yet carrying its id, which leaves the ledger untouched. The end row's `outcomes` holds one entry per item, its latest, at the position of the item's first: an item parked and then taken again from `--enqueue` is listed once, as what the second take came to. An end row written by a drover that still asked for `--auto` can carry an entry with `merge_queued` and `pr`, the pull request the forge queued; a current drive writes neither. The end row reports what the drive saw as it exited; reconciliation does not read it. It asks git whether each unmerged item's branch reached the trunk, and for a ledger an older drover wrote it also reads the pull request row carrying `merge_queued` that drover appended when the forge accepted the request, holding that queue outstanding until a completion row for the item or a row naming another pull request, in the order the rows were appended. So a later drive that holds, skips or parks the item without opening a new pull request leaves the next reconciliation's `merged` event owed, and a replacement pull request or a completion recorded while the queuing drive was still running ends the queue even though the end row written after it still carries `merge_queued`. An end row written by an older drover can hold two entries for such an item, the park first; `drives` and `status` read the latest entry per id and leave the row as written, and a reader of the ledger itself should do the same. A drive that [re-executed onto a new binary](#resuming-onto-a-new-binary) writes a second start row under the same id, with `version` the binary it now runs and `replaced` the one it ran before; `drives` keeps the first row's start time and queue.

```json
{"id":"drive","drive":"D2","label":"night run","queued":["4","5"],"jobs":2,"date":"2026-10-17","ts":"2026-10-17T01:00:00-07:00"}
{"id":"drive","drive":"D2","exit":0,"outcomes":[{"id":"4","outcome":"ready_to_merge","merge_queued":true,"pr":31},{"id":"5","outcome":"parked"}],"date":"2026-10-17","ts":"2026-10-17T03:12:40-07:00"}
```

`verify_ms` is the wall time in milliseconds of a [`[verify]`](configuration.md#verify) command drive ran for the item, on an updated tree or after a resume changed the command. It rides on the next row drive appends for that item through its own session once the verify command exits, whatever the row's type: a head, pull request, stage, park or completion row. On a branch update whose verify command passed, or failed on a trunk that passes it alone, that is the head row naming the merged tree, which drive writes before it pushes and before anything is sent back. When drive undoes the merge instead, because the trunk fails the verify command alone or could not run it to say so, no such head row exists and the time rides the park row that stops the item, `trunk_red` or `gate_unrunnable`. A `changes` row never carries it, because drive sends work back through `drover request-changes`, outside its own session. A red verify command on the updated tree is followed by the same verify command on the trunk alone; the item's own run is the one recorded. [`trends`](#trends) reads it as `verify`.

```json
{
  "id": "16",
  "branch": "16-cache",
  "head": "3f9c2a1",
  "verify_ms": 1184220,
  "drive": "D3",
  "date": "2026-10-17",
  "ts": "2026-10-17T02:14:09-07:00"
}
```

A usage row records one session's tokens, and a set `usage` object is what makes a row one. A headless session gets one when its runner exits, written by `run --headless`, `review --headless`, `draft --headless` and `drive`, whenever the log's result event carries usage, a failed or interrupted session included. `drover usage --backfill` writes the same rows from logs already on disk, and `drover usage --sync` backfills first and then writes `"source":"interactive"` rows from Claude Code's transcripts, one per session per local day, with `first_ts` and `ts` the day's first and last message, `started` the session's first message on any day, and `id` empty. `session` is the drover session id the stage row carries and `log` the log's basename; a backfilled row has no `session`, since a log does not record one. `context_max` is the largest `input + cache_read + cache_create` of any one message, how full the context window got. `session_id` is the runner's own id. A headless session has one usage row; an interactive one has one per local day, and a session any run log names is never recorded as interactive. Claude Code writes a transcript for every headless session as well, so `--sync` backfills before it reads a transcript, and `drive` runs the same step before it reads its queue and after its end row. Write a usage row only when no usage row names its `session_id` yet, or the session counts twice. A Claude log can hold several result events, one per segment of a session that ended a turn and resumed; `duration_ms`, `turns` and `usage` sum every segment's, and `cost_usd` is the last one's, since Claude Code already reports cost as a running total. Rows written before drover summed segments recorded the last segment alone and undercount a multi-segment Claude session, often by one to two orders of magnitude. The ledger is append-only and `--backfill` skips a session a usage row already names, so those rows stay as written.

```json
{
  "id": "16",
  "stage": "implement",
  "session": "8f2c41d09a7e",
  "log": "16-cache.20261017-021409.512003000.jsonl",
  "usage": { "input": 140, "output": 57264, "cache_create": 1261852, "cache_read": 11419637 },
  "model": "claude-opus-5-5[1m]",
  "session_id": "ad34e1d2-b659-4b2b-be96-29cfd8220613",
  "cost_usd": 9.8969774,
  "duration_ms": 3669375,
  "turns": 72,
  "source": "headless",
  "context_max": 412880,
  "drive": "D3",
  "date": "2026-10-17",
  "ts": "2026-10-17T03:15:18-07:00"
}
```

A codex session writes a usage row from the `usage` object on its terminal `turn.completed`, `turn.failed` or `turn.aborted` event, and a terminal event carrying none writes no row. Its fields map as `output_tokens` to `output`, `cached_input_tokens` to `cache_read`, `cache_write_input_tokens` to `cache_create`, and `input_tokens` less both cache fields, floored at 0, to `input`: codex counts its cached input inside `input_tokens`, while `input` means uncached input, the way Claude Code reports it. `reasoning_output_tokens` is already inside `output_tokens` and is not recorded apart. `session_id` is the `thread_id` from codex's `thread.started` event. A codex row carries no `model` and no `cost_usd`, since codex reports neither, and `context_max` is 0, because codex states usage per turn rather than per message.

A row from a session whose log carried a `rate_limit_event` adds `rate_limit`, the last one it wrote, renamed into the ledger's own case: `status` (`allowed`, `allowed_warning`, `rejected`), `rate_limit_type` (the window that status is about), `resets_at` (a Unix second), `five_hour` and `seven_day` (each `utilization` from 0 to 1, its `resets_at`, and `status` where the event gave one), `is_using_overage`, `overage_status` and `overage_disabled_reason` (`out_of_credits` once purchased credit is spent). It is absent on a row from a runner that writes no such event, codex and agy among them. `status --json` reports the newest one as [`rate_limit`](#rate_limit), and `drive`'s usage admission judges it.

```json
"rate_limit": {
  "status": "rejected",
  "rate_limit_type": "seven_day",
  "resets_at": 1792983600,
  "five_hour": { "utilization": 0, "resets_at": 1792834200, "status": "allowed" },
  "seven_day": { "utilization": 1, "resets_at": 1792983600, "status": "rejected" },
  "overage_status": "rejected",
  "overage_disabled_reason": "out_of_credits"
}
```

A `usage_warning` drive sent leaves one row under `"id":"usage"`, an id no namespace can mint, naming the window and the reset it was about, with `note` the sentence. Any row naming the pair stops a second notice, in this project or another registered one; `check` and every item reader skip it.

```json
{"id":"usage","usage_warning":"weekly","resets_at":1792983600,"note":"the weekly window has 18% left, at or below [usage.weekly] notify_remaining 20; it resets Sun Oct 25 20:00 PDT","drive":"D9","date":"2026-10-23","ts":"2026-10-23T11:40:02-07:00"}
```

The counts are tokens as the API reported them, not the subscription's quota accounting, and `cost_usd` is list price, not what a subscription charged. drover's other readers ignore usage rows: no stage, pass or run count changes because one is there, and `check` reads one with an empty `id` as a usage row rather than a malformed line.

Every row type resolves last-row-wins, and `status` and `show` also count the rows a fold discards: `runs`, `audits` and, in `show`, the length of the `audits` array. A disposition carries `settled` once two independent passes recorded the same answer in a row with nothing since disagreeing.

A withdrawal is an append (`"merged":false`), never a rewrite, which keeps the file append-only for every reader and stops a withdrawal from destroying that item's audit history as a side effect.

That protocol is the reason the ledger is text rather than a database. A malformed line is reported by `check`, never skipped: a silently dropped row is indistinguishable from a record nobody ever made.
