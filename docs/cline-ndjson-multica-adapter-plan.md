# Cline 3.x NDJSON → Multica Adapter Plan

**Status:** A base NDJSON parser exists, but execution-log parity is incomplete.
Fresh-only dedicated-Hub discovery, shared-Hub resume, and fixture-driven event
mapping are not complete until plan 04's acceptance tests pass.
**Scope:** Adapt a Cline-based internal coding CLI to Multica via **`--json` NDJSON (形态 B / Cline 3.x)**  
**Out of scope:** ACP (`--acp`), Codex-style JSON-RPC app-server, upstream contribution process  

Related:

- Probe / capture cookbook: [`docs/cline-ndjson-probe.md`](./cline-ndjson-probe.md)
- Confirmed launch/session/resume design: [`docs/plan/04-cline-dedicated-hub-per-run.md`](./plan/04-cline-dedicated-hub-per-run.md)
- Fork long-lived branch sync patrol: [`docs/fork-sync-patrol.md`](./fork-sync-patrol.md)
- Multica control plane: `server/internal/daemon/daemon.go` (`runTask`)
- Backend contract: `server/pkg/agent/agent.go` (`Backend.Execute` → `Message` / `Result`)
- Custom profiles: [`docs/custom-runtimes.md`](./custom-runtimes.md)

---

## 1. Goal

Let Multica’s local daemon **spawn an internal CLI** that speaks **Cline 3.x NDJSON**, stream tool/text events to the UI, and finish tasks with correct status / usage / optional session resume — without implementing ACP.

Multica still does **not** call the LLM. Business reads/writes stay on `multica` CLI inside the agent process (`MULTICA_TOKEN`, etc.).

---

## 2. Confirmed inputs

| Fact | Decision impact |
| --- | --- |
| Wire format is **形态 B** (Cline 3.x envelopes) | Parse `agent_event` / `hook_event` / `run_result`, not Overview `say`/`ask` only |
| Flags used by Multica | `--json`, `-c`, optional `-m` / `--id`; **not** `-t` / `-s` / `--config` / `--auto-approve` / **`--data-dir`** on argv |
| **`-s` / system prompt not usable** | Do **not** pass `-s`; deliver the full brief and user prompt on stdin; argv carries only a newline sentinel |
| Real NDJSON has **no** resume `sessionId` | Fresh runs use a dedicated Hub and exact native-history matching; resume already has Multica's authoritative ID and uses the long-lived shared Hub |
| `--data-dir` enables **sandbox** | **Do not pass it.** Cline owns one persistent state root across fresh and resumed runs |
| Runtime routing can fall back locally | Force `CLINE_SESSION_BACKEND_MODE=hub`; Hub failure is fatal |
| Prefer **simplest** reliable path | One `cline` Backend; internal binary name via Custom Runtime Profile |

---

## 3. Chosen approach

### 3.1 Provider key: `cline`

Add a first-class Multica provider / `protocol_family` named **`cline`**.

| Host setup | How it runs |
| --- | --- |
| Binary on PATH as `cline` (or `MULTICA_CLINE_PATH`) | Built-in daemon probe registers runtime |
| Internal binary, different name/path | **Custom Runtime Profile**: `protocol_family=cline`, `command_name=/path/to/internal-cli` |

One NDJSON parser serves both. No second Backend for branding.

### 3.2 Why not the alternatives

| Alternative | Why rejected |
| --- | --- |
| Reuse `cursor` / `opencode` / `claude` family | Different event schema and flags |
| Custom profile only (no new Backend) | No existing family understands Cline 3.x NDJSON |
| ACP / JSON-RPC | Internal CLI has no ACP SDK; Multica does not need it for this path |

---

## 4. Control-plane flow

```text
Server  enqueue task → claim
Daemon  prepare workdir / skills / AGENTS.md (best-effort)
        BuildPrompt(task) → userPrompt
        runtimeBrief → ExecOptions.SystemPrompt  (inline path)
        agent.New("cline").Execute(...)
Backend prep:
          if fresh:
            allocate private Hub discovery path + loopback port
            start and validate one dedicated Hub
            snapshot native history
          if resume:
            use the normal long-lived shared Hub
            do not create/start/status/stop a private Hub
          force Hub backend; no Multica data-dir / settings seed
Backend argv:
          <cli> --json
                -c <workdir>
                [-m <model>]
                [--id <prior_session>]
                $'\n'                              # gate only; NO -s; no full prompt on argv
                # do NOT pass --data-dir
Backend stdin:
          SystemPrompt + "\n\n" + userPrompt
        parse stdout NDJSON → Message stream
        if fresh:
          freeze first protocol timestamp
          discover session_id by history delta + dedicated Hub PID + cwd + timestamp
          early-pin exact-one match; final lookup never widens the time window
          stop the owned Hub after session drain
        if resume:
          preserve the supplied ID unless positively rejected/replaced
          never call, stop, or signal the shared Hub control plane
        last run_result → Result
Daemon  CompleteTask / FailTask (+ session_id, work_dir, usage)
```

