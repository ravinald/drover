# Changelog

## v0.1.1

- **A path in `[drive] worktree_links` reaches every session.** drive linked it only when it opened a worktree, so a branch update that merged a trunk which had stopped tracking the path (a `CLAUDE.md` moved out of git) deleted the branch's copy, and the sessions after it ran without the file. drive now links those paths again before each session.

## v0.1.0

The first public release.

- **Work items are Markdown files.** One file per item in a backlog directory, with a closed frontmatter schema and numbered requirements that become the audit checklist. `new`, `intake`, `draft`, `accept` and `split` write and gate them.
- **`run` and `review` hand an item to an agent.** `run` builds the prompt from the item, its dependencies and the verify command, then launches the configured runner on the item's branch. `review` launches a second session to check the branch against the requirements.
- **`drive` works a queue unattended.** It runs implementing and reviewing sessions per item in worktrees of their own, up to `--jobs` at once, sends findings back to the branch, opens a pull request for each item `[drive] merge_gate` passes and merges it. `[supervise]` sets, per action, whether drive acts on its own, a supervising session acts, or drover stops and notifies a person.
- **An append-only JSONL ledger records completion and audit.** `audit` records how closely someone checked an item (`claimed`, `reviewed`, `verified`), refuses `verified` from whoever wrote the code, and `unaudited` lists merged work nobody checked.
- **`status` renders anywhere.** A table, `--json`, or a template written to a file, with built-in HTML and Obsidian dashboards carrying trends over the ledger.
- **Follow-ups and GitHub.** Open follow-ups live in tracker issues or a local file, each linked to the item that owns it. Accepted items can be mirrored to GitHub issues, and ids can be reserved on a git ref so two hosts cannot assign the same one.
- **`examples/` is a runnable demo**, `herdsay`, with one merged item to read and three for an agent to build.
