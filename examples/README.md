# herdsay: a drover demo

`herdsay` is a small `cowsay` clone in Go with no dependencies outside the standard library. It is also a complete drover project: a config, four work items, a ledger and one saved report. Item 1 is already merged, so there is history to read, and three items are waiting to be built by an agent.

| Item | State         | What it asks                                   |
| ---- | ------------- | ---------------------------------------------- |
| `1`  | merged        | A message prints in a bubble above a cow       |
| `B1` | ready         | A long message runs off the side of the bubble |
| `F1` | ready         | The herd has a sheep                           |
| `F2` | blocked by B1 | A thought bubble for thinking out loud         |

`1` has no prefix because it was planned. `B` marks a bug and `F` a feature, as `[ids] namespaces` in [`.drover.toml`](.drover.toml) declares. Commands take the bare id (`B1`); filenames pad it (`B01-...md`).

## Look around

Every read command works right here, with no agent and no setup:

```sh
drover check                # validates the config, the items and the ledger
drover status               # what is ready, what is blocked and why
drover status --all         # merged items included
drover show 1               # the merged item, with its audit history
drover run B1 --dry-run     # the exact prompt an agent would be handed
```

`drover run B1 --dry-run` is the one to read. It shows the item's numbered requirements, the verify command the work must pass, and the branch the run would use. It writes nothing and launches nothing.

## Build an item with an agent

`run` switches branches, so copy the demo out of the drover clone into a git repository of its own first. You need `go` and an agent CLI drover knows: `claude` by default, set in `[run]`.

```sh
cp -R drover/examples /tmp/herdsay && cd /tmp/herdsay
git init -q -b main && git add -A && git commit -qm "herdsay"

drover run B1               # branch, then hand B1 to the agent
drover review B1            # a second session reviews the branch against the requirements
git switch main && git merge --no-ff b01-a-long-message-runs-off-the-side-of-the-bubble
drover mark-merged B1       # record the merge; there is no forge here to ask
drover status               # F2 is ready now that B1 has merged
```

`go run . "a message long enough to need a second line in its bubble"` shows what the agent built.

## Drive the whole queue

`drive` runs items unattended: an implementing session and a reviewing session per item, a pull request for each one that passes, and a merge once `[drive] merge_gate` says the branch is green. It pushes and opens pull requests, so it needs a GitHub repository and an authenticated `gh`.

```sh
gh repo create herdsay --private --source . --push
drover drive --dry-run --jobs 2 B1 F1 F2   # the plan, with nothing launched
drover drive --jobs 2 B1 F1 F2
```

The dry run shows the plan: B1 and F1 run side by side, and F2 waits until B1 merges, because it depends on B1 and both change `bubble` in `say.go`.

Copy this tree as a starting point for a project of your own, or delete it. Nothing else in the drover repository reads it.