Timeout: **Multica `runContext` / daemon timeout owns the wall clock.**  
Implementation **does not pass CLI `-t`**, to avoid dual-timeout semantics.

---

## 5. Launch contract

### 5.1 Required / used flags

```bash
cli --json \
  -c <workdir> \
  [-m <model>] \
  [--id <session_id>] \
  $'\n'
# stdin: SystemPrompt + "\n\n" + userPrompt
```

| Flag / channel | Source | Notes |
| --- | --- | --- |
| `--json` | Fixed | NDJSON on stdout |
| `--data-dir` | **Never passed** | Cline persistent state is required for resume |
| Settings seed | **Removed** | Cline reads its authenticated persistent settings directly |
| `--config` | **Not used** | The configured Cline installation owns settings |
| `--auto-approve` | **Not passed** | CLI headless default is true; still blocked in CustomArgs |
| `-c` | `opts.Cwd` | Also set `cmd.Dir` |
| `-m` | `opts.Model` | Agent model or daemon default |
| `--id` | `opts.ResumeSessionID` | Only when non-empty and exact workdir reuse is allowed |
| argv prompt | Fixed `"\n"` | Cline truthy-prompt gate only |
| stdin | `SystemPrompt + "\n\n" + prompt` | Full Multica payload; replaces unusable `-s` and huge argv |
| `-t` | **Not used (v1)** | Daemon timeout only |
| `-s` | **Never** | Confirmed unsupported / unusable |

Also filter `CustomArgs` / `ExtraArgs` so users cannot override `--json`,
`--data-dir`, `--config`, `--auto-approve`, `-c`, `--id`, `-m`, timeout,
system prompt, or Hub routing in ways that break the protocol.

### 5.1.1 Ops prerequisite (auth)

Daemon host must have authenticated Cline settings before tasks run:

```text
~/.cline-sr/data/settings/providers.json
```

If authentication is missing, Cline fails through its native persistent
settings path. Multica does not create or seed a replacement settings tree.

### 5.2 Environment (Daemon already injects)

Backend must use `buildEnv(cfg.Env)` so the child keeps:

- `MULTICA_TOKEN`, `MULTICA_SERVER_URL`, `MULTICA_WORKSPACE_ID`
- `MULTICA_AGENT_ID` / `MULTICA_TASK_ID` / …
- `PATH` with `multica` binary first

Do **not** call Multica HTTP from the Backend.

---

## 6. NDJSON → Multica mapping (形态 B)

Authoritative shapes follow open-source Cline 3.x + [`docs/cline-ndjson-probe.md`](./cline-ndjson-probe.md).

### 6.1 Top-level stdout types

| Line `type` | Multica handling |
| --- | --- |
| `agent_event` | Nested `.event.type` drives text / usage / done |
| `hook_event` | Lifecycle; `tool_call` / `tool_result` → tool messages |
| `run_result` | **Authoritative final line** for `Result` |
| other / invalid JSON | Skip + log; do not abort the scan |

### 6.2 Event → `Message` / `Result`

| Source | Multica |
| --- | --- |
| `agent_event` text content block | `Message{Type: text, Content}` with delta/snapshot deduplication |
| `agent_event` thinking/reasoning content block | `Message{Type: thinking, Content}` |
| NDJSON tool-start event | Normalize observed field aliases to `Message{Type: tool-use, Tool, CallID, Input}` |
| NDJSON tool-end event | Normalize observed field aliases to `Message{Type: tool-result, Tool, CallID, Output}` |
| NDJSON error event | `Message{Type: error, Content}` |
| `agent_event.event.type == "usage"` | Accumulate `TokenUsage` |
| Session id on NDJSON (if any) | **Ignored for `Result.SessionID`** — plan 04's exact native-history match is authoritative |
| **Last** `run_result` with `finishReason == "completed"` | `Result{Status: "completed", Output: text, Usage}` + exact-history ID for fresh runs or the supplied ID for resume |
| Last `run_result` with other `finishReason` (`aborted`, `error`, …) | `failed` (or `timeout` if stderr indicates timeout) |
| No `run_result` | Fallback: last `done` + process exit code |
| stderr JSON / text matching timeout | Prefer `Result.Status = "timeout"` |

