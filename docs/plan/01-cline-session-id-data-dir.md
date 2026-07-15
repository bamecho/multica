# 01 — Cline Session ID via Data-Dir Discovery

**Status:** Implemented (2026-07-15) — P0: isolated `--data-dir` + post-exit disk discovery for `Result.SessionID`. P1 (persist data-dir / verified `--id` resume) not done.  
**Scope:** Obtain a resume-capable `sessionId` for Multica’s `cline` NDJSON backend  
**Related:**

- [`docs/cline-ndjson-multica-adapter-plan.md`](../cline-ndjson-multica-adapter-plan.md) — overall adapter design
- [`docs/cline-ndjson-probe.md`](../cline-ndjson-probe.md) — probe cookbook
- Backend: `server/pkg/agent/cline.go`
- Daemon session plumbing: `server/internal/daemon/daemon.go` (`PriorSessionID` → `ExecOptions.ResumeSessionID` → `Result.SessionID`)

---

## 1. Problem

After a real Cline 3.0.40 `--json` run, Multica’s `Result.SessionID` is always empty.

### Evidence (local probe, 2026-07-15)

| Source | What appears |
| --- | --- |
| NDJSON `run_result` | `finishReason`, `text`, `usage`, `model` — **no `sessionId`** |
| NDJSON `hook_event` | `hookEventName`, `agentId`, `taskId` (`conv_…`) — **not** resume id |
| Disk | `~/.cline/data/sessions/<id>/<id>.json` with `"session_id": "<id>"` |
| `cline history --json` | `"sessionId": "<id>"` matching disk |
| Resume flag | `--id <session-id>` (official CLI reference) |

Unit fixtures in `cline_test.go` inject synthetic `"sessionId":"ses_…"` lines that **real CLI never emits**, so tests green-washed the gap.

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

---

## 3. Chosen solution: per-task `--data-dir` + post-exit disk discovery

### 3.1 Why this path

| Candidate | Verdict |
| --- | --- |
| Parse NDJSON `sessionId` | **Rejected** — absent on real 3.0.40 streams |
| Use `hook_event.taskId` / `agentId` | **Rejected** — not `--id` values |
| Global `~/.cline` + `cline history` only | **Weak** — concurrent tasks race; hard to match |
| Embed Cline SDK | **Out of scope** — daemon spawns CLI |
| Multica invents a random `--id` every run | **Unverified** — CLI may require a previously created session |

**Primary:** isolate Cline local state with `--data-dir`, then after `cmd.Wait()` read `session_id` from that tree.

Same *family* of solutions as:

- **antigravity** — authoritative conversation id / transcript under app data + log, not stdout
- **pi** — Multica owns session carrier (`--session` path) outside stream fields
- **openclaw** — Multica owns `--session-id` (Cline differs: id is CLI-generated, then rediscovered)

Unlike **claude / cursor / copilot / opencode**, Cline does not put a resume id in the stream.

### 3.2 `data-dir` vs `config` (Cline CLI)

| Flag | Default (3.0.40 help) | Contents | Role |
| --- | --- | --- | --- |
| `--config <path>` | `~/.cline/data/settings` | Provider, model, API keys, settings | **Who / how to call the model** |
| `--data-dir <path>` | `~/.cline` | Sessions, db, cache, logs (under `data/`) | **Where this run’s state lives** |

Default layout:

```text
~/.cline/                          ← --data-dir
├── data/
│   ├── settings/                  ← --config default
│   ├── sessions/<session_id>/
│   │   ├── <session_id>.json      ← "session_id", pid, cwd, prompt, …
│   │   └── <session_id>.messages.json
│   ├── db/
│   ├── cache/
│   └── logs/
└── …
```

Implications:

- Session discovery reads **data-dir**, not config.
- A fresh `--data-dir` can also get an empty `data/settings` (probe: only default `cline` provider). Auth must stay reachable via:
  - **A (preferred):** pair `--data-dir <isolated>` with `--config <daemon-or-user settings>`, if the CLI honors both independently, or
  - **B:** seed provider settings into the isolated data-dir before spawn, or
  - **C (fallback only):** use global data-dir and match by `pid` / cwd / time window (concurrency-weak).

`cline history` exposes `--config` but **not** `--data-dir`; prefer **filesystem scan** of `sessions/` over history subprocess.

### 3.3 Launch contract (delta to current adapter)

Current argv (v1 adapter):

```bash
cline --json --auto-approve true \
  -c <workdir> \
  [-m <model>] \
  [--id <prior>] \
  "<SystemPrompt>\n\n<userPrompt>"
```

Proposed:

```bash
cline --json --auto-approve true \
  --data-dir <clineDataDir> \
  [--config <clineConfigDir>] \
  -c <workdir> \
  [-m <model>] \
  [-P <provider>] \
  [--id <prior>] \
  "<combined prompt>"
```

| Path | Recommendation |
| --- | --- |
| `clineDataDir` | Per task under daemon temp / cache, **not** inside a user’s git worktree for `local_directory` mode. Example: `$TASK_TMPDIR/cline-data` or daemon cache keyed by `(agent, issue)` when resume must survive. |
| `clineConfigDir` | Existing authenticated settings (user or daemon-managed), unless seeding into data-dir. |

