<p align="center"><img src="docs/assets/drover-emblem.png" width="256" alt="Pixel-art drover in an orange coat and red scarf, holding a staff and herding three cream robot terminals with glowing code on their screens"></p>

# drover

[![CI](https://github.com/ravinald/drover/actions/workflows/ci.yml/badge.svg)](https://github.com/ravinald/drover/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ravinald/drover?sort=semver)](https://github.com/ravinald/drover/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/ravinald/drover)](go.mod)
[![License](https://img.shields.io/github/license/ravinald/drover)](LICENSE)

Drive AI coding agents from Markdown requirements to reviewed code, with an audit trail for every work item.

A work item is one Markdown file, and the directory holding them is the backlog. Completion and inspection are recorded in an append-only JSONL ledger that stays diffable in git. Everything a project decides for itself (where those files live, what decides whether a change merges, what "done" costs) is configuration, not code. The [Glossary](docs/glossary.md) defines every term drover prints, one line each.

## Install

Three paths. A release binary needs nothing installed; building from source needs Go 1.26.

```sh
# a release binary: verify it, then put it on your PATH
shasum -a 256 -c checksums.txt --ignore-missing
install -m 0755 drover_*_darwin_arm64 /usr/local/bin/drover

# the tree, with the build stamped from the commit you checked out
git clone https://github.com/ravinald/drover.git && cd drover && make install

# go install, with no clone to keep
go install github.com/ravinald/drover/cmd/drover@v0.1.0
```

Binaries and `checksums.txt` are on the [latest release](https://github.com/ravinald/drover/releases/latest). [Releasing](docs/releasing.md#what-each-install-path-can-report) says what `drover version` can report from each path, and why a module zip reports less.

`gh` is optional: without it the follow-up commands exit 1 and everything else works offline, saying what it could not read rather than reporting a zero.

`.drover.toml` is executable configuration: `[runners]` names the binary `drover run` launches. Read [Trust](docs/concepts.md#trust) before running drover in a repository you did not write.

## Quick start

The repository ships a runnable demo at [`examples/`](examples/): `herdsay`, a small `cowsay` clone in Go, with one merged item to read and three for an agent to build. From the clone, `cd examples` and every read command answers:

```sh
drover check                  # config, items, ledger and the paths between them
drover status                 # B1 and F1 ready, F2 blocked by B1
drover show 1                 # the merged item, with its audit history
drover run B1 --dry-run       # exactly what an agent will be handed
```

[`examples/README.md`](examples/README.md) goes on to build B1 with an agent, and to drive all three with `drover drive --jobs 2`.

In your own project, an item goes:

```sh
drover new bug "the resolver drops a trailing dot"   # a scaffold, inert
drover draft B1              # the drafting prompt for its requirements
drover accept B1             # you read that list and accept it
drover run B1                # branch, then hand it to the runner
drover review B1             # a second session reviews the branch
drover audit B1 verified --code-by agent "R1-R2 traced to shipped code"
drover finish B1             # after merging: ledger row, report, trunk
```

`B1` is the first bug: the letter names the kind of work and the number counts within it. [Item ids](docs/concepts.md#item-ids) explains the scheme.

[Lifecycle](docs/lifecycle.md) walks the same sequence with what each step refuses to do for you.

## Start your own project

Two files, in this order. `drover init --user` writes the per-user half at `$XDG_CONFIG_HOME/drover/config.toml` (or `~/.config/drover/`), once per machine: where work lives, which runner you launch, the frontmatter schema your items share. `drover init` writes each project's own `.drover.toml`, filling in what it can read off the tree and leaving out whatever the per-user file already supplies. It asks one question, the command that must pass before an item merges, because nothing can infer that:

```sh
drover init --user
cd myproject && drover init
drover check                  # exit 0 or a finding that names what to fix
```

`--defaults` on the first and `--verify "go test ./..."` on the second answer every question, which is the form a script uses.

[Getting started](docs/getting-started.md) is that sequence through to an agent running an item, including the tracker labels `intake` reads and the one step no agent performs. [Configuration](docs/configuration.md) carries every key; [`examples/.drover.toml`](examples/.drover.toml) is a filled-in one with the choices commented.

## Commands

**Who** is the actor each step belongs to, and drover enforces it. A command marked `you` refuses inside a session drover launched and exits 3, because it either certifies work somebody else has to trust or reaches a tracker other people read. Separately, a launched session cannot launch another, so `run`, `draft --runner` and `review --runner` refuse there too. `either` is safe from a coding session. [Agent guide](docs/agent-guide.md) is this table written for the agent, and it explains what the check is and is not.

| Command                              | Who      | Does                                                                                                                                                                                                                                                                                                                 |
| ------------------------------------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `init --user`                        | either   | Write the per-user config, once per machine. `--defaults` asks nothing.                                                                                                                                                                                                                                              |
| `init`                               | either   | Write a starting `.drover.toml` and the directories it names. Asks for the verify command; `--verify` answers it.                                                                                                                                                                                                    |
| `check`                              | either   | Validate config, backlog, ledger and the paths between them. Exit 2 on findings.                                                                                                                                                                                                                                     |
| `agents`                             | either   | Generate this project's agent instructions from its own config. `--write <path>` replaces the marked block; `--pointer <path>` prints the stanza that sends `AGENTS.md` there.                                                                                                                                       |
| `config`                             | either   | Print the `.drover.toml` this binary would write for this project today, with the project's own values in it. `--diff` prints only what differs. Never writes.                                                                                                                                                       |
| `schema`                             | either   | Print the JSON Schema for `.drover.toml`, or `--frontmatter` for this project's body contract.                                                                                                                                                                                                                       |
| `projects`                           | either   | Every project drover has been pointed at on this machine.                                                                                                                                                                                                                                                            |
| `new <kind> <title>`                 | either   | Write a scaffolded item. No tracker involved. `--sev` ranks it.                                                                                                                                                                                                                                                      |
| `intake <n>`                         | you      | Turn a tracker issue into a scaffolded item, and label the issue.                                                                                                                                                                                                                                                    |
| `intake --list`                      | either   | Tracker issues drover could take in and has not.                                                                                                                                                                                                                                                                     |
| `status`                             | either   | What is left, each item's state, its audit level and its pull request. `--all` adds the merged rows, `--sev C,H` narrows by severity, `--by-sev` orders worst first, `--no-tracker` asks the forge nothing, `--json` for CI. `status <id>` is `show <id>`.                                                           |
| `show <id>`                          | either   | Everything about one item, including its full audit history.                                                                                                                                                                                                                                                         |
| `skills`                             | either   | Which guides the backlog references, and which are vendored. `--from` vendors them, `--prune` deletes what nothing references.                                                                                                                                                                                       |
| `shelve` / `unshelve <id>`           | either   | Retire a body to `[paths] shelf`, or bring it back. Neither records a completion.                                                                                                                                                                                                                                    |
| `draft <id>`                         | either\* | Assemble the prompt for drafting a scaffold's requirements. Refuses a body past intake. `--runner` hands the prompt to a session instead of printing it.                                                                                                                                                             |
| `accept` / `unaccept <id>`           | you      | Delete or restore the stop marker. The one step drover does not automate, and no flag on another command reaches it.                                                                                                                                                                                                 |
| `run <id>`                           | either\* | Assemble the prompt and hand it to a runner. `--dry-run` reads only. Launching refuses inside a launched session. `--runner`, `--headless`.                                                                                                                                                                          |
| `review <id>`                        | either\* | Assemble the prompt for reviewing a finished item. `--runner` hands it to a session; never switches the branch.                                                                                                                                                                                                      |
| `drive [id...]`                      | you      | Walk the ready queue unattended: implement, review, send findings back and re-run, then open a pull request for what `[drive] merge_gate` passes and merge it, bounded by `--for` or `--until`. `--jobs N` runs N items at once. `--no-merge` holds every merge and keeps the pull requests. `--dry-run` reads only. |
| `drives`                             | either   | Every drive this project has run, its label, when it started and ended, and what each item came to. `--all` covers every registered project.                                                                                                                                                                         |
| `attach` / `watch`                   | either   | Follow a running drive's events, or block until it emits a named one.                                                                                                                                                                                                                                                |
| `supervise`                          | you      | Launch a supervising session that watches drives and handles what `[supervise]` lets it.                                                                                                                                                                                                                             |
| `request-changes <id>`               | either   | Write what a review sent back into the item body, for the next run to read.                                                                                                                                                                                                                                          |
| `close-changes <id>`                 | either   | Record that the block a review sent back is closed.                                                                                                                                                                                                                                                                  |
| `invariant <id>`                     | either   | Record a property the code must keep, for every pass after this one.                                                                                                                                                                                                                                                 |
| `split <id>`                         | either   | Move work the contract never named into its own item, and block this one on it.                                                                                                                                                                                                                                      |
| `link <id> <issue>...`               | either   | Add an issue to the item's `closes` key, so `finish` answers whoever filed it.                                                                                                                                                                                                                                       |
| `signoff <id> <n> <what>`            | you      | Record that a person met one `[operator]` requirement.                                                                                                                                                                                                                                                               |
| `open-pr [id]`                       | you      | Open the pull request for a branch the merge gate and the recorded review pass both approved. Nothing else opens one. `--force` opens it on your own judgement.                                                                                                                                                      |
| `report <id>`                        | either   | What the last run claimed. `--save` writes it beside a checklist.                                                                                                                                                                                                                                                    |
| `finish [id]`                        | you      | After merging: ledger row, saved report, back to the trunk. Defaults to the checked-out branch's item. `--delete-branch` drops the local branch, `--force` records a completion the branch does not show.                                                                                                            |
| `mark-merged` / `unmark-merged <id>` | you      | Record or withdraw a merge. `unmark-merged` appends a tombstone.                                                                                                                                                                                                                                                     |
| `audit <id> <level>`                 | either   | Record how hard someone looked. `--code-by` is required, and `verified` from whoever wrote the code is refused.                                                                                                                                                                                                      |
| `resolve <id> <n> <disposition>`     | either   | Record whether the item's work resolves one issue its `closes` key names. `--code-by` is required. Writes to no tracker; `finish` applies it.                                                                                                                                                                        |
| `unaudited`                          | either   | Merged items nobody inspected, worst blast radius first.                                                                                                                                                                                                                                                             |
| `follow-ups [id]`                    | either   | The open follow-up list, all of it or one item's. `--live` hides rows whose owners have all merged.                                                                                                                                                                                                                  |
| `follow-up open` / `follow-up close` | either   | File or close a row, on a project whose follow-ups live in a file.                                                                                                                                                                                                                                                   |
| `version`                            | either   | Print version, commit and build date.                                                                                                                                                                                                                                                                                |

\* Reading is safe from a session; launching one is not.

`drover <command> --help` carries the reasoning behind each, a worked `Examples:` block, and a `Safety:` line naming what that command writes, resolved against this project's own paths, plus an `Actor:` line on the ten that refuse inside a session. The root listing groups them by kind, not in lifecycle order; [Lifecycle](docs/lifecycle.md) is the order an item moves through them in. Ids are bare (`B1`); the padded form (`B01`) belongs to filenames only, and a padded id is refused with the bare form spelled out.

Exit `0` clean, `1` drover could not answer, `2` findings against the project (`check`), `3` a rule drover enforces refused the operation. `--json` is on `status`, `show`, `unaudited`, `drive`, `drives`, `attach`, `check`, `follow-ups`, `intake --list`, `projects`, `skills` and `report`; [Automation](docs/automation.md#--json) carries every shape.

## Documentation

For a person:

- [Getting started](docs/getting-started.md): install to a first run, in a repository of your own. **Start here.**
- [Example project](examples/): `herdsay`, a runnable demo with one merged item and three for an agent to build.
- [Lifecycle](docs/lifecycle.md): one item start to finish, and what each step refuses to do for you.
- [Playbook](docs/playbook.md): sizing an item so a run can finish it, choosing interactive or `--headless`, and what a review is worth when one runner did both stages. The three judgement calls drover leaves to you.
- [Concepts](docs/concepts.md): the data model, item ids, the audit levels, how a follow-up finds its owner, and why each store is where it is.
- [Configuration](docs/configuration.md): every `.drover.toml` key.
- [Glossary](docs/glossary.md): every term drover prints or reads, one line each.
- [Releasing](docs/releasing.md): the tag format, what the pipeline stamps into each artifact, and what to check once it has run.

For an agent, or for whoever is wiring one up:

- [Agent guide](docs/agent-guide.md): what a coding session may run, what it may not, the refusals it will hit and the exit code each one carries.
- [Agent instructions](docs/agents.md): `drover agents` generates a project's own instructions from its config, so they cannot drift past the binary, and `--pointer` prints the line in `AGENTS.md` that sends an agent there.
- [Automation](docs/automation.md): exit codes, every `--json` shape, and the ledger protocol for a sandbox with no binary.
- [Prompt templates](docs/prompts.md): the data a template reaches and how to write your own.
- [rtk filters](docs/rtk-filters.toml): copy to `<project>/.rtk/filters.toml` if you use [rtk](https://www.rtk-ai.app/).

## Status

Pre-1.0. Until v1.0.0 a minor release may rename a command, a config key or a stored field, and the release notes list every rename. The config schema is version-gated, so drover refuses a file it does not understand rather than misreading it.

## Name

A drover moves someone else's livestock along a road to market. Not the owner, not the buyer: the one who keeps the herd on the road, counts what arrives, and says what condition it arrived in. That is the boundary this tool draws. `finish` refuses a branch it cannot see merged, and an author cannot record their own work `verified`, because escorting work is not the same as owning it.