### 6.3 Parse policy (v1)

1. Line-scan stdout; large scanner buffer (same order as other backends).  
2. Prefer **last** `run_result` over mid-stream `done` for status and final `Output`.  
3. Empty `Output` with `completed` is valid (work may be only `multica` side effects).  
4. Build the field map from sanitized output captured from the exact Cline
   version Multica runs. Normalize existing aliases and nested shapes in the
   adapter; do not require a Cline protocol change for data already present.
5. Track content per block. Preserve whitespace, map reasoning separately, and
   convert cumulative snapshots to suffix deltas so the execution log does not
   repeat text.
6. Resume: pass `--id` when `ResumeSessionID` is set and preserve it unless
   Cline positively reports rejection or replacement.
7. Every NDJSON line must be flushed before a provider request or tool execution
   can block. Events must reach `Session.Messages` before process exit, then use
   the existing daemon `ReportTaskMessages` path for DB + WebSocket delivery.
8. Pair tools with explicit call IDs or another probe-confirmed correlation
   field and an in-flight map. A single `lastToolCallID` is invalid when calls
   overlap.
9. Do not use the lossy shared `trySend` behavior for Cline execution events.
   Backpressure must be context-cancellable, and a burst larger than the channel
   capacity must not disappear from the execution log.
10. Unknown/malformed events are logged by type/keys without sensitive values
    and do not fail the task. Generic `"tool"` is diagnostic fallback only, not
    the accepted steady-state display.

If the captured NDJSON truly lacks a required semantic field or is buffered
until process exit, record that as a Cline protocol gap and make the smallest
upstream/fork change. This is a fallback after adapter coverage, not the default
implementation strategy.

### 6.4 Illustrative stream (reference only)

```jsonl
{"type":"hook_event","event":{"type":"agent_start"}}
{"type":"agent_event","event":{"type":"iteration_start","iteration":1}}
{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"Working..."}}
{"type":"hook_event","event":{"type":"tool_call","name":"bash","callId":"c1","input":{"command":"pwd"}}}
{"type":"hook_event","event":{"type":"tool_result","name":"bash","callId":"c1","output":"/work\n"}}
{"type":"agent_event","event":{"type":"usage","inputTokens":100,"outputTokens":20}}
{"type":"agent_event","event":{"type":"done","reason":"completed","text":"Summary","iterations":1}}
{"type":"run_result","finishReason":"completed","text":"Summary","durationMs":1234,"usage":{"inputTokens":100,"outputTokens":20}}
```

---

## 7. Context injection without `-s`

| Mechanism | Role |
| --- | --- |
| `providerNeedsInlineSystemPrompt("cline") == true` | Daemon sets `ExecOptions.SystemPrompt = runtimeBrief` |
| Backend stdin payload | **Required** — `SystemPrompt + "\n\n" + userPrompt`; argv is only `"\n"` |
| Write `AGENTS.md` via `InjectRuntimeConfig` | Best-effort secondary; not relied on |
| Skills dir | v1: default `.agent_context/skills/` until a native Cline project skill path is confirmed |

---

## 8. Implementation checklist

### 8.1 Required for the complete path

| Area | Change |
| --- | --- |
| Backend | `server/pkg/agent/cline.go` + unit tests with NDJSON fixtures |
| Factory | `SupportedTypes`, `New("cline")`, `launchHeaders` |
| Lockstep test | `agent_supported_types_test.go` whitelist |
| Daemon probe | `MULTICA_CLINE_PATH` / default binary `cline` in `config.go` |
| Inline brief | `providerNeedsInlineSystemPrompt` includes `cline` |
| Brief file | `runtimeConfigPath` → `AGENTS.md` for `cline` |
| DB | migration widen `runtime_profile.protocol_family` CHECK with `cline` |
| Display name | optional override e.g. `Cline` in `runtimeDisplayNameOverrides` |
| Stdin prompt | argv `"\n"` + full payload on stdin; consolidated in plan 04 |
| Dedicated Hub | One private Hub/discovery identity per fresh invocation only |
| Session discovery | Exact history delta + Hub PID + root fields + cwd + frozen timestamp window |
| Persistent state | No temporary data-dir and no settings seed |
| Early pin | Persist the first exact match while the task is still running |
| Execution events | Named tool/input/result, thinking, text, and errors stream and flush before task completion |
| Resume path | When Multica supplies a SessionID, use the long-lived shared Hub, skip all private-Hub/history/timestamp work, and use `--id` directly |