Filter `CustomArgs` so users cannot override `--data-dir` / `--config` / `--id` in ways that break isolation or resume.

### 3.4 Discovery algorithm (after `cmd.Wait()`)

```text
1. List <clineDataDir>/data/sessions/*/*.json
   (exact nesting may be <data-dir>/data/sessions or <data-dir>/sessions —
    implement against real layout; default tree uses data/sessions under ~/.cline)

2. Score each session JSON:
   - pid == child Process.Pid          (strongest)
   - cwd / workspace_root == opts.Cwd
   - started_at ∈ [startTime, endTime]
   - prompt prefix matches combined prompt
     (Cline may wrap with <user_input mode="act">…; match flexibly)

3. Pick unique best match → session_id

4. Result.SessionID = session_id
   If no match: leave empty + log warn (daemon keeps current no-resume behavior)
```

Session JSON fields already observed:

`session_id`, `pid`, `cwd`, `workspace_root`, `prompt`, `started_at`, `ended_at`, `status`, `exit_code`, `provider`, `model`.

### 3.5 Resume

- When `opts.ResumeSessionID != ""`, pass `--id` and **reuse the same `clineDataDir`** that still holds that session.
- Align with existing `gateResumeToReusedWorkdir`: if workdir/state is gone, clear prior session.
- **P1 verification required:** local probes saw `--json --id …` fail with  
  `JSON output mode requires a prompt argument or piped stdin`  
  even when a prompt was passed. Treat resume argv/ordering as a separate hardening task; **P0 may only record SessionID**.

### 3.6 Explicit non-goals (this plan)

- Mapping full tool timeline from real `hookEventName` / `contentType=tool` (separate plan)
- Fixing “command is too long” argv limits (separate plan)
- Changing Multica’s generic task session DB schema

---

## 4. Implementation sketch

| Area | Change |
| --- | --- |
| `server/pkg/agent/cline.go` | Choose/pass `--data-dir` (+ optional `--config`); after Wait, `discoverClineSessionID(dataDir, matchHints)`; set `Result.SessionID` |
| `ExecOptions` or backend-local path | Prefer backend-owned path under env `TMPDIR` / opts already passed by daemon; avoid new daemon special-cases if possible |
| Blocked args | Add `--data-dir`, `--config` to `clineBlockedArgs` if Multica owns them |
| Tests | Fake CLI that writes a real-shaped session JSON under a temp data-dir; assert `Result.SessionID`. Replace synthetic NDJSON `sessionId` fixtures as “not real CLI contract” |
| Docs | Update adapter plan § session + this file’s Status when implemented |

### Phasing

| Phase | Deliverable |
| --- | --- |
| **P0** | Isolated data-dir + disk discovery → non-empty `Result.SessionID` on success/failure when session files exist |
| **P1** | Persist data-dir lifecycle with workdir reuse; verify `--id` resume under `--json` |
| **P2** | Real hook/tool schema + long-prompt delivery (out of this doc’s implementation, track separately) |

---

## 5. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Isolated data-dir drops auth | Always pair with working `--config` or seed settings |
| Concurrent runs share global `~/.cline` | Default to per-task data-dir |
| Session file layout changes across Cline versions | Match on fields + pid; log path used; keep probe fixtures |
| Resume + `--json` broken on some versions | P0 record id only; P1 gate resume behind verified CLI version |
| Local mode pollutes user repo | Never put data-dir under user project root; use task temp / daemon cache |
| `taskId` mistaken for session | Code comments + tests that reject `conv_` / `agent_` prefixes for `--id` |

---

## 6. Verification plan

1. **Unit:** fake binary writes `sessions/<id>/<id>.json` with known pid/cwd → `Result.SessionID == id`.
2. **Integration (optional, real CLI):** `MULTICA_CLINE_PATH=cline` short `--json` run with temp data-dir + openai-compatible provider → assert id on disk equals Multica result (manual or opt-in test).
3. **Negative:** NDJSON without disk session → empty SessionID, no panic.
4. **Regression:** existing NDJSON text/usage/`run_result` mapping still passes.

---

## 7. Decision log

| Date | Decision |
| --- | --- |
| 2026-07-15 | Real Cline 3.0.40 NDJSON has no `sessionId`; disk/history is source of truth for `--id` |
| 2026-07-15 | Prefer `--data-dir` isolation + post-exit filesystem discovery over history CLI |
| 2026-07-15 | Do not store `taskId`/`agentId` as Multica `SessionID` |
| 2026-07-15 | Config vs data-dir are distinct; session lives under data-dir |
| 2026-07-15 | P0 = record SessionID; P1 = prove resume |

---

## 8. Open questions

1. Should per-agent long-lived data-dir (better resume) or per-task data-dir (better isolation) be the default when workdir is reused?
2. Does the target internal CLI fork honor `--data-dir` / `--config` the same as open-source 3.0.40?
3. Can Multica pre-create a session id, or must the first run always be id-less?

Resolve against the **actual** binary used in production before locking P1.
