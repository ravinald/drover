#!/usr/bin/env bash
# ci-same-tree.sh <owner/repo> <sha>
#
# Writes passed=true to $GITHUB_OUTPUT when <sha> merged a pull request whose
# head has the same tree and a successful Gate check run. Exits 0 in every other
# case without writing, so CI falls back to running the full gate: this script
# may only skip work, never fail a build.
set -uo pipefail

repo=${1:?usage: ci-same-tree.sh <owner/repo> <sha>}
sha=${2:?usage: ci-same-tree.sh <owner/repo> <sha>}

skip() { echo "same-tree: $*; running the full gate"; exit 0; }

command -v gh >/dev/null 2>&1 || skip "gh is not on PATH"

head=$(gh api "repos/$repo/commits/$sha/pulls" --jq '.[0].head.sha // empty' 2>&1) ||
	skip "pull request lookup failed: $head"
[ -n "$head" ] || skip "$sha merged no pull request"

ours=$(git rev-parse "$sha^{tree}" 2>&1) || skip "cannot read the tree of $sha: $ours"
theirs=$(gh api "repos/$repo/git/commits/$head" --jq .tree.sha 2>&1) ||
	skip "cannot read the tree of $head: $theirs"
[ "$ours" = "$theirs" ] || skip "$sha's tree differs from pull request head $head"

passed=$(gh api --paginate "repos/$repo/commits/$head/check-runs" \
	--jq '.check_runs[] | select(.name == "Gate" and .conclusion == "success") | .id' 2>&1) ||
	skip "check-run lookup failed: $passed"
[ -n "$passed" ] || skip "Gate has not passed on $head"

echo "same-tree: $sha has the tree $head already gated; skipping the gate"
echo "passed=true" >>"${GITHUB_OUTPUT:-/dev/stdout}"
