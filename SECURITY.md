# Security

## Reporting

Report privately through GitHub: **[open a draft advisory](https://github.com/ravinald/drover/security/advisories/new)**. It goes to the maintainer and nobody else, and it stays unreadable until there is a fix to publish alongside it.

Do not open an issue for a vulnerability. The tracker is the front door here: `drover intake` turns issues into work items, so a filed report is public, indexed and on a work item's record before anyone reads it.

Expect an acknowledgment within a week. drover has one maintainer, so that is a realistic figure rather than a service level: if the report is real, the fix lands in a patch release and the advisory names you unless you would rather it did not.

## Supported versions

drover is pre-1.0, so the supported set is the current minor and nothing else.

| Version | Supported    |
| ------- | ------------ |
| v0.1.x  | Yes          |
| < v0.1  | No — upgrade |

A fix ships as a new patch on the current minor. Nothing is backported to an earlier one; a minor bump before 1.0 is allowed to change behavior, which is what makes backporting a guess rather than a cherry-pick.

## By design, not a vulnerability

`.drover.toml` is executable configuration. The full account is [`## Trust` in `docs/concepts.md`](docs/concepts.md#trust); the short version is that a repository you cloned brings its own config, and three keys in it reach past drover's own process. Reports describing the following are answered there:

- **`[runners]` launches an arbitrary binary with your privileges.** That is what a runner is. drover keeps no allowlist of runner binaries and does not prompt before launching one, because anyone running a cloned repository's build made that decision several commands earlier.
- **`[verify]` is a command drover runs.** `run` renders it into the prompt for the agent to execute, and `drive` executes it itself after a branch update, in the item's worktree, with your privileges. Read it the way you would read a Makefile target.
- **`[github] repo` decides which repository `gh` writes to.** A config naming someone else's repository sends `intake`'s label there.
- **A cloned repository's `.drover.toml` is read without confirmation.** Reading it before `drover run` is the operator's job. What drover does instead is print the runner name, the resolved binary and the full argv to stderr before it starts, so a run that launched something unexpected is in the scrollback. `drover run <id> --dry-run` shows the same thing and starts nothing.

## In scope

Anything that escapes that model:

- drover executing something no key in `.drover.toml` named.
- drover reading or writing outside the paths `[paths]` resolves to, including a path traversal through an id, a slug or a frontmatter value.
- Ledger entries that can be forged or silently rewritten. It is append-only and diffable in git on purpose; a way to lose that is a finding.
- Credential exposure. drover shells out to `gh` and never reads a token itself, so a path where a token reaches drover's process, its output or the ledger is a bug regardless of how it got there.
