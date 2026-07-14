#!/usr/bin/env bash
# fork-upstream-patrol.sh — early-warning check for long-lived fork feature branches.
#
# Compares a feature ref against community main (default: upstream/main), reports
# divergence, hot-file drift, merge conflicts (dry-run), and optionally runs
# targeted Go tests. Never rewrites history, never pushes, never opens PRs.
#
# Exit codes:
#   0  — no action required (merge-clean; optional tests green; within lag budget)
#   1  — attention needed (conflicts, test failure, or lag over threshold)
#   2  — setup error (missing remote/ref, not a git repo, etc.)
#
# See docs/fork-sync-patrol.md

set -euo pipefail

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
UPSTREAM_BRANCH="${UPSTREAM_BRANCH:-main}"
# Feature side: branch name, tag, or commit. Default = current HEAD.
FEATURE_REF="${FEATURE_REF:-HEAD}"
ORIGIN_REMOTE="${ORIGIN_REMOTE:-origin}"
OUT_DIR="${OUT_DIR:-.}"
REPORT_FILE="${REPORT_FILE:-$OUT_DIR/fork-upstream-patrol-report.md}"
# Max commits feature is behind upstream before we flag lag (even if merge-clean).
MAX_BEHIND="${MAX_BEHIND:-50}"
RUN_TESTS="${RUN_TESTS:-1}"
FETCH="${FETCH:-1}"
# Space-separated paths that almost always conflict when community adds agents.
HOT_PATHS="${HOT_PATHS:-server/pkg/agent/agent.go server/pkg/agent/agent_supported_types_test.go server/pkg/agent/models.go server/internal/daemon/config.go server/internal/daemon/daemon.go server/internal/daemon/execenv/runtime_config.go server/migrations}"

log() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 2; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

need_cmd git
need_cmd mktemp

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git repository"
cd "$ROOT"

if [[ "$FETCH" == "1" ]]; then
  if ! git remote get-url "$UPSTREAM_REMOTE" >/dev/null 2>&1; then
    die "remote '$UPSTREAM_REMOTE' not configured (git remote add $UPSTREAM_REMOTE <community-url>)"
  fi
  log "Fetching $UPSTREAM_REMOTE ..."
  git fetch "$UPSTREAM_REMOTE" --prune
  # origin is optional; ignore failures (read-only mirrors, etc.)
  if git remote get-url "$ORIGIN_REMOTE" >/dev/null 2>&1; then
    git fetch "$ORIGIN_REMOTE" --prune 2>/dev/null || true
  fi
fi

UPSTREAM_REF="${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}"
git rev-parse --verify "$UPSTREAM_REF" >/dev/null 2>&1 || die "missing ref $UPSTREAM_REF (fetch failed or branch name wrong)"
git rev-parse --verify "$FEATURE_REF" >/dev/null 2>&1 || die "missing feature ref: $FEATURE_REF"

FEATURE_SHA="$(git rev-parse "$FEATURE_REF")"
UPSTREAM_SHA="$(git rev-parse "$UPSTREAM_REF")"
MERGE_BASE="$(git merge-base "$FEATURE_SHA" "$UPSTREAM_SHA")"

# left = commits reachable from feature not in upstream (ahead)
# right = commits reachable from upstream not in feature (behind)
read -r AHEAD BEHIND <<<"$(git rev-list --left-right --count "${FEATURE_SHA}...${UPSTREAM_SHA}")"

STATUS="ok"
ISSUES=()
HOT_CHANGED=()
CONFLICT_FILES=()
TEST_NOTE="skipped (RUN_TESTS=$RUN_TESTS)"

# Hot paths changed on upstream since merge-base
while IFS= read -r path; do
  [[ -z "$path" ]] && continue
  HOT_CHANGED+=("$path")
done < <(git diff --name-only "$MERGE_BASE" "$UPSTREAM_SHA" -- $HOT_PATHS 2>/dev/null || true)

# Dry-run merge in a disposable worktree (no dirtying the current tree)
WORKTREE="$(mktemp -d "${TMPDIR:-/tmp}/fork-patrol.XXXXXX")"
cleanup() {
  git worktree remove --force "$WORKTREE" 2>/dev/null || true
  rm -rf "$WORKTREE" 2>/dev/null || true
}
trap cleanup EXIT

git worktree add --detach "$WORKTREE" "$FEATURE_SHA" >/dev/null
MERGE_OK=0
(
  cd "$WORKTREE"
  # Avoid editor / GPG prompts
  export GIT_MERGE_AUTOEDIT=no
  if git -c commit.gpgsign=false merge --no-commit --no-ff "$UPSTREAM_SHA" >/dev/null 2>&1; then
    exit 0
  fi
  exit 1
) && MERGE_OK=1 || MERGE_OK=0

