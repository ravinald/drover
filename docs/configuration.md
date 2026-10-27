# Configuration

Configuration comes in two halves: `drover init --user` writes the per-user file once per machine, and `drover init` writes each project's `.drover.toml` with the inferable keys filled and the rest commented. `init` asks one question (the command that must pass before an item merges) because an empty verify command is the only reason a generated project fails its own `drover check`. Answer it, or pass `--verify` for a script, and `drover init && drover check` exits 0. Decline, and `check` reports `NO VERIFY` and nothing else. This page is the reference for editing either file; [Layering](#layering) is how the two combine.

drover reads `.drover.toml`, found by walking **up** from the working directory. The directory holding it is the project root, and every relative path resolves against that root. Discovering the root rather than deriving it from the binary's location is what lets drover run from anywhere inside the tree, including through a symlink.

An unrecognized key is an error. A typo that took effect as silence would be worse than a refusal.

`drover schema` prints this page's contract as JSON Schema, for an editor completing keys or a CI job validating a config before drover runs. It is embedded in the binary and held against the Go types by a test, so the two cannot disagree.

[`examples/.drover.toml`](../examples/.drover.toml) is a small working example: the config of the `herdsay` demo, with no spec tree and no custom templates, leaning on the defaults for everything it does not need to state and commenting each choice it makes.

## Layering

A per-user config at `$XDG_CONFIG_HOME/drover/config.toml`, or `~/.config/drover/config.toml` when `XDG_CONFIG_HOME` is unset, is read **before** the project's file, and the project's file is decoded over the top of it. It is optional, and a machine without one loads the project file alone.

Each file is refused for its own `schema_version` before its keys are read, so a per-user config written for another format is told its version is the problem rather than that one of its keys is unrecognized. A per-user config declaring no `schema_version` is exempt.

`drover init --user` writes it, walking through the settings that are the same in every project you drive and showing each default before it takes one. It refuses to overwrite a file that is already there, and refuses to run at all without a terminal to ask on; `drover init --user --defaults` is the form for a script, and takes every default without asking. Put a setting there when it would otherwise be retyped in every project; leave it out when it belongs to one.

Every key on this page is valid in either file except `project`, which drover refuses in the global config: one name for every project on the machine collides silently, since two projects' run logs would share one directory.

Three merge behaviors, and they differ on purpose:

| Type   | Example                                   | Behavior                                                                 |
| ------ | ----------------------------------------- | ------------------------------------------------------------------------ |
| Scalar | `intake.stop_marker`                      | The project wins. The global value survives where the project is silent. |
| Table  | `[verify]`, `[runners]`, `ids.namespaces` | Merged per key. A project overriding one verify command keeps the rest.  |
| Array  | `frontmatter.required`                    | Replaced whole. An author writing a schema means that list and no other. |

A location in the global file must start with `/` or `~/`. A relative one has no root of its own to resolve against, so it would resolve against whichever project happened to read it, and every project on the machine would keep its items at the same spot inside itself. drover rejects it by name at load. `root_marker` is exempt, because naming a file that must exist in whatever tree reads it is the whole job of that key.

One setting serving every project needs a way to name the project it is resolving for, so every `[paths]` value is a Go template over `{{ .Project }}`:

```toml
# ~/.config/drover/config.toml
[paths]
backlog  = "~/.local/share/drover/{{ .Project }}/items"
ledger   = "~/.local/share/drover/{{ .Project }}/ledger.jsonl"
reports  = "~/.local/share/drover/{{ .Project }}/reports"

[verify]
default = "make check"
```

`drover init` reads the global config and leaves out every key it already sets, so the generated project file carries what differs and nothing else. A key repeated in a project file shadows the global value rather than inheriting it, and the next edit to the global file then silently misses that project.

`drover check` prints the files in effect, global first, whenever more than the project's own config had a say, and `check --json` carries them in `config`. Naming the project's own file on every run is noise; naming the one a reader would not think to open is the point.

## Upgrades

A `.drover.toml` is written once, against whatever the binary understood that day. Nothing afterwards tells the project that the binary has moved. A key added since gets its zero value, a table added since is absent, and both read the same as a project that declared them and chose the default.

`drover config --diff` is the answer. It prints what this build would write differently, one paste-ready line each. This project declared no `[intake]`:

```console
$ drover config --diff
# Top-level keys from the sections below: each goes above the file's first [table].

runners = { claude = { interactive = ["claude"], headless = ["claude", "-p", "--output-format", "stream-json", "--verbose"], stdin = false } }

# 1 key `drover init` writes today that this config does not declare.
# This is what `drover check` reports as STALE CONFIG.
#
# Each line goes under the table named above it, and a top-level key goes
# above the file's first table. TOML refuses a second [table] header, so a
# whole stanza appends only where the file has no such table yet.

[intake]
stop_marker = "STOP: requirements not yet written"

# 33 further keys this build reads, at the value drover resolves each to.
# Nothing reports these: a default left alone is not a gap. They are here
# because a key you do not know about is one you cannot go looking for.
# Its top-level key (runners) prints at the head of this report.
#
# Each line goes under the table named above it, and a top-level key goes
# above the file's first table. TOML refuses a second [table] header, so a
# whole stanza appends only where the file has no such table yet.

[paths]
specs = ""
skills = ""
...
```

Two sections, two standards, and the difference is the whole point of splitting them. The first is what `drover init` writes today, which is what a config written today would carry, and it is the only set `drover check` counts. The second is every other key this build reads, printed at the value drover resolves it to. Nothing reports those, because a default left alone is not a gap. They are there so a key you do not know about is one you can go looking for.

A third section appears where either file declares a key nothing here reads, each named with the file it sits in, because whoever wrote that file chose the value and that file is where it comes out. A `schema_version` line opens the report where the project's file declares none or one this build does not read. Both are commented or stand alone; neither is part of the paste.

A `[supervise]` section lists every supervised action neither file sets, one pasteable line each at its default policy (`resolve_conflict = "supervisor"`). `[supervise]` is an open table, so the key sections above would see it as one table and never name an action in it. The header names the actions that default to `supervisor`, says each of those parks as `needs_supervisor` unless a `drover supervise` session is running, and points at `drover init supervise`, which writes the table interactively. A project that sets every action, in either file, gets no such section. `drover check` reports the same gap as `SUPERVISOR UNSET` once the ledger records a drive and neither file has a `[supervise]` table.

The headers name where each line goes rather than being part of the paste. A stanza for a table the file does not have yet appends whole; one for a table it already declares does not, because TOML refuses the second header.

Every top-level key, whichever section counts it, prints in one block at the head of the report, before any `[table]` header, and the section that counts it names it there. A bare key belongs to the last header above it and a comment line does not end a table, so `runners = {…}` printed below a section's last stanza would paste back as a key of that table: the file loads, nothing errors, and the next `--diff` reports `runners` missing again. The top-level block goes above the file's first table.

A pasted line never changes what the config resolves to, so a key printed at its zero value would have to mean what leaving it out means. The usage levels do not: `pause_remaining = 0` is a hold at 100% used, and `max_usd = 0` refuses to load. The six usage level keys in `[usage.weekly]`, `[usage.five_hour]` and `[usage.overage]` print as `"off"`, which says no such step the way leaving them out does. A key with no value that means unset, such as `[usage] weekly_budget`, prints commented and names what unset means (`# weekly_budget =   # unset: no budget`), so pasting the stanza leaves it out.

A key this build reads that the per-user config supplies reaches neither of the first two sections. Repeating it in a project file would shadow the global value rather than inherit it, and the next edit to that file would then silently miss this project. A retired key in the per-user config is still reported, in the third section, against the per-user file.

Without `--diff` the whole file is printed, from the same generator `drover init` uses and carrying this project's own values. That answers a different question: what would today's config look like with my decisions in it. The live lines in it are what the project's own file declares, plus the current `schema_version`:

- A key the project declared prints at the project's value.
- A key the per-user config supplies and the project does not is left out, so the print stays a file that inherits rather than one that pins today's global value.
- A key neither file declares prints commented, at the value `drover init` would write. Uncommenting it is a decision; printing it live would make that decision for you, and a copy carrying it would resolve differently from its source the moment the per-user config or drover's own default moved.
- A table the project opened and left empty prints as a bare header, with nothing under it. The header is itself a setting: `[severity]` with no keys is what makes `[severity] key` a thing drover checks, so a copy assembled from values alone reported the project clean where its source reported `UNDECLARED KEY`. The leaves stay out for the reason above, since filling them would pin today's default where the original inherits.

So a copy of the print, loaded beside the same per-user config, resolves to the settings its source does, and keeps doing so when that per-user config changes.

Open tables (`[verify]`, `[runners]`, `ids.namespaces`, `severity.ranks`, `frontmatter.shape`) follow the same rule as a whole. The project's own declaration comes back as written: absent stays absent, an empty table stays empty, and a partial one names only the entries the project named, never filled out with the ones `init` suggests. An empty table is a decision in its own right: `[frontmatter.shape]` with nothing in it leaves every declared value unchecked on purpose, and an empty `[verify]` under a per-user verify command inherits that command where a project-local `default = ""` would shadow it.

Below the file, commented, is a notice per key this build does not read, with the file that declares it: those are not carried into the print, and a report that dropped them without a word would read as a complete account of a file it had quietly edited.

`drover init` differs in two places. It writes every suggestion live, because there is no file yet and what it prints is the file. And it reads three values off the tree rather than off a file: the build file that becomes `root_marker`, whether the directory is a work tree, and the origin remote. `init` writes those over a per-user value on purpose, because it is producing a project's first config and a global `root_marker` naming `go.mod` would refuse to load every project that is not a Go module. `drover config` leaves all three alone where the per-user config supplies them, and prints them commented where nothing does.

`drover check` reports **only the first section**, as `STALE CONFIG`, and exits 2. It says how many and never which:

```console
$ drover check
STALE CONFIG        .drover.toml does not declare 1 key `drover init` would write today
                    run `drover config --diff`, which names each one at the value drover resolves it to
drover: 1 finding
```

The comparison is on keys and never on text, because a config carries hand-written comments recording why each value is what it is and a text comparison would report every project that wrote one.

A `schema_version` this build does not understand is the one other thing `check` reports, and it reaches the report through the same inspection path `drover config` uses, because loading the config the ordinary way refuses it first. That report is the finding alone: nothing else ran, so the body and ledger counts beside it are unread rather than zero, and `check` says so on a `stopped` line.

```console
$ drover check
STALE CONFIG        .drover.toml declares schema_version 1 and this build understands 2
                    run `drover config --diff`, which reads the file without validating it and names every key the gap left behind
stopped: .drover.toml is not at this build's schema_version, so no body, ledger row or path was read: bodies and ledger_rows are unread rather than zero
drover: 1 finding
```

A file that declares no `schema_version` at all is the same finding, worded so the reader is not sent looking for a line that is not there:

```console
$ drover check
STALE CONFIG        .drover.toml declares no schema_version and this build understands 2
```

An explicit `schema_version = 0` is reported as the 0 it wrote. Both read as version 0 internally and the finding says which, because the fix differs: one file gains a line and the other has one corrected.

The project's own file decides. A project declaring no `schema_version` is reported even where the per-user config declares the current one: that lets the project load, and says nothing about which format the project's file was written against. The file named is the project's, except where the project declares none and inherits a version this build does not read from the per-user config. That mismatch is the per-user file's, so the finding names it there. Either way the report stops, and `drover check` exits 2.

A key this build does not read is not reported by `check` at all: it is an error at load. `drover config --diff` lists every such key.

Two things the command will not do:

- **It does not write, and there is no `--write`.** Those comments are the project's reasoning and no TOML round-trip preserves them, so a regenerating writer would flatten the more valuable half of the file. The generated block in `AGENTS.md` has no such problem because markers fence it off and nobody writes prose inside them; a config has no markers to fence. This prints, and a human pastes.
- **It does not judge whether a default is right for the project.** Most projects are right to leave most keys alone, and a default drover picked is the one it will keep picking. The report says what you have not decided, never what you decided wrongly.

It validates nothing, which is what lets it answer at all: an unknown `schema_version` and every key this build does not read are both errors at load, so a command reading the config the ordinary way would refuse the very files worth asking about. `drover check` does the validating.

### The release stamp

`schema_version` catches a breaking change. Nothing else in a config says which release it was last read against, and a key added since reads as its zero value, which looks the same as a decision. `drover_version` records that review, and every project command refuses (exit 3) while it is missing or names an older minor release than the binary:

```console
$ drover status
drover: this project's files differ from what this binary renders (drover v0.2.0):
  .drover.toml: it was last reviewed against v0.1.0, and this binary is v0.2.0
    fix: drover config --diff, decide what to declare, then drover config --stamp
Nothing was run. Bring them level, or pass -I (--ignore-stale) to run drover status anyway
```

`drover config --stamp` writes `drover_version` as this binary's release and changes no other line. Run `--diff` first: the stamp says you read what it printed. A patch release never makes a config stale, a build carrying no release version (a plain `go build`) never judges the stamp, and `drover init` writes the stamp into a new config.

The same refusal covers the generated agent instructions. Any root-level markdown file carrying the `drover agents` block is compared, as text, against what this binary renders from this config, and a difference is named with the `drover agents --write <file>` that brings it level. So the block goes stale when the binary moves and when the config does.

`agents`, `config`, `init`, `init --user`, `schema`, `version`, `help`, `projects` and shell completion never refuse, because they are how a project gets level or they answer without one. Neither does anything inside a session drover launched: the operator decided when they started it, and an agent's `drover audit` refused halfway through a review would lose the review. `-I` (`--ignore-stale`) runs any other command anyway, once.

## Top level

