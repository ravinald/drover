# Contributing

By participating here you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## The gate

```
make build && make check
```

`make check` is `lint test vuln`: `golangci-lint run`, `go test ./...`, `govulncheck ./...`. Build comes first and separately, because a lint failure on a tree that will not compile buries the compiler error under a hundred lines that all have the same cause.

That string is not this document's invention. It is the `[gates] default` line in `.drover.toml`, which is where drover reads it from, so it is the same command the tool prints to whoever picks the work up. Run it before you open a pull request.

Go 1.26. `golangci-lint` and `govulncheck` are the only other tools the gate needs; `gh` is optional and only the register commands refuse without it.

## Tests

The packages under `internal/` are pure functions over data and are covered accordingly. `internal/cmd` is where they compose against git, `gh` and the filesystem, so it is where an answer comes back confident and wrong: a branch switch that lands work somewhere nobody looked, a merge check that refuses on a repository shaped unlike this one, a `--json` sample a wrapper already codes against.

The floor for new code in `internal/cmd`: a new command, or a new path through an existing one, arrives with a test that drives it the way a caller does. Against a temporary git repository, against a stub on `PATH`, or through the command itself so the exit code is what gets asserted. Reviewers hold that floor; nothing enforces it.

The suite reads nothing from the environment that started it. `internal/cmd`'s `TestMain` clears `DROVER_ACTOR` and `COLUMNS` for the whole package: the first is exported into every session `drover run` launches and seals the operator commands, the second sizes the status table, and a suite whose result depends on either is testing the shell. A test that wants a value sets it with `t.Setenv`, which layers over the clear and restores it.

There is no coverage threshold and none is planned. A percentage buys tests written for the percentage, and the paths above are not the ones a number sends you to. Keep `go test ./...` under 30 seconds: a suite that shells to git for every case gets skipped, which costs more than it covers.

## Commits

Terse. A subject line, then at most one short paragraph saying why, for someone reading `git log` two years from now with no access to the pull request:

```
fix: race in refreshIndex
```

The subject says what changed. The paragraph is for the reasoning that does not belong in a comment: the alternative you rejected, or the constraint that forced this design.

## Pull requests

No marketing. Say what changed and what you verified. A description that opens by explaining why the feature matters is describing a decision already made by the fact that the branch exists.

Fill in the template: the item id this closes, and confirmation that the gate passed. It is two lines because a longer one gets deleted rather than read.

## How work is tracked

drover drives its own backlog, so the tracking is the tool. A work item is a markdown file, not a ticket field.

```
drover status              # the backlog, and what is READY versus blocked
drover show <id>           # one item's body and its dependencies
drover run <id> --dry-run  # what a run would carry, without starting one
```

Ids are bare in every command (`1`, `2b`, `B1`) and zero-padded in filenames only. `drover status` prints the form to type.

`drover intake <issue-number>` turns a filed issue into a scaffolded item: it allocates the next free id, fills the frontmatter and quotes the issue as raw material. It deliberately stops there. The scaffold carries a stop marker and drover refuses to launch it, or even preview it, until a human deletes that marker and writes the requirements by hand.

### The numbered list is the contract

`drover report <id> --save` turns every top-level line in a body that opens with a number, a period and a space into an audit checkbox. Nothing else in the body becomes one. A requirement written as prose is on no checklist and nobody will ever confirm it, which is the failure this rule exists to prevent.

Cite files and lines. An item saying "improve the handler" produces a change that improves something; one citing `internal/cmd/run.go:120-145` produces a change anyone can check.

File an issue rather than an item; that is the intake path, and it is the one `drover intake` consumes.
