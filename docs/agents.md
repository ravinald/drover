# Agent instructions

How a project's own agent instructions get written. For what an agent may then do with them, see [Agent guide](agent-guide.md).

Generate them. Do not copy them.

```bash
drover agents                       # to stdout
drover agents --write DROVER.md     # replace the marked block in place
drover agents --pointer DROVER.md   # the stanza AGENTS.md needs so an agent finds it
```

Everything a project's agent instructions need to say is already in `.drover.toml`: the backlog path, the verify command table and its precedence, the tracker label vocabulary, the frontmatter schema, the stop marker. `drover agents` renders those alongside the parts that are true of drover itself: the command safety classes, the exit codes, the authorship rule, the one-hop owner rule.

The safety table comes from the same table each command's `--help` prints its own `Safety:` line from, so the generated instructions and the binary cannot disagree about what writes. It renders in two parts: the classes, and then what each command changes with this project's paths already resolved. A resolved path carrying a pipe or a newline is escaped for its table cell, and `--help` prints the same sentence unescaped. A command added without an entry there fails the test suite rather than rendering blank, and so does one listed by class whose effect reaches no reader.

## Why this is a command

Instructions copied from boilerplate with `<angle brackets>` for a human to fill in drift the moment the binary grows a command, and nothing reports the conflict. A copy written before `finish` existed keeps telling operators to record completion by hand, and no reader has a reason to doubt it.

Generated instructions cannot describe a command surface the installed binary does not have.

## The one thing it cannot infer

The bar a reviewer holds the code to. Put it in config:

```toml
[agents]
principles = "migrations append-only, permissions per object via the host's own system, no undocumented internal imports"
```

It lives there rather than as a hand-edit in the generated file so regenerating cannot lose it. Left unset, the output carries a placeholder that says so, which is the honest rendering: a review step that quietly loses its standard reads as a review with no standard.

## The project addendum

Anything longer than the principles goes in a file of the project's own, named in config:

```toml
[agents]
addendum = ".drover/agents-addendum.md"
```

The principles line is the bar a reviewer holds the code to, in one sentence. The addendum is everything else a project wants every agent to know: the team's conventions, where its notes live, a dashboard to keep current. Without it, that prose gets hand-edited into the host file, outside the markers where nothing tracks it, or inside them where the next `--write` deletes it.

drover appends the file at the end of the block, inside the markers, after everything it generates, under a heading it supplies:

```markdown
## Project addendum

<the file, as written>
```

The text is not processed: a `{{ .Project }}` in the file renders as `{{ .Project }}`. `--bare` carries it too. The path may sit outside the repository (`~/notes/project.md`, an absolute path), and whoever owns that file owns what it says.

Editing the addendum makes every generated copy stale, the same as a config change. Every project command refuses until you run `drover agents --write <file>`, and the refusal names the addendum as what moved. A configured addendum that cannot be read stops `drover agents` with exit 1 naming the resolved path; `check` reports it as `BAD ADDENDUM`, and the staleness refusal skips the comparison rather than refuse every command over a missing notes file. A marker line or an unclosed code fence in the file would end the block early, so `drover agents` refuses that as well, and `check` reports it as `BAD ADDENDUM` the same way.

## Where the block goes

`DROVER.md` rather than `AGENTS.md`, and `init` suggests it. The block runs to several hundred lines; in a project whose `AGENTS.md` also carries its own rules, that leaves drover as most of the first thing an agent reads, and most of it is irrelevant until the agent is driving an item.

`--write` takes any path, so a project that wants the block directly in `AGENTS.md` writes it there and nothing objects. That arrangement needs no pointer and `check` asks for none.

Splitting costs something, and the cost falls on the half you least want behind a link. `AGENTS.md` is a plain-markdown convention with no import syntax, so the pointer is a link an agent may or may not follow, and what it would miss includes which commands refuse inside a launched session. `--pointer` answers that by putting the refusal list in the stanza itself:

```bash
drover agents --pointer DROVER.md >> AGENTS.md
```

```markdown
## Tooling: drover

This project's work items, completion ledger and audit trail are driven by
`drover`. **Read [DROVER.md](DROVER.md) before running any drover command.**
`accept`, `amend`, `drive`, `finish`, `intake`, `mark-merged`, `open-pr`,
`signoff`, `supervise`, `touch`, `unaccept` and `unmark-merged` refuse inside a session drover
launched and exit 3, because each certifies work somebody else has to trust or
reaches a tracker other people read. `run`, `draft --runner` and `review
--runner` refuse there too: a launched session cannot launch another.

`drover agents` prints the same text if DROVER.md is missing.
```

The command names come off the same actor table each command's `Actor:` line reads, so a command that gains or loses that boundary changes the stanza. Regenerate it the way you regenerate the block. A hand-written pointer naming five refusing commands after a sixth gained an `Actor:` line is the drift this whole command exists to stop.

## What `check` reports

Five findings. `BAD ADDENDUM` fires whenever a configured addendum cannot be read; the other four stay silent for a project that generated no block at all, since `init` suggests writing one rather than requiring it.

