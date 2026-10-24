# Cost

drover is cheap to run well and expensive to run badly. The same backlog can finish inside a usage limit or exhaust it days early, depending on how the sessions around it are arranged, because almost all of the difference comes from patterns outside the work items themselves. This page explains where tokens go, what drover does to bound them, and the patterns that multiply them. [Operating modes](operating.md) covers the modes this page refers to.

drover spends tokens on your account through the runners you configure, and it can't see your plan's limit or how much of it is left. Your provider's usage page is the only record of headroom.

## How a session spends tokens

Each turn of a session sends the whole conversation to the model again: the system prompt, the item, every file read, every tool result and every reply so far. Prompt caching makes re-sending a prefix cheaper per token than sending it fresh, but it isn't free, and providers count cached reads toward usage limits too.

So the cost of a turn follows the size of the context, not the size of the request. A session whose context has grown to 300,000 tokens reads those 300,000 tokens again on its next turn, whether that turn writes a feature or answers "is the build green yet?". The cost of a whole session is roughly its turn count times its average context, and both grow the longer it stays open. Compaction summarizes the conversation and starts the context over, then the session fills it again.

Two consequences follow, and the rest of this page is built on them:

- **A short session is cheap per turn.** A session that starts from one item's prompt and ends when that item is done never carries more than one item's context.
- **A turn that does nothing still costs a full read.** A session that wakes to check on something pays for its entire context, every time, even when the answer is "not yet".

## What drover bounds

drover's design keeps sessions short where it controls them. Each of the following limits applies without any configuration:

- **One item per session.** A work item is sized so one run can finish it ([Size the item](playbook.md#1-size-the-item)), and the session ends when the run does.
- **A fresh session per stage.** The review starts from the diff and the contract, not from the implementer's conversation, so neither session inherits the other's context.
- **Bounded review rounds.** `[review] max_review_rounds` caps how many times one item can go back, and a review that sends back the same evidence twice parks instead of looping.
- **Waiting outside the model.** `drive` waits for checks, merges, slots and the trunk in its own loop, and `drover watch` blocks without a session behind it.
- **Bounded drives.** `--for`, `--until` and `--max-items` put a ceiling on how much one drive can launch ([Bound every drive](playbook.md#bound-every-drive)).

The patterns in the following section all live outside those limits, in sessions drover didn't launch and can't bound.

## Patterns that multiply cost

Each of these costs nothing extra on any single turn. They add up over days, and none of them produces an error, so the first sign is usually a limit reached earlier than expected.

### Long-lived sessions

An interactive session kept open as a console across many drives, used to read results, accept items, check CI and land merges, grows its context with every one of those. Each later turn pays for all of it. Over days such a session can cost more than the drives it supervises while doing none of their work.

Open a session for a decision and close it when the decision is made ([Operator sessions](operating.md#operator-sessions)). If you find yourself resuming one for a week, the context it carries is history you are paying to re-read on every turn.

### Polling from a session

A background loop started from inside an agent session, such as `sleep 60` followed by a status check, wakes the session every time it finishes. Each wake is a turn, and each turn is a full read of the context. Watching a drive's next row, a CI run or a deploy this way turns minutes of waiting into dozens of turns.

Let something that isn't a model do the waiting:

- `drover drive --detach` waits for everything a drive waits on, at no token cost.
- `[supervise] notify` tells you when a drive needs a person, so nothing has to watch for it.
- `drover watch --until <events>` blocks until an event arrives, so a session that must wait wakes once per event instead of once per check.

### Parallel work

`--jobs N` runs N items at once, so a queue finishes about N times sooner and spends tokens about N times as fast. A usage limit belongs to the account, not the project, so drives in three projects at `--jobs 3` each spend as fast as nine sessions do, plus whatever interactive sessions are open beside them.

Parallelism doesn't make an item cheaper; it moves the same spend earlier. Raise `--jobs` when finishing sooner is worth reaching the limit sooner, and count every project that drives at the same time against the same limit.

### Oversized items

An item too large for one run goes back for changes, comes back larger, and is reviewed against a diff that keeps growing. Each round is two more sessions, each reading more code than the last. Splitting it into a chain costs some re-reading across the parts and almost always costs less than the rounds it avoids ([Splitting into a chain](playbook.md#splitting-into-a-chain)).

### Review loops

A review that keeps sending an item back is spending two sessions per round on a disagreement that more rounds won't settle. `[review] max_review_rounds` stops it, and raising the limit is rarely the fix: an item that needs many rounds usually needs a narrower contract or a clearer requirement. [How many rounds](playbook.md#how-many-rounds) covers the choice.

### Model and context window

The runner's argv decides the model. A model with a very large context window lets a session grow far past the point where a smaller one would have compacted, and every turn after that pays for the larger context. Choose the model in `[runners]` per stage if a cheaper one does the job: see [Configuration](configuration.md#runners).

## What you spent

drover reports cost only for the sessions it records. The following command prints a headless run's outcome line, including its turn count and the cost its runner reported:

```bash
drover report B1
```

```text
outcome: success · error false · 72 turns · $9.90
```

The dollar figure is the runner's own `total_cost_usd`, at the provider's list price. On a subscription plan it isn't what you pay, and it doesn't measure the plan's limit, which providers weight in their own way. Use it to compare runs with each other, not to predict when a limit arrives. Every headless log under the log directory keeps the same result event, so the figures stay readable after the item merges.

Not yet covered:

- Token counts per session are not recorded in the ledger, so `drover status` and the built-in dashboards show no usage.
- Interactive sessions, including operator sessions, leave no log, so drover records nothing about what they spent.
- `drive` has no budget: nothing holds dispatch as an account approaches its limit. Bound each drive with `--for` or `--until` and `--max-items` instead.

## Where to go next

- [Operating modes](operating.md): each way of running drover, and the one to settle into.
- [Playbook](playbook.md): sizing items and bounding drives, the two choices that move cost the most.
- [Configuration](configuration.md#review): `[review] max_review_rounds` and the other bounds.
