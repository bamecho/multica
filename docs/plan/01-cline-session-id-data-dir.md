# 01 — Cline Session ID via Data-Dir Discovery

**Status:** **Obsolete (2026-07-24). Do not implement.** Superseded by the
confirmed [`04-cline-dedicated-hub-per-run.md`](./04-cline-dedicated-hub-per-run.md).

This file is retained only as a historical record of the temporary
`--data-dir` and settings-seed approach. All current Cline launch, prompt,
session discovery, and resume decisions live in plan 04.

Historical note: P0 was implemented (2026-07-15) as:

- Isolated `--data-dir` + post-exit disk discovery for `Result.SessionID`
- Auth via **seed** of `~/.cline-sr/data/settings` into the data-dir (sandbox ignores `--config` for providers)
- No `--config` on Multica argv

P1 (persist data-dir / verified `--id` resume) was never done. Forward work
must follow plan 04.

**Scope (historical):** Obtain a resume-capable `sessionId` for Multica’s `cline` NDJSON backend
**Related:**

- **Current design:** [`docs/plan/04-cline-dedicated-hub-per-run.md`](./04-cline-dedicated-hub-per-run.md)
- [`docs/cline-ndjson-multica-adapter-plan.md`](../cline-ndjson-multica-adapter-plan.md) — overall adapter design
- [`docs/cline-ndjson-probe.md`](../cline-ndjson-probe.md) — probe cookbook
- [`docs/plan/02-cline-prompt-stdin-hybrid.md`](./02-cline-prompt-stdin-hybrid.md) — long prompt via stdin
- Backend: `server/pkg/agent/cline.go`
- Daemon session plumbing: `server/internal/daemon/daemon.go` (`PriorSessionID` → `ExecOptions.ResumeSessionID` → `Result.SessionID`)

---

## 1. Problem

After a real Cline 3.0.40 `--json` run, Multica’s `Result.SessionID` is always empty if it only trusts NDJSON.

### Evidence (local probe, 2026-07-15)

| Source | What appears |
| --- | --- |
| NDJSON `run_result` | `finishReason`, `text`, `usage`, `model` — **no `sessionId`** |
| NDJSON `hook_event` | `hookEventName`, `agentId`, `taskId` (`conv_…`) — **not** resume id |
| Disk | `~/.cline/data/sessions/<id>/<id>.json` with `"session_id": "<id>"` |
| `cline history --json` | `"sessionId": "<id>"` matching disk |
| Resume flag | `--id <session-id>` (official CLI reference) |

Unit fixtures that inject synthetic `"sessionId":"ses_…"` lines **green-wash** the gap: real CLI never emits those fields.

