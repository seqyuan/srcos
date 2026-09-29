#!/usr/bin/env bash
#
# scripts/verify-commits.sh — verify that each commit in a range can build, vet
# and test *on its own* (AGENTS.md「每个提交要能独立 build + test 通过」).
#
# It uses `git worktree`: every commit is checked out into a throwaway
# directory, so the caller's working tree and HEAD are NEVER touched. That is
# the whole point — a naive `git checkout --detach <sha>` loop mutates the
# shared checkout's HEAD and, if it is killed (SIGKILL), leaves HEAD detached
# (a trap does not fire on SIGKILL). This version is safe to run while an agent
# or a developer is using the same repository, and safe to interrupt.
#
# Usage:
#   scripts/verify-commits.sh [<rev-range> | <commit>]
#
#   <rev-range>   a git rev-list range, e.g. `origin/main..HEAD` (default),
#                 `v1.2.3..HEAD`, `HEAD~5..HEAD`.
#   <commit>      a single commit (verified alone).
#   Default: @{upstream}..HEAD when an upstream is set, else origin/main..HEAD.
#
# Environment:
#   GO=/path/to/go         go binary (default: `go`)
#   SRCOS_VERIFY_KEEP=1    keep the temp dir with per-commit logs on exit
#
# Exit: 0 if every commit passes; 1 if any fails; 2 on bad usage.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
cd "$ROOT" || exit 2

git rev-parse --git-dir >/dev/null 2>&1 || { echo "not a git repository: $ROOT" >&2; exit 2; }

range="${1:-}"
if [ -z "$range" ]; then
  if upstream="$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null)"; then
    range="${upstream}..HEAD"
  else
    range="origin/main..HEAD"
  fi
fi

# A bare commit means "verify this one"; a range means "verify every commit in
# it". Without this distinction `git rev-list <sha>` would mean "this commit
# and all its ancestors".
case "$range" in
  *..*) commits="$(git rev-list --reverse "$range" 2>/dev/null)" ;;
  *)    commits="$(git rev-parse "$range" 2>/dev/null)" ;;
esac
if [ -z "$commits" ]; then
  echo "nothing to verify in range: $range"
  exit 0
fi

tmp_parent="$(mktemp -d "${TMPDIR:-/tmp}/srcos-verify.XXXXXX")"
log="$tmp_parent/verify.log"
worktrees=()

cleanup() {
  local wt
  for wt in ${worktrees[@]+"${worktrees[@]}"}; do
    git worktree remove --force "$wt" >/dev/null 2>&1
  done
  git worktree prune >/dev/null 2>&1
  if [ "${SRCOS_VERIFY_KEEP:-0}" = "1" ]; then
    echo "[verify] kept: $tmp_parent"
  else
    rm -rf "$tmp_parent"
  fi
}
trap cleanup EXIT INT TERM

n="$(printf '%s\n' $commits | wc -l | tr -d ' ')"
printf 'verifying %s commit(s) in %s\n\n' "$n" "$range"

pass=0
fail=0
for sha in $commits; do
  subj="$(git log -1 --format='%h %s' "$sha")"
  wt="$tmp_parent/wt-$sha"
  if ! git worktree add -q --detach "$wt" "$sha" 2>"$tmp_parent/wt-err.log"; then
    printf 'FAIL  %s  (worktree)\n' "$subj"
    sed 's/^/      /' "$tmp_parent/wt-err.log"
    fail=$((fail + 1))
    continue
  fi
  worktrees+=("$wt")
  # A fresh worktree shares the module cache, so repeated runs are fast (go's
  # test cache is content-keyed); `make vet && make test` is the local bar.
  if (cd "$wt" && "$GO" build ./... && "$GO" vet ./... && "$GO" test ./...) >"$log" 2>&1; then
    printf 'PASS  %s\n' "$subj"
    pass=$((pass + 1))
  else
    printf 'FAIL  %s\n' "$subj"
    tail -15 "$log" | sed 's/^/      /'
    fail=$((fail + 1))
  fi
  git worktree remove --force "$wt" >/dev/null 2>&1
done

echo
if [ "$fail" -eq 0 ]; then
  printf 'ALL %d COMMIT(S) PASS\n' "$pass"
  exit 0
fi
printf 'SOME COMMITS FAILED: %d passed, %d failed\n' "$pass" "$fail"
exit 1
