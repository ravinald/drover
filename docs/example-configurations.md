# Example configurations

Two projects drover has driven for months, described by shape rather than by name. They sit at opposite ends of one decision, how much of CI the verify command repeats, and each one hit a gap drover has since closed or a key it now documents. Read the one closer to your project, then [Configuration](configuration.md) for every key either of them leaves at its default.

## Choosing between them

The question both answer differently is what an implementing session can run before it hands back. The session fixes what it can reproduce, so a test CI runs and the verify command does not reaches it only as a red check after review, through [`fix_gate_refusal`](configuration.md#supervise).

- **The verify command is CI** when the whole suite runs in minutes. A red check then means the runner's environment differed, never that a test went unrun. The Go service in the next section works this way.
- **The verify command is a subset** when the whole suite is too slow to run two or three times per item, which is how often drive runs it. CI stays the authority for the rest, and the send-back's failure lines are all a session gets. The web app with a sharded browser suite works this way.

## Go service on hosted runners

A Go service with a unit suite, a lint and format pass, and an end-to-end suite that needs real client hosts no session can reach. CI runs on hosted runners, one job per check, and triggers on pull requests.

```toml
[verify]
# One target running a leg per CI job. `make ci-drift`, one of the legs,
# fails when the job list here and the workflow's stop agreeing.
default = "make check"

[drive]
# Untracked files the verify command reads. A worktree holds only what git
# tracks, so without these the drift leg skips silently and the end-to-end
# suite, which fails closed without its hosts file, parks every item.
worktree_links = ["local.mk", ".drover.toml", "test/e2e/hosts.env", "AGENTS.md"]
merge_gate     = ["scripts/merge-gate.sh"]

[forge]
# CI fires on pull_request, so a pushed branch has no run until the pull
# request opens. The shorter wait still tells "never started" from "slow".
checks_wait = "2m"

[finish]
# The live suite, run against the operator's hosts once the item merges.
post_check = ["test/e2e/post-finish.sh"]

[supervise]
resolve_conflict  = "auto"
post_finish_check = "auto"
```

What each part answers:

- **`default = "make check"` replaced a chain of four commands.** The chain was a strict subset of what CI blocks on: neither the format check nor `go mod tidy` was in it, so an item reported green locally and was refused at the merge gate. One target with a drift check means a job added to CI cannot reach the trunk without reaching the verify command too.
- **`worktree_links` carries what git does not.** A local make override and a hosts file are both deliberately uncommitted. [`worktree_links_gap`](configuration.md#supervise) reports a file the verify command reads that the worktree lacks, on a Linux host with `strace`.
- **`checks_wait` is short because CI is pull-request-only.** The default wait expired once per item before the pull request existed. See [`[forge]`](configuration.md#forge).
- **`post_check` covers what no session can prove.** A requirement cannot be checked against a real client from a headless session, so the live suite runs after the merge, and at `post_finish_check = "auto"` a failure files a follow-up owned by the item that merged.

## Web app with a sharded browser suite

A web app and a Go backend. The browser suite is `node --test` files driving Playwright, about 30 minutes run one file at a time, so CI splits it across five jobs fed by one shared build, on self-hosted runners. A failed file is retried once inside its shard, and fail-fast cancels the sibling shards.

```toml
[verify]
# Build, unit and source tests, and only the browser files the branch
# changed. CI runs the whole browser suite, and the merge gate reads it.
default  = "bash scripts/item-gate.sh"
web_only = "bash scripts/item-gate.sh web_only"

[drive]
worktree_links  = [".claude", ".drover.toml", "AGENTS.md"]
# A fresh worktree has no node_modules, and the web build needs them.
worktree_setup  = ["npm", "ci", "--prefix", "web"]
# A shim around the project's own gate. See the notes after this block.
merge_gate      = ["bash", "scripts/drive-merge-gate.sh"]
protected_paths = ["deploy/*", "infra/*", ".github/*"]

[forge]
# TAP's `not ok` lines are built in. These add Playwright's failed action
# and the selector it waited for, which usually name the cause.
failure_patterns = [
  '^(locator|page|frame|browserContext)\.\w+: ',
  '^- waiting for ',
  '^AssertionError',
]

[numbered_files]
# The migration tool applies forward from the database's version, so a
# branch migration numbered at or below the trunk's newest never runs.
migrations = "backend/migrations/{n}_*.up.sql"

[git]
merge_method = "merge"

[supervise]
resolve_conflict            = "auto"
relaunch_after_runner_death = "auto"
cleanup_interrupted_drive   = "auto"
# fix_gate_refusal = "auto" is the default, and this project depends on it.
```

What each part answers:

- **The verify command is a subset on purpose.** Run two or three times per item on the same machine as the runners, the whole suite cost about 30 minutes each time. A browser test the branch broke without editing it therefore shows up first as a red shard, after review passed.
- **`fix_gate_refusal` closes that loop.** Before it existed, the red shard parked the item `gate_refused` with the request for changes written and nobody to act on it, twice in a row on one item, until the operator queued it again by hand. Now the item goes back to its implementing session in the same drive, once. See [Sending a refused branch back](lifecycle.md#sending-a-refused-branch-back).
- **`failure_patterns` makes the send-back carry the cause.** drover's built-in patterns read Go output only, so a JavaScript shard's log yielded no line at all and the send-back named only the check. TAP's `not ok` is built in now; the Playwright lines stay project configuration because they are one framework's. drover keeps each line once, since the in-shard retry prints every failure twice, and drops a suite's own `not ok` after the subtests it summarizes.
- **The implement template says how to reproduce.** A line in the project's own template tells a correction pass to read the failed job's log and run each failing browser file alone with `node --test` until it passes. drover's built-in template now carries a general version. See [Prompt templates](prompts.md).
- **The merge gate is a shim.** The project's own gate script wanted a pull request number, which does not exist yet when drive asks, and used exit `2` for "no green run yet", which drive reads as stop the whole drive. The shim takes the branch and the trunk and translates both to the [merge gate contract](configuration.md#drive).
- **`numbered_files` catches a merge that applies nothing.** Two branches each adding migration `0042` merge without a conflict, and the second migration never runs. Drive sends such a branch back naming the number to use.
- **`protected_paths` holds a merge for a person.** A change to deployment, infrastructure or CI config merges only once someone has read it, because each of those judges every later item.
