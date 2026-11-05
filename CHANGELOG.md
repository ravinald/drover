# Changelog

## v0.2.0

A minor release, so every project's config is stale until you review it against this one. The upgrade takes three commands and two checks per project, listed under **Upgrading**.

### Upgrading

1. **Review and stamp the config.** Every project command refuses on a `.drover.toml` stamped with an older minor release, because a key added since reads as its zero value and nothing says it was never decided. Run `drover config --diff`, read what it prints, then run `drover config --stamp`.
2. **Regenerate the agent block.** The block `drover agents` writes now carries the binary that rendered it, and a project command refuses on a block that differs from what the binary renders. Run `drover agents --write AGENTS.md`, or whichever file holds the block, once.
3. **Move whole-file prompt overrides onto blocks.** `[prompts]` overrides now layer on the built-in templates by named block. A file with text outside any `{{define}}` still works, but it replaces the whole built-in: `drover check` reports it as `WHOLE TEMPLATE` and every launch from it warns. `drover schema --prompts` lists each slot's blocks, and [Prompt templates](https://github.com/ravinald/drover/blob/main/docs/prompts.md) shows the conversion.
4. **Check the review cap.** `[review] max_review_rounds` now defaults to 4 where it used to mean no cap, so an item a review sends back a fifth time parks `needs_decision` for you. Set `max_review_rounds = 0` to keep the old behavior, or a higher number where items have gone round more often and merged correctly.
5. **Check item size.** `accept` now refuses an item with more than `[backlog] max_requirements` numbered requirements, 8 unless you set it, and `--yes` accepts one anyway. Items already accepted are not re-checked. Set `max_requirements = 0` to turn the limit off.

### Changes

- **Prompt overrides replace named blocks, not whole files.** A project overrides `intro`, `scope`, `context`, `done`, `verify` or `checks` and keeps the rest of the built-in, including every later change to it. A replaced block opens with a `drover-base` marker naming the default it was written against, and `drover check` reports `STALE OVERRIDE` when that default has changed since. `drover check --diff` shows what changed, word by word.
- **drive merges synchronously and finishes the item in the same drive.** It merges with `--match-head-commit` on the head the merge gate passed, never through the forge's `--auto`, then confirms the merge with the forge and records it as `finish` does, so dependents run behind it in the same drive.
- **drive waits for checks that start with the pull request.** On a repository whose CI triggers on `pull_request` only, the head has no checks before the pull request opens. drive now settles them again after opening it and asks the merge gate before merging, where it used to merge at once and park `merge_failed` on branch protection.
- **Live checks run before the merge.** A `[live]` table names commands that test a branch against real systems. Exit 0 merges, exit 4 parks `live_unavailable`, and any other exit sends the work back. `drover live <id>` runs one by hand.
- **Review findings on an unmerged item go back to its branch.** A defect the branch's own diff introduced is never filed as a new item or issue. A requirement no session can meet as written goes to the operator as `needs_decision`, and `amend`, `touch` and `signoff` change or meet it.
- **A requirement sent back twice running needs an invariant.** `request-changes` refuses a review's second consecutive send-back on one requirement until `drover invariant` has recorded the property behind it, so the next round reads what may not break rather than one more broken case.
- **Overlapping branches are refused at accept.** An item citing lines another unmerged branch changes must depend on it, or `accept` refuses. `drover fold` merges a small item into the one it overlaps.
- **Token usage is recorded per session**, headless and interactive, and drive pauses and notifies before the weekly allotment or purchased credits run out.
- **Notifications** go to ntfy or any command, routed per destination and per event, and a `needs_operator` event can open a T3 Code thread ready to answer it.
- **`status` names its columns**, keys its markers, and shows each item's branch, origin and last edit.

Full list: https://github.com/ravinald/drover/compare/v0.1.1...v0.2.0

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
