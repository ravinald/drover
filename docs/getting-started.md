# Getting started

One sequence, from an install to an agent running a work item, in a repository that is not drover. If step 4 does not exit 0, step 3 was wrong and you have one line to act on. Every term the steps use, from item to merge gate, has a one-line entry in the [Glossary](glossary.md).

**Every command on this page is one you type.** Steps 2, 3, 5 and 7 answer questions about your project and your machine that nothing can infer, and step 7 is the boundary drover deliberately does not automate. An agent's half of the same lifecycle is [Agent guide](agent-guide.md); the split per command is the **Who** column in the [README](../README.md#commands).

[Lifecycle](lifecycle.md) is the same sequence with the reasoning behind each refusal. [Playbook](playbook.md) is the judgement this page does not make for you: how big one item should be, when to run headless, and who reviews. [Concepts](concepts.md) is why each store is where it is. Read those second.

## 1. Install, and prove it

```bash
git clone https://github.com/ravinald/drover.git
cd drover && make install
drover version
```

`make install` puts the binary in `$(go env GOPATH)/bin`, which has to be on your `PATH`. If `drover version` prints nothing, that is why. A release binary needs no Go toolchain; the [README](../README.md#install) carries that path and the `go install` one.

## 2. Once per machine

```bash
drover init --user
```

Seven questions, each showing its default first: whether every project keeps its backlog, ledger and reports inside itself under `.drover/` or centrally under `~/.local/share/drover/<project>/`; a verify command, if and only if every project you drive runs the same one; which runner to launch; the id prefix for a bug; the frontmatter keys every item must declare, and those it may; and the bar you hold a review to. `drover init --user --defaults` takes all of them and asks nothing, which is the form for a script and the only form that works without a terminal.

It writes `$XDG_CONFIG_HOME/drover/config.toml`, or `~/.config/drover/config.toml`, and refuses to overwrite one. Leave the verify command empty here unless you mean it: a wrong one inherited by every project is worse than none reported per project, and step 3 asks for the right one.

## 3. In your repository

```bash
cd ~/src/yourthing
drover init
```

`init` infers what it can (the project name from the directory, the tracker from `git remote get-url origin`, a root marker from your build file) and asks one question: the command that must pass before an item merges. Answer it. `drover init --verify "go test ./..."` skips the question for a script.

It writes `.drover.toml`, creates the backlog and reports directories, and appends one line to `projects.jsonl` beside the per-user config, so `drover projects` can tell you later what this machine drives.

## 4. Check, and expect zero

```bash
drover check
```

This is the contract of the whole page. Exit 0 means the config, the backlog directory, the ledger and the paths between them agree. Exit 2 means drover ran and your project failed its own validation, and each finding names what to fix. The one you are most likely to see is `NO VERIFY`, which means step 3's question went unanswered.

## 5. Choose where follow-ups go, once

drover keeps three stores, and this is the only one with a choice in it. The **backlog** is what to do, the **ledger** is what got done, the **follow-up list** is what is unresolved and nobody has priced.

**Your tracker holds the follow-up list.** This is the default. `drover intake <n>` turns an issue into an item, and `drover follow-ups` reads the rows. drover creates the `follow-up` and `severity:` labels the first time it applies one, but a person filing by hand needs them first, and an id namespace label is yours to create:

```bash
gh label create follow-up --description "In this project's follow-up list"
gh label create severity:critical --description "Critical"
gh label create severity:high --description "High"
gh label create severity:medium --description "Medium"
gh label create severity:low --description "Low"
gh label create feature --description "A drover id namespace"
```

A follow-up's owner is a link, not a label: file it as a sub-issue of the owning item's issue (`gh issue create --label follow-up --parent <n>`), or put an `Owner: <id>` line in its body when the owner has no issue.

`bug` and `enhancement` ship with a new GitHub repository; `feature` does not, and `intake` refuses an issue whose labels match none of your id namespaces.

**A file holds the follow-up list.** Delete the `repo` line `init` wrote, set `[register] backend = "file"`, and `drover follow-up open|close` writes rows to a JSONL beside your ledger. `intake` refuses, because there is no tracker to read.

**Nothing holds the follow-up list.** Set `[register] backend = "none"`. `drover new` is your only path to an item.

## 6. Your first item

```bash
drover new bug "the resolver drops a trailing dot"
```

That writes a scaffold and nothing else, and prints its id: `B1`, the first in the bug namespace (see [Item ids](concepts.md#item-ids)). Open the file it names and write the requirements:

```markdown
---
title: the resolver drops a trailing dot
deps:
kind: bug
severity: M
---

Act as a Senior Engineer. `Resolve` strips a trailing dot from a fully qualified name before comparing, so `example.com.` and `example.com` hash differently and the cache misses every time.

Requirements:

1. `resolver.go:88` normalizes the trailing dot before the cache key is built.
2. A test drives both forms through `Resolve` and asserts one cache entry.

Out of scope: the cache eviction policy.
```

**The numbered list is the contract.** `drover report --save` turns every top-level line that opens with a number, a period and a space into an audit checkbox, so a requirement written as prose is on no checklist and nobody will ever confirm it. `drover check` reports `NO REQUIREMENTS` on an accepted item that has none, until it merges.

Cite files and lines. An item saying "improve the resolver" produces a run that improves something; one citing `resolver.go:88` produces a run you can check.

Keep it to one session's worth of work. A body carrying eight requirements across four subsystems produces a run that completes three of them and reports success, because a self-report is what the session believes rather than what the tree holds. [Playbook](playbook.md#1-size-the-item) is the test for too big and how to split into a `deps:` chain.

## 7. The one step no agent performs

```bash
drover accept B1
```

The scaffold carries a stop marker, and `run` refuses the item, `--dry-run` included, until a human deletes it. `accept` is that act. It is a command you type rather than a flag, so that nothing a runner's argv can carry reaches it, and it refuses outright inside a session drover launched. Nine more do: `drover --help` lists them under "Whose step it is", with the reason for each.

`drover draft B1 --runner claude` hands an agent the prompt for drafting the requirements. It leaves the marker in place. You read the list it wrote, then accept it.

## 8. Run, review, audit, finish

```bash
drover run B1 --dry-run     # exactly what the runner will be handed
drover run B1               # switches to the branch, launches the session
drover run B1 --headless    # unattended; the event stream goes to a log
```

Only a headless run leaves a log, and the log is what `drover report` reads, what moves the item to `[~]` implemented in `status`, and what the reviewer gets as the run's own account. An interactive run does the work and leaves none of that. [Playbook](playbook.md#2-interactive-or-headless) is the whole trade.

Read the dry run before spending a real one. It prints the prompt, the resolved argv, the selected verify command, and the open follow-ups this item inherits. It refuses an undeclared runner, so a dry run that exits 0 means the launch can start.

Once the run finishes, and **before you merge**:

```bash
drover review B1            # the reviewer's prompt: contract, deps, follow-ups, reports, verify command
```

`review` never switches branches. It reads the tree `run` left standing, so a requirement it finds unmet is this item unfinished rather than a new defect: `drover request-changes B1 -f findings.md` writes the report into the body, `drover run B1` returns to the same branch, and the review runs again. No pull request exists while that loop is running: `drover open-pr` opens one, and it refuses a review that sent work back. After the merge the loop is gone and the same gap has nowhere to go but the tracker.

`drover review B1 --runner <x>` hands that prompt to a reviewing session instead of printing it. Point it at a runner other than the one that implemented, and an independent audit becomes an assertion the ledger supports rather than one drover warns about. [Playbook](playbook.md#3-who-reviews) is what you gain, what it costs, and what a second agent still cannot catch.

Close the review pass with the audit, then open the pull request, merge it, and record the merge:

```bash
drover audit B1 verified --code-by agent "R1-R2 traced to shipped code"
drover open-pr B1           # merge gate, recorded review pass, then one pull request
drover finish B1            # ledger row, saved report, back to the trunk
```

`open-pr` is the only thing that opens one. It runs `[drive] merge_gate` and reads back what the last review concluded, so an open pull request means both have passed rather than that a session finished writing.

`--code-by` names who wrote the code: `agent` for a session drover launched, a person's name, or a comma list of them. drover refuses `verified` when the person or session recording it matches an entry: whoever wrote the code cannot certify every requirement against it.

## 9. Tell your agent

```bash
drover agents --write DROVER.md
drover agents --pointer DROVER.md >> AGENTS.md
```

The first generates this project's instructions from its own config: its paths, its verify command table, its label vocabulary. The second appends the short stanza that sends an agent from `AGENTS.md`, which most agent CLIs load on their own, to the file holding them.

Write the block straight into `AGENTS.md` instead if you prefer; it runs to several hundred lines, which is the whole reason for the split. `drover check` reports `NO POINTER` when the block sits outside `AGENTS.md` and nothing there names it, and `STALE BLOCK` when either file falls behind the config.

## 10. What you did not have to configure

Specs, vendored skill guides, a shelf for items set aside unmerged, per-stage runners, custom implement and review templates, severity colors. All optional, all documented in [Configuration](configuration.md), and none of them needed to get here.

`drover --help` groups every command in the order an item moves through them, and `drover <command> --help` carries a worked example and a `Safety:` line naming what that command writes, resolved against this project's own paths.
