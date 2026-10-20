# Prompt templates

The prompt is the text an agent is handed. It is a Go `text/template` a project owns, not prose compiled into drover, because every rule it carries (what a bug owes over a feature, how wide a diff may go, what the report must carry) is a property of the project rather than of the tool.

Point at yours with `[prompts] implement`. Omit it for the built-in, which is deliberately generic. Two more render the same way and from the same data: `[prompts] draft` for the prompt `drover draft` hands an agent drafting an item's requirements, and `[prompts] review` for the one `drover review` hands an agent checking the finished work. Each is documented below by what it adds.

Preview before spending a run:

```bash
drover run <id> --dry-run
```

## Data

Three commands render a template and they share one data type, so the tables below split by which command fills a field rather than by where it is declared. A field a command does not fill is present and empty, never absent: a review template that reaches `.Posture` renders a blank, it does not fail.

### Every template

| Field                 | Type     | Notes                                                                                                                                                                                                                                                         |
| --------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.Project`            | string   | From config.                                                                                                                                                                                                                                                  |
| `.ID`                 | string   | Bare: `2b`, `B1`.                                                                                                                                                                                                                                             |
| `.Stem`               | string   | Zero-padded: `02b`, `B01`.                                                                                                                                                                                                                                    |
| `.Slug`               | string   | From the filename.                                                                                                                                                                                                                                            |
| `.Title`              | string   |                                                                                                                                                                                                                                                               |
| `.Branch`             | string   | Already rendered from `[run] branch`.                                                                                                                                                                                                                         |
| `.Body`               | string   | The item text below the frontmatter fence, verbatim.                                                                                                                                                                                                          |
| `.Path`               | string   | That file on disk, project-relative.                                                                                                                                                                                                                          |
| `.Planned`            | bool     | False for a namespaced item.                                                                                                                                                                                                                                  |
| `.Kind`               | string   | The item's frontmatter `kind`, else the kind its namespace maps to in `[ids]`, else empty. Reach it with `with` rather than `if`, since empty is a real answer.                                                                                               |
| `.Meta`               | map      | The keys present in this item's fence and no others. `drover check` renders against every key `[frontmatter]` declares, so `{{ .Meta.phase }}` passes there and still fails at launch for an item that omits `phase`: read an optional key with `.Get "key"`. |
| `.Deps`               | []Dep    | `.ID .Title .Done .Unreadable .Level .By .Independent .Weak .Report`, all of them populated by every command. `.Level` is the audit level and `.Report` the path to the dep's saved report                                                                  |
| `.Specs`              | []string | `spec:` tokens resolved against `[paths] specs`.                                                                                                                                                                                                              |
| `.Skills`             | []Skill  | `.Name`, and `.Path` when vendored.                                                                                                                                                                                                                           |
| `.VerifyCommand`               | string   | The verify command selected for this item.                                                                                                                                                                                                                    |
| `.MergeGate`          | string   | `[drive] merge_gate` joined into one line, empty when unset. The built-in review brief asks for the full `.VerifyCommand` only when this is empty.                                                                                                                     |
| `.VerifyCommands`              | map      | Every verify command, when you want to choose differently.                                                                                                                                                                                                    |
| `.FollowUps`          | []Issue  | Open follow-ups, one hop. `.Number .Title .Severity .Owners .Mine .Body .Unavailable`                                                                                                                                                                         |
| `.RegisterUnreadable` | string   | Why the follow-up list did not answer, empty when it did.                                                                                                                                                                                                     |
| `.Owned`              | []Issue  | Open issues owned by this item or its deps that are not follow-ups, one hop. `.Number .Title .Labels .Owners .Mine .Body`                                                                                                                                     |
| `.OwnedUnreadable`    | string   | Why the owned-issue read failed, empty when it answered or was never made.                                                                                                                                                                                    |
| `.Runs`               | int      | Implement launches the ledger records before this pass.                                                                                                                                                                                                       |
| `.Reviews`            | int      | Review launches drover resolved a runner for.                                                                                                                                                                                                                 |
| `.Audits`             | int      | Recorded audits.                                                                                                                                                                                                                                              |
| `.Rounds`             | int      | Times a review has sent it back.                                                                                                                                                                                                                              |
| `.Repeated`           | bool     | Method: this item has been round the loop before.                                                                                                                                                                                                             |
| `.Changes`             | string   | Method: what the last review sent back, the heading excluded.                                                                                                                                                                                                 |
| `.ChangesHeading`      | string   | Method: the heading `drover request-changes` writes under.                                                                                                                                                                                                    |
| `.Cleared`            | bool     | Method: that section holds a discharged block rather than findings.                                                                                                                                                                                           |
| `.Guarantees`         | string   | Method: what every pass so far established this code must keep true.                                                                                                                                                                                          |
| `.GuaranteeHeading`   | string   | Method: the heading `drover invariant` appends under.                                                                                                                                                                                                         |
| `.Trail`              | []Round  | Every round a review sent back so far, oldest first. `.Number .Date .Headline .Path`                                                                                                                                                                          |
| `.Earlier`            | []Round  | Method: the trail without the round already in the body, all of it once cleared.                                                                                                                                                                              |

`.Done` is the ledger's answer and `.Unreadable` is the tree's, and they are independent: a dep recorded done whose body was deleted is both merged and unreadable. `.Unreadable` is empty when there is a body to read. Otherwise it carries `check`'s own wording for the state (`has no body`, `will not parse`, `is shelved`), or `does not resolve to an item id` for a dep that is not an id this project can parse.

`.Earlier` is what a prompt renders, never `.Trail`: the newest round is the one already under `.ChangesHeading`, and a prompt listing both hands the current findings over twice, once as instructions and once as history. Once `.Cleared` it returns every kept round instead, the newest included: a cleared block replaced that round's evidence with the statement that nothing is outstanding, so the list is the only place a reader can still see what `close-changes` discharged. A `Round` carries no body on purpose. The headline and the path are enough to see the course of a loop, and a closed finding rendered whole reads as an open one, which is the failure the replaced section exists to prevent.

`.Guarantees` and `.Changes` are the two sections of the body a prompt renders separately from `.Body`, which carries both anyway. Rendering them again is how a prompt names the section rather than leaving an agent to notice a heading: the one that accumulates and the one that is replaced every round read alike inside a quoted body, and acting on the wrong one means re-verifying finished work or breaking a property four passes established. A template that renders neither is not wrong; one that renders the findings and not the invariants has told the agent what broke and not what may not.

`.Runs`, `.Reviews` and `.Audits` are counted separately rather than added into one round number, because they come apart: a project that pipes `drover review` records no review launch, and a person who reads the diff by hand records no launch at all. `.Repeated` is the one condition worth branching on: on a first pass there is no trend to report.

Every prompt carries them, the implement prompt included. The agent choosing how wide to look is the implementing one, and it was the one drover told nothing: without the counts, a fifth correction pass reads the same prompt as the first, so each fix closes the case it was handed and the next review finds the one beside it. An implement prompt that renders `.Rounds` tells that agent the reproduction in front of it is one input to an invariant somebody keeps breaking, rather than the whole defect.

`.Level` and `.By` are a dep's audit provenance, and `.Weak` folds the four states that make a foundation unconfirmed into one bool: unaudited, `claimed`, audited by its own author, or audited before authorship was recorded at all. A template that surfaces `.Weak` tells the agent which of its givens are unconfirmed. Every command fills all three, the implement prompt included: `.Weak` is false on a dep nobody confirmed and false on a dep an independent pass audited, so a field left zero would read as the second.

`.Path` is on every prompt, not only the drafting one. A draft writes that file, a review rewrites one section of it, and an implement prompt names it so an instruction about the item body points at something rather than starting a search.

A `.FollowUps` row carries the report that filed it in `.Body`. Empty means the row carries none and a template renders the headline alone; `.Unavailable` means drover could not reach the text and says why, which is what every row from a file register carries, that backend having no column for one. Rendering neither field prints a title and implies whoever filed it wrote nothing under it. `.Mine` is true when this item is one of the row's owners and false when the row arrived from a dependency: one hop, and somebody else's to discharge.

`.RegisterUnreadable` is why the read failed. `.FollowUps` is then a floor rather than the list, and a template branching on `.FollowUps` alone renders an unreachable tracker exactly as it renders a clean one. A project that declared no follow-up list leaves it empty: that project has failed nothing, and reporting an absence there would report a setting as a fault.

`.Owned` holds what names an owner and carries no follow-up label: a `bug` filed against the branch, say. The follow-up list is read with its label server-side, so these never reach `.FollowUps`, and they are read per id instead, from the sub-issues of the id's own issue and an `Owner: <id>` body search. `.Mine` and `.Body` follow the `.FollowUps` rules, and `.Labels` is every label on the issue, since nothing else says what kind of report it is. Only the GitHub backend makes the read: under a file register or `backend = "none"` both fields stay empty and gh is not called. `.OwnedUnreadable` is why the read failed, for the reason `.RegisterUnreadable` exists.

Methods: `.Get "key"` reads a frontmatter value without erroring on a project that does not declare it; `.Is "key"` reports it set to `yes`.

`.Changes` is the section of `.Body` a review last sent back, the heading line excluded, and empty when the body carries none. `.ChangesHeading` is that heading. Reach it through the method rather than writing the string: `drover request-changes` writes to the same constant, and a template naming its own copy of the wording sends the next reader to a section that does not exist. Nothing else can show that evidence: the ledger records a digest of it, and `request-changes` replaces the section rather than appending, so the text of one round leaves the tree when the next is written.

`.Cleared` is true when that same section holds a block `drover close-changes` discharged rather than findings `drover request-changes` wrote. Branch on it wherever you branch on `.Changes`: both are the same section, the text reads alike to a template that only tests whether it is empty, and rendering a discharged block as current findings sends the next run to re-verify finished work, which is the state `close-changes` exists to end. The two built-in prompts condition the send-back prose on `and .Changes (not .Cleared)` and render a paragraph of their own for the cleared case.

`.Destinations` is the doctrine for where a finding goes, one `.Token .Where .Found .Do` per place a finding can go, with `.Wrap <indent>` rendering one as an indented paragraph. It is a property of the tool rather than of the item, so a template that renders the list stays current as the list changes.

`.MergedDeps`, `.UnmergedDeps` and `.UnreadableDeps` partition `.Deps` so a template branches rather than classifies. `.UnreadableDeps` takes every dep with a non-empty `.Unreadable`, merged or not, and the other two split what is left on `.Done`. Unreadability wins so that no dep is described twice: the ledger will still hand out a level for one it recorded done, and printing that level under a heading saying this item's correctness rests on the dep describes code nothing can show the reader. `.Deps` still carries all of them, so a template that ignores these renders unchanged.

Functions: `lower` `upper` `join` `trim`.

A template naming a field that does not exist fails rather than rendering a hole, and a template that renders nothing at all is an error. Both would otherwise reach an agent that acts on them.

### The review prompt adds

`[prompts] review` renders for `drover review`, whose job is to check what the run reported against the contract rather than re-derive either. These carry both halves of that comparison.

| Field               | Type          | Notes                                                                                                |
| ------------------- | ------------- | ---------------------------------------------------------------------------------------------------- |
| `.Requirements`     | []Requirement | The item's numbered contract. `.Num .Text`                                                           |
| `.SelfReport`       | string        | What the run reported, from its most recent log.                                                     |
| `.Outcome`          | string        | That run's one-line result.                                                                          |
| `.LogPath`          | string        | Where the stream lives. Empty when no log exists.                                                    |
| `.Unfinished`       | bool          | A log exists but carries no result event.                                                            |
| `.Level`            | string        | This item's own current audit level, empty when nobody recorded one.                                 |
| `.By`               | string        | The `--code-by` list on that audit row, as typed.                                                    |
| `.Note`             | string        | That audit's note, verbatim.                                                                         |
| `.AuditDate`        | string        | When it was recorded.                                                                                |
| `.Closes`           | []Resolution  | The tracker issues the item says it resolves.                                                        |
| `.MaxReviewRounds`        | int           | `[review] max_review_rounds`, zero when uncapped.                                                    |
| `.Modified`         | int           | Source files this branch changed, tests excluded.                                                    |
| `.Unnamed`          | []string      | Those the item body never mentions.                                                                  |
| `.Named`            | int           | Method: the complement, so two numbers cannot disagree.                                              |
| `.ScopeUnavailable` | string        | Why git could not say, empty when it did.                                                            |
| `.Capped`           | bool          | The next `drover request-changes` refuses.                                                           |
| `.Recent`           | Recent        | The last commits on the branch. `.Commits` (`.Abbrev .Lines`), `.Unavailable`, and `.Sizes` as prose |

`.Unfinished` is a different answer from "never ran", and the distinction is worth branching on: a reviewer told the wrong one reviews the wrong tree. An empty `.LogPath` means no run log; a set `.LogPath` with `.Unfinished` means the run is still in flight or it died.

The audit note rides verbatim because a re-review needs to read what the last pass recorded, and structuring it would cost the prose that makes it worth reading.

`.Recent.Unavailable` says why git could not answer and is never set at the same time as `.Commits`, so a prompt either reports sizes or reports what it could not ask.

`.Unnamed` is rendered as a list, never as a count and never against a threshold. A body citing few paths has most of its diff unnamed whatever the work did, so the number is ambiguous where the list is not: a reviewer reading three storage backends recognizes a contract the item never named. An item whose diff mostly leaves its cited files tends to take more passes, which is enough to ask the question and nowhere near enough to answer it. `.ScopeUnavailable` carries the reason git could not say, for `Recent.Unavailable`'s reason: an unasked question and a clean answer render alike.

Modified files only. A feature creates files no body could have named, and a body cites the code it is about rather than the test that does not exist yet, so counting either puts clean work beside work that spread.

`.Capped` is the pair to `.Rounds` and `.MaxReviewRounds`: a prompt that instructs `drover request-changes` beside a command that will refuse is the tool arguing with itself, and the agent meeting that spends its turn retrying. Render the filing path instead when it is set.

A `Resolution` is `.Number .Title .Body .State .Disposition .By .Note .Applied .Unavailable`, plus `.Undisposed`, which reports one nobody has judged yet. `.State` is the issue's own, so a reviewer sees one somebody closed by hand; `.Applied` names what `finish` already wrote to the tracker, so a reviewer does not re-litigate a public comment; `.Unavailable` says why an issue could not be read, which renders as a question drover could not ask rather than as an empty report. `.UndisposedCloses` filters `.Closes` to the reviewer's actual worklist, and it is the list to render: `finish` refuses to act on anything else.

### The drafting prompt adds

`[prompts] draft` renders for `drover draft`, which writes an item's numbered requirements rather than executing them.

| Field         | Type      | Notes                                                                                                                                          |
| ------------- | --------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `.Posture`    | string    | `[agents] principles`, the project's bar.                                                                                                      |
| `.StopMarker` | string    | The line the draft must leave in place.                                                                                                        |
| `.Origin`     | Origin    | The issue behind the scaffold. `.Number .Title .Body .Unavailable`                                                                             |
| `.Finding`    | Finding   | The item the scaffold's `Found while working` line names, and its audit notes newest first. `.From .Notes`, each note `.Date .Level .By .Note` |
| `.Siblings`   | []Sibling | The rest of the backlog, for filing a requirement where it belongs. `.ID .Title .Path .Done`                                                   |

`.Origin` is read fresh from the tracker rather than from the copy the scaffold quoted, because an issue gains detail after intake and drafting from the stale copy is drafting from what was known that day. `.Unavailable` says why it could not be read, so a prompt assembled without the network reports the gap instead of implying the issue was empty.

`.Finding` is empty for a body with no provenance line. For one that has it, `.Notes` is every audit row on the named item that carries a note, read from the ledger at draft time. A review finding filed without `drover new -f` is described only there, in the note on the item under review. An empty `.Notes` under a set `.From` means the item has no audit notes, and the built-in prompt says so in one line.

`.Siblings` exists so a drafting agent can send a requirement that belongs elsewhere to its owner. Without the list, the only item it can see is the one it was pointed at, and everything it finds becomes that item's problem.

### The branch template sees less

`[run] branch` renders from the same data **minus `.FollowUps` and `.RegisterUnreadable`**, and `.Branch` itself is naturally empty there. `.Kind` is available, and is the one field a branch template is likely to want that the frontmatter alone cannot always supply. Those are the only fields read from the tracker, and a branch name that changed the moment `gh` was unreachable would leave `run` creating one name and `finish` hunting for another. A branch template referencing any of them renders it as empty rather than failing.

## Writing one

Start from the built-in and edit. The parts worth keeping:

**Put preconditions before the body, postconditions after.** A guide the agent must read before writing code does not belong under "definition of done".

**Give `.Body` its own fenced region.** The item text is the contract; an agent needs to see where it starts and stops.

```text
--- {{ .ID }} ---
{{ .Body }}
--- END {{ .ID }} ---
```

**Say what is inherited and what to do with it.** A list of issue numbers with no instruction is noise:

```text
{{- if .FollowUps }}
Open follow-ups for this item and its direct dependencies. Address them in this
run, or say in your report why you are carrying one forward.
{{- range .FollowUps }}
  - #{{ .Number }} [{{ if .Severity }}{{ .Severity }}{{ else }}-{{ end }}] {{ .Title }}
{{- end }}
{{ end }}
```

**Hand over dependency reports.** A follow-up records what an item left open; a saved report records what it closed by inspection. `.Report` is its path, and a run that cannot see the second re-litigates the finding or regresses the invariant it rested on:

```text
{{- range .Deps }}{{ if .Report }}
  - {{ .Report }}
{{- end }}{{ end }}
```

**Vary by kind where it matters.** Read the kind out of `.Meta` rather than expecting drover to know your vocabulary:

```text
{{- if eq (.Get "kind") "bug" }}
- Reproduce first. Ship a test that FAILS on the current HEAD, and quote its
  pre-fix output in your report. A fix with no failing-first test is evidence about
  one run, not a property of the code.
{{- end }}
```

**Avoid opening on a prohibition.** The first lines carry the most attention; spending them on what not to do is a poor trade. Enforce exclusions in tooling where you can.

## Selecting a verify command

`.VerifyCommand` is resolved by convention: a verify command named for a frontmatter key wins when that key is `yes`. Names are tried in sorted order and the first match wins. Most templates write `{{ .VerifyCommand }}`.

**Exactly one verify command runs.** drover selects, it never composes, so a command named for a key is safe only as a superset of `default`: whatever it omits stops running for every item that sets the flag, and stops running silently. A per-key verify command that is narrower than the default is the pattern to check for first when a class of defect keeps reaching main.

When you need more, ignore it and reach `.VerifyCommands`:

```text
- The verify command passes before commit: {{ if .Is "migration" }}{{ index .VerifyCommands "migration" }}{{ else }}{{ .VerifyCommand }}{{ end }}
```

## Whitespace

Go templates keep the newlines around actions. Use `{{-` and `-}}` to trim, and check the result with `--dry-run`: a prompt with ragged blank lines reads as carelessly assembled, and an agent takes its cue from that.