if [[ "$MERGE_OK" -eq 0 ]]; then
  STATUS="attention"
  ISSUES+=("merge of $UPSTREAM_REF into $FEATURE_REF has conflicts")
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    CONFLICT_FILES+=("$f")
  done < <(git -C "$WORKTREE" diff --name-only --diff-filter=U 2>/dev/null || true)
  # Fallback: unmerged paths via ls-files
  if [[ ${#CONFLICT_FILES[@]} -eq 0 ]]; then
    while IFS= read -r f; do
      [[ -z "$f" ]] && continue
      CONFLICT_FILES+=("$f")
    done < <(git -C "$WORKTREE" ls-files -u 2>/dev/null | awk '{print $4}' | sort -u || true)
  fi
fi

if [[ "$BEHIND" -gt "$MAX_BEHIND" ]]; then
  STATUS="attention"
  ISSUES+=("feature is ${BEHIND} commits behind $UPSTREAM_REF (threshold MAX_BEHIND=$MAX_BEHIND)")
fi

if [[ "$RUN_TESTS" == "1" ]]; then
  if [[ -d "$ROOT/server/pkg/agent" ]] && command -v go >/dev/null 2>&1; then
    if (
      cd "$ROOT/server"
      go test ./pkg/agent/ -count=1 -run 'Cline|SupportedTypes' >/dev/null 2>&1
    ); then
      TEST_NOTE="go test ./pkg/agent/ -run 'Cline|SupportedTypes' — PASS"
    else
      STATUS="attention"
      ISSUES+=("targeted go tests failed (Cline|SupportedTypes)")
      TEST_NOTE="go test ./pkg/agent/ -run 'Cline|SupportedTypes' — FAIL"
    fi
  else
    TEST_NOTE="skipped (no server/pkg/agent or go not installed)"
  fi
fi

mkdir -p "$(dirname "$REPORT_FILE")"
{
  echo "# Fork upstream patrol report"
  echo
  echo "- Generated (UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "- Repo root: \`$ROOT\`"
  echo "- Feature: \`$FEATURE_REF\` (\`${FEATURE_SHA:0:12}\`)"
  echo "- Upstream: \`$UPSTREAM_REF\` (\`${UPSTREAM_SHA:0:12}\`)"
  echo "- Merge-base: \`${MERGE_BASE:0:12}\`"
  echo "- Ahead / behind: **${AHEAD}** / **${BEHIND}** (threshold behind: ${MAX_BEHIND})"
  echo "- Status: **${STATUS}**"
  echo
  echo "## Issues"
  echo
  if [[ ${#ISSUES[@]} -eq 0 ]]; then
    echo "_None. Merge looks clean and lag is within budget._"
  else
    for issue in "${ISSUES[@]}"; do
      echo "- $issue"
    done
  fi
  echo
  echo "## Hot paths changed on upstream since merge-base"
  echo
  if [[ ${#HOT_CHANGED[@]} -eq 0 ]]; then
    echo "_None of the configured hot paths changed on upstream._"
  else
    for p in "${HOT_CHANGED[@]}"; do
      echo "- \`$p\`"
    done
    echo
    echo "These paths often conflict when community adds agents/runtimes. Review after rebase."
  fi
  echo
  echo "## Conflict files (dry-run merge)"
  echo
  if [[ "$MERGE_OK" -eq 1 ]]; then
    echo "_No conflicts (dry-run merge succeeded)._ "
  elif [[ ${#CONFLICT_FILES[@]} -eq 0 ]]; then
    echo "_Merge failed but no unmerged paths listed; inspect manually._"
  else
    for f in "${CONFLICT_FILES[@]}"; do
      echo "- \`$f\`"
    done
  fi
  echo
  echo "## Tests"
  echo
  echo "- $TEST_NOTE"
  echo
  echo "## Suggested next steps"
  echo
  if [[ "$STATUS" == "ok" ]]; then
    echo "1. No sync required right now."
    echo "2. Re-run after large community agent/daemon releases, or on the weekly schedule."
  else
    echo "1. \`git fetch $UPSTREAM_REMOTE\`"
    echo "2. \`git checkout <your-feature-branch>\`"
    echo "3. \`git rebase $UPSTREAM_REF\` (or \`git merge $UPSTREAM_REF\`)"
    echo "4. Resolve conflicts — keep **both** community agents and your \`cline\` entries in whitelists; renumber migrations if the number was taken."
    echo "5. \`cd server && go test ./pkg/agent/ -run 'Cline|SupportedTypes' && go test ./pkg/agent/\`"
    echo "6. Push to **origin (fork) only**: \`git push --force-with-lease origin <branch>\` after rebase."
    echo "7. Do **not** open a PR against upstream unless you explicitly intend to contribute."
  fi
  echo
  echo "## Config used"
  echo
  echo "| Variable | Value |"
  echo "| --- | --- |"
  echo "| UPSTREAM_REMOTE | \`$UPSTREAM_REMOTE\` |"
  echo "| UPSTREAM_BRANCH | \`$UPSTREAM_BRANCH\` |"
  echo "| FEATURE_REF | \`$FEATURE_REF\` |"
  echo "| MAX_BEHIND | \`$MAX_BEHIND\` |"
  echo "| RUN_TESTS | \`$RUN_TESTS\` |"
  echo "| FETCH | \`$FETCH\` |"
} >"$REPORT_FILE"

log "Wrote report: $REPORT_FILE"
log "Status: $STATUS (ahead=$AHEAD behind=$BEHIND merge_ok=$MERGE_OK)"

if [[ "$STATUS" != "ok" ]]; then
  exit 1
fi
exit 0
