---
name: Enhancement
about: drover should do something it does not do yet
labels: enhancement
---

<!-- Leave the `enhancement` label on. `drover intake` reads it to pick the id
     namespace, and refuses an issue whose labels match none of them. -->

## The problem

What you were trying to get done, and where drover made you stop or work around it. Describe the situation rather than the feature — a request naming the fix gives away the one thing worth arguing about.

## What you do today

The commands, script or manual step you use instead.

## What you have ruled out

Configuration is the first answer here more often than code is. Say which `.drover.toml` keys you tried, or why none of them reach it. If this is a case where a project's own config should decide rather than drover, say that too.

## `drover version`

Command surfaces move with the binary, so an enhancement request against an older build may already be answered in a newer one.

```
$ drover version
```

## The `.drover.toml` you ran against

The config is what makes the request concrete: it says what your ids, paths, gates and runners already look like.

```toml

```