Official docs ([CLI reference](https://docs.cline.bot/cli/cli-reference), [SDK events](https://docs.cline.bot/sdk/events)) confirm:

- Session list / resume is via **history** and **`--id`**
- SDK exposes `sessionId` on host events
- **CLI NDJSON is not documented to carry `sessionId` on every line**

---

## 2. Goal

1. Fill `Result.SessionID` with the **same id** Cline accepts for `--id` (disk / history id).
2. Keep Multica’s existing resume pipeline working:

   ```text
   Result.SessionID → CompleteTask/FailTask
   next claim → PriorSessionID → ExecOptions.ResumeSessionID → cline --id …
   ```

3. Do **not** treat `taskId` / `agentId` as `SessionID` (wrong type; resume would fail).
4. Keep headless auth working under isolated `--data-dir` (no interactive token/model prompts).

---

## 3. Chosen solution: per-task `--data-dir` + seed settings + post-exit disk discovery

### 3.1 Why this path

| Candidate | Verdict |
| --- | --- |
| Parse NDJSON `sessionId` | **Rejected** — absent on real 3.0.40 streams |
| Use `hook_event.taskId` / `agentId` | **Rejected** — not `--id` values |
| Global `~/.cline` + `cline history` only | **Weak** — concurrent tasks race; hard to match |
| Embed Cline SDK | **Out of scope** — daemon spawns CLI |
| Multica invents a random `--id` every run | **Unverified** — CLI may require a previously created session |

**Primary:** isolate Cline local state with `--data-dir`, seed auth into that tree, then after `cmd.Wait()` read `session_id` from disk.

Same *family* of solutions as:

- **antigravity** — authoritative conversation id / transcript under app data + log, not stdout
- **pi** — Multica owns session carrier (`--session` path) outside stream fields
- **openclaw** — Multica owns `--session-id` (Cline differs: id is CLI-generated, then rediscovered)

Unlike **claude / cursor / copilot / opencode**, Cline does not put a resume id in the stream.

### 3.2 `data-dir` vs `config` vs sandbox (Cline CLI)

| Flag / concept | Default (OSS 3.x) | Contents | Role |
| --- | --- | --- | --- |
| `--config <path>` | `~/.cline/data/settings` | Provider, model, API keys, settings | **Who / how to call the model** when **not** in sandbox |
| `--data-dir <path>` | `~/.cline` | Sessions, db, cache, logs (under `data/`) | **Where this run’s state lives** |
| Sandbox (implicit) | off | — | **Enabled automatically when `--data-dir` is set** (OSS + Multica fork) |

Default global layout (no Multica isolation):

```text
~/.cline/                          ← default --data-dir (OSS)
├── data/
│   ├── settings/                  ← default --config
│   │   └── providers.json
│   ├── sessions/<session_id>/
│   │   ├── <session_id>.json      ← "session_id", pid, cwd, prompt, …
│   │   └── <session_id>.messages.json
│   ├── db/
│   ├── cache/
│   └── logs/
└── …
```

Multica-targeted CLI home (settings **source** for seed):

```text
~/.cline-sr/data/settings/
  providers.json                   ← required for headless runs
  global-settings.json             ← optional; copied if present
  …
```

**Sandbox behavior (open-source `configureSandboxEnvironment`):**

When `--data-dir` is set, CLI sets (among others):

| Env | Value |
| --- | --- |
| `CLINE_SANDBOX` | `1` |
| `CLINE_DATA_DIR` | `<data-dir>` |
| `CLINE_SESSION_DATA_DIR` | `<data-dir>/sessions` |
| **`CLINE_PROVIDER_SETTINGS_PATH`** | **`<data-dir>/settings/providers.json`** |

So a separate **`--config` does not restore provider auth** under sandbox. Passing only `--config ~/.cline-sr/data/settings` still leaves empty providers inside the data-dir and can force interactive token/model validation.

Auth options:

| Option | Verdict |
| --- | --- |
| **A** — `--data-dir` + `--config` only | **Not viable** under sandbox for providers |
| **B** — seed home settings into data-dir before spawn | **Chosen (P0 implemented)** |
| **C** — global data-dir + pid/time match | **Fallback only** — concurrency-weak |

`cline history` exposes `--config` but **not** `--data-dir`; Multica prefers **filesystem scan** of `sessions/` over a history subprocess.

### 3.3 Launch contract (implemented)

```bash
# Multica-owned prep (not argv):
#   dataDir = $TMPDIR/multica-cline-data-*
#   seed ~/.cline-sr/data/settings → 
#     <dataDir>/settings/
#     <dataDir>/data/settings/
#   (providers.json required; other files copied when present)

cli --json \
  --data-dir <clineDataDir> \
  -c <workdir> \
  [-m <model>] \
  [--id <prior>] \
  $'\n'                    # argv prompt gate only (plan 02)

# stdin: SystemPrompt + "\n\n" + userPrompt
```

| Item | Multica behavior |
| --- | --- |
| `clineDataDir` | Per-run `os.MkdirTemp` under daemon `TMPDIR` (outside user git worktree) |
| Settings source | `~/.cline-sr/data/settings` |
| Seed destinations | `<dataDir>/settings` **and** `<dataDir>/data/settings` (OSS sandbox path + nested layout) |
| `--config` | **Not passed**; blocked in `CustomArgs` |
| `--auto-approve` | **Not passed** (CLI default true for headless); still blocked in `CustomArgs` |
| Session discovery | After `Wait`, scan data-dir session JSON → `Result.SessionID` |

Filter `CustomArgs` so users cannot override `--data-dir`, `--config`, `--id`, `-c`, `-m`, protocol flags, etc.

### 3.4 Discovery algorithm (after `cmd.Wait()`)

```text
1. List <clineDataDir>/data/sessions/*/*.json
   and <clineDataDir>/sessions/*/*.json

2. Score each session JSON:
   - pid == child Process.Pid          (strongest)
   - cwd / workspace_root == opts.Cwd
   - started_at ∈ [startTime, endTime]
   - prompt matches Multica stdin payload (Cline may wrap <user_input>…)

3. Pick unique best match → session_id
   Single candidate under isolated data-dir is accepted even if hints are weak.

4. Result.SessionID = session_id
   If no match: leave empty + log warn (daemon keeps current no-resume behavior)

5. Never accept conv_* / agent_* as resume ids.
```

Session JSON fields used: `session_id`, `pid`, `cwd`, `workspace_root`, `prompt`, `started_at`, `ended_at`, `status`.

### 3.5 Resume

- When `opts.ResumeSessionID != ""`, pass `--id`.
- **P1:** reuse the same durable `clineDataDir` that still holds that session; seed only on first create.
- Align with existing `gateResumeToReusedWorkdir`: if workdir/state is gone, clear prior session.
- **P1 verification required:** local probes saw `--json --id …` fail with  
  `JSON output mode requires a prompt argument or piped stdin`  
  even when a prompt was passed. Treat resume argv/ordering as a separate hardening task; **P0 only records SessionID**.

### 3.6 Explicit non-goals (this plan)

- Mapping full tool timeline from real `hookEventName` / `contentType=tool` (separate plan)
- Long-prompt delivery (plan 02 — implemented)
- Changing Multica’s generic task session DB schema

---

## 4. Implementation (code map)

| Area | Implementation |
| --- | --- |
| `prepareClinePaths` | `MkdirTemp` + `seedClineSettingsIntoDataDir` |
| `defaultClineSettingsSourceDir` | `~/.cline-sr/data/settings` |
| `seedClineSettingsIntoDataDir` | Require `providers.json`; shallow-copy tree to both dest layouts; files `0o600` |
| `buildClineArgs` | `--json --data-dir …` only; **no** `--config` / `--auto-approve` |
| `discoverClineSessionID` | Post-exit disk scan; ignore NDJSON `sessionId` |
| Blocked args | `--data-dir`, `--config`, `--id`, `-c`, `-m`, `--json`, … |
| Tests | `cline_test.go`: seed layouts, missing providers, SessionID from disk, no `--config` on argv |

### Phasing

| Phase | Deliverable | Status |
| --- | --- | --- |
| **P0** | Isolated data-dir + seed + disk discovery → non-empty `Result.SessionID` when session files exist | **Done** |
| **P1** | Persist data-dir lifecycle with workdir reuse; verify `--id` resume under `--json` | Not done |
| **P2** | Real hook/tool schema hardening (track separately) | Not this plan |

---

## 5. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Isolated data-dir drops auth | **Seed** `providers.json` (+ other settings) into data-dir; fail closed if missing |
| `--config` assumed to fix sandbox | Documented non-viable; Multica never relies on it for providers |
| Concurrent runs share global home | Default to per-task data-dir; seed is a **copy**, not symlink |
| Session file layout changes | Match on fields + pid; accept both `data/sessions` and `sessions` |
| Resume + `--json` broken on some versions | P0 record id only; P1 gate resume behind verified CLI |
| Local mode pollutes user repo | Never put data-dir under user project root; use task temp / daemon cache |
| `taskId` mistaken for session | Code + tests reject `conv_` / `agent_` prefixes for `--id` |

---

## 6. Verification

1. **Unit:** fake binary writes `data/sessions/<id>/<id>.json` → `Result.SessionID == id`.
2. **Unit:** seed places `providers.json` under both settings layouts; missing source fails with clear error.
3. **Unit:** argv has `--data-dir`, no `--config`; stdin holds Multica payload (plan 02).
4. **Ops prerequisite:** machine has authenticated `~/.cline-sr/data/settings/providers.json`.
5. **Negative:** NDJSON without disk session → empty SessionID, no panic.

---

## 7. Decision log

| Date | Decision |
| --- | --- |
| 2026-07-15 | Real Cline 3.0.40 NDJSON has no `sessionId`; disk/history is source of truth for `--id` |
| 2026-07-15 | Prefer `--data-dir` isolation + post-exit filesystem discovery over history CLI |
| 2026-07-15 | Do not store `taskId`/`agentId` as Multica `SessionID` |
| 2026-07-15 | Config vs data-dir are distinct; session lives under data-dir |
| 2026-07-15 | P0 = record SessionID; P1 = prove resume |
| 2026-07-15 | **`--data-dir` enables sandbox; `--config` does not supply providers** (OSS + fork) |
| 2026-07-15 | **Auth path = seed `~/.cline-sr/data/settings` into data-dir; Multica does not pass `--config`** |
| 2026-07-15 | Dual seed destinations: `<data-dir>/settings` and `<data-dir>/data/settings` |

---

## 8. Open questions (P1)

1. Should per-agent long-lived data-dir (better resume) or per-task data-dir (better isolation) be the default when workdir is reused?
2. Can Multica pre-create a session id, or must the first run always be id-less?
3. Exact resume argv + stdin ordering on the production internal CLI fork.

Resolve against the **actual** binary used in production before locking P1.
