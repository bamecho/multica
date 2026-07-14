# Fork sync patrol

Long-lived **fork feature branches** (custom backends, private adapters) will
diverge from community `main`. This repo ships a **read-only patrol** that
detects drift and merge risk early. It does **not** rewrite branches, push, or
open PRs against upstream.

Related Multica fork work: Cline NDJSON adapter
([`docs/cline-ndjson-multica-adapter-plan.md`](./cline-ndjson-multica-adapter-plan.md)).

---

## What it checks

| Check | Purpose |
| --- | --- |
| Ahead / behind vs `upstream/main` | How far the feature branch has drifted |
| Lag threshold (`MAX_BEHIND`, default 50) | Flag “sync soon” even when merge is still clean |
| Hot-path diff since merge-base | Files that almost always conflict when community adds agents |
| Dry-run merge (temporary worktree) | Real conflict list without dirtying your tree |
| Optional `go test` | `Cline\|SupportedTypes` still green on the feature tree |

**Hot paths (default):**

- `server/pkg/agent/agent.go`
- `server/pkg/agent/agent_supported_types_test.go`
- `server/pkg/agent/models.go`
- `server/internal/daemon/config.go`
- `server/internal/daemon/daemon.go`
- `server/internal/daemon/execenv/runtime_config.go`
- `server/migrations/`

---

## One-time setup (fork clone)

```bash
# Community remote (name must match UPSTREAM_REMOTE, default: upstream)
git remote add upstream https://github.com/multica-ai/multica.git   # if missing
git remote -v

# Your fork should remain "origin"
```

---

## Local run

From the repo root, on the branch you care about (or pass `FEATURE_REF`):

```bash
chmod +x scripts/fork-upstream-patrol.sh   # once

# Current HEAD vs upstream/main
./scripts/fork-upstream-patrol.sh

# Explicit feature branch
FEATURE_REF=feat/cline-ndjson-adapter ./scripts/fork-upstream-patrol.sh

# Report path + skip tests (faster)
OUT_DIR=/tmp/patrol RUN_TESTS=0 ./scripts/fork-upstream-patrol.sh

# Stricter lag budget
MAX_BEHIND=20 FEATURE_REF=feat/cline-ndjson-adapter ./scripts/fork-upstream-patrol.sh
```

Default report file: `./fork-upstream-patrol-report.md` (gitignored if you prefer;
CI uploads it as an artifact). Add to `.gitignore` locally if you do not want it
committed:

```gitignore
fork-upstream-patrol-report.md
```

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | OK — merge-clean, tests green (if enabled), behind ≤ threshold |
| `1` | Attention — conflicts, test failure, and/or too far behind |
| `2` | Setup error — missing remote/ref, not a git repo |

---

## How to read the report

1. **Status `ok`** — no action required this week.
2. **Status `attention`**
   - **Conflict files** → rebase/merge and resolve (see below).
   - **Hot paths only, no conflicts** → still plan a sync soon; next community
     agent PR will likely collide.
   - **Behind > threshold** → sync even if merge is clean, so conflicts stay small.
3. **Tests FAIL** — fix compile/test break after a partial sync, or rebase first.

---

## When the patrol flags attention

```bash
git fetch upstream
git checkout feat/cline-ndjson-adapter   # your branch
git rebase upstream/main                 # or: git merge upstream/main
```

Conflict tips for Multica agent registration:

| File | Resolution hint |
| --- | --- |
| `SupportedTypes` / `New` / `launchHeaders` | Keep **community entries and** your `cline` entry |
| `protocol_family` migration | If number taken, **renumber** your migration; CHECK list = community families **+** `cline` |
| `config.go` probes | Keep both `MULTICA_*_PATH` probes |
| `cline.go` / `cline_test.go` | Usually take yours unless upstream added a real `cline` |

Then:

```bash
cd server
go test ./pkg/agent/ -run 'Cline|SupportedTypes'
go test ./pkg/agent/
git push --force-with-lease origin feat/cline-ndjson-adapter   # fork only, after rebase
```

Do **not** open a PR against `multica-ai/multica` unless you intentionally want
to contribute upstream.

---

## GitHub Actions (fork)

Workflow: [`.github/workflows/fork-upstream-patrol.yml`](../.github/workflows/fork-upstream-patrol.yml)

| Trigger | Behavior |
| --- | --- |
| Weekly cron | Runs on forks only (`github.repository_owner != 'multica-ai'`) |
| `workflow_dispatch` | Manual run; inputs for feature branch and thresholds |

On failure (exit 1), the job fails so the run is red — treat that as “sync this
branch”. The markdown report is uploaded as a workflow artifact.

### Recommended fork settings

1. Actions enabled on the fork.
2. Optional repo variable / default branch: keep a long-lived feature branch name
   stable (e.g. `feat/cline-ndjson-adapter`) so the schedule input stays valid.
3. Do **not** grant the workflow write permissions to push remotes (default
   `contents: read` is enough).

### Manual dispatch

GitHub → Actions → **Fork upstream patrol** → Run workflow → set
`feature_ref` to your branch.

---

## What this deliberately does not do

- Auto-rebase or force-push your feature branch  
- Open PRs to upstream or to fork `main`  
- Merge community `main` into your branch without a human  
- Replace a real review of migration / whitelist conflicts  

Patrol = **early warning**. Sync = **you (or an agent you invoke)** apply
rebase + tests + push to the fork.

---

## Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `UPSTREAM_REMOTE` | `upstream` | Community remote name |
| `UPSTREAM_BRANCH` | `main` | Community branch |
| `FEATURE_REF` | `HEAD` | Feature branch / SHA to inspect |
| `ORIGIN_REMOTE` | `origin` | Fork remote (fetch only) |
| `OUT_DIR` | `.` | Directory for the report |
| `REPORT_FILE` | `$OUT_DIR/fork-upstream-patrol-report.md` | Report path |
| `MAX_BEHIND` | `50` | Flag if behind by more than this many commits |
| `RUN_TESTS` | `1` | Set `0` to skip Go tests |
| `FETCH` | `1` | Set `0` if remotes already fetched |
| `HOT_PATHS` | (see script) | Paths watched for upstream drift |