| Key              | Required | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| ---------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `schema_version` | yes      | Config format. This build understands `2` and refuses anything else, rather than misread a format it does not know. Each file answers for its own line: a per-user config declaring another version is refused whatever the project declares, since a current project would otherwise mask it and the reader would be told a key does not exist rather than that the format differs. A per-user config declaring none is not stale. The refusal names the file that declared the version, which is the per-user file when the project declares none and inherits one. |
| `drover_version` | no       | The release this config was last reviewed against, as `vX.Y.Z`. Written by `drover init` and `drover config --stamp`. Missing, or older by a minor release than the binary, every project command refuses until it is stamped; see [The release stamp](#the-release-stamp).                                                                                                                                                                                                                                                                                           |
| `project`        | yes      | Names the log directory and the status banner.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |

## `[paths]`

Project-relative unless absolute. A leading `~` expands to the home directory, and every value is a Go template over `{{ .Project }}` (see [Layering](#layering), which is what the template is for).

| Key           | Default                            | Meaning                                                                                                    |
| ------------- | ---------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `backlog`     | `.drover/backlog`                  | The item files. The directory listing is the backlog, so adding one needs no index edit.                   |
| `ledger`      | `.drover/ledger.jsonl`             | Completion and audit records.                                                                              |
| `reports`     | `.drover/reports`                  | Saved run reports.                                                                                         |
| `specs`       | none                               | Directory each `spec:` token resolves against.                                                             |
| `skills`      | none                               | Vendored guides, one directory per guide holding `SKILL.md`.                                               |
| `shelf`       | none                               | Retired bodies. Read for ids, never parsed. See [Lifecycle](lifecycle.md#retiring-an-item-nobody-will-do). |
| `root_marker` | none                               | A path that must exist. Catches a config that traveled to a tree it does not describe.                     |
| `register`    | `.drover/register.jsonl`           | The follow-up list, read only under [`[register] backend = "file"`](#register).                            |
| `logs`        | `$XDG_CACHE_HOME/drover/<project>` | Run logs. Multi-MB, so they belong outside the repo.                                                       |
| `log_prefix`  | none                               | Prepended to every log filename. Set it to keep another driver's existing runs discoverable.               |

Set `root_marker`. Without it a config copied to the wrong repository resolves every path against a stranger's tree and reports confidently on nothing.

`shelf` has no default. Leave it unset and nothing consults a shelf, and no finding fires, except that `shelve` refuses and names the key to set. Set it and four commands consult it. Two read it for the questions where a missing id yields a wrong answer rather than a smaller one: `intake` allocates above the highest id in either directory, and `check` stops calling a ledger row orphaned when its body is on the shelf. The other two own the move itself: `shelve` renames a body into this directory and `unshelve` renames it back. Nothing else reads it. A shelved item never renders in `status`, never enters the graph and never moves a count.

`check` validates the path. A `shelf` naming a directory that is not there is a `NO SHELF` finding and one naming the backlog directory is a `SHELF IS BACKLOG`, because a shelf drover cannot open reads as no shelf at all: `ORPHAN ROW` returns for shelved ids and `intake` starts reissuing them. An unset key is validated against nothing and stays silent.

drover reads ids from the filenames there and never opens the files, so a retired body with broken frontmatter, an undeclared key or an intake stop marker is not a finding: nothing is going to run it. Keep the README explaining the directory beside the backlog; a filename carrying no id is skipped.

## `[ids]`

These keys bound the planned backlog and name the prefixes. How an id is written, and why commands take `B1` while filenames carry `B01`, is under [Item ids](concepts.md#item-ids).

| Key           | Default         | Meaning                                                                                                           |
| ------------- | --------------- | ----------------------------------------------------------------------------------------------------------------- |
| `planned_max` | `0` (unbounded) | Rejects an out-of-range planned id as a typo. Set it only when the plan is closed.                                |
| `namespaces`  | none            | Prefix to kind: `{ B = "bug", E = "enhancement", F = "feature" }`.                                                |
| `allocator`   | `"local"`       | Where `new`, `intake` and `split` reserve a new id: `"local"` (from disk) or `"git"` (a counter ref on `remote`). |
| `remote`      | `"origin"`      | The git remote name or URL holding the counter under `allocator = "git"`. Ignored under `"local"`.                |

Prefixes must be uppercase ASCII, since they become filename components. Longer prefixes are matched first, so a two-character namespace is not split by the one-character scan.

The **kind** is matched against tracker labels by `intake`, so it must be the label's exact text.

### Allocation

Under `"local"` an id is the highest number in the backlog and on the shelf, plus one. `new`, `intake` and `split` read that number and create the body under one exclusive lock on the backlog directory, and create the file with `O_EXCL`, so two commands running at once on one host (two review sessions under `drive --jobs 2`, whose worktrees link back to this backlog) never write two bodies under one id. On a platform without `flock` the lock is a no-op and only the `O_EXCL` create remains, which catches a collision on the full filename and not one on the id alone.

`"local"` cannot see another host. Two machines allocating before either syncs both issue the same id, and the two files merge in git without a conflict because their slugs differ. Syncing the backlog afterwards does not help: nothing reserved the id first.

`"git"` reserves it first. Each allocation, still under the local lock:

1. Fetches `refs/drover/ids` from `remote` into `refs/drover/fetched-ids` in this repository. The ref points at a commit whose tree holds one file per prefix, named for the prefix and containing the highest number issued under it.
2. Takes the larger of that number and the local highest, adds one, and commits the new value on top of the fetched commit. Nothing touches the index or the work tree.
3. Pushes with `--force-with-lease=refs/drover/ids:<fetched sha>` and `--no-verify`, so the repository's `pre-push` hook does not run for a ref that carries no code. A rejected lease means another host allocated in between: drover prints the retry to stderr, refetches and tries again, up to 5 attempts, then refuses with exit 3 naming the remote.

A remote with no `refs/drover/ids` is seeded from the local highest for every prefix, so turning `"git"` on in a project that already has items never reissues an id on disk or on the shelf. Every push also raises any prefix the remote holds below the local highest.

When the remote cannot be reached, `new`, `intake` and `split` refuse with exit 3, name the remote and git's error, and write no body. The same holds when the whole allocation, every attempt included, runs past 30s: git is killed and the refusal names the remote and the bound, so a remote that never answers holds the backlog lock for that long at most. A remote that answers and refuses the ref (a protected-ref rule, a permission, a server hook) refuses with exit 3 as well, quoting git's first stderr line rather than calling the remote unreachable. drover never falls back to disk on its own, because that is the race this allocator closes. `--local` on any of the three allocates from disk for that one call, and can reissue an id another host has already taken.

No allocator git call can prompt. Each runs with `GIT_TERMINAL_PROMPT=0`, and with `-o BatchMode=yes` appended to whichever ssh git would have used (`GIT_SSH_COMMAND`, then `core.sshCommand`, then `GIT_SSH`, then `ssh`), so a custom key or proxy still applies and a passphrase or host-key question fails instead of waiting. That assumes OpenSSH; a `plink` command rejects the option.

The ref names drover on whatever remote holds it. `refs/drover/ids` and its commit messages carry the prefixes and the highest number under each, nothing from any body, but anyone who can read the remote learns that this project uses drover and roughly how large its backlog is. Point `remote` at a private remote, or a URL other than `origin`, when that matters. The ref sits outside `refs/heads` and `refs/tags`, so a default clone or fetch does not bring it along.

## `[frontmatter]`

```toml
[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "closes", "severity", "skills", "spec", "runner", "migration"]
```

The schema is closed: any key outside these two lists is a parse error. Required keys must be present, though they may be empty: presence is what proves the author considered the key.

`title` is special: it is the one value drover renders itself, and it may not be empty. `slug` is rejected outright; the filename is its only authored copy.

A key appearing in both lists is a config error.

Six of these keys drover reads for itself rather than passing through to a template: `title`, `deps`, `closes`, `issue`, `skills` and `spec`. Leaving one out is invisible to the parser: the read returns empty everywhere, and whether anything says so depends on the command doing the reading. `drover init` and `drover init --user` therefore declare all six, checked by a test against the keys drover reads, so a key drover learns to read cannot ship without reaching the generated defaults. `kind` needs no such check: `intake` writes it into a scaffold only when the project declares it.

`severity` is read through a setting rather than a name, so it is the one you may omit deliberately: leave it out of both lists and no item can rank, which is a supported configuration rather than a broken one. See [`[severity]`](#severity). `[run] runner_key` works the same way one table over. Set either one to a key `[frontmatter]` does not declare and `drover check` reports `UNDECLARED KEY`; leave either at its default and nothing is said, because a project that ranks nothing or pins no runner has failed nothing.

### `[frontmatter] shape`

The lists above close which keys may appear. They say nothing about what a key may hold, so without the table below every value is a string and a typo is a value: `severity: Z` ranks as `Unranked` and renders identically to a body that declared no rank at all.

```toml
[frontmatter.shape]
issue    = "int"
severity = "enum:C,H,M,L"
blocks   = "idlist"
closes   = "intlist"
```

| Type         | Holds                                                                                                                                                      |
| ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `int`        | A whole number. `issue: banana` fails; `issue: 46` passes.                                                                                                 |
| `enum:A,B,C` | One of the listed members, read case-sensitively. `severity: Z` and `severity: m` both fail an `enum:C,H,M,L`.                                             |
| `idlist`     | Whitespace-separated ids in bare form, resolved through the project's namespace prefixes. `B0004` fails as the padded form and `nonsense` as no id at all. |
| `intlist`    | Whitespace-separated positive whole numbers. `#47` fails on the sigil, `0` and `-3` on the value.                                                          |

`intlist` backs the `closes` key, whose numbers `finish` turns into a close or a comment on somebody's report, so a token that is not an issue number is caught in `drover check` rather than at the tracker. See [issue dispositions](concepts.md#issue-dispositions).

`idlist` checks the form and never whether the id names a body. That is [`MISSING DEP`](concepts.md#validation)'s question, and answering it twice is how two classifiers come to disagree.

The table is optional and additive: a project that writes none has no value checked. An empty value passes every type, because `required` is what separates presence from content, and a type that also demanded a value would move that line without anyone declaring it moved.

`drover check` reports a value that fails its type as `BAD VALUE` and exits 2. The body stays readable: the frontmatter parsed, so `status` still shows the item and `run` still launches it. An entry naming a key that is in neither list is `UNDECLARED KEY`, since nothing can carry it and nothing is ever checked. A type that is not one of the four forms is a config error at load, for the same reason a typo'd palette color is: the author just typed it, and no item is at fault.

`drover schema --frontmatter` emits this contract as JSON (the required keys, the optional ones and the type declared for each), so an agent drafting a body reads the target instead of imitating a neighboring file.

## `[verify]`

```toml
[verify]
default   = "go build ./... && go test ./..."
web_only  = "npm run build"
migration = "makemigrations --check && pytest"
```

Each value is a verify command: what the agent runs to verify its work. `default` applies unless a command named for a frontmatter key wins. **A verify command whose name matches a frontmatter key is selected when that key is `yes`**, so an item with `web_only: yes` gets the `web_only` command. Names are tried in sorted order and the first match wins, so two set flags resolve the same way every run.

A template that needs more control reaches `.VerifyCommands` directly and ignores `.VerifyCommand`.

drover hands the selected verify command to the implementing session through the prompt, and runs it itself in one place: `drover drive`, after a branch update merges origin's trunk into an item's branch. It runs the same command the item's run was told to pass, as `sh -c` in the item's worktree with `DROVER_ROOT` set, before the merge is pushed. When it fails there, drive runs it once more on the trunk alone to decide whether the branch or the trunk is at fault. A verify command that cannot run twice per item in reasonable time makes every moved trunk cost that much more. See [Keeping each branch level with the trunk](lifecycle.md#keeping-each-branch-level-with-the-trunk).

## `[status]`

| Key             | Meaning                                                                                                |
| --------------- | ------------------------------------------------------------------------------------------------------ |
| `column`        | Frontmatter key shown beside the id.                                                                   |
| `column_prefix` | Prepended to that value: under `column = "wave"`, `W` renders `wave: 3` as `W3`.                       |
| `timezone`      | The zone every time in `status` is reported in: `UTC` (the default), `local`, or an IANA name.         |
| `refresh`       | Seconds between the reloads `builtin:html` asks a browser for. `60` by default; `0` writes no refresh. |
| `index`         | The href of the cross-project index `builtin:html` links its header back to. Absent by default.        |
| `render`        | `{ template, path }` pairs `status` renders to files. Empty by default.                                |

`column` names one of your own [`[frontmatter]`](#frontmatter) keys, and drover never interprets what it holds: the value prints verbatim beside the id and is read by nothing, since `deps` alone decides what runs and no flag filters on this column. Which axis it carries is the project's to pick (a wave, a phase, the subsystem an item touches), so `wave` above is an example, not a key drover knows.

An item with no value for `column` falls back to its namespace kind, which is what lets one setting serve both a planned backlog and an intake queue.

`timezone` converts every time `status` reports: the table, `--json`, and the data a `--template` reads. The ledger stores each row in the zone of whoever wrote it, so a project worked from two machines reads `-07:00` beside `Z` without one. Each time keeps its offset after conversion, so `America/Los_Angeles` gives `-08:00` in January and `-07:00` in July. `local` is the zone of the machine running `status`, which for a drive's renders is the drive's machine. An unknown name fails at load, naming the key and the value:

```text
.drover.toml: status.timezone is "America/Los_Angles": unknown time zone America/Los_Angles (known: UTC, local, or an IANA name such as America/Los_Angeles)
```

`drover status --tz <zone>` overrides it for one call.

```toml
[status]
timezone = "America/Los_Angeles"
```

`render` keeps a status view current in a file: a notes vault, a wiki page, a status board. Each pair names a Go `text/template` and the file it renders to, both relative to the project root unless absolute, and a leading `~/` resolves against `$HOME`. `builtin:obsidian` and `builtin:html` name the [templates drover ships](automation.md#built-in-templates) instead of a file, and any other `builtin:` name fails at load with the list. The template reads the object `status --json` emits, under the same field names, plus `now`; [`status --template`](automation.md#status---template) documents the data and carries a sample.

```toml
[[status.render]]
template = "docs/status.tmpl"
path     = "~/notes/drover/status.md"
```

`drive` re-renders every pair after each ledger row it appends and each change to its queue, so the file follows the drive as it moves. A render that fails is reported once per drive and never parks, stops or fails it. `drover status --render` renders them all once by hand. Every write replaces the file through a temp file and a rename, so a reader never sees half of one; the file is replaced whether or not it exists, because naming it here is the decision to replace it.

`refresh` is the interval `builtin:html` writes into its `<meta http-equiv="refresh">` tag, in whole seconds, so a browser left open on the page follows the drive. `0` leaves the tag out, and a negative value fails at load.

```toml
[status]
refresh = 30

[[status.render]]
template = "builtin:html"
path     = "/srv/www/drover/index.html"
```

`index` is the href of the page [`status --all-projects --html`](automation.md#status---all-projects) renders, the cross-project index, as the project's `builtin:html` page should link to it: a path relative to that page, an absolute path, or an `http`, `https` or `file` URL. When it is set, the page header is a breadcrumb, `drover` linking to `index` and then the project name; absent, the header is the project name alone. The template reads it as `.index`, an empty string when unset. A value that does not parse as a URL reference, carries surrounding whitespace, or names any other scheme fails at load, since `html/template` would turn a `javascript:` link into a dead one without saying so:

```text
.drover.toml: status.index is "javascript:alert(1)": scheme "javascript" is not one a page links to (known: http, https, file, or none for a path)
```

```toml
[status]
index = "../index.html"

[[status.render]]
template = "builtin:html"
path     = "/srv/www/drover/webapp/index.html"
```

with `drover status --all-projects --html -o /srv/www/drover/index.html` writing the page it points at.

## `[severity]`

| Key     | Default                                                 | Meaning                                 |
| ------- | ------------------------------------------------------- | --------------------------------------- |
| `key`   | `severity`                                              | Frontmatter key holding an item's rank. |
| `ranks` | `{ C = "red", H = "orange", M = "yellow", L = "blue" }` | Rank to palette name.                   |

Both defaults apply with no block written, so a project that wants the standard scale configures nothing. Palette names: `red`, `orange`, `yellow`, `blue`, `green`, `grey`, `white`. A name outside that set is a config error naming the set, because rendering a typo'd color plain would hide the one thing the block exists to make visible.

`ranks` is keyed by rank, and `C` and `Critical` are the same rank, so write either. Anything that is not a rank is a config error. An explicit table **replaces** the defaults rather than merging into them, so a project that ranks only two levels writes only two rows.

```toml
[severity]
key   = "impact"
ranks = { Critical = "red", High = "orange" }
```

`key` must also appear in `[frontmatter]`, or no item can declare it and nothing ever colors. `drover check` reports that as `UNDECLARED KEY`, but only for a project that wrote the block: the defaults are on everywhere, and a project that does not rank its work is not misconfigured.

Color reaches the status table only when `[status] column` names this same key. `drover show` colors the line regardless. An absent, empty or unrecognized value is never an error anywhere: it renders as written, sorts last under `--by-sev`, and is selected by `--sev none`.

### `--sev`

Three commands spell a flag `--sev` and mean the vocabulary above. They differ in arity, and the difference follows from what each one does, so the three are described here once rather than in each command's own section.

| Flag                          | Arity                       | Accepts                                                    | Omitted                           |
| ----------------------------- | --------------------------- | ---------------------------------------------------------- | --------------------------------- |
| `drover status --sev`         | Repeatable, comma-separated | `C`, `H`, `M`, `L`, `none`                                 | No filter                         |
| `drover follow-up open --sev` | One value                   | `C`, `H`, `M`, `L`, or `critical`, `high`, `medium`, `low` | Files unranked                    |
| `drover new --sev`            | One value                   | `C`, `H`, `M`, `L`                                         | `M` for a bug, unranked otherwise |

`status --sev` selects rows, so it needs a token for the rows carrying no rank drover ranks: that is `none`, and it means the absence of a rank rather than the lowest one. `--sev none` and `--sev L` never select the same row.

`follow-up open --sev` records a rank on one row, so `none` is not a value it could write. The way to file an unranked row is to omit the flag, which is what the row already means when it prints with `-` for a rank.

`new --sev` seeds the scaffold's frontmatter rank, and is the one of the three whose default depends on what it is writing: a bug scaffolds `M` and every other kind scaffolds unranked. That is what `intake` does with an issue the tracker never ranked, so a hand-written item and a taken-in one start at the same rank. Omitting it is therefore not the same as filing unranked, which is why the flag has no `none` either.

All three refuse a value outside their own list, exit `3`, and name what they would have taken:

```console
$ drover status --sev P0
drover: unknown severity "P0" (known: C, H, M, L, none)
$ drover follow-up open "a gap" --sev P0
drover: unknown severity "P0" (known: C, H, M, L, critical, high, medium, low)
$ drover new bug "a gap" --sev P0
drover: unknown severity "P0" (known: C, H, M, L)
```

Refusing rather than matching nothing is deliberate on the `status` side: an empty table would read as "no items at that rank", which is a wrong answer, not a degraded one.

## Color

Color is not a config key. drover decides it per stream at each invocation, so `drover drive > drive.log` still colors the stderr a terminal reads. Every colored state already has a word or a glyph beside it, so a plain run reads the same and loses nothing.

The decision, highest precedence first:

1. `--color=auto|always|never` on any command. `--no-color` is `--color=never`, and the later of the two wins.
2. `NO_COLOR`: any non-empty value turns color off.
3. `CLICOLOR_FORCE`: non-empty and not `0` turns color on, a pipe included.
4. `TERM=dumb` turns color off.
5. Otherwise a stream is colored when it is a terminal.

OSC 8 links on issue and pull request numbers follow the same decision for the stream they are written to.

Never colored, whatever the flag says: `--json` and `--events` output, every file drover writes (the detached drive's `<drive id>.log`, reports, `status -o` and `--render` outputs, the ledger), notification bodies and the generated `AGENTS.md`. `--color=always` reaches terminal-facing text only. A detached drive runs with `--color=never`, since its stderr is a log file.

The palette names roles rather than colors. It uses the 16 base ANSI colors plus bold and dim, so the terminal's theme picks the shade and the text stays legible on a light background and a dark one.

| Role      | Code    | Used for                                                                       |
| --------- | ------- | ------------------------------------------------------------------------------ |
| `ok`      | green   | merged, passed, done, a clean `check`                                          |
| `warn`    | yellow  | paused, held, waiting, skipped, blocked, `needs_operator`, a `usage_warning`   |
| `fail`    | red     | parked, refused, a red check, a `check` finding, the `drover:` before an error |
| `heading` | bold    | the `==>` lines of a drive, help's section headings and `Safety:` label        |
| `detail`  | dim     | the `·` lines of a drive that report no state                                  |
| `id`      | cyan    | an item or drive id                                                            |
| `command` | magenta | a command the operator is told to run, such as `drover drive --resume`         |
| `muted`   | dim     | timestamps and the comments beside help's examples                             |

The `[severity]` colors are applied through the same mechanism and draw on the same 16 base colors: `red` is red (31), `orange` is bright red (91, since the base set has no orange), `yellow` is yellow (33), `blue` is bright blue (94, because base blue is hard to read on a dark background), `green` is green (32), `grey` is bright black (90), and `white` emits no escape, leaving the terminal's own foreground.

## `[run]`

| Key              | Default   | Meaning                                                                                                                                                                                                                                                           |
| ---------------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `branch`         | see below | Template over the prompt data, minus `.FollowUps`.                                                                                                                                                                                                                |
| `default_runner` | `claude`  | Used when neither the flag nor the item names one.                                                                                                                                                                                                                |
| `runner_key`     | `runner`  | Frontmatter key that pins a runner per item. Set it to a key `[frontmatter]` does not declare and no item can carry it: `run` falls back to `default_runner` and says nothing about the pin it ignored, which is what `drover check` reports as `UNDECLARED KEY`. |
| `review_runner`  | unset     | Runner `drover review` hands the prompt to. Unset means review prints it.                                                                                                                                                                                         |
| `draft_runner`   | unset     | Runner `drover draft` hands the drafting prompt to. Unset means draft prints it.                                                                                                                                                                                  |
| `trunk`          | detected  | The branch work merges into, and the point `run` and `drive` create an item's branch from. Empty asks `origin/HEAD`, then falls back to `main`. Also read by the review prompt's diff range, `reconcile` and the merge gate.                                      |

```toml
branch = "{{ if .Planned }}{{ .Slug }}{{ else }}{{ lower .Stem }}-{{ .Slug }}{{ end }}"
```

The default prefixes a namespaced item with its padded id because an issue-derived slug collides far more readily than a hand-picked one, and ending up on somebody else's branch is silent.

`.Kind` is what the item is, and a project wanting its branches grouped by that puts it in front of the id rather than in place of it, for the reason above:

```toml
branch = "{{ with .Kind }}{{ . }}/{{ end }}{{ if .Planned }}{{ .Slug }}{{ else }}{{ lower .Stem }}-{{ .Slug }}{{ end }}"
```

which renders `bug/b07-login-redirect-loops` and `feature/f03-export-to-csv`. The kind is the item's own frontmatter `kind` where it declares one, else the kind its id's namespace maps to in [`[ids]`](#ids), and empty when neither answers. `with` rather than `if` is what makes that last case render the plainer name: a branch template that errors takes the item out of the branch lookup without a word, while `run` and `finish` on the same id refuse with the template error, so `drover status` reports that no item owns a branch the item is standing on.

`.FollowUps` is the one field this template cannot reach, because a name derived from the tracker would change the moment `gh` went unreachable. See [the prompt data](prompts.md#the-branch-template-sees-less).

`review_runner` and `draft_runner` are read from the project alone. No frontmatter key selects a reviewer or a drafter, and `runner_key` is never consulted for either: it names who implements an item. Reviewing under that name would have the implementer certify its own work, which `drover audit` refuses to record as `verified`, and an item with no requirements yet has nothing to say about who should draft them. `--runner` covers the exception on both commands. See [the three stages](lifecycle.md#three-stages-three-runners).

## `[runners]`

```toml
[runners.claude]
interactive = ["claude"]
headless    = ["claude", "-p", "--output-format", "stream-json", "--verbose"]
stdin       = false
```

The prompt is appended as the final argument unless `stdin` is set, in which case it is piped instead. A runner is a whole binary, not a flag: swapping runners swaps how the work executes.

Where the prompt ends up is the argv's problem, not drover's. A flag that takes its value from the next argument has to come last, or the argument drover appends goes to that flag and the prompt becomes a positional the binary ignores. `claude -p` is a boolean and can sit anywhere; `agy -p` is an alias for `--print` and takes the prompt, so it goes at the end:

```toml
[runners.agy]
interactive = ["agy", "-i"]
headless    = ["agy", "--output-format", "stream-json", "-p"]
```

`codex` takes its prompt as a positional argument in both modes, so it needs no stdin flag:

```toml
[runners.codex]
interactive = ["codex"]
headless    = ["codex", "exec", "--json", "--yolo"]
```

A headless launch gets the null device on stdin unless `stdin` is set, and that is doing real work: `codex exec` reads stdin for additional input and waits for an EOF a terminal never sends, so a session that inherited it announces "Reading additional input from stdin..." and then hangs before its first event. An interactive launch keeps the terminal, because a TUI reading from the null device draws once and exits.

### What drover reads back

Starting a binary is half the contract. A headless runner must write a JSONL event stream to stdout, ending in a **terminal event** that says how the session finished, and drover has to recognize that event: it is the only thing separating a session that completed from one that stopped. drover reads four formats, matched against the event itself rather than declared in config:

| Runner   | Terminal event                                                               | Success reported by           |
| -------- | ---------------------------------------------------------------------------- | ----------------------------- |
| `claude` | `{"type":"result","subtype":"success","is_error":false,...}`                 | `subtype` and `is_error`      |
| `agy`    | `{"event":"result","result":{"status":"SUCCESS",...}}`                       | `status`                      |
| `codex`  | `{"type":"turn.completed","usage":{...}}`, or `turn.failed` / `turn.aborted` | the terminal event's own name |
| `t3`     | `{"t3":"result","subtype":"success","is_error":false,...}`                   | `subtype` and `is_error`      |

Matching is on the event's kind, not on the key that names it. `claude` and `codex` both name their kinds under `type`, so a reader matching a line by its key alone hands `claude` every `codex` event, finds no terminal one among them, and reports a reviewer that finished as one that died.

`codex` is why two of those columns read oddly. It reports its outcome as the name of the terminal event rather than as a field, and it reports its final message as an `item.completed` event well before the end, so drover carries the last agent message forward to stand as the run's report. A turn that failed states its own reason under `error.message` and keeps that instead.

A runner writing none of these formats is launched, runs, and reads as silence. `drover report` finds no report, `drover status` leaves the item `READY` rather than `implemented`, and `drover drive` parks it as `unreadable`, naming the runner and the log path. That is a distinct park from `died`, which is a session that stopped: an empty log, or a stream drover reads that ended before its terminal event. No flag makes a fourth format readable: a runner drover does not know needs a schema in `internal/runlog/runlog.go`, which is one entry naming the key its events carry, every kind they use, which of those kinds end a session, and how to read one.

`drover check` reports a `[runners]` entry whose binary is on PATH and whose argv that binary cannot use, as `BAD RUNNER`, and carries the argv that works. It judges only the binaries drover ships knowledge of, so a wrapper is silent there; a wrapper's output is judged where the output exists, which is the park above.

drover tees the stream to the log and renders assistant text and tool markers as a live feed, reading all four formats above. Discovery is by filename, so any runner writing into the log directory is discoverable whether or not drover knows what produced it.

### T3 Code threads

A runner can be drover's own bridge to a [T3 Code](https://github.com/pingdotgg/t3code) server: `drover t3 run` launches each stage as a T3 Code thread over the server's MCP endpoint, follows it to the end, and prints the `t3` format above. The argv shape is the one every runner has, so `run`, `review --runner` and every `drive` stage use it unchanged:

```toml
[runners]
t3       = { headless = ["drover", "t3", "run", "--provider", "claude", "--permission", "full", "-p"] }
t3-codex = { headless = ["drover", "t3", "run", "--provider", "codex", "--permission", "full", "-p"] }

[run]
default_runner = "t3"
review_runner  = "t3-codex"
```

`-p` takes the next argument as the prompt, so it goes last. With `stdin = true` the argv carries `--stdin` instead. `--model` takes a model id as the server's `orchestrator_capabilities` lists it, and defaults to the first one listed for the provider. `--permission full` launches at T3's `full-access` runtime mode, which a drive needs because nobody answers approvals during one; `supervised`, the default, launches at `approval-required`.

`drover check` judges a bridge argv as `BAD RUNNER`: it must be `drover t3 run`, and `-p` must be last. Where the host holds `[t3] provider_logs`, it also reports `BRIDGE SIGNED OUT`, `BRIDGE UNREACHABLE` and `BRIDGE VERSION`; see [`[t3]`](#t3). [Operating](operating.md#t3-code-threads) covers setting the host up.

Omitting the section entirely gives you the built-in `claude` runner above.

## `[prompts]`

| Key         | Meaning                                                                          |
| ----------- | -------------------------------------------------------------------------------- |
| `implement` | Path to your own prompt template for an implementing run. Omit for the built-in. |
| `draft`     | Path to your own drafting prompt. Omit for the built-in.                         |
| `review`    | Path to your own review prompt. Omit for the built-in.                           |

See [Prompt templates](prompts.md).

`drover check` renders each template these keys name against a sample prompt in which every field is set, every list holds one element, `.Meta` holds every key `[frontmatter]` declares and `.VerifyCommands` holds every command `[verify]` names, and reports `BAD TEMPLATE` for one that cannot be read or will not render. A field your template names that this build does not have, or a key or verify command the project does not declare, fails there rather than at `drover run`.

The drafting prompt is what `drover draft` hands an agent drafting an item's numbered requirements, which is not what a run needs to execute them: the originating issue, `[agents] principles`, the numbered-list contract, the sibling items with their paths, and the body file to write into. It reaches the same data as the implement prompt plus `.Path`, `.Posture`, `.StopMarker`, `.Origin` and `.Siblings`. Whatever it says, `draft` refuses an item whose stop marker is gone and never deletes one. See [the three stages](lifecycle.md#three-stages-three-runners).

The review prompt is the mirror of the implement prompt: same data, plus the item's numbered requirements, its dependencies' audit levels, and the run's own report. Put the project-specific half of your review doctrine (the security principles, the quality bar) in this template rather than in a `CLAUDE.md` an agent has to remember to consult.

## `[review]`

| Key                 | Meaning                                                                                                                      |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `max_review_rounds` | How many times a review may send one item back before `drover request-changes` refuses. `0`, and an absent key, mean no cap. |

`max_review_rounds` bounds a review loop that converges without terminating: each pass is correct on its own, the diffs get smaller, and nothing stops. Attended, that costs patience. Driven unattended it costs money until something outside drover intervenes, which is the case the cap is for.

Off unless you type a number, because a default would change when `request-changes` refuses for every project on the day it upgraded, and how much a loop may spend is not something drover can know. Set it higher than the worst item that went round the loop and merged correctly; three is a reasonable start. At the cap the review prompt stops instructing `drover request-changes` and renders the filing path instead, so a reviewer is told where a finding goes before it is refused.

The escape is `drover request-changes --over-limit`, and it refuses inside a session drover launched. An agent requests changes freely below the cap; deciding a finding is worth exceeding it is the operator's, because the agent is the party being bounded. A refusal that names its own override is an instruction, and the reviewer the prompt warns about being agreeable is who would take it. Raising the number is the other way out, and the one to prefer when you find yourself overriding.

## `[drive]`

| Key               | Meaning                                                                                                                                                         |
| ----------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `merge_gate`      | The project's go/no-go for `drover drive`, as an argv array.                                                                                                    |
| `protected_paths` | Globs a merge refuses to carry. Empty means the built-in set.                                                                                                   |
| `worktree_links`  | Paths `drive` symlinks into each item's worktree from your checkout, beyond the record directories it links unasked.                                            |
| `worktree_setup`  | An argv `drive` runs in each new worktree before the first session, such as `["npm", "ci", "--prefix", "web"]`.                                                 |
| `max_load`        | The 1-minute load average above which `drive` holds dispatch while [`[supervise] host_load_admission`](#supervise) is `auto`. Unset means the host's CPU count. |

`drover drive` walks the ready queue unattended: implement, review, remediate until the review stops sending work back, then ask the merge gate and merge what it passes. The first two keys here are about the last step, and `merge_gate` is not optional: running `drive` is the operator saying the code may merge, so a project that declares no merge gate is refused before the first launch. `--no-merge` is the run that needs none.

`merge_gate` runs in the item's worktree with the branch and the trunk appended as the last two arguments, as [`[git] merge_check`](#git) does, and with `DROVER_ROOT` set to the project root and `DROVER_GATE_MERGED` set as described below the exit codes. A relative argv resolves inside the worktree, so a merge gate script your project keeps outside version control has to be reachable through `worktree_links`. Whether a head may merge is the merge gate's answer alone: a required-check list and how long a run may take are each a property of the project, and the whole answer a loop needs is an exit status. A merge gate that has to wait waits in its own process, where the project already knows what it is waiting for. What drive does first is wait for the head's checks to finish and classify any red one, rerunning a failure the forge's infrastructure caused and parking one the trunk fails too, so the merge gate is not the one to send them back; see [`[forge]`](#forge).

**It runs before any pull request exists, so it cannot ask about one.** drover opens a pull request only for a branch this merge gate has passed, which means a merge gate reading `gh pr checks` would need the document its own answer decides. What it can read is the branch head: drover pushes what it drives, so the commit is on the remote and its check runs are readable. A project whose CI fires on `pull_request` alone has nothing there to read and needs its merge gate to answer some other way. The reference merge gate, [`scripts/merge-gate.sh`](https://github.com/ravinald/drover/blob/main/scripts/merge-gate.sh), waits up to `MERGE_GATE_WAIT` for an answer, which has to exceed the project's longest run: CI starts on the push and usually finishes during the review, but a `FIX-IN-REVIEW` commit moves the head and starts it again.

It carries three exit codes rather than `merge_check`'s two, because a red check means opposite things depending on what reddened it:

| Exit | Means                                                    | drive does                                                                              |
| ---- | -------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `0`  | green                                                    | open the pull request, then merge                                                       |
| `1`  | red, and this diff caused it                             | sends the merge gate's output back to the branch and re-runs                            |
| `2`  | red, and the code did not cause it                       | stops the whole drive                                                                   |
| `4`  | the head is behind the trunk, and nothing else was asked | updates the branch and runs the merge gate once more; a second `4` parks `gate_refused` |

drive also reads the merge gate once after a merge it did not make itself: when the forge merges an `--auto` request on the spot, the merge gate runs on the merged head, and anything but `0` stops the drive with the item merged and not finished. A merged head cannot be updated, and after a squash or a rebase it is never an ancestor of the trunk, so a merge gate that answers `4` from an ancestry test would refuse every one. drive sets `DROVER_GATE_MERGED=1` on that read and leaves it empty on every other, and a merge gate must not exit `4` under it. A `4` there still stops the drive, with a reason naming the variable, because the merge gate never said whether the checks passed. `scripts/merge-gate.sh` skips its behind check under the variable, and also when the head is already reachable from the trunk.

Anything other than 0, 1 or 4 is read as 2. An unclassified failure stops rather than loops: a trunk that did not receive this merge is the trunk every later item builds on, items declaring it on `deps:` would run against code that is not there, and a forge that is not answering will not answer for the next item either. Only the command that read the forge can tell "the tests failed" from "a runner never started", which is why the distinction is the project's to make.

Undeclared, `drive` refuses at the start of the run rather than discovering it per item: the operator asked drover to merge and named nothing that could say green, which is a config error to report before any session is paid for. Under `--no-merge` nothing needs to say green, and there an absent merge gate stops each item green and reviewed, with no pull request, since nothing said the branch may merge. `drover open-pr <id> --force` opens one on the operator's own judgment, which is what a project declaring no merge gate has instead.

`protected_paths` is matched against the files the branch changed, including files it added and both sides of a rename, as paths relative to the repository root. An entry ending in `/` names a directory and holds every file below it; one ending in `*` holds every path with that prefix at any depth; anything else is a [`path.Match`](https://pkg.go.dev/path#Match) glob, so `migrations/*.sql` reaches one level only. Names are matched as the bytes git stores, whatever `core.quotePath` says, so a non-ASCII name or one carrying a space, tab, quote or newline is held like any other. `drover check` reads each entry the same way against the tracked files and reports one that matches nothing as `PROTECTED UNMATCHED`. An agent that edits the merge gate, a workflow or `.drover.toml` in its own pull request changes what every later item that night is measured by, and that change would be reviewed by the same loop it just altered.

`worktree_links` and `worktree_setup` exist because `drive` builds every item in a worktree of its own rather than in your checkout (see [Where each item runs](playbook.md#where-each-item-runs)). A worktree holds what git tracks on that branch and nothing else. `drive` links the top directory of every `[paths]` entry inside the root unasked, and `worktree_links` adds what else a session or the merge gate reads from outside version control: an overlay's docs, an agent's settings directory. Each link has to be a path git ignores on the item's branch, written without a trailing slash, because a link is a file to git and `.drover/` matches only a directory. A record directory that fails that test is skipped with a note, since drover reaches its records through `DROVER_ROOT` either way; a `worktree_links` entry that fails it parks the item as `worktree_failed` before any session is launched. `drive` checks the `worktree_links` entries again before every session, so a path the branch stopped tracking after the worktree opened, which a branch update deletes, is linked before the next session starts.

A linked file is read-only to sessions in effect. The link points into your checkout, so a write through it lands there before the item's branch merges: a session that ran `drover agents --write AGENTS.md` with its branch's build once replaced the checkout's generated block with one describing commands the trunk did not have, and every drover command in the checkout refused as stale until the branch merged. So `agents --write` in a session drover launched writes the worktree's copy rather than the checkout's: it replaces an untracked link pointing out of the worktree, which is what `drive` makes, with a file of the worktree's own and leaves your copy unchanged, while a link the branch tracks, or one in your checkout, is written through as it is outside a session; and before each later session `drive` turns an untracked file it finds where a link belongs back into the link, so the next session reads your copy again. The replacement file is ignored as the link was, so it never reaches the branch. Other tools a session runs still write through the link, so keep a file a session has to edit out of `worktree_links`.

`worktree_setup` runs once, when the worktree is created, and a non-zero exit parks that item with the output and leaves the queue running.

`merge_gate` and `worktree_setup` are executed, like [`[runners]`](#runners) and `[git] merge_check`. A [`[verify]`](#verify) command mostly is not: it renders into the prompt as text and the agent reading it runs it, and drover runs it itself only in `drive`, after a branch update. Check that distinction before adding another executed key. `gh` belongs on the same list without being a key at all: `open-pr` and the merge run it, so a drive reaching the forge depends on a binary no configuration names.

## `[finish]`

| Key          | Meaning                                                                                   |
| ------------ | ----------------------------------------------------------------------------------------- |
| `post_check` | An argv run after every finish, such as `["scripts/post-finish.sh"]`. Unset runs nothing. |

```toml
[finish]
post_check = ["ssh", "dev.example.com", "scripts/check-migrations.sh"]

[supervise]
post_finish_check = "auto"
```

`post_check` runs once an item is finished, from `drover finish` or from a drive finishing a merge it made, and only while [`[supervise] post_finish_check`](#supervise) is above `off`. Its default is `off`, so declaring `post_check` alone runs nothing, and `finish` prints one line saying so. It runs from the project root with three variables set:

| Variable      | Holds                                                                        |
| ------------- | ---------------------------------------------------------------------------- |
| `DROVER_ITEM` | the bare id that finished                                                    |
| `DROVER_PR`   | the pull request number the ledger recorded for it, empty when none was      |
| `DROVER_SHA`  | the commit origin's trunk points at after the finish, else the local trunk's |

Exit 0 appends a ledger row `{"id","post_finish","date"}` carrying the first line of what the command printed. Any other exit, including a binary that is not on `PATH`, files a follow-up owned by the item in the project's [follow-up list](#register) with the command's last 40 lines in its body, and emits `needs_operator` for `post_finish_check`, which reaches each [`[notify]`](#notify) destination routing it. Neither undoes the merge or changes `finish`'s exit status. Nothing bounds how long it runs, as nothing bounds `[drive] merge_gate`.

`post_check` is executed, like `merge_gate` and `worktree_setup`: see the paragraph closing [`[drive]`](#drive). A first element that is empty is a load error.

## `[numbered_files]`

| Key    | Meaning                                                                                                                                                            |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| _name_ | A glob relative to the repository root with one `{n}` where the number sits, such as `"backend/migrations/{n}_*.up.sql"`. The name labels the sequence in reports. |

```toml
[numbered_files]
migrations = "backend/migrations/{n}_*.up.sql"
adr        = "docs/adr/{n}-*.md"
```

Some files carry a number whose order matters after the merge: database migrations above all, also ADRs and numbered fixtures. Two items branched from the same trunk both add the next number, and git merges `000112_a.up.sql` and `000112_b.up.sql` without a conflict. Ordering is the worse half. [golang-migrate](https://github.com/golang-migrate/migrate) only moves forward from the database's current version, so a branch that adds `000112` after the trunk gained `000113` merges cleanly, and every database already at 113 skips 112 with no error. Neither shows up in a diff, and a test that runs migrations against an empty database passes both.

`{n}` matches a whole run of digits, compared as an integer, so `000112` and `112` are the same number. The text either side of it is a [`path.Match`](https://pkg.go.dev/path#Match) glob, so `*` stays inside one directory. Declare the file that carries the version: for golang-migrate that is the `.up.sql` side, since the `.down.sql` beside it shares its number by design and would read as a clash. A glob with no `{n}` or more than one, an absolute path, one that leaves the repository or is not in clean form, and one `path.Match` refuses are each refused at load.

`drover drive` checks every declared sequence at the branch update, on the tree it merged the trunk into, before the item's own verify command. It fails when two files in that tree share a number, or when a file the branch added since the merge base carries a number at or below the highest the trunk held at the commit just merged. A rename counts as an add, so a file moved to a new number is judged at the new one. A failure goes back to the branch through `drover request-changes`, as a verify command failure the trunk does not share does, and counts against [`[review] max_review_rounds`](#review). What it sends back names each file, the trunk file it clashes with or sits below, and the number to use: one above the trunk's highest, in the branch's own order and at the width the file already uses. Unlike a failed verify command, the merge is undone and nothing is pushed: the fix is a rename on the branch's own commits, and a pushed merge would be one more commit carrying the old number.

The same check runs on the branch head before `drive` opens a pull request, against origin's trunk, or the local trunk where there is no origin. That is the case the branch update never sees: a branch cut after the trunk already held `000113`, whose branch update merged nothing, still has its `000112` compared. A clash among the trunk's own files, with no file of the branch's in it, is no branch's to fix, so `drive` stops as it does for a trunk that is red on its own. `drover check` reports two tracked files sharing a number as `SEQUENCE CLASH`, which is how a clash a hand merge put on the trunk gets found. `drover drive --dry-run` prints each sequence with the trunk's highest number and the one the next item would have to use.

drover checks and never renumbers. A migration's content can refer to its own version: a down file named to match, a row written into a version table, a test naming the migration it exercises. Renaming the file leaves each of those pointing at the old number, and only the session that wrote the file can say where they are. Reserving numbers up front, as [`[ids] allocator`](#allocation) does for ids, is the wrong tool too: it makes numbers unique, and it cannot make them increase in merge order. The trunk stage is the one place that knows the right number, and `drive` already runs it one item at a time.

## `[agents]`

| Key          | Meaning                                                                                                                                          |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `principles` | The bar a reviewer holds the code to, in one sentence.                                                                                           |
| `addendum`   | A file whose text the generated block carries verbatim, under `## Project addendum`, after everything drover generates. Unset means no addendum. |

`drover agents` reads everything else it needs off the rest of this file. The principles line is the one thing it cannot, so it lives here rather than as a hand-edit in the generated output, where regenerating would lose it. See [Agent instructions](agents.md).

The addendum is for what does not fit in a sentence: a team's conventions, where the project's notes live, a dashboard every agent should keep current. A relative path resolves against the project root. An absolute path, or one starting with `~/`, is allowed, because the file may live outside the repository in a notes vault or a dotfiles repo; any other leading `~` is refused at load, since only `~/` expands. `drover config` prints where the path resolved and whether it read.

The text goes in as written, with no template processing, so a `{{` renders as `{{`. Because it is part of the block, editing the file makes every generated copy stale and every project command refuses until `drover agents --write <file>` runs again, naming the addendum as the reason. A file that cannot be read is not a load error: `drover agents` exits 1 naming the resolved path, `drover check` reports `BAD ADDENDUM`, and the staleness refusal skips the comparison rather than refuse every command over it. An addendum holding a drover marker on a line of its own, or an unclosed code fence, would end the block early, so `drover agents` exits 1 on that too and `drover check` reports it as `BAD ADDENDUM`.

## `[intake]`

| Key            | Meaning                                                                                                                           |
| -------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `stop_marker`  | Text that holds a scaffolded item inert until a human writes its requirements.                                                    |
| `hold_markers` | Texts that say a body is not ready to accept. Default `["OPERATOR DECISION PENDING", "BEFORE ACCEPT"]`; `[]` turns the check off. |

drover refuses to launch or preview an item whose body carries this text on a line of its own, and `status` shows it as `[?]`: `scaffold · needs requirements` while no numbered requirement is written, `drafted · N reqs · drover accept <id>` once the list is there and waits on accept. The match is a whole line outside any fenced block, never a substring: a body quoting the marker in a config sample is somebody's accepted contract, and matching the quote would make `run` refuse to preview it.

`drover accept <id>` removes the line and `drover unaccept <id>` restores it. Both refuse a project that leaves this key unset, because neither can tell a scaffold from a contract there.

`accept` refuses, unless given `--yes`, a body with a line containing one of `hold_markers` outside a fenced block or a code span holding a space, and names the line. The match is a substring, so `OPERATOR STEP BEFORE ACCEPT: measure X` is a hold under the default. `drover check` reports `HOLD MARKER` for one in an accepted, unmerged item. See [What accept checks](lifecycle.md#what-accept-checks).

## `[git]`

| Key            | Default    | Meaning                                                                                             |
| -------------- | ---------- | --------------------------------------------------------------------------------------------------- |
| `enabled`      | `true`     | Whether drover may drive a work tree here. `false` turns branch automation and the merge check off. |
| `merge_check`  | unset      | Command answering whether a branch merged, for a forge that is not GitHub. Unset, `gh` answers.     |
| `merge_method` | `"squash"` | How `drover drive` merges: `merge`, `squash` or `rebase`. Project file only.                        |

Branch automation and the merge check are the only parts of drover that need a repository. The backlog, the ledger, the audit levels, the verify commands and every `--json` surface are files on disk, and a directory that was never a repository drives them unchanged.

With `enabled = false`:

- `run` skips the branch switch and executes in place. Its prompt names no branch, because the prompt is an instruction and naming a branch nothing will create is a wrong one.
- `finish` refuses and exits 3, naming `drover mark-merged` and `drover audit`, the two commands that complete an item where there is no merge to verify.
- `status` omits the `▸ here:` footer. There is no "here" to name.
- `show` reports `created` as unknown, naming this key as the reason. It never shells out to git to find out, so the answer is the same in a tree that has a `.git` and one that does not.

### `[git] merge_check`

```toml
[git]
merge_check = ["sh", "scripts/merged-on-gitlab.sh"]
```

`finish` refuses a branch it cannot see merged. `gh` is the only thing that can see a squash or rebase merge on GitHub, and on GitLab, Gitea or a self-hosted remote it sees nothing at all, which leaves git ancestry, which reports every squash-merged branch unmerged forever. `merge_check` is that forge's authority.

drover runs it from the project root with **the branch and the trunk appended as the last two arguments**, so the example above is invoked as `sh scripts/merged-on-gitlab.sh e10-merge-detection main`. The first element is resolved on `PATH`, the way a `[runners]` binary is; a script living in the repo gets there as an argument to its interpreter, which is why the example names `sh`. Exit 0 means merged. Any non-zero exit means **not merged**, not an error: a check that needs to report a failure writes it to stderr and exits non-zero anyway, and `finish` then falls through to ancestry the same as it would for a no from `gh`. A `merge_check` whose binary is not on `PATH` is a misconfiguration and fails loudly, because answering "not merged" to a typo produces a refusal nobody can explain.

Ancestry is consulted after any no and never on its own. Its yes stands: that is what catches a branch merged by direct push to the trunk, which leaves no pull request and no merge request behind. See [the merge check](lifecycle.md#what-cannot-see-merged-means).

`drover init` writes `enabled = false` in a directory with no work tree, and writes nothing in a directory that has one. That inference happens **once**, into a file you can read and correct. drover never repeats it at run time: probing for `.git` on every command means the tool changes what it does when a directory is copied, and the failure is silent in the direction that matters: a run that quietly skipped the branch switch leaves the work on whatever was already checked out.

Set it back to `true` after `git init`, or write `[git] enabled = true` in a project sitting under a global config that turned it off.

### `[git] merge_method`

```toml
[git]
merge_method = "merge"
```

The flag `drover drive` hands `gh pr merge`: `--merge`, `--squash` or `--rebase`. It applies to both of drive's merges, the one it queues with `--auto` and the one it makes on [`[drive] merge_gate`](#drive)'s word when the forge has no auto-merge (see [When the forge has no auto-merge](lifecycle.md#when-the-forge-has-no-auto-merge)). Unset is `squash`. Any other value is refused at load.

It is a fact about the repository rather than about whoever drives it, so only the project's `.drover.toml` may set it. A value in the per-user config is refused at load, naming the project file it belongs in: one line there would squash every project on the machine, including one whose history depends on merge commits.

`drover config` prints the effective value under `[git]` with where it came from: a live line when this project set it, a commented line naming the per-user file when that file carries it (and refuses to load), and a commented `squash` naming drover's default otherwise. `drover init` writes nothing for it.

`finish` does not read it. Whether a branch merged is answered by the forge or by [`merge_check`](#git-merge_check), whichever way the merge was made.

## `[github]`

`[git]` and `[github]` answer different questions. `[git]` is whether there is a work tree at all; `[github]` is whether there is an issue tracker. A project can have either without the other.

| Key               | Default     | Meaning                                                                                                                         |
| ----------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `repo`            | inferred    | `owner/name`. Set it when the config does not live in the work tree.                                                            |
| `follow_up_label` | `follow-up` | **Membership.** An issue carrying it is a follow-up; the query asks for it server-side, so nothing else in the tracker is read. |
| `severity_prefix` | `severity:` | Label prefix carrying severity as a spelled-out word: `severity:critical`, `severity:high`, `severity:medium`, `severity:low`.  |
| `page_limit`      | `300`       | Issues read in one call. Hitting it is reported, never silent.                                                                  |

A follow-up's owners are links, not labels, so no key configures them. drover reads two places: the issue's GitHub sub-issue parent, when that parent is the `issue:` of a backlog item, and every `Owner: <id>` line in the issue body. A follow-up drover files is created as a sub-issue of the owner's issue when the owning item names one, and carries an `Owner: <id>` line otherwise. drover creates the follow-up and severity labels it applies when the repository lacks them.

Without `repo`, `gh` infers the repository from the project root's git remote. drover always runs `gh` from the project root rather than the caller's working directory, so a subdirectory cannot change the answer.

[`[register] backend = "none"`](#register) is how a project says the answer should be no repository at all. `repo` cannot say it: an empty `repo` is what triggers the inference, so a project nested inside another repository (`examples/` in this tree) reads the enclosing tracker and reports its follow-ups as its own.

With it set, `drover follow-ups` says so and reads nothing, `status` and `unaudited` count no follow-ups, and `drover intake <n>` refuses with exit 3 rather than labeling an issue on somebody else's tracker. That refusal keys on the resolved [`[register] backend`](#register), not on this key, so a project declaring `backend = "file"` gets it too: it has no tracker either. It scopes to the follow-up list alone: `drover finish` still asks `gh` whether a branch merged, which is the one answer git ancestry cannot give under a squash or rebase merge.

A project that wants a follow-up list without a tracker sets [`[register] backend = "file"`](#register) instead.

## `[forge]`

| Key                | Default | Meaning                                                                                                                   |
| ------------------ | ------- | ------------------------------------------------------------------------------------------------------------------------- |
| `infra_patterns`   | `[]`    | Log lines that mark a failed job as the forge's infrastructure failing, added to the built-in list.                       |
| `failure_patterns` | `[]`    | Go regular expressions picking out the lines of a `code` failure's log that say what failed, added to the built-in list.  |
| `reruns`           | `2`     | How many times `drive` reruns one head's infrastructure failures before reading the next as the code's. `0` never reruns. |
| `checks_wait`      | `10m`   | How long a head may show no check run at all before `drive` reports that no workflow ran on it, as a Go duration.         |

`[drive] merge_gate` cannot tell whose failure a red check is, so every one it sees goes back to the item as a send-back. Before the merge gate reads a pushed head, before the pull request opens and again at the merge when the forge has no auto-merge, `drive` waits for the head's checks to finish and classifies each red one (a conclusion of `failure`, `timed_out`, `startup_failure` or `cancelled`):

| Class   | When                                                                                          | drive does                                                                                                                                                  |
| ------- | --------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `infra` | The run was canceled, the job ran no step, or its log carries one of the infrastructure lines | reruns the failed jobs with `gh run rerun --failed` and waits again, under [`rerun_infra_failure`](#supervise)                                              |
| `trunk` | Not `infra`, and the trunk's latest completed run of the same check concluded failure         | parks the item `trunk_red`, naming the check and that run's URL, without sending anything back, and stops the drive, under [`report_trunk_red`](#supervise) |
| `code`  | Anything else                                                                                 | lets the merge gate decide                                                                                                                                  |

The built-in infrastructure lines, matched case-insensitively as substrings of the failed job's log: `toomanyrequests`, `rate limit exceeded`, `429 Too Many Requests`, `No space left on device`, `The runner has received a shutdown signal`, `lost communication with the server`, `Could not resolve host`, `Temporary failure in name resolution`, `connection reset by peer` and `TLS handshake timeout`. A govulncheck finding is `code` whether it is in a module or in the standard library. A rerun does not fix a standard-library one: `actions/setup-go` takes whatever patch release the runner image has cached, so the rerun lands on the same toolchain. The fix is a `toolchain` line in `go.mod` naming the `Fixed in` release or a newer one, and the body sent back says so beside govulncheck's `Found in` and `Fixed in` lines. A check another app reported, rather than an Actions job, has no log drive reads, so only its conclusion and the trunk decide it, and an `infra` one there has no run to rerun and is read as `code`.

For a `code` check, `drive` also reads what failed out of the same job log: up to 5 lines matching one of the failure patterns, each with its Actions timestamp, its `##[error]` annotation and its terminal colors stripped. The built-in patterns, matched as Go regular expressions against each stripped line:

| Pattern                         | Matches                                               |
| ------------------------------- | ----------------------------------------------------- |
| `^--- FAIL`                     | A failed Go test                                      |
| `^FAIL\b`                       | A failed Go test package                              |
| `^\S+:\d+:\d+: .*\(\w[\w-]*\)$` | A golangci-lint finding, its linter named last        |
| `^\S+\.\w+:\d+:\d+: `           | A compiler or `go vet` error                          |
| `^Vulnerability #\d+`           | A govulncheck finding                                 |
| `^\s*Found in: `                | The module or package version govulncheck found it in |
| `^\s*Fixed in: `                | The version that fixes it                             |
| `^panic: `                      | A Go panic                                            |

A log matching none of them falls back to the last 5 non-blank lines before its first `make: ***` or `Error:` line. `failure_patterns` adds to the list, so a project whose test runner prints `ERROR <name>` adds `'^ERROR '`; a pattern that does not compile is refused at load. Those lines are what the [`gate_refused`](automation.md#drive---json) park carries when the merge gate then blames the diff: its reason names the check and the first line, its `detail` is `code`, its `evidence` is the lines, and the body sent back to the item adds them and the check's URL under the merge gate's output.

`reruns` counts `gh run rerun` calls on one head across drives, from the ledger, so a later drive does not rerun a head the earlier one already spent. A head that needs more reruns than remain is read as `code`, and the merge gate sends it back. Beside a `code` failure, an `infra` one is recorded and not rerun, since the head is going back either way.

The trunk's latest completed run is found by walking back from the trunk's tip along its first parents, up to 10 commits, and stopping at the first commit with a completed run of the check, passed or failed. Under `drive` the tip has often just moved, so its own run is still in progress or, on a path-filtered workflow, never started. A check no commit in that window completed, or a commit drive cannot read before reaching one, does not make the check `trunk`.

The policies decide who acts. `rerun_infra_failure` at `off` reads every `infra` check as `code`; at `operator` or `supervisor`, `drive` parks the item `infra_failure` with each check and its log line rather than rerun it. `report_trunk_red` at `off` reads a `trunk` check as `code`; at any other policy the item parks `trunk_red` and the drive stops, and at `operator`, its default, the park also emits `needs_operator`.

A head with no check run at all after `checks_wait` is not red. `drive` reports `no workflow ran on this head`, records it, and lets the merge gate decide; a merge gate refusal that follows opens its reason with the same words, and so does the send-back when the merge gate blames the diff, because the fix is CI that fires on a branch push rather than a check that failed.

Each classification is a ledger row (`{"id","head","check","class","evidence","rerun","date"}`, with `rerun` on a check drive reran) and a `check` event under [`drive --events`](automation.md#drive---events). Whatever drive cannot read (the check runs, a job, its log, the trunk's run) leaves the merge gate to answer, as it did before any of this existed.

## `[usage]`

| Key                 | Default  | Meaning                                                                                                              |
| ------------------- | -------- | -------------------------------------------------------------------------------------------------------------------- |
| `weekly_budget`     | unset    | Your estimate of the weekly limit in weighted tokens, a positive integer. Needs `reset`.                             |
| `reset`             | unset    | When the weekly limit starts over: a weekday, a 24-hour time and an IANA zone, as `"Sat 20:00 America/Los_Angeles"`. |
| `session_max_hours` | `24`     | How many hours a session's usage rows may span before `status` lists it under `long_sessions`.                       |
| `context_warn`      | `400000` | The largest context, in tokens, a session may reach before `status` lists it under `long_sessions`.                  |

`status --json` and the built-in templates read the ledger's usage rows into a report-level [`usage`](automation.md#usage) object, which covers the last 7 days unless `reset` is set. With `reset`, the window runs from the most recent reset to now, so the total measures the same week the limit does. The weekday takes its three-letter or full English name in any case, and the time is computed on the zone's own calendar, so a 20:00 reset stays at 20:00 across a daylight-saving change.

With `weekly_budget` as well, the report gains `budget` and `budget_used`, and both built-in templates show the fraction in their footer, in the warning color from 0.8 and the critical color from 1.0. The budget is your estimate of the limit in drover's weighting (`input + output + cache_create + 0.1 * cache_read`), not a figure the service publishes: the counts are tokens as the API reported them, not the subscription's quota accounting, so calibrate it against a week whose limit you hit. A good first value is the weighted total `status --json` shows for the week the limit last refused a run. `weekly_budget` without `reset` is refused at load, because a budget measured over a rolling window would never line up with the week the limit counts.

The limit is per account, so `budget_used` sums every project in the [project registry](#the-project-registry), not only the one `status` runs in, counting each ledger once. A registered project whose config or ledger would not read is named in `budget_unread` and left out of the fraction, rather than counted as zero; a registered root that is gone is not a project and is skipped, as `status --all-projects` skips it. Set the table in the per-user file, where one budget serves every project, unless a project wants its own thresholds.

`session_max_hours` and `context_warn` decide which sessions `status` names under [`long_sessions`](automation.md#usage), and both templates list them under "Needs you", under Long sessions, with the reason each one qualified and a state: `ended` for a session whose every usage row is `headless`, and otherwise `last seen` with how long ago its latest row places it. Neither says "open", because the ledger has no row for a session still running: a headless row is written when it exits and an interactive row when its day ends. Live work is under Running drive. A session open for days at a context near a million tokens is the opposite of the short, well-defined unit of work drover hands a run.

### `[usage.weekly]` and `[usage.five_hour]`

`weekly_budget` is an estimate in drover's own weighting. These two tables act on the figure the service itself reports. Claude Code writes a `rate_limit_event` into every headless log, and drover keeps the last one of each session on its usage row as [`rate_limit`](automation.md#writing-to-the-ledger-without-the-binary). Each window's level is the **percent of the window left**, `100 - utilization * 100`, as the service reports it in steps of 1%. It is not tokens and not dollars.

| Key                | Type               | Range   | Default | Meaning                                                                                                                                                                                                                                                                                    |
| ------------------ | ------------------ | ------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `notify_remaining` | integer or `"off"` | 0 to 99 | unset   | The notify level. At or below it, `drive` emits one [`usage_warning`](automation.md#drive---events) per window per reset and sends it to each [`[notify]`](#notify) destination routing `usage_warning`. It does not pause or slow the drive. Unset or `"off"` sends none for this window. |
| `pause_remaining`  | integer or `"off"` | 0 to 99 | unset   | The fail-safe level. At or below it, [`usage_admission`](#supervise) holds the drive: running sessions finish their stage, nothing new starts, and the drive pauses until the window resets or `drover drive --resume`. Unset or `"off"` holds nothing on this window.                     |

`[usage.weekly]` is the seven-day window and `[usage.five_hour]` the five-hour one. When both keys of a table are set, `pause_remaining` must be below `notify_remaining`, so the notice comes before the hold; load refuses the pair otherwise, naming both values. `0` is a level, not unset: `pause_remaining = 0` holds once the window reads 100% used.

`"off"` is unset, written down. Every file is decoded over the one before it and the last file to write a key wins, so a project cannot leave unset a level the per-user file sets: any number it writes is a level of its own, `0` included. `"off"` is how a project removes the inherited level for itself alone. It reads exactly as a key nobody wrote: no warning, no hold, and no pair check, so `pause_remaining = "off"` beside `notify_remaining = 20` loads. A project's number written over a per-user `"off"` is a level again. Any other string in one of these keys is refused at load naming the file, the key and the two accepted forms; `"off"` in a key that does not take it is refused as any wrong value there is, and `[usage.overage] mode = "off"` names `pause` and `continue`. `drover config` prints each level switched off with the file that wrote it and, where that file is the project's, the per-user value it replaced, and `drover drive --dry-run` names the same on its `usage_admission` line, as `weekly pause_remaining off in .drover.toml (the per-user file sets 5)`. [Running one project on credit](#running-one-project-on-credit) is the case it exists for.

Each warning is recorded as a ledger row naming the window and its reset, and drover reads every registered project's ledger for one before sending, so a restarted drive, a drive in another project on the same account, and a reading that dips and rises again send nothing twice. The newest reading among every registered project's usage rows is the one judged, because the limit is the account's. A reading taken before its window's reset counts as 100% left once the reset passes. A runner that writes no such event (codex, agy) leaves no reading, and nothing here acts for it.

### `[usage.overage]`

An account runs out twice, in sequence: first the weekly allotment, then any extra-usage credit bought to work past it. The credit has no remaining figure. Once the weekly window reads 100%, the reading says only whether overage is in use (`is_using_overage`) and, at the end, `out_of_credits`. drover sees the percent of each window and never a credit balance.

| Key          | Type              | Range                    | Default   | Meaning                                                                                                                                                                                                                                  |
| ------------ | ----------------- | ------------------------ | --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `mode`       | string            | `pause`, `continue`      | `"pause"` | `pause` holds the drive as soon as a reading shows the weekly window rejected with overage in use, as `pause_remaining` would. `continue` keeps dispatching on purchased credit, bounded by `max_usd` when set.                          |
| `max_usd`    | number or `"off"` | above 0                  | unset     | Under `continue`, the summed `cost_usd` of usage rows whose reading was on overage since the weekly window began, across every project in the per-user registry, at which `usage_admission` holds the drive. Unset or `"off"` is no cap. |
| `notify_usd` | number or `"off"` | above 0, below `max_usd` | unset     | Under `continue`, the same sum at which `drive` emits one `usage_warning` on the way to `max_usd`. Unset or `"off"` sends none.                                                                                                          |

Entering overage emits a `usage_warning` with window `overage` once per weekly reset whenever `[usage.weekly] notify_remaining` is set, under either mode. Both dollar sums are the runners' **list price**, the `cost_usd` each session reported, not the credit the account was charged: treat `max_usd` as an approximate bound, set below what you bought. Under `continue`, a weekly `pause_remaining` still holds the drive before the allotment runs out; leave it unset to run straight onto purchased credit, or `drover drive --resume` past the hold. A project inheriting the level from the per-user file leaves it unset by writing `pause_remaining = "off"` under its own `[usage.weekly]`. `"off"` on `max_usd` or `notify_usd` does the same for a per-user dollar cap or notice, and the check that `notify_usd` sits below `max_usd` reads it as unset too. `mode` takes no `"off"`: unset is already `pause`, so write the mode you want.

One reading can cross several holds: under `mode = "pause"`, a week at 0% left with overage in use crosses the weekly `pause_remaining` and the overage hold together. Each `drover drive --resume` releases one, in the order `usage_admission` takes them (weekly, five-hour, overage or `max_usd`, then a refusal), so resuming past the weekly percent never consents to purchased credit. The pause line on stderr names every hold standing behind the one taken, the `paused` and `needs_operator` events carry them all in `holds`, the resume prints which hold it released and which still stand, and `drover drives` lists the standing ones beside `paused`.

A session the service refused (the weekly window rejected with no overage, or `out_of_credits`) holds the drive the same way at any of these settings, rather than parking the item `runner_fault`: the item goes back to the head of the queue at the stage that was refused. [Running past the weekly allotment](operating.md#running-past-the-weekly-allotment) covers choosing the levels.

### Worked example

```toml
# ~/.config/drover/config.toml: one account, every project.
[usage.weekly]
notify_remaining = 20   # tell me at 20% of the week left
pause_remaining  = 5    # stop starting sessions at 5%

[usage.five_hour]
pause_remaining = 2

[usage.overage]
mode = "pause"          # the default: never spend purchased credit unattended
```

With this file, a drive whose newest reading says the week has 18% left sends one `usage_warning` and keeps going. At 5% it pauses with `needs_operator`; under `drive --for 48h`, a reset 30 hours away lifts the hold on its own at the reset, and under an unbounded drive it waits for `drover drive --resume`.

### Running one project on credit

The per-user file above pauses every project at 5% of the week left. One project is meant to keep running on purchased credit, so it switches the pause off for itself and sets the mode:

```toml
# .drover.toml of the project that runs on credit
[usage.weekly]
pause_remaining = "off"   # the per-user 5 holds every other project, not this one

[usage.overage]
mode    = "continue"
max_usd = 40.0            # the bound on purchased credit
```

At 0% of the week left this project's drive dispatches, with only the inherited five-hour pause left to hold it, and every other registered project holds at 5% as before. Without the `"off"` line the project inherits `pause_remaining = 5` and holds every week until `drover drive --resume`, whatever `mode` says, and removing the line from the per-user file instead removes it from every project. The per-user `notify_remaining = 20` still reaches this project, so it still sends the 20% warning and the warning on entering overage. Its `drover drive --dry-run` ends the `usage_admission` line with:

```text
weekly pause_remaining off in .drover.toml (the per-user file sets 5)
```

## `[register]`

| Key       | Default  | Meaning                                                     |
| --------- | -------- | ----------------------------------------------------------- |
| `backend` | `github` | Where the follow-up list lives: `github`, `file` or `none`. |

drover reads the follow-up list from one place, and everything above that read (owners, severity ranking, `unowned`, `orphaned`, the counts in `status` and `unaudited`, the issues a prompt inherits, the `review` prompt) reads the same index. So this selects a source rather than adding a second follow-up list with its own vocabulary to drift from.

| Backend  | Follow-ups                                 | Still works                                                                                                                                                                                    |
| -------- | ------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `github` | Open issues, through `gh`.                 | Everything, `drover intake <n>` included.                                                                                                                                                      |
| `file`   | The JSONL at [`[paths] register`](#paths). | `follow-ups`, the counts in `status` and `unaudited`, the rows a prompt inherits, and `drover follow-up` to write one. `intake` refuses: it reads an issue tracker, and this project has none. |
| `none`   | Nothing.                                   | Everything local. `follow-ups` says there is no follow-up list, `status` and `unaudited` count none, and `intake` and `follow-up` refuse with exit 3.                                          |

An absent `backend` is `github`, which is what nearly every project wants and the only backend needing no declaration. Two settings that disagree are refused at load: `backend = "file"` with [`[github] repo`](#github) set is one author saying two things, and picking one silently is how a project reads a follow-up list it thought it had configured somewhere else.

### The file register

One JSON object per line, appended, never rewritten. It matches the ledger's discipline for the same reasons: it lives in a git-tracked documentation tree whose purpose is auditable history, closing a row must not delete the line that opened it, and an agent with no binary and no network can record one by appending a line.

```json
{"number":1,"title":"contract checks assert format","severity":"M","owners":["4"],"state":"open","date":"2026-08-27"}
{"number":2,"title":"resolver drops the trailing dot","severity":"H","owners":["4"],"state":"open","date":"2026-08-27"}
{"number":1,"title":"contract checks assert format","severity":"M","owners":["4"],"state":"closed","date":"2026-08-28"}
```

| Field      | Meaning                                                                                                                                                                                                                                              |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `number`   | Keys the fold. Allocated one past the highest the file has ever used, closed rows included.                                                                                                                                                          |
| `title`    | One line, the same thing a tracker issue's title carries.                                                                                                                                                                                            |
| `severity` | `C`, `H`, `M` or `L`. Empty ranks last and still counts as open.                                                                                                                                                                                     |
| `owners`   | Bare ids that own this row. `drover follow-up` refuses an id no body answers to, since a row naming one prints as owned and reaches no run. Empty is an unowned row: it records provenance and reaches no run, and says so on every line it appears. |
| `state`    | `open` or `closed`. Anything else fails the read.                                                                                                                                                                                                    |
| `date`     | When the event was recorded.                                                                                                                                                                                                                         |

The read folds newest-wins per `number` and drops anything whose latest state is `closed`, so the third line above closes the first and #1 leaves the index while its history stays. A missing or empty file is an empty follow-up list rather than an unreadable one ("nobody filed one" and "nobody could ask" rank the same item very differently), and a line that does not parse fails the whole read, naming the file and the line number. Answering with the rows that happened to parse would report a shorter list as a whole one.

Nothing here paginates, so the `page_limit` notice cannot fire against a file: a file holding `page_limit` rows was read whole.

`drover follow-up` is the write path, and it exists only under this backend:

```bash
drover follow-up open "contract checks assert format" --owner 4 --sev M
drover follow-up open "resolver drops the trailing dot" --owner 4 --sev H
drover follow-up close 1
```

Under `github` and `none` it refuses with exit 3, naming the backend that is configured. Editing the file by hand is equally valid: drover only ever reads it.

## `[backlog]`

| Key      | Default | Meaning                                                 |
| -------- | ------- | ------------------------------------------------------- |
| `mirror` | `none`  | Where accepted items are published: `none` or `github`. |

Under `github`, `accept` opens an issue per accepted item carrying its title and numbered requirements, writes the number into the item's `issue:` key, and keeps a `drover:<state>` label, an open or closed state and blocked-by links in step with the backlog. The file stays the contract and drover never reads the issue back. [The mirror](concepts.md#the-mirror) covers what each command writes and how `drover check` reports a write that did not go through.

Publishing makes item text readable to everyone who can read the repository, so the default is `none` and a project whose items must stay private keeps it there. Two settings are refused at load. The per-user config may not set the key, since one value there would publish every project on the machine. And `github` needs `issue` declared in [`[frontmatter]`](#frontmatter): an item that cannot record its issue number would be published again by every `accept`.

## `[supervise]`

A drive runs unattended, and what it parks still needs somebody: a conflict after a sibling merged, a scaffold a review filed, a review session that died. `[supervise]` says, per action, who that is.

| Key          | Default             | Meaning                                                                                                                                                                                                                  |
| ------------ | ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `template`   | unset               | The template [`drover supervise`](automation.md#the-supervisor) renders its prompt from, resolved like [`[prompts]`](#prompts) `implement`. Unset uses the built-in one; `drover supervise --dry-run` prints the result. |
| _action key_ | the catalog default | The policy for one action below.                                                                                                                                                                                         |

`[supervise]` takes neither a `notify` argv nor an `ntfy` table: [`[notify]`](#notify) destinations do that work, and a config carrying either is refused at load with an error naming the key and `scripts/migrate-notify.sh`, which rewrites it in place.

Every other key is an action from the catalog, set to a policy:

| Policy       | Means                                                                                                                        |
| ------------ | ---------------------------------------------------------------------------------------------------------------------------- |
| `auto`       | Drive does it.                                                                                                               |
| `supervisor` | A session `drover supervise` started may do it, and reports it.                                                              |
| `operator`   | drover stops, emits `needs_operator`, sends it to each [`[notify]`](#notify) destination routing it, and waits for a person. |
| `off`        | Nothing happens. Today's park stands.                                                                                        |

The policies are ordered `off` < `operator` < `supervisor` < `auto`, and each action has a ceiling: a policy above it is a load error naming the key, the policy requested and the ceiling. An omitted key takes its default, and an unknown key is a load error. Each command listed beside an action is that action when a supervisor runs it, and refuses there with exit `3` unless the action is at `supervisor` or `auto`.

| Action                        | Category     | Default      | Ceiling      | Command                        | What it is                                                                                                      |
| ----------------------------- | ------------ | ------------ | ------------ | ------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `update_open_prs`             | merge        | `auto`       | `auto`       |                                | After a merge, merge the trunk into every other open drive pull request, run its verify command, and push.      |
| `retry_moved_trunk`           | merge        | `auto`       | `auto`       |                                | Retry, once, a merge gate that refused only because the trunk moved under the head.                             |
| `resolve_conflict`            | merge        | `supervisor` | `auto`       |                                | Resolve a branch update merge conflict in the item's worktree.                                                  |
| `merge_drive_pr`              | merge        | `operator`   | `supervisor` | `finish`                       | Merge a pull request drive opened for an item, then finish it.                                                  |
| `merge_protected_paths`       | merge        | `operator`   | `supervisor` |                                | Merge a pull request that changes a [drive] protected_paths file.                                               |
| `rerun_infra_failure`         | checks       | `auto`       | `auto`       |                                | Rerun a check that failed on the forge's infrastructure rather than on the code.                                |
| `report_trunk_red`            | checks       | `operator`   | `auto`       |                                | Report a trunk that fails the verify command on its own, and stop dispatching.                                  |
| `post_finish_check`           | checks       | `off`        | `auto`       |                                | Run a live check after each finish.                                                                             |
| `rerun_lost_review`           | sessions     | `auto`       | `auto`       |                                | Rerun, once, a review pass whose session died or was cut.                                                       |
| `relaunch_after_runner_death` | sessions     | `supervisor` | `auto`       |                                | Relaunch an item whose implementing session died.                                                               |
| `auth_expired`                | sessions     | `operator`   | `operator`   |                                | Stop when a runner's credentials expired.                                                                       |
| `push_refused_by_hook`        | sessions     | `operator`   | `operator`   |                                | Stop when a pre-push hook refused the item's branch.                                                            |
| `write_scaffold_requirements` | requirements | `supervisor` | `supervisor` |                                | Write the numbered requirements of a scaffold a review filed.                                                   |
| `triage_finding`              | requirements | `operator`   | `supervisor` |                                | Find an owner for a follow-up a drive's session filed that nothing is scheduled to carry.                       |
| `close_changes`               | requirements | `supervisor` | `supervisor` | `close-changes`                | Close a sent-back section whose findings a later pass closed.                                                   |
| `accept`                      | requirements | `operator`   | `operator`   | `accept`                       | Accept an item's requirements.                                                                                  |
| `override_review_limit`       | requirements | `operator`   | `operator`   | `request-changes --over-limit` | Send an item back past [review] max_review_rounds.                                                              |
| `move_requirement`            | requirements | `operator`   | `operator`   |                                | Move a numbered requirement from one item to another.                                                           |
| `signoff`                     | requirements | `operator`   | `operator`   |                                | Meet a requirement only a person can meet.                                                                      |
| `enqueue`                     | queue        | `supervisor` | `auto`       | `drive`                        | Queue the next drive.                                                                                           |
| `pause_for_config_change`     | queue        | `operator`   | `supervisor` |                                | Pause a running drive to take a config change.                                                                  |
| `cleanup_interrupted_drive`   | queue        | `supervisor` | `auto`       |                                | Remove the worktrees and branches an interrupted drive left behind.                                             |
| `host_load_admission`         | queue        | `off`        | `auto`       |                                | Hold dispatch while the host is loaded.                                                                         |
| `usage_admission`             | queue        | `auto`       | `auto`       |                                | Hold dispatch at a `[usage]` pause level, on purchased credit, and on a quota refusal, and resume at the reset. |
| `drive_digest`                | queue        | `auto`       | `auto`       |                                | Summarize each drive when it exits.                                                                             |
| `stale_citations`             | preflight    | `auto`       | `auto`       |                                | Report cited lines that no longer exist on the trunk before launching.                                          |
| `worktree_links_gap`          | preflight    | `auto`       | `auto`       |                                | Report files the verify command reads that the worktree lacks.                                                  |
| `deps_lint`                   | preflight    | `auto`       | `auto`       |                                | Refuse a malformed deps line.                                                                                   |

A drive judges every finding its sessions file when it notices it, after each item's result: each follow-up that appeared while the item ran, and each scaffold the backlog gained. Its coverage says whether anything will carry it:

| Coverage   | Means                                                                                                                                             |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `queued`   | An owner is in this drive's queue, waiting or running.                                                                                            |
| `unqueued` | An owner is ready or ran, and is not queued.                                                                                                      |
| `blocked`  | Every owner that has not merged is blocked; `blocked_on` names what holds each: its unmerged deps, `scaffold`, `broken`, or `not in the backlog`. |
| `orphaned` | Every owner has merged, as `drover follow-ups` counts orphaned.                                                                                   |
| `unowned`  | Neither a sub-issue parent nor an `Owner:` line names an owner.                                                                                   |
| `scaffold` | A scaffold whose requirements are unwritten.                                                                                                      |

A follow-up at `blocked`, `orphaned` or `unowned` goes through `triage_finding`, naming the row, the item that filed it and its coverage: `needs_operator` at `operator`, `needs_supervisor` at `supervisor`, and nothing at `off`. A scaffold stops under `write_scaffold_requirements` alone. Nothing stores coverage: `status --json` (`findings_needing_triage`) and `drover follow-ups --coverage` judge every row again from the ledger, the follow-up list and the backlog on each read.

The shipped defaults keep every merge with a person. `merge_drive_pr` at `supervisor` lets a `drover supervise` session merge a pull request drive opened for an item, with `[git] merge_method`, and then run `drover finish <id>`, when the ledger names the pull request on a row drive wrote, the item's latest review pass is `passed` and came from a session drover launched, every check on the head is green, and the forge reports it mergeable. `finish` under a supervisor checks the two ledger conditions and refuses with exit `3`, naming the one that failed; the forge conditions are the session's to read before it merges. `finish --force` is refused to a supervisor at any policy. `merge_protected_paths` at `supervisor` extends that to a pull request touching a `[drive] protected_paths` file, once the session has read the diff of each denied file and named each in its report. At any other policy, `finish` under a supervisor also refuses with exit `3` when the ledger holds a `protected_paths` park for the item newer than its latest drive pull request row, so the operator merges and finishes it. [`drover agents --write <file> --permissive-merge`](agents.md#the-merge-policy) sets both to `supervisor` and renders the agent instructions under that policy, and `--restrictive-merge` sets both back to `operator`.

Both keys work in the per-user config like every other `[supervise]` key: one `merge_drive_pr = "supervisor"` line there reaches every project that sets none, and a project restricts it again with its own `merge_drive_pr = "operator"`.

`signoff` is what drive's merge hold on an `[operator]` requirement with no signoff reports, and `post_finish_check` decides whether [`[finish] post_check`](#finish) runs at all: `off` skips it, and any other policy runs it after every finish.

`update_open_prs`, `retry_moved_trunk` and `resolve_conflict` change what drive does: see [Updating the other open pull requests](lifecycle.md#updating-the-other-open-pull-requests). `resolve_conflict` also decides the branch update drive makes before each implement session: at `auto` a conflict there goes to that session to resolve, at `supervisor` and `operator` it parks the item for whoever the hand-off names, and at `off` drive makes no branch update at dispatch at all. See [Updating the branch at dispatch](lifecycle.md#updating-the-branch-at-dispatch). For the rest, a policy decides who is told about the park the action owns and who may run the command beside it. The parks each action owns are listed under [`drive --events`](automation.md#drive---events).

The five preflight and admission keys do change what drive does. Each check emits a `preflight` event, and `drive --dry-run` prints a preflight section naming what each found:

| Key                   | What drive checks                                                                                                                                                                                                                                                                                                                                                                                                                                                            | At `off`                                           | At `operator`                                                              | Otherwise                                                                                                                                                                                                                                                                                                                                                          |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `stale_citations`     | Each cited `path:line` or `path:start-end` in a queued item's body, against `origin/<trunk>` (the local trunk where origin has none), as the item is dispatched, and not again for an item a drive's re-exec carried over, which passed it when it was first dispatched: a file the trunk lacks, or a range past the file's last line. A citation with no slash (`go.mod:3`) is held to its range only where the trunk has the file, so `db.internal:5432` is never reported | not checked                                        | the item parks `stale_citations` before any session, with `needs_operator` | printed under the item, which launches anyway                                                                                                                                                                                                                                                                                                                      |
| `deps_lint`           | A `deps:` value holding a comma, a YAML list bracket or quote, or an id that does not parse                                                                                                                                                                                                                                                                                                                                                                                  | not checked                                        | refused                                                                    | `accept`, `run` and `drive` exit `3` naming the file, the line and the corrected form; `drover check` reports any value that is not space-separated bare ids as `BAD DEPS` with the same corrected form, and an id with no body as `MISSING DEP`, either way                                                                                                       |
| `worktree_links_gap`  | Files the item's verify command reads that git does not track in your checkout and no `worktree_links` entry carries: the untracked and ignored files under each path the command line names, or under `drive --dry-run` on Linux with `strace` on `PATH`, what the command opened when run once in your checkout                                                                                                                                                            | not checked                                        | warned                                                                     | a warning before the first verify command run, naming the path and the `worktree_links` line that would carry it                                                                                                                                                                                                                                                   |
| `host_load_admission` | The 1-minute load average before each dispatch, against `[drive] max_load`                                                                                                                                                                                                                                                                                                                                                                                                   | not read                                           | not read                                                                   | at `auto`, dispatch holds while the load is above it, printing once per hold; a load that cannot be read never holds                                                                                                                                                                                                                                               |
| `usage_admission`     | The newest `rate_limit` reading among every registered project's usage rows, before each dispatch and before each stage that launches a session, against [`[usage.weekly]`, `[usage.five_hour]` and `[usage.overage]`](#usageweekly-and-usagefive_hour)                                                                                                                                                                                                                      | not acted on; a quota refusal parks `runner_fault` | the drive holds and waits for `drover drive --resume`                      | the drive holds and pauses, emitting `paused` and `needs_operator` with `run` `drover drive --resume`; a hold whose reset falls inside `--for` or `--until` lifts on its own at the reset plus a minute, emitting `resumed`; a deadline that arrives first ends the drive with exit 0 and each held item's outcome `held`. `usage_warning` is sent at every policy |

The path scan reads the command line alone, so a file a verify command reaches only through a `Makefile` or a script is not found that way; the traced run finds it and costs one run of the verify command.

`drover config` prints every action's effective policy after the file, marked `default` where no file sets it. `drover init supervise` walks the catalog by category and offers each action the policies up to its ceiling; drover never rewrites `.drover.toml`, so it appends a `[supervise]` table when the file has none and prints one to paste over it when the file does. The printed table carries the existing `template` and any key this build does not know as written, and the walk offers each action the policy the project's own file sets, so pressing return keeps it. `--defaults` resets the policies and still carries those lines. Values the per-user file supplies are never copied in. `drover agents` renders the effective table into the generated instructions.

```toml
[supervise]
resolve_conflict  = "operator"   # nobody but a person touches a conflict here
post_finish_check = "auto"
```

## `[notify]`

A drive runs unattended, so what it does has to reach somebody who is not watching the terminal. `[notify]` names the destinations its events go to, and each destination chooses which events it takes and how loudly. The table may sit in `.drover.toml` or in the per-user config: a per-user destination serves every project, a project destination of the same name wins over it key by key, and destinations of different names both apply.

| Key            | Type     | Default | Meaning                                                                                                                                                                                                                                                                                                                                                                  |
| -------------- | -------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `coalesce`     | duration | `"0s"`  | Events of one kind for one destination that arrive within this window go out as one message listing their ids, such as `filed: B91 B91 B92`. `0s` is off. `needs_operator`, `needs_supervisor` and `exit` are never coalesced or delayed.                                                                                                                                |
| `quiet_hours`  | string   | unset   | `"HH:MM-HH:MM"` in local time, which may cross midnight. Messages routed below `high` are held until the window ends, then sent as one message per destination. `needs_operator`, `needs_supervisor` and `exit` are never held. A drive that exits inside the window folds what it held into its exit message, sent at `min` unless `exit` is routed at `high` or above. |
| `destinations` | table    | none    | One `[notify.destinations.<name>]` table per destination, keyed below. A drive with none sends nothing.                                                                                                                                                                                                                                                                  |

Each destination takes:

| Key          | Type     | Applies to | Default  | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| ------------ | -------- | ---------- | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `type`       | string   | both       | required | `ntfy` POSTs each routed event to an [ntfy](https://ntfy.sh) topic; `command` runs an argv with each routed event on stdin.                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `events`     | table    | both       | required | An inline table mapping each event the destination takes to an ntfy priority: `min`, `low`, `default`, `high` or `urgent`. An event the table does not name is not sent there. Any event a drive emits may be named, and `exit_undone`, described below.                                                                                                                                                                                                                                                                                        |
| `url`        | string   | `ntfy`     | required | The topic URL, such as `https://ntfy.sh/<topic>`. It must be `https://` unless the host is a loopback address, must name a topic in its path, and may not carry credentials.                                                                                                                                                                                                                                                                                                                                                                    |
| `token_file` | string   | `ntfy`     | unset    | A file holding the access token, sent as `Authorization: Bearer <token>`. It must start with `/` or `~/`, and is read at every send, so a rotated token takes effect on the next message. Unset sends no token.                                                                                                                                                                                                                                                                                                                                 |
| `argv`       | array    | `command`  | required | The command, run from the project root with 30 seconds to finish. It reads the event as one JSON line on stdin with `priority`, `message` (the body an ntfy destination would send) and, for a coalesced or held message, `folded` added, `reminder` on a reminder, and on a `needs_operator` or `needs_supervisor` for an item this drive already opened a T3 Code thread for, that thread's `thread` and `url`. A `thread_opened` line it prints is recorded in the drive's event file. See [`drive --events`](automation.md#drive---events). |
| `repeat`     | duration | both       | unset    | How often an unanswered `needs_operator` this destination took is sent again while the drive runs, at least `5m`. Unset sends each one once. See [Reminders](#reminders).                                                                                                                                                                                                                                                                                                                                                                       |
| `repeat_max` | integer  | both       | `6`      | The most reminders one `needs_operator` gets here. Read only where `repeat` is set.                                                                                                                                                                                                                                                                                                                                                                                                                                                             |

The events a drive emits are `started`, `queued`, `dispatched`, `skipped`, `preflight`, `stage`, `check`, `parked`, `filed`, `needs_operator`, `needs_supervisor`, `merged`, `paused`, `resumed`, `reexec_waiting`, `config_changed`, `usage_warning`, `thread_opened` and `exit`, each described under [`drive --events`](automation.md#drive---events). `exit_undone` is a routing key rather than an event: an exit that leaves any item parked, skipped or held is sent at `exit_undone`'s priority instead of `exit`'s, and it defaults to `high` wherever `exit` is routed, so a drive that exited 0 with work left behind reaches the phone louder than one that finished everything.

An unknown key, type, event name or priority, a malformed URL, a destination with no `type`, `events`, or the `url` or `argv` its type needs, a `coalesce` that is not a duration, a `quiet_hours` that is not two `HH:MM` times, a `repeat` under `5m` and a `repeat_max` that is not a positive integer are load errors naming the destination, the key and the file that wrote it.

### Sending

Sends to different destinations do not wait on each other or on the drive: each destination has a queue of its own, an ntfy send is bounded at 10 seconds and a command at 30. A send that fails, whether a non-2xx status, a timeout, an unreadable token file or a command exiting non-zero, is reported once per destination per drive on stderr, with the status and the body ntfy returned or the command's last lines, and never parks or stops the drive or the other destinations. A destination more than 256 messages behind drops the rest and says so once.

An ntfy message carries `Title: drover: <project> <id>` (`drover: <project>` for an event with no item, and for a coalesced or held message), the routed `Priority`, `Tags: robot`, and a body written for its event:

| Event                                | Body                                                                                                                                                                                                                                                                      |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `started`                            | The drive id, the queue in order, `--jobs` above 1, and the deadline or `no deadline`.                                                                                                                                                                                    |
| `merged`                             | The pull request's number and title.                                                                                                                                                                                                                                      |
| `parked`                             | The park, its detail and reason, then up to 5 lines of the park's evidence, which for a `gate_refused` park are the lines its check's log says failed.                                                                                                                    |
| `needs_operator`, `needs_supervisor` | `<action>: <run>`, the action and the command to run. A `usage_admission` `needs_operator` carries `holds`, each usage hold the reading crosses with its `window`, `reason` and `resets_at`, the one taken first; with two or more, `run` says what each resume releases. |
| `usage_warning`                      | The window, the percent of it left and its reset in local time.                                                                                                                                                                                                           |
| `thread_opened`                      | `T3 Code thread opened: <url>`.                                                                                                                                                                                                                                           |
| `exit`                               | The summary below.                                                                                                                                                                                                                                                        |
| any other                            | A line naming what happened: the stage, the check, the finding, the position.                                                                                                                                                                                             |

Where the event concerns a pull request drover recorded, the message sets ntfy's `Click` header to that pull request's URL, so tapping it opens the pull request. The repository address comes from one `gh repo view` per drive, the first time a message needs it. Where it names no pull request (or the address would not resolve) and a command destination running [`drover t3 thread --from-event`](operating.md#operator-sessions) opened a T3 Code thread for the item during this drive, `Click` is that thread's `t3-thread://` link instead, so a reminder, the item's `parked` or its `merged` opens the thread that answers it. The first `needs_operator` itself goes out before the thread exists and carries no thread link.

The exit message says what the drive left undone, within ntfy's 4096-byte limit:

```text
D10 exited 0: the queue drained
1 merged · 1 parked · 1 skipped · 0 held
F14 parked gate_refused (code): revive failed: internal/cmd/notify.go:28:16: var-declaration; sent back
E72 skipped: blocked by F14 (parked gate_refused)
48.2M weighted tokens
```

It names why the drive ended (the queue drained, the deadline arrived, `--max-items`, `drive --stop`, an interrupt, or a fault), its exit code, how many items merged, parked, skipped and held, each item that did not merge with its cause, and the weighted tokens of the usage rows its sessions wrote. Items past the limit are listed as `and N more`.

### Reminders

A `needs_operator` the operator misses leaves the queue idle until they look. A destination that sets `repeat` sends it again every `repeat`, up to `repeat_max` times, at the priority it first went out at, with the title suffixed `(reminder N of M)`, until it is answered. Each destination keeps its own count, and a destination without `repeat` sends it once.

Before each reminder the drive reads the ledger, the backlog and its lease, and sends nothing for a `needs_operator` already answered. It counts as answered when:

- the item it names merged (a done row), was run again (a stage row, from `drover drive <id>` or `drive --enqueue`), was accepted (the body row `drover accept` writes, on a body that no longer carries the stop marker), or was shelved;
- for one a usage hold raised, the drive resumed from that pause, and a resume answers no other stop;
- the drive exited.

When a reminded `needs_operator` is answered while the drive runs, each destination that sent at least one reminder for it gets one message at `low`: `answered: <action> <id>`, where the action is `merged`, `rerun`, `accepted`, `shelved` or `resumed`. A destination that sent only the first message gets nothing more. The answer is read when the next reminder comes due, so the message trails the answer by up to `repeat`; past `repeat_max` the drive still reads at that interval, sending nothing until the answer.

Reminders live in the drive process and nowhere else. A drive that exits, including one that re-executes onto a new binary, has nothing left to repeat, and the `exit` message is the last word on it. A `triage_finding` stop is raised by a follow-up's coverage, which the ledger does not record, so whether it names its one filing item or none, its filer's rows and a resume answer nothing and it repeats until `repeat_max` or the exit. So does any other `needs_operator` naming no item that no usage hold raised.

### Tokens and testing

The token appears in no output drover writes: not in an error, the event file, `--events`, or `drover config`, which prints each ntfy destination's `url`, its resolved `token_file` and whether that file exists and reads. A `token_file` that group or others can read draws one warning per destination per drive naming the path and `chmod 600`, and the send still goes out.

`drover notify --test` sends one synthetic `needs_operator` event, id `TEST`, through every destination, whatever its `events` name, at its routed priority or `high` where it routes none. `--event <name>` sends a sample of that kind instead, rendered as the real one would be, at each destination's routed priority, to the destinations that route it. It prints each destination's HTTP or exit status, and exits 0 when every send succeeded, 1 when one failed and 2 when no destination is configured or none routes the `--event` named.

### Example

Two destinations: a phone that hears what needs a person, reminded every 15 minutes until somebody acts, and how each drive ended, and a desktop command that hears every merge. Overnight, merges wait for the morning and arrive as one message:

```toml
# ~/.config/drover/config.toml
[notify]
quiet_hours = "22:30-07:00"
coalesce    = "2m"

[notify.destinations.phone]
type       = "ntfy"
url        = "https://ntfy.example.com/drover"
token_file = "~/.config/drover/ntfy-token"
repeat     = "15m"
events     = { needs_operator = "high", needs_supervisor = "default", parked = "default", exit = "low", exit_undone = "high", usage_warning = "high" }

[notify.destinations.desk]
type   = "command"
argv   = ["notify-send", "drover"]
events = { merged = "default", needs_operator = "default" }
```

A project that wants its own exits louder writes only the keys that differ, and keeps the per-user `type`, `url` and `token_file`. A key it writes replaces the per-user one whole, so its `events` names every event the phone should still hear:

```toml
# .drover.toml
[notify.destinations.phone]
events = { needs_operator = "urgent", needs_supervisor = "default", parked = "default", exit = "default", exit_undone = "high", usage_warning = "high" }
```

iOS delivers a notification from a self-hosted server instantly only when that server sets `upstream-base-url` (usually `https://ntfy.sh`), so Apple's push service is told a message is waiting. That is a setting in the ntfy server's own `server.yml`, not in drover; without it the iOS app picks messages up only when it next polls.

### Migrating from `[supervise]`

`[supervise] notify` and `[supervise.ntfy]` are refused at load. `scripts/migrate-notify.sh <file>`, in the drover repository, rewrites either in place in a project or per-user config: `notify = [...]` becomes a `command` destination named `command` routed `{ needs_operator = "default" }`, and `[supervise.ntfy]` an `ntfy` destination named `ntfy` carrying its `url`, `token_file`, and each of its `events` at its `priority`. It changes only those lines, writes through a temp file and a rename, follows a symlink to the file it names, and leaves a file with neither key untouched. The old `notify` argv also ran on `usage_warning` and on the `resumed` that lifts a usage hold; add `usage_warning` to the migrated destination's `events` to keep hearing it.

## `[t3]`

Where the `drover t3` bridge finds a T3 Code server, and what it reads on the host beside it. Every key may sit in the per-user config, which is where a server serving every project belongs; a project's file overrides it key by key.

| Key                   | Default                                 | Meaning                                                                                                                                                                                                                                                  |
| --------------------- | --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `url`                 | unset                                   | The server's MCP endpoint, such as `https://host.example.ts.net/mcp`. HTTPS, or `http://localhost` only: the bearer token rides every request, so any other address is a load error.                                                                     |
| `token_file`          | `~/.config/drover/t3-token`             | The OAuth grant `drover t3 login` writes, at mode 0600.                                                                                                                                                                                                  |
| `project`             | the drover project's own name           | The T3 Code project title threads are launched in. The bridge never creates a project: add it on the host with `t3 project add`.                                                                                                                         |
| `poll`                | `2s`                                    | The bridge's step between reads of a running thread, as a Go duration. Each `t3_thread_wait` call waits up to three of them.                                                                                                                             |
| `provider_logs`       | `~/.t3/userdata/logs/provider`          | The directory T3 Code writes each thread's provider event log to. The bridge reads turns, tokens, cost and the rate-limit reading there, because no MCP response carries them.                                                                           |
| `tested_version`      | `0.1.2`, the release this build records | The T3 Code version the bridge was last verified against. `drover check` reports `BRIDGE VERSION` when the server's `initialize` names another.                                                                                                          |
| `operator_permission` | `supervised`                            | The permission a thread `drover t3 thread --from-event` opens on the operator's behalf runs under. `"full"` is a load error: that thread is opened unattended by a `needs_operator` or `needs_supervisor`, so nothing it does may skip an approval step. |

```toml
[t3]
url           = "https://mac-studio.example.ts.net/mcp"
token_file    = "~/.config/drover/t3-token"
project       = "drover"
poll          = "2s"
provider_logs = "~/.t3/userdata/logs/provider"
```

The provider event log is the one part of the bridge a T3 release can break without an error from the MCP server: it is T3's internal format, named `events.<thread id with ":" as "-">.log`, each line `[<time>] <tag>: <json>`. When it is missing, unreadable, or holds no Claude SDK `result` by the end of a run (a Codex thread, or a release that moved it), the bridge's log carries a `usage_unavailable` line and its result carries no counts. `drover drive --dry-run` then says `usage_admission` is blind for that runner, judged on the newest session of it that completed (an interrupted or failed one may have ended before the SDK wrote its result, so it says nothing about the format), and a drive on it refuses to start while `[usage.weekly]` sets a level, unless `--allow-blind-usage` is passed. `tested_version` is how `drover check` says the format may have moved before a drive finds out.

The grant in `token_file` records the endpoint it was issued for, and drover sends it to that endpoint only. A `url` naming any other, compared after normalizing the case of scheme and host, a default port and a trailing slash, is refused before a request is made, naming both and `drover t3 login`; so is a grant that records none. A project's `.drover.toml` can set `url`, and without this a cloned repository naming its own server would receive the operator's token from `drover check`.

`drover check` reads this table only on the T3 host, the one holding `provider_logs`: there it reports `BRIDGE SIGNED OUT` for an unset `url`, a grant issued for another endpoint, an expired grant that will not renew, or a grant the server will not take, `BRIDGE UNREACHABLE` for a server that does not answer `initialize` within 5 seconds, and `BRIDGE VERSION`. An expired grant carrying a refresh token is renewed in memory for that one call and the file left as it is, unless the server rotated the refresh token, when the renewed grant is written back because the one on disk may be spent. A CI host declaring the same runners is not the T3 host and reports none of them.

## Worked example

A Go and TypeScript monorepo with a generated API contract:

```toml
schema_version = 2
project = "example.app"

[paths]
backlog     = ".drover/backlog"
ledger      = ".drover/ledger.jsonl"
reports     = ".drover/reports"
specs       = "docs/design"
skills      = ".drover/skills"
root_marker = "go.work"

[ids]
planned_max = 25
namespaces  = { B = "bug", E = "enhancement", F = "feature" }

[frontmatter]
required = ["title", "spec", "deps"]
optional = ["phase", "wave", "contract", "migration", "web_only",
            "skills", "runner", "kind", "issue", "severity"]

[status]
column        = "wave"
column_prefix = "W"

[intake]
stop_marker = "STOP: requirements not yet written"

[verify]
default  = "make codegen && (cd backend && go build ./... && go test ./...) && (cd web && npm run build)"
web_only = "make codegen && (cd web && npm run build)"

[github]
repo = "owner/example.app"
```

A Django plugin, same tool:

```toml
schema_version = 2
project = "example-plugin"

[paths]
backlog     = ".drover/work"
ledger      = ".drover/ledger.jsonl"
reports     = ".drover/reports"
root_marker = "pyproject.toml"

[ids]
namespaces = { B = "bug", E = "enhancement", F = "feature" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "issue", "severity", "skills", "runner", "migration", "ui_only"]

[verify]
default   = "ruff check . && ruff format --check . && pytest"
migration = "python manage.py makemigrations --check --dry-run && ruff check . && pytest"
```

## The project registry

The first drover command run in a project appends one line to `projects.jsonl`, beside the per-user config, and later commands append nothing:

```json
{
  "project": "webapp",
  "root": "/home/me/src/webapp",
  "repo": "me/webapp",
  "registered": "2026-08-28"
}
```

`drover projects` reads it, folds to the newest row per root, and marks a root that is gone. `status --all-projects`, `drives --all` and `drive --pause --all` read it as their list of projects; nothing inside a project reads it, and it is an audit trail, not configuration. It exists so that "what is drover driving on this machine" has an answer that does not depend on remembering, and so that a change to the config vocabulary has a list of trees to migrate.

A row whose root is gone stays in the file. Removing it would erase the record that drover was pointed at that tree, which is the one fact the file holds. `drover projects --forget <root>` takes a root off every list by appending a tombstone row instead. A command run in a session drover launched never registers. A root under the OS temp directory does, and `status --all-projects` counts it as scratch rather than showing it; a git worktree of a registered project is shown through that project. See [`projects --json`](automation.md#projects---json).
