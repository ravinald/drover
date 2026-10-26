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

To be told rather than to watch, give `[notify]` a destination. An `ntfy` destination POSTs to an [ntfy](https://ntfy.sh) topic your phone subscribes to; each event it routes arrives with the project and item in the title, the priority you chose for that event, and a body saying what happened or what to do. Put it in the per-user config and one entry serves every project:

```toml
[notify.destinations.phone]
type       = "ntfy"
url        = "https://ntfy.example.com/drover"
token_file = "~/.config/drover/ntfy-token"
events     = { started = "low", merged = "low", parked = "default", needs_operator = "high", needs_supervisor = "default", exit = "default", exit_undone = "high" }
```

That one table tells you when a drive starts and what it queued, each merge and park, every stop that needs you, and how the drive ended. The exit names what was left undone and why, such as `E72 skipped: blocked by F14 (parked gate_refused)`, and an exit that left anything parked, skipped or held goes out at `exit_undone`'s priority, so a drive that exits 0 with work behind it still rings.

For anything ntfy does not reach, a `command` destination runs an argv with each event it routes as one JSON line on stdin, from the project root, with 30 seconds to finish. The line carries the event, the `priority` it was routed at and the `message` an ntfy destination would send. The following entry raises a desktop notification for each stop and merge:

```toml
[notify.destinations.desk]
type   = "command"
argv   = ["notify-send", "drover"]
events = { needs_operator = "high", merged = "default" }
```

Destinations of different names both apply, each to the events it names, and a slow one delays neither the drive nor the other. `drover notify --test` sends a test event through each and prints what it answered, so a wrong token turns up before a drive stops for you; `drover notify --test --event exit_undone` shows what a drive that left work behind sends, at the priority each destination routes it.

Two `[notify]` keys keep a long drive from waking you for nothing. `quiet_hours` holds every message routed below `high` until the window ends and then sends what it held as one message per destination; `needs_operator` and `needs_supervisor` are never held, and a drive that exits inside the window folds what it held into its exit, sent at `min` so ntfy delivers it without sound unless you routed `exit` at `high` or above. `coalesce` folds events of one kind that arrive close together into one message listing their ids, such as `filed: B91 B91 B92`:

```toml
[notify]
quiet_hours = "22:30-07:00"
coalesce    = "2m"
```

A stop you miss still idles the queue until you look. `repeat` on a destination sends an unanswered `needs_operator` again on that interval (at least `5m`), up to `repeat_max` times (default 6), titled `(reminder N of M)`, and stops once the item is accepted, merged, rerun or shelved, or the drive resumes from the pause that raised it. Each destination that reminded you then gets one `answered: <action> <id>` at `low`. Reminders run only while the drive does: once it exits, its `exit` message is the last you hear from it.

```toml
[notify.destinations.phone]
repeat     = "15m"
repeat_max = 4
```

A config written for an earlier release with `[supervise] notify` or `[supervise.ntfy]` is refused at load; `scripts/migrate-notify.sh <file>` rewrites either into a destination. See [`[notify]`](configuration.md#notify) for every key.

This is the mode to settle into. The queue runs, the waiting costs nothing, and the only thing that reaches you is an event a person has to handle.

## Running past the weekly allotment

A Claude account runs out twice, in sequence: first the weekly allotment, then any extra-usage credit you bought to work past it. A drive that runs into either without a plan loses the session in flight and stops. Two levels per window tell `drive` what to do before that.

drover can see **the percent of each window left**: Claude Code reports the five-hour and seven-day windows on every request, in steps of 1%, and drover keeps the last reading of each session on its usage row. It cannot see **a remaining credit balance**: once the week reads 100%, the reading says only that sessions are drawing on purchased credit and, at the very end, `out_of_credits`. The only dollar figure drover holds is each session's list-price `cost_usd`, which is not what the account was charged.

Choose the levels in the per-user config, since the limit is the account's and every project shares it:

```toml
[usage.weekly]
notify_remaining = 20
pause_remaining  = 5
```

- **The notify level** is how much warning you want. At 20% of the week left, `drive` sends one `usage_warning` through `notify` and `ntfy` and keeps going. Pick it at roughly a day's worth of your usual use, so the notice arrives with time to decide.
- **The fail-safe level** is what you keep back for yourself. At 5% left, `usage_admission` holds the drive: sessions already running finish their stage, since that work is what the margin is for, and nothing new starts. The drive pauses as `drive --pause` would and emits `needs_operator`. Pick it at what an operator session or two of your own needs, so a drive never spends the last of the week on your behalf.
- **A hold inside the bound lifts itself.** Under `--for 48h`, a weekly reset 30 hours away is inside the bound, and the drive resumes at the reset with no one typing anything. Under an unbounded drive it waits for `drover drive --resume`, and a deadline that arrives first ends the drive with exit 0 and the waiting items reported `held`.
- **A refusal holds too.** A session the service turns away for quota sends its item back to the head of the queue at the stage that was refused, and the drive holds as above, instead of parking the item `runner_fault`.
- **One reading can cross several holds, and each `--resume` releases one.** A week at 0% left with sessions on purchased credit crosses both the weekly `pause_remaining` and `[usage.overage] mode = "pause"`. Resuming past the weekly percent is spending its last points, not consenting to credit, so the first `drover drive --resume` releases the weekly hold and the drive pauses again on overage; the second releases that. The pause line names every hold standing behind the one taken, `needs_operator`'s `run` says what each resume releases, `drover drive --resume` prints the hold it released and the ones still standing, and `drover drives` lists them beside `paused`.

`[usage.overage]` decides what happens once the week is spent and credit is in play. The default, `mode = "pause"`, holds the drive the moment a reading shows sessions on purchased credit, so nothing spends it unattended. `mode = "continue"` runs on credit, and `max_usd` bounds it at a list-price sum across every registered project since the week began, with `notify_usd` as the warning on the way. Since the sum is list price and the balance is invisible, set `max_usd` comfortably below what you bought, and expect `out_of_credits` to hold the drive anyway if the estimate runs long. Under `continue`, leave `[usage.weekly] pause_remaining` unset if you mean to run straight onto credit, or `drover drive --resume` past the weekly hold when it comes.

`drover status` prints the reading as one footer line, and `drover drive --dry-run` says whether `usage_admission` would hold now and why. Every key is in [`[usage]`](configuration.md#usageweekly-and-usagefive_hour).

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
| `operator`   | The drive emits `needs_operator`, sends it to `[notify]`, and waits for you.       |
| `off`        | Nothing happens, and the item stays parked.                                        |

**A project with no `[supervise]` table runs every action at its default, and several defaults are `supervisor`.** `resolve_conflict`, `relaunch_after_runner_death`, `write_scaffold_requirements`, `close_changes`, `enqueue` and `cleanup_interrupted_drive` all default there. With no `drover supervise` session running, those events park and wait, and a `[notify]` destination reaches you for them only when its `events` names `needs_supervisor`. In practice someone notices the parks and starts handling them in whatever chat is open, and that chat becomes a supervisor nobody configured: the [long-lived session](cost.md#long-lived-sessions) that cost tracking catches later.

Decide each action once instead. The following command walks the catalog by category and offers each action only the policies it allows. It appends a `[supervise]` table to `.drover.toml` when the file has none, and prints one to paste over the existing table when it has one:

```bash
drover init supervise              # asks about every action
drover init supervise --defaults   # writes every action at its default, to edit by hand
```

Without `--defaults` it needs a terminal to ask on, and inside a session drover launched it refuses whatever the flags, because those policies bound that session. A policy left at its default is written commented out, so `--defaults` changes nothing about how a drive behaves: it lists every action in the file, and you decide by uncommenting a line and changing its value. Some guidance for the choices:

- **Raise what you'd always approve to `auto`.** `resolve_conflict` and `relaunch_after_runner_death` can reach `auto`, and at `auto` neither needs a session.
- **Some actions stop short of `auto` by design.** `merge_drive_pr`, `merge_protected_paths`, `write_scaffold_requirements`, `triage_finding` and `close_changes` cap at `supervisor`, and `accept`, `signoff`, `override_review_limit`, `auth_expired` and `push_refused_by_hook` cap at `operator`. Asking for more is a load error that names the ceiling.
- **Don't leave anything at `supervisor` unless you run `drover supervise`.** Without one, set it to `operator` so a `[notify]` destination routing `needs_operator` reaches you, or to `auto` where the ceiling allows.

`drover config` prints every action's effective policy, marked `default` where the file sets nothing. Read it after any drover upgrade. A project whose config predates `[supervise]` has no table, so the defaults apply without the file saying so. `drover config --diff` lists each action no file sets at its default and names the ones at `supervisor`, and once the ledger records a drive `drover check` reports `SUPERVISOR UNSET` for them. Both point at `drover init supervise`.

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
4. Let a [`[notify]`](configuration.md#notify) destination reach you on `needs_operator` and `exit_undone`, and use `drover supervise` only if routine parks keep waiting overnight.
5. Set [`[usage.weekly]`](#running-past-the-weekly-allotment) `notify_remaining` and `pause_remaining` in the per-user config, so no drive spends the end of the week for you.
6. Handle each event in a fresh operator session.
7. Read `drover status`, or the page `[[status.render]]` writes, whenever you like: reading it costs nothing.

## Where to go next

- [Cost](cost.md): where tokens go in each mode, and the patterns that multiply them.
- [Playbook](playbook.md): sizing items, choosing a reviewer, and the case for and against driving.
- [Configuration](configuration.md#supervise): every `[supervise]` key, `notify` included.
- [Automation](automation.md): the event shapes `drover watch` and `drover attach` print.
