# Releasing

A release exists so a bug report can name the binary it came from. Everything below serves that: the tag names the source, the pipeline stamps it into each artifact, and `drover version` reads it back.

## The tag

`vMAJOR.MINOR.PATCH`, annotated, on a commit already merged to `main`. The `v` prefix is not decoration: `on: push: tags: ["v*"]` in [`.github/workflows/release.yml`](../.github/workflows/release.yml) matches on it, and Go's module proxy serves no version without it.

```bash
git switch main && git pull
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Update `CHANGELOG.md` and merge that first. The workflow generates release notes from the commits since the last tag, which is a list rather than a summary, and nothing edits the changelog for you.

A minor bump takes [`SECURITY.md`](../SECURITY.md) with it. Its supported-versions table names one minor and nothing else, so both rows move to the minor you are cutting: `| vX.Y.x | Yes |` and `| < vX.Y | No — upgrade |`. Merge that with the changelog, before the tag exists. `TestSupportedVersionsTableNamesTheCurrentMinor` reads the table in one direction: it may name the minor you are cutting, never an older one. So the pull request that moves the rows passes, and a tag pushed over a table still naming the previous minor fails `make check` inside the release job and publishes nothing.

The same pull request sets `drover_version` in [`examples/.drover.toml`](../examples/.drover.toml) to the release you are cutting, by hand, since `drover config --stamp` writes the running binary's release and that is still the old one. `make example` runs `drover check` over that project, and a binary of the new minor refuses a config stamped with the previous one, so a tag pushed without the change fails the release job the same way. A stamp newer than the binary passes, which is what lets the pull request merge before the tag exists.

## What the pipeline does

One job, in this order. Each step failing stops the job, so the ordering is the guarantee:

1. Checkout at `fetch-depth: 0`. The Makefile reads `VERSION` from `git describe --tags`, which finds no tag in the default shallow clone and would stamp every artifact `dev`.
2. Install `golangci-lint` and `govulncheck` at the versions `ci.yml` pins.
3. `make build && make check`: build, then lint, test and `govulncheck`. This runs before `dist` exists, so a tag that fails `make check` publishes nothing.
4. `make dist`.
5. Run the `linux/amd64` artifact and require its `drover version` to name the tag, a hex commit and an ISO date. An artifact whose `-ldflags` went nowhere fails here rather than shipping.
6. `gh release create` with every file in `dist/`.

The workflow's `permissions:` block grants `contents: write` and nothing else, which is what creating a release and uploading its assets needs. Every action is pinned by commit SHA with the version in a trailing comment; publishing runs through the runner's own `gh`, so the pinned surface is `checkout` and `setup-go`.

## What it stamps

`make dist` cross-compiles the five targets `ci.yml` already builds on every pull request, so a target that stopped compiling never reaches a tag. Each binary is built `-trimpath` with three `-ldflags`:

| Variable               | Source                                 | Reads back as                  |
| ---------------------- | -------------------------------------- | ------------------------------ |
| `internal/cmd.version` | `git describe --tags --always --dirty` | `drover v0.1.0`                |
| `internal/cmd.commit`  | `git rev-parse --short HEAD`           | `commit: 3a9f1c2`              |
| `internal/cmd.date`    | `date -u +%Y-%m-%dT%H:%M:%SZ`          | `built:  2026-08-26T22:03:52Z` |

`VERSION` reads `--dirty`, so run `make dist` by hand only on a clean checkout of the tag. A stray edit ends up in every filename and in every binary.

Artifacts are named `drover_<version>_<os>_<arch>`, with `.exe` on Windows. `checksums.txt` is `shasum -a 256` over those five files and nothing else, so `shasum -a 256 -c checksums.txt --ignore-missing` verifies whichever one a reader downloaded.

## What each install path can report

Three kinds of build reach users, and `drover version` distinguishes them:

| Install                                | version        | commit                    | built       |
| -------------------------------------- | -------------- | ------------------------- | ----------- |
| Release binary, or `make install`      | the tag        | short hash                | build time  |
| `go install ./cmd/drover` from a clone | pseudo-version | full hash                 | commit time |
| `go install …@version` from the proxy  | the tag        | `unrecorded (module zip)` | same        |

The third is not a defect to chase. A module zip carries no `vcs.revision` and no `vcs.time`, so nothing inside the binary can recover them; `drover version` prints the reason and names the paths that do carry a commit rather than reporting `none`. A build with no VCS behind it at all still reports `dev`, `none` and `unknown`, which is the honest answer for a binary nothing stamped.

## After the tag

```bash
gh run watch                                    # the Release run, until it is green
gh release view v0.1.0                          # six assets: five binaries, one checksums.txt
```

Then download one and run it:

```bash
shasum -a 256 -c checksums.txt --ignore-missing
./drover_v0.1.0_linux_amd64 version
```

The first line must be `drover v0.1.0`, and neither `commit:` nor `built:` may say `unrecorded`. If either does, the checkout was shallow or the `-ldflags` did not reach the build; delete the release and the tag, fix it, and tag again.

A tag that fails `make check` leaves no release behind, so the recovery is to delete the tag and re-push it once `main` is green:

```bash
git push origin :refs/tags/v0.1.0 && git tag -d v0.1.0
```

## Not in the pipeline

- **Signing and attestation.** Nothing signs the artifacts and nothing publishes provenance. `checksums.txt` proves the download matches what the release holds, not who built it.
- **Publishing a module.** Nothing here publishes to the module proxy. `go install …@latest` resolves off the tag on its own, by proxy or by clone. [What each install path can report](#what-each-install-path-can-report) says what that build can stamp, and what it cannot.
- **Homebrew, or any package manager.** A downloaded binary and `make install` are the two supported paths.
