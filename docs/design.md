# Design

drover runs items. It does not decide what they should be. An item is only as good as the design behind it: one written against an interface nobody settled sends the review loop after a moving target, and the loop then argues the design one finding at a time, two sessions per round. This page covers the work before the first `drover new`, which is getting from an idea to a design, from the design to a few items, and back to the design as those items ship.

## The loop

The work goes in this order, and the last step returns to the first:

1. Write the design in an interactive session.
2. Spike what the design cannot answer yet.
3. Ship the smallest slice that runs end to end through the riskiest part of the design.
4. Cut items into outcomes someone can verify.
5. Write a few items ahead, ship them, and revise the design from what landed.

Design and exploration default to interactive sessions. Work comes into drover once its deliverable and its completion criteria are settled, which usually means the first implementation slice. A bounded research question can be an item too, on the terms in [Spikes](#spikes).

## Design doc

Write it with an agent, interactively. The agent is good at proposing options, finding the case your sketch missed and drafting the prose; you decide. Argue the alternatives in the session and write down the ones you rejected, with the reason, because the next session (and every reviewer after it) reopens a choice the document does not defend.

A page or two covers most projects:

- **Problem:** who has it, and what they do today instead.
- **Success criteria:** the observable result that makes the project worth doing, and how the first slice moves toward it.
- **Non-goals:** what this will not do, so no item grows into it.
- **Constraints:** compatibility, platforms, performance bounds, and anything an outside party owns.
- **Interfaces:** the commands, APIs, file formats and wire formats other code will depend on.
- **Data model:** what is stored, where, and how it migrates.
- **Failure behavior:** errors, retries, partial writes and recovery, at the boundaries where they matter.
- **Release and recovery,** where it applies: what has to exist before users see the feature, how it is switched on, and how a failed migration is undone.
- **Open questions:** each one named, with who settles it and what evidence would.

Keep it in the repository where items can cite it, such as `docs/design/`.

## Readiness

The design is ready for items when the next slice has a measurable outcome, settled interfaces, a way to verify it, and no open question that could change its acceptance criteria. That bar applies to the next slice, not to the whole project: a question about a feature three batches out does not hold up the first one. A question that does touch the next slice gets settled in the design session or with a spike before you write that item, never inside it.

## Spikes

Some questions only code answers: whether a library handles the case you need, how slow the obvious approach is, what a third-party API returns on a bad day. Set the question, the time you will spend, the experiment and the threshold that decides it before you start. Record the environment, the command, the result and the decision it led to in the design doc, with enough detail to reproduce it. A negative result settles the question as well as a positive one.

Run a spike interactively while the question still needs exploring, on a scratch branch. The prototype code is thrown away unless it goes through an item's implementation and review like any other code.

A spike can be a drover item once it is bounded, as long as it commits something a review can check. Committing a benchmark script and its recorded output against a stated threshold works, and so does committing a test harness that exercises the third-party API and the responses it saw. A recommendation in prose with nothing behind it gives the merge gate and the review nothing to read. The requirements ask for the measurement and the recommendation, not for a favorable answer. If the budget runs out with the question open, the item records what is still unknown, and you decide whether another experiment is worth its cost.

## First item

Make the first item the smallest path that runs end to end through the riskiest part of the design, including one representative failure case. A skeleton that connects empty layers and skips the hard boundary passes its tests and proves nothing about the design. Run it interactively (see [Playbook](playbook.md#2-interactive-or-headless)).

That item does three jobs a later feature item cannot:

- **It tests the design.** An interface that looked right on the page and is awkward in code shows up here, while changing it costs one item.
- **It sets the conventions.** A session copies the patterns it finds in the tree, so the package layout, error style and test helpers in the first slice turn up in every item after it.
- **It gives drive something to trust.** `[verify]` and `[drive] merge_gate` decide every later merge, so they have to exist and pass before anything runs unattended.

Keep setup that lives outside the repository out of the item: credentials, CI settings at the forge, and operator-owned configuration are yours to prepare before the run. Repository changes and executable checks go in the item. A setup or live check that is part of acceptance opens with `[operator]`, as [Playbook](playbook.md#operator-work) describes, rather than landing on an unattended session. The first slice needs useful verification; it does not need every piece of infrastructure because it comes first.

## Items

Cut items into outcomes someone can verify on their own. Settled interfaces bound each item's scope and show where the dependencies are, but they do not dictate one item per layer: an item crosses as many layers as one outcome needs. A stored-format change and its migration are one item.

`deps:` is for an outcome that needs another one merged first, not for two items written in sequence. Two items that edit the same file are not a dependency either; under `drive --jobs` they are held apart at dispatch and run one after the other. [Playbook](playbook.md#1-size-the-item) covers sizing and writing each item.

Point each item at the section of the design it implements. Declare a `spec` key in the project's frontmatter schema, set the directory it resolves against, and name the file in the item:

```toml
[paths]
specs = "docs/design"
```

```markdown
---
title: sessions are stored server-side and expire
deps:
spec: auth.md
---
```

The implementing prompt then opens with "Before writing code, read: docs/design/auth.md." The frontmatter schema is closed, so `drover schema --frontmatter` shows whether this project accepts `spec`. The built-in drafting prompt does not name the spec file, so cite the section in the body when you have `drover draft` write the requirements.

The design doc is context, not contract. A behavior the item must deliver goes in its numbered list, because the review traces those lines and nothing else: a rule stated only in the design doc is on no checklist.

## A few items ahead

Write three to five items, ship them, and read what landed (the diffs, the saved reports, the audit notes) before writing the next batch. Update the design doc with what changed, then write the next items against the updated version.

The first items to ship change the design: a name, a data shape, an error contract. An item written further ahead was written against a design no code has tested yet. It gets amended, shelved or rewritten, or it runs anyway and the review loop argues it into shape one round at a time.

Deferring an implementation is not deferring its design. A later capability can constrain the first storage model or interface, so design those constraints in before the first slice is built, and name which deferred capabilities block a release. An increment can be safe to merge before it is safe to show users; when it is, the design names the control that keeps it hidden and the evidence that lets you remove it.

Authentication is an example. The session store, the login handler and the middleware that rejects unauthenticated requests are three chained items. The known security, compatibility and migration constraints shape the session store's design from the start. The detailed items for password reset, rate limiting and the user-table migration wait until those three ship and the session record exists.

Queue a batch in one drive, dependencies first. drover merges each item as it passes and finishes it in the same drive, so a dependent queued behind it runs that night.

## Decisions

A decision that applies to every item belongs where every session reads it, not only in the design doc:

- **`[agents] principles`:** one line stating the bar a reviewer holds the code to. The drafting prompt carries it as well. See [Agent instructions](agents.md#the-one-thing-it-cannot-infer).
- **`[agents] addendum`:** a file appended to the generated agent instructions, for conventions longer than one line.
- **`drover invariant`:** a property one item's later passes must keep, appended to its body.

## When the design is wrong

The loop shows the symptoms. An item parks `needs_decision` because a requirement cannot be met as written; reviews keep sending back the same finding; you run `drover amend` on the same area twice. Each is a reason to look at the design, not proof it is wrong: the cause can as easily be an implementation defect or an ambiguous requirement, and the findings themselves say which.

When the design is the cause, stop driving that area and change the design before resuming:

1. Record in the design doc what decision changed, and why.
2. Find every item it reaches, by its `spec:` key and by what its requirements cite: scaffolds, accepted items, items a drive is running, and merged ones.
3. Amend each accepted contract with `drover amend`, or set it aside with `drover shelve`. A scaffold is yours to rewrite before acceptance.
4. Let a running item whose assumptions change finish, or stop its drive with `drover drive --stop`, before changing them under it.
5. Write a new item for merged behavior the change makes wrong, carrying its migration where stored data is involved.
6. Resume only when the design, each contract and the code agree.

Editing a design doc changes what every item citing it through `spec:` reads, and leaves each item's body untouched. drover tracks body edits, so `drover check` reports nothing and no requirement changes. Step 2 is the only thing that finds those items.

Every round the review loop spends on a design question costs two sessions and settles one finding. A design session works on the shared decision behind all of them, and either settles it or names the experiment that will.

## Where to go next

- [Playbook](playbook.md): sizing, writing and accepting one item, and deciding who reviews it.
- [Lifecycle](lifecycle.md): every step an item goes through, and what each one refuses.
- [Configuration](configuration.md#paths): `[paths] specs` and the other directories drover reads.
- [Agent instructions](agents.md): the principles line and the addendum.
