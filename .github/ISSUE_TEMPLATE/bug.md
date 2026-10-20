---
name: Bug
about: drover did something other than what it said it would do
labels: bug
---

<!-- The `bug` label above is not decoration: `drover intake` reads it to pick
     the id namespace, and refuses the issue when no label matches one. -->

## What happened

The command you ran, and what it printed. Paste it rather than describing it — drover's errors are written to name what failed and what to do next, so the literal text is evidence and a paraphrase is not.

```
$ drover ...
```

## What you expected instead

## `drover version`

Every surface moves with the binary: generated help, `drover agents`, the embedded config schema. A report that does not name the build describes something nobody can reproduce.

```
$ drover version
```

## The `.drover.toml` you ran against

Paths, ids, gates and runners all change what drover does, so the config is half the reproduction. Redact a private URL or repository name if you need to; say that you did.

```toml

```

## Environment

- OS and architecture:
- Installed via (`go install`, `make install`, other):
- `gh` present, and authenticated to which host (if the command touches it):