### 8.2 Deferred (do not block current path)

- Fancy provider logo / onboarding marketing copy  
- Dynamic `ListModels` discovery (empty catalog + manual `-m` is OK)  
- CLI `-t` passthrough  
- Native Cline skill directory layout (if later confirmed)  
- Docs row in `CLI_AND_DAEMON.md` (nice-to-have)  

### 8.3 Explicit non-goals

- ACP host  
- File-level timeline from stream (use git outside if needed)  
- Separate Hub-to-dashboard event bridge; task stdout NDJSON already feeds the
  existing daemon task-message/WebSocket pipeline
- Falling back to daemon PAT as agent token  
- Opening PRs to upstream unless explicitly requested  
- Relying on `--config` alone under `--data-dir` sandbox for provider auth  

---

## 9. Verification plan

1. **Unit:** captured fixture NDJSON for text/thinking, delta and snapshot
   content, serial and interleaved tools, errors, usage, aborted, missing
   `run_result`, and bad lines -> assert exact `Message` sequence and `Result`.
2. **Unit:** emit more than 256 events while the consumer is temporarily slow;
   assert no Cline event is silently dropped and cancellation releases any
   blocked sender.
3. **Unit:** block between tool start/result and assert `tool_use` reaches the
   consumer before the process exits; interleave two call IDs and assert each
   result retains the correct tool.
4. **Unit:** fresh dedicated-Hub environment, no `--data-dir`, frozen timestamp
   window, exact-one matching, early pin, fail-closed ambiguity, plus a resume
   branch that never starts/stops a private Hub and preserves its ID.
5. **Integration:** concurrent fresh dedicated Hubs and long-lived shared-Hub
   resume against one persistent SQLite state root; no local or file-backend
   fallback.
6. **Local daemon:** `MULTICA_CLINE_PATH=... multica daemon start --foreground` with authenticated persistent Cline settings.
7. **Happy path:** assign a read-only / low-risk issue -> claim -> observe
   thinking/text and each tool start/result in the execution log before task
   completion -> complete.
8. **Resume:** second task with `--id` / `prior_session_id` and exact workdir
   reuse on the long-lived shared Hub; no private Hub command runs.
9. **Crash/cancel:** fresh owner lease aborts the dedicated session; resume
   cancellation only terminates the task CLI, whose disconnect aborts its own
   turn without any Multica shared-Hub control call.
10. **Custom profile:** same Backend with non-default `command_name`.
11. **Regression:** `go test ./pkg/agent/ -run Cline` and SupportedTypes lockstep tests.

---

## 10. Working branch policy (this effort)

- Develop and push only on the **fork** remote (`origin` = personal fork).  
- Do **not** open a PR against upstream unless the owner explicitly asks.  
- Design docs for this adapter live under `docs/` on the feature branch.

---

## 11. Decision log

| Date | Decision |
| --- | --- |
| 2026-07-14 | Use NDJSON, not ACP/JSON-RPC |
| 2026-07-14 | Confirmed wire format = Cline 3.x 形态 B |
| 2026-07-14 | Provider key `cline`; internal binary via Custom Runtime Profile |
| 2026-07-14 | No `-s`; inject brief with user prompt (later refined to stdin) |
| 2026-07-14 | v1: no CLI `-t`; Multica daemon timeout only |
| 2026-07-14 | Simplest implementation preferred over feature-complete UI |
| 2026-07-15 | Full Multica payload on **stdin**; argv positional prompt is `"\n"` only (plan 02) |
| 2026-07-15 | `Result.SessionID` from **disk** under `--data-dir`, not NDJSON (plan 01) |
| 2026-07-15 | `--data-dir` enables sandbox; **seed** `~/.cline-sr/data/settings`; **do not pass `--config`** |
| 2026-07-15 | Do not pass `--auto-approve` (CLI default true); still block CustomArgs override |
| 2026-07-24 | Plans 01-03 retired; plan 04 is the sole source of truth |
| 2026-07-24 | Fresh runs use one dedicated Hub plus exact Hub-PID/time matching; `--id` resume uses the long-lived shared Hub and skips private-Hub discovery |
| 2026-07-24 | Execution-log visibility is fixture-driven Multica NDJSON adapter work; change Cline events only when native NDJSON lacks required semantics or timely flush |

---

## 12. Next step after this doc

The base NDJSON adapter exists, but its current temporary-data-dir discovery
path is legacy implementation. Implement and verify plan 04 before treating
fresh SessionID discovery and shared-Hub resume as complete. Plan 04 is the only
source of truth for future Cline behavior changes.
