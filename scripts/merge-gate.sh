#!/usr/bin/env bash
# The go/no-go drover drive asks before merging a branch.
#
#   merge-gate.sh <branch> <trunk>
#
#   0  green, merge
#   1  red, and this diff caused it: the output goes back to the branch
#   2  red, and the code did not cause it: stop the drive
#   4  the branch is behind the trunk and nothing else was asked: drive catches
#      it up and asks once more. Never on a merged head: drive sets
#      DROVER_GATE_MERGED=1 when the forge has already merged it, and a squash
#      or a rebase leaves a merged head that is no ancestor of the trunk
#
# The forge is the authority, not the local tree. A local `make check` passes on
# a machine whose PATH, toolchain and caches differ from the runner's, and the
# branch has already passed the item's own verify command before it committed;
# what nobody has asked yet is whether the checks the trunk requires went green.
#
# It asks about a commit rather than about a pull request. drover pushes what it
# drives, so the branch head is on the remote and its check runs are readable
# before any pull request exists — which is the point, because drover opens one
# only for a branch this script has already passed. Asking `gh pr checks` would
# need the document whose existence this answer decides.
#
# Everything that is neither a failing check nor an accepted conclusion exits 2.
# Pending is not green and not the code's fault, and a drive that treated "no
# answer yet" as either would merge on a check that had not run or spend a round
# re-fixing code nothing found wrong.
#
# So it waits: CI starts on the push, runs through the review, and is
# usually finished by the time this asks, but a review that committed a fix moves
# the head and starts the clock again. MERGE_GATE_WAIT bounds that wait and must
# exceed the project's longest run; reaching it stops the drive by design.
set -uo pipefail

branch=${1:?usage: merge-gate.sh <branch> <trunk>}
trunk=${2:?usage: merge-gate.sh <branch> <trunk>}

wait_for=${MERGE_GATE_WAIT:-900}
poll=${MERGE_GATE_POLL:-15}

command -v gh >/dev/null 2>&1 || {
	echo "merge-gate: gh is not on PATH, so nothing here can read the forge" >&2
	exit 2
}

sha=$(git rev-parse --verify "$branch" 2>&1) || {
	echo "merge-gate: cannot resolve $branch: $sha" >&2
	exit 2
}

# Asked before the forge, because checks that passed on a head the trunk has
# since moved past say nothing about the merge. origin's trunk where there is
# one: the local trunk can lag it, and a drive merges into origin's.
git fetch --quiet origin "$trunk" 2>/dev/null || true
base=$trunk
if git rev-parse --verify --quiet "origin/$trunk^{commit}" >/dev/null; then
	base=origin/$trunk
fi
# A head the trunk already contains is merged rather than behind, and asking
# it to catch up would refuse a merge that cannot be undone.
if [ "${DROVER_GATE_MERGED:-}" != 1 ] &&
	git rev-parse --verify --quiet "$base^{commit}" >/dev/null &&
	! git merge-base --is-ancestor "$sha" "$base" &&
	! git merge-base --is-ancestor "$base" "$sha"; then
	echo "merge-gate: $branch is behind $base, which moved since it was pushed; catch it up and ask again" >&2
	exit 4
fi

deadline=$(($(date +%s) + wait_for))
while :; do
	# --paginate, because a check-run listing is paged: a repository with more
	# checks than one page returns a green first page while the failure sits on the
	# second, and a gate that read one page would call that green.
	runs=$(gh api --paginate "repos/{owner}/{repo}/commits/$sha/check-runs" \
		--jq '.check_runs[] | "\(.status) \(.conclusion) \(.name)"' 2>&1) || {
		echo "merge-gate: the forge would not answer about $sha: $runs" >&2
		exit 2
	}

	# Three buckets, and the third is the one worth naming. A conclusion is
	# accepted only if it is on the list: cancelled, stale, action_required and
	# anything GitHub adds later are not failures this diff caused and are not
	# evidence that it passed, so they wait and then stop the drive rather than
	# merging on a check nobody watched finish.
	failed=$(printf '%s\n' "$runs" |
		awk '$2 == "failure" || $2 == "timed_out" || $2 == "startup_failure"')
	if [ -n "$failed" ]; then
		echo "merge-gate: a required check failed on $sha" >&2
		printf '%s\n' "$failed" >&2
		exit 1
	fi

	total=$(printf '%s' "$runs" | grep -c . || true)
	unresolved=$(printf '%s\n' "$runs" |
		awk '$1 != "completed" || ($2 != "success" && $2 != "neutral" && $2 != "skipped")')
	pending=$(printf '%s' "$unresolved" | grep -c . || true)
	if [ "$total" -gt 0 ] && [ "$pending" -eq 0 ]; then
		exit 0
	fi

	if [ "$(date +%s)" -ge "$deadline" ]; then
		if [ "$total" -eq 0 ]; then
			# A commit no workflow reported on reads the same as one whose
			# workflow has not started. Say which this is, because the fixes
			# differ: a paths filter or a fork restriction against a slow runner.
			echo "merge-gate: no workflow reported on $sha after ${wait_for}s." >&2
			echo "merge-gate: the project's CI has to run on a branch push, or nothing can answer before a pull request exists" >&2
		else
			echo "merge-gate: $pending of $total checks on $sha are unresolved after ${wait_for}s" >&2
			echo "merge-gate: unresolved means still running, cancelled, stale, or a conclusion this gate does not accept; none of them is a pass and none is this diff's fault" >&2
			printf '%s\n' "$unresolved" >&2
		fi
		exit 2
	fi
	sleep "$poll"
done