| Finding        | Means                                                                                                                                                                                                                                                                                                     |
| -------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `STALE BLOCK`  | The block differs from what this binary, this config and the addendum would produce. The comparison is on the rendered text, so a config change or an addendum edit goes stale exactly the way a binary upgrade does.                                                                                     |
| `STALE POLICY` | The block's policy line records a `[supervise]` policy the config does not hold. It names each action and whether it widened or narrowed, and stands in for `STALE BLOCK` on that file, since the fix is the same command plus `--yes` when an action widened. See [The merge policy](#the-merge-policy). |
| `BAD ADDENDUM` | The file `[agents] addendum` names is missing, unreadable or a directory, or holds a marker line or an unclosed code fence that would end the block early. No block is compared until it renders.                                                                                                         |
| `NO POINTER`   | The block lives outside `AGENTS.md`, and `AGENTS.md` never names the file holding it. A project with no `AGENTS.md` at all is not this: it told its agents some other way.                                                                                                                                |
| `BAD MARKERS`  | The markers are unclosed or inverted, so `--write` refuses to regenerate in place rather than guess which half to keep.                                                                                                                                                                                   |

`check` finds the block by its markers rather than by filename, since `--write` takes any path and a check keyed on `AGENTS.md` would report nothing about the projects that took the suggestion. It scans markdown at the project root only, and a marker counts only as a whole line outside any fence: the same rule the stop marker is matched by, and for the same reason. Prose quotes a marker inline or in a code block, and a changelog that does so is not a block host.

## Markers

`--write` replaces the text between `<!-- BEGIN drover -->` and `<!-- END drover -->`. If the markers are absent it appends the block, creating the file if needed. Nothing outside them is touched, so a project keeps its own preamble, its own house rules, and whatever else belongs in an `AGENTS.md` that is not about drover.

A marker counts only as a whole line outside any fence, the same rule the intake stop marker is matched by. A file that quotes the markers in prose, as this page does two paragraphs up, holds no block: `--write` appends one rather than splicing into the sentence. `check` reads them the same way through the same function, so a file it calls a block host is the file `--write` regenerates.

An opening marker with no closing one after it is refused rather than repaired, because there is no way to tell which half to keep.

`--bare` omits the markers and the generated-by note, for piping into something that will wrap it itself.

## The merge policy

Merging a pull request is the operator's by default, and the generated review loop ends on "Stop at the merge point". An operator running drover for their own projects can hand one kind of merge to a supervising session:

```bash
drover agents --write DROVER.md --permissive-merge --yes
```

That sets [`[supervise]`](configuration.md#supervise) `merge_drive_pr` and `merge_protected_paths` to `supervisor` and renders the block under that policy. A `drover supervise` session may then merge a pull request drive opened for an item, with `[git] merge_method`, and run `drover finish <id>`, when the ledger names the pull request on a row drive wrote, the item's latest review pass is `passed` and came from a session drover launched, every check on the head is green, and the forge reports it mergeable. `finish` checks the two ledger conditions itself and refuses with exit `3` otherwise; the forge conditions are the supervisor's to read. A pull request touching a `[drive] protected_paths` file is merged only after the supervisor reads the diff of each denied file and names each in its report. Everything else still stops at the merge point, and `finish --force` stays refused to a supervisor at any policy.

`--restrictive-merge` sets both keys back to `operator`, written live so a per-user `supervisor` cannot reach the project.

drover does not rewrite `.drover.toml`, so either flag appends a `[supervise]` table when the file has none. When it has one, the flag prints the two lines to set in it and exits `3` without writing the block, so the config and the block cannot disagree. With neither flag, `--write` renders whatever the config says, so a later regeneration keeps the choice.

### The policy line

Each block carries one line recording the policy of every `[supervise]` action it was rendered under, sorted by key:

```text
<!-- drover policy: accept=operator auth_expired=operator ... write_scaffold_requirements=supervisor -->
```

A block with no such line reads as the shipped defaults. `--write` compares the line already in the file with the policy it is about to render, action by action, ignoring every other difference in the text, and prints each action that moves:

```text
the block in DROVER.md changes policy:
  merge_protected_paths widens: operator > supervisor
  merge_drive_pr widens: operator > supervisor
```

A move toward `auto` widens what the sessions reading the file are told they may do, so it is refused with exit `3`, writing neither the block nor `.drover.toml`, unless `--yes` is passed or you confirm at a terminal. A change that only narrows is printed and written. A file with no block yet has told no session anything, so its first write asks nothing.

## Keeping it current

```yaml
- run: |
    drover agents --write DROVER.md
    git diff --exit-code DROVER.md
```

Fail the build on a diff. The generated block is then an assertion CI re-checks rather than a document someone remembers to update. `drover check` reports the same drift as `STALE BLOCK` without needing a clean tree, so a project already running `check` in CI gets it for free.

## Documentation links

The generated block ends with links to the pages an agent needs once the block runs out: the agent guide, the `--json` formats, the template data, the rtk filters. None of those is on disk in a project that installed only the binary.

They are pinned to the ref the binary was built from. A tagged build links its own tag, so an agent reads the surface its binary actually has; a build from a work tree or the module proxy links `main`, which is the closest true answer for a binary that is not standing on a release. `drover version` says which one you have.

## What still belongs in a template

Per-item doctrine, not per-reader doctrine. `[prompts] implement` reaches the item, its dependencies, the selected verify command and the open follow-ups, so a rule stated there applies to the run that needs it. `[prompts] review` does the same for a review. A rule that only lives in `AGENTS.md` applies to whoever remembered to read `AGENTS.md`.

See [Prompt templates](prompts.md) for the data a template reaches.
