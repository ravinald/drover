# Operating modes

drover runs the same lifecycle five ways, from a session you steer turn by turn to a queue that implements, reviews and merges overnight while you sleep. They differ in who steers, what gets recorded, and who does the waiting, and the waiting is where most of the cost goes. This page covers what each mode is for and how they fit together. [Lifecycle](lifecycle.md) gives the order of the commands, and the [Playbook](playbook.md) covers sizing an item and choosing a reviewer.

## At a glance

Each mode in the following table is a command you type. Operator sessions are not a command: they're the short interactive sessions you open yourself to make the decisions drover leaves to you.

| Mode                                    | Who steers           | Who waits                    | Choose it for                                       |
| --------------------------------------- | -------------------- | ---------------------------- | --------------------------------------------------- |
| [Interactive run](#interactive-run)     | you, every turn      | you                          | the first item in a project, an item you doubt      |
| [Headless run](#headless-run)           | nobody, once started | you, at the terminal         | one item at a time, reviewed and audited by hand    |
| [Drive](#drive)                         | nobody               | drover's own loop            | a queue, bounded by a clock                         |
| [Detached drive](#detached-drive)       | nobody               | drover's own loop            | the steady state: queue it, get notified, walk away |
| [Supervised drive](#supervised-drive)   | an agent, per event  | `drover watch`, which blocks | handling routine parks without you                  |
| [Operator sessions](#operator-sessions) | you                  | nobody                       | writing items, accepting, handling a park           |

## Interactive run

The following command opens the runner's own interface on the item's branch, with the item's prompt as the first message:

```bash
drover run B1
```

You read every turn and can stop it at the third, which makes this the right mode for the first item in a new project and for any item you're not sure is sized right. A badly sized item shows itself early, and you find out while it's cheap.

What it doesn't leave behind is the reason to stop using it: no log, so no self-report for the reviewer, no `[~]` implemented state, and no record of turns or cost. [Interactive or headless](playbook.md#2-interactive-or-headless) has the full comparison.

## Headless run

The following commands run one item unattended, then hand its review to a second session:

```bash
drover run B1 --headless
drover review B1 --runner claude --headless
drover audit B1 verified --code-by agent "traced each requirement to the diff"
```

The event stream goes to a log, so `drover report B1` shows the run's own account, its turn count and its cost, and the reviewer gets that account to check against the diff. You still decide when each step starts, which is the point: it's the mode for learning what your merge gate and review doctrine catch before you let anything merge without you.

A headless run is not detached. It inherits your terminal, and closing the terminal kills it.

## Drive

The following command takes the ready queue and walks every item through implement, review, merge gate, pull request and merge, until the clock runs out:

```bash
drover drive --for 8h
```

Running `drive` is deciding that green-and-reviewed code may merge without you reading it first. [Whether to drive](playbook.md#4-whether-to-drive) covers what stands between a session and your trunk, and the case for `--no-merge` on the first night.

Two properties make `drive` cheaper than the same work done by hand:

- **Every stage is a fresh session.** An implement session ends when its item is done, and the review starts from the diff and the contract, not from the implementer's conversation. No session's context grows past one item.
- **The waiting is Go, not a model.** Waiting for CI, for a merge, for the next slot or for a sibling to clear the trunk happens in drover's own loop, and costs nothing.

`--jobs N` runs up to N items at once, each in its own worktree. That finishes a queue sooner and spends tokens N times as fast. [Cost](cost.md#parallel-work) covers what that means across several projects.

## Detached drive

The following commands start a drive in its own session, return once it holds the lease, and tell you when it needs a person:

```bash
drover drive --detach --for 8h
drover watch --until needs_operator,exit
```

`--detach` is `drive` with the terminal taken out of the picture: stdin on `/dev/null`, output in a log beside the drive's event file, and nothing tying it to the shell you started it from. `drover drive --enqueue <id>...` adds work to the running drive, `drover attach` follows it, and `drover drive --stop` drains it. `drover attach --session` shows what a running item is doing, each tool call and message of its session beneath the item's `stage` line, and `--item <id>` narrows it to one item.

To be told rather than to watch, set `[supervise] notify` to a command that reaches you. drover runs it once for each `needs_operator` event, from the project root, with the event as JSON on stdin and 30 seconds to finish. The following entry raises a desktop notification; a script that forwards stdin to your phone works the same way:

```toml
[supervise]
notify = ["notify-send", "drover"]
```

This is the mode to settle into. The queue runs, the waiting costs nothing, and the only thing that reaches you is an event a person has to handle.

## Supervised drive

`drover supervise` launches one interactive session with `DROVER_ACTOR=supervisor` to watch drives and take the actions this project's `[supervise]` policy assigns to a supervisor: resolving a branch-update conflict, relaunching an item whose session died, writing the requirements of a scaffold a review filed. The following command prints what that session will be told without launching anything:

```bash
drover supervise --dry-run
```

Its prompt is built to keep the session small. It waits with `drover watch --until parked,needs_operator,merged,exit`, which blocks until one of those events arrives, so the session wakes once per event rather than once per poll. It is told not to loop on `sleep`, the ledger or the lease, and it stops when the queue is empty and nothing at `supervisor` level is left.

Choose it when routine parks would otherwise wait for you overnight. Skip it when a detached drive and a notification cover what comes up: a session that wakes on events still pays to read its whole conversation on every wake, and a drive with notify configured pays nothing to wait.

## Who handles each event

A drive meets situations it can't settle from the merge gate and the review alone: a branch-update conflict after a sibling merged, an implementing session that died, a scaffold a review filed, a pull request held by `protected_paths`. `[supervise]` decides, action by action, who handles each one. Every action takes one of four policies:

| Policy       | What happens                                                                       |
| ------------ | ---------------------------------------------------------------------------------- |
| `auto`       | The drive does it itself, with no session involved.                                |
| `supervisor` | The drive emits `needs_supervisor` and leaves it for a `drover supervise` session. |
| `operator`   | The drive emits `needs_operator`, runs `notify`, and waits for you.                |
| `off`        | Nothing happens, and the item stays parked.                                        |

**A project with no `[supervise]` table runs every action at its default, and several defaults are `supervisor`.** `resolve_conflict`, `relaunch_after_runner_death`, `write_scaffold_requirements`, `close_changes`, `enqueue` and `cleanup_interrupted_drive` all default there. With no `drover supervise` session running, those events park and wait, and `notify` doesn't fire for them, because it runs only on `needs_operator`. In practice someone notices the parks and starts handling them in whatever chat is open, and that chat becomes a supervisor nobody configured: the [long-lived session](cost.md#long-lived-sessions) that cost tracking catches later.

Decide each action once instead. The following command walks the catalog by category and offers each action only the policies it allows. It appends a `[supervise]` table to `.drover.toml` when the file has none, and prints one to paste over the existing table when it has one:

```bash
drover init supervise              # asks about every action
drover init supervise --defaults   # writes every action at its default, to edit by hand
```

Without `--defaults` it needs a terminal to ask on, and inside a session drover launched it refuses whatever the flags, because those policies bound that session. A policy left at its default is written commented out, so `--defaults` changes nothing about how a drive behaves: it lists every action in the file, and you decide by uncommenting a line and changing its value. Some guidance for the choices:

- **Raise what you'd always approve to `auto`.** `resolve_conflict` and `relaunch_after_runner_death` can reach `auto`, and at `auto` neither needs a session.
- **Some actions stop short of `auto` by design.** `merge_drive_pr`, `merge_protected_paths`, `write_scaffold_requirements`, `triage_finding` and `close_changes` cap at `supervisor`, and `accept`, `signoff`, `override_review_limit`, `auth_expired` and `push_refused_by_hook` cap at `operator`. Asking for more is a load error that names the ceiling.
- **Don't leave anything at `supervisor` unless you run `drover supervise`.** Without one, set it to `operator` so `notify` reaches you, or to `auto` where the ceiling allows.

`drover config` prints every action's effective policy, marked `default` where the file sets nothing. Read it after any drover upgrade. A project whose config predates `[supervise]` has no table, and `drover config --diff` doesn't list the table, so the defaults apply without anything pointing at them.

## Operator sessions

Some decisions drover leaves to you on purpose: writing an item's requirements, `drover accept`, merging what `protected_paths` held, reading a park and deciding what it means. You make them in an interactive session of your own, and how long that session lives decides most of what it costs.

Open one for a decision and close it when the decision is made. For example:

1. A notification says `B12` parked on `trunk_conflict`.
2. Open a session, run `drover show B12`, resolve the conflict, and push.
3. `drover drive --enqueue B12`, then close the session.

The next park gets a new session with an empty context. One session kept open across days of drives becomes the most expensive thing on the account without doing any of the work: [Long-lived sessions](cost.md#long-lived-sessions) explains why.

## Recommended setup

Most projects end up with the following arrangement once the first few items have proved the merge gate:

1. Run `drover init supervise`, so every action has an owner you chose ([Who handles each event](#who-handles-each-event)).
2. Write and accept items in a short operator session.
3. Start `drover drive --detach --for <hours>` from a terminal, not as a background task inside an agent session.
4. Let `[supervise] notify` reach you on `needs_operator`, and use `drover supervise` only if routine parks keep waiting overnight.
5. Handle each event in a fresh operator session.
6. Read `drover status`, or the page `[[status.render]]` writes, whenever you like: reading it costs nothing.

## Where to go next

- [Cost](cost.md): where tokens go in each mode, and the patterns that multiply them.
- [Playbook](playbook.md): sizing items, choosing a reviewer, and the case for and against driving.
- [Configuration](configuration.md#supervise): every `[supervise]` key, `notify` included.
- [Automation](automation.md): the event shapes `drover watch` and `drover attach` print.
