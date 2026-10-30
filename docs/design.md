# Design

drover runs items. It does not decide what they should be. An item is only as good as the design behind it: one written against an interface nobody settled sends the review loop after a moving target, and the loop then argues the design one finding at a time, two sessions per round. This page covers the work before the first `drover new`, which is getting from an idea to a design, from the design to a few items, and back to the design as those items ship.

## The loop

The work goes in this order, and the last step returns to the first:

1. Write the design in an interactive session.
2. Spike what the design cannot answer yet.
3. Ship the thinnest slice that runs end to end.
4. Cut items along the seams the design settled.
5. Write a few items ahead, ship them, and revise the design from what landed.

None of this needs drover until step 3. Steps 1 and 2 are conversations and throwaway code, and an item there would have nothing falsifiable to review.

## Design doc

Write it with an agent, interactively. The agent is good at proposing options, finding the case your sketch missed and drafting the prose; you decide. Argue the alternatives in the session and write down the ones you rejected, with the reason, because the next session (and every reviewer after it) reopens a choice the document does not defend.

A page or two covers most projects:

- **Problem:** who has it, and what they do today instead.
- **Non-goals:** what this will not do, so no item grows into it.
- **Constraints:** compatibility, platforms, performance bounds, and anything an outside party owns.
- **Interfaces:** the commands, APIs, file formats and wire formats other code will depend on.
- **Data model:** what is stored, where, and how it migrates.
- **Open questions:** each one named, with what would settle it.

Keep it in the repository where items can cite it, such as `docs/design/`. An open question blocks every item that touches it: settle it in the design session or with a spike before writing those items, not inside one.

## Spikes

Some questions only code answers: whether a library handles the case you need, how slow the obvious approach is, what a third-party API returns on a bad day. Answer each in an interactive session on a scratch branch, write the answer into the design doc, and throw the code away.

A spike is not a drover item. It has no requirement anyone could falsify ("find out whether X works"), so a review has nothing to measure, and the code it leaves was written to answer a question rather than to be kept.

## First item

Make the first item the thinnest slice that runs end to end: one input through every layer to its output, with a test, CI, and the verify command the project will use from then on. Run it interactively (see [Playbook](playbook.md#2-interactive-or-headless)).

That item does three jobs a feature item cannot:

- **It gives drive something to trust.** `[verify]` and `[drive] merge_gate` decide every later merge, so they have to exist and pass before anything runs unattended.
- **It sets the conventions.** A session copies the patterns it finds in the tree, so the package layout, error style and test helpers in the first slice turn up in every item after it.
- **It tests the design.** An interface that looked right on the page and is awkward in code shows up here, while changing it costs one item.

## Items

Cut items along the interfaces the design settled. Each item owns one side of a seam, and `deps:` follows the direction the design says depends on what. A stored-format change and its migration are one item. [Playbook](playbook.md#1-size-the-item) covers sizing and writing each one.

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

Authentication is an example. The session store, the login handler and the middleware that rejects unauthenticated requests are three chained items. Password reset, rate limiting and the user-table migration get written after those three ship, once the session record exists and you know what it looks like.

Queue a batch in one drive, dependencies first. drover merges each item as it passes and finishes it in the same drive, so a dependent queued behind it runs that night.

## Decisions

A decision that applies to every item belongs where every session reads it, not only in the design doc:

- **`[agents] principles`:** one line stating the bar a reviewer holds the code to. The drafting prompt carries it as well. See [Agent instructions](agents.md#the-one-thing-it-cannot-infer).
- **`[agents] addendum`:** a file appended to the generated agent instructions, for conventions longer than one line.
- **`drover invariant`:** a property one item's later passes must keep, appended to its body.

## When the design is wrong

The loop tells you when the design is wrong. An item parks `needs_decision` because a requirement cannot be met as written; reviews keep sending back the same finding; you run `drover amend` on the same area twice. Each of these is a design question showing up as an implementation failure.

Stop driving that area. Go back to the design session, change the document, then amend the affected items with `drover amend` or set them aside with `drover shelve`. Every round the review loop spends on a design question costs two sessions and settles one finding; the design session settles all of them at once.

## Where to go next

- [Playbook](playbook.md): sizing, writing and accepting one item, and deciding who reviews it.
- [Lifecycle](lifecycle.md): every step an item goes through, and what each one refuses.
- [Configuration](configuration.md#paths): `[paths] specs` and the other directories drover reads.
- [Agent instructions](agents.md): the principles line and the addendum.
