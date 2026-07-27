# 03 - Cline Session ID via Default Home and Native History

**Status:** **Obsolete (2026-07-24). Do not implement.** Replaced by the
confirmed [`04-cline-dedicated-hub-per-run.md`](./04-cline-dedicated-hub-per-run.md).
**Historical relationship:** This proposal superseded plan 01 but was never the
final design.
**Scope:** Remove Multica's per-run `--data-dir` ownership and obtain the
resume-capable Cline session ID through the native `history --json` command.
The existing one-shot `--json` NDJSON transport remains in place; ACP is out of
scope.

Related code and documents:

- Backend: `server/pkg/agent/cline.go`
- Workdir preparation: `server/internal/daemon/execenv/execenv.go`
- Resume/workdir gating: `server/internal/daemon/daemon.go`
- Overall adapter: [`docs/cline-ndjson-multica-adapter-plan.md`](../cline-ndjson-multica-adapter-plan.md)
- Probe guide: [`docs/cline-ndjson-probe.md`](../cline-ndjson-probe.md)

---

## 1. Problem

Real Cline 3.x `--json` output contains `agent_event`, `hook_event`, and
`run_result`, but does not expose the resume-capable session ID. The value in
hook `taskId` / `agentId` fields is not the value accepted by `--id`.

The current Multica implementation tries to solve this by creating a new
temporary `--data-dir` for every run, seeding provider settings into it, and
scanning session manifests after exit. That creates two problems:

1. `--data-dir` changes Cline into isolated/sandbox state mode.
2. A new state tree is created for every task, so a later `--id` invocation
   does not naturally see the transcript created by the earlier run.

Multica should let Cline own its durable state under the CLI's default home.
Multica should persist only the opaque session ID that Cline returns through
its native history control plane.

---

## 2. Confirmed constraints

| Constraint | Decision |
| --- | --- |
| ACP | Not supported by the company Cline fork for security reasons |
| Execution transport | Keep one-shot `spawn` + `--json` NDJSON |
| `--json + --id` | Fixed in the internal fork, according to its owner; local verification still required |
| Session creation | Cline generates and persists the ID |
| Session lookup | `cline history --json` returns `sessionId`, time, cwd, prompt metadata, and pid |
| Multica persistence | Store only the Cline session ID and workdir |
| Cline database/files | Read through the native CLI; Multica does not modify them |
| Ambiguity | Fail closed with an empty session ID; never resume a guessed session |

---

## 3. Why child PID is not a universal identity

`cmd.Process.Pid` is the PID of the immediate process created by Go. It is not
necessarily the process whose PID Cline stores in history.

The npm-installed open-source Cline 3.0.46 process tree observed during the Go
probe was:

```text
Multica-style Go process
  -> Node `cline` launcher              (cmd.Process.Pid)
       -> platform `.cline` binary
            -> long-lived Cline Hub     (history.pid)
```

The launcher uses `spawnSync` rather than `exec`, so the platform binary already
has a different PID from `cmd.Process.Pid`. Cline may then dispatch more than
one session through one long-lived Hub.

Observed concurrent run:

```text
Go child PID A: 20997
Go child PID B: 20996
history PID A:  19572
history PID B:  19572
```

PID `19572` was a live process with this command:

```text
.cline --cline-hub-daemon --host 127.0.0.1 ...
```

There is no operating-system PID collision here. The fields identify different
process layers.

The internal fork may have a different launcher and may store the immediate
spawn PID. If an internal-fork probe proves that contract, PID can be a hard
match for that binary. Until then, PID is only a supporting signal and a PID
mismatch must not reject an otherwise unique history candidate.

---

## 4. Multica workdir invariants

The current execution environment significantly reduces history ambiguity.

### 4.1 Managed workdirs

A fresh managed task uses:

```text
{workspacesRoot}/{workspaceID}/{shortTaskID}/workdir
```

`execenv.Prepare` creates this path from the workspace and task IDs. Fresh task
IDs are unique, so concurrently running managed tasks normally have different
CWDs.

### 4.2 Reused workdirs

For a follow-up, Multica may reuse `PriorWorkDir`. The daemon already clears
`PriorSessionID` unless the execution runs in exactly that recorded workdir.

A reused run does not need fresh history discovery: it already has the prior
session ID and passes it through `--id`. History lookup is needed primarily for
the first fresh run that creates the conversation.

### 4.3 Local directories

A `local_directory` task uses the user-supplied absolute path as its CWD.
Multiple issues may therefore refer to the same directory. Before execution,
the daemon acquires a mutex keyed by the real path; another task targeting the
same path waits instead of running concurrently.

Within one daemon, the local-path lock means a CWD + start-time match cannot be
confused by two simultaneous Multica tasks using the same local directory.

### 4.4 Consequence for discovery

For normal Multica scheduling, the strongest portable identity is:

```text
history delta + canonical CWD + start-time window
```

PID can strengthen this identity when its semantics are proven. Prompt or task
markers are final tie-breakers, not primary requirements.

---

## 5. Launch contract

### 5.1 Fresh run

```bash
# No --data-dir: Cline uses its durable default home.
# The internal fork keeps the existing Multica stdin/argv transport contract.
cline --json \
  -c <workdir> \
  [-m <model>] \
  <prompt-transport-required-by-internal-fork>
```

Multica continues to block user overrides for `--json`, `--id`, `--data-dir`,
`--config`, cwd, model, and other daemon-owned control flags.

### 5.2 Resume run

```bash
cline --json \
  -c <same-workdir> \
  [-m <model>] \
  --id <prior-session-id> \
  <prompt-transport-required-by-internal-fork>
```

The internal fork owns `--json + --id` compatibility. Multica's existing
`gateResumeToReusedWorkdir` remains mandatory.

---

## 6. Fresh session discovery

### 6.1 Before spawn

Run the same resolved executable used for the task:

```bash
<cline-executable> history --json --limit <page-size>
```

Record the existing session IDs. Also record:

- Multica task ID
- canonical workdir (`filepath.Clean`, plus symlink resolution when possible)
- time immediately before `cmd.Start`
- immediate child PID as an observational signal

History failure must not block the agent task. It only disables session resume
for that fresh run.

### 6.2 Lookup timing

Perform two best-effort lookups:

1. **Early lookup:** after the first semantic `agent_event`, with a small bounded
   retry if the history row has not been committed yet. On success, emit a
   `MessageStatus` carrying the session ID so the daemon pins it while the task
   is still running.
2. **Final lookup:** after `cmd.Wait`, to populate `Result.SessionID` and cover a
   missed or delayed early lookup.

An initial implementation may ship final lookup first, but early lookup is
required before considering crash recovery complete.

### 6.3 Candidate filters

Read native JSON history pages until an entry is older than the lookup window.
Do not assume a fixed limit is enough under high daemon concurrency.

A fresh candidate must satisfy all of these portable conditions:

1. `sessionId` is non-empty and was not in the pre-spawn snapshot.
2. `startedAt` is within the observed run window, with a small clock-skew
   allowance (for example, start minus 30 seconds through lookup plus 30
   seconds).
3. Canonical `cwd` or `workspaceRoot` equals `ExecOptions.Cwd`.
4. The entry represents a non-interactive CLI session when those fields are
   present.

Additional signals, in priority order:

1. **PID:** hard condition only if the internal fork is proven to store the
   immediate spawned process PID; otherwise a matching PID is only a score.
2. **Prompt:** compare the normalized payload already known to the backend.
3. **Task marker:** if needed, include an inert Multica task ID marker in the
   runtime brief and compare it only when the portable filters leave multiple
   candidates.

### 6.4 Decision

```text
exactly one candidate -> accept sessionId
zero candidates       -> empty SessionID + warning
multiple candidates   -> empty SessionID + warning
```

Never select merely the newest record. Never use hook `taskId` / `agentId` as a
resume ID.

### 6.5 Early pin

On a unique early match, emit:

```go
Message{
    Type:      MessageStatus,
    Status:    "running",
    SessionID: candidate.SessionID,
}
```

The existing daemon consumer persists the first non-empty message session ID
with `PinTaskSession`. The final result repeats the same ID:

```go
Result{
    // Existing status/output/usage fields...
    SessionID: candidate.SessionID,
}
```

---

## 7. Resume behavior

When `ExecOptions.ResumeSessionID` is non-empty:

1. Require the exact prior workdir through the existing daemon gate.
2. Pass `--id <ResumeSessionID>` to the internal fork.
3. Do not require a new history record; Cline may update the existing session.
4. Preserve the prior ID in `Result.SessionID` unless the CLI provides positive
   evidence that the session was rejected or replaced.
5. Map a structured internal-fork "session not found" result to
   `ResumeRejected=true` when that signal is available.

The daemon's fresh-session fallback remains responsible for a positively
rejected stale ID. Network, quota, authentication, or provider failures must
not be misclassified as session rejection.

---

## 8. Concurrency analysis

| Scenario | Identity / serialization |
| --- | --- |
| Two fresh managed tasks | Different task-derived CWDs |
| Follow-up on a managed task | Same CWD intentionally; prior session ID already known |
| Two tasks on one local directory | Real-path mutex serializes them |
| User runs Cline interactively | Filter interactive/source fields and CWD/time |
| Cline Hub serves multiple sessions | PID may be shared; CWD/time/history delta remain usable |
| PID reused later by the OS | Time window and history delta reject old entries |
| Multiple unexplained candidates | Fail closed; optional prompt/task marker can disambiguate |

---

## 9. Implementation impact

### `server/pkg/agent/cline.go`

- Remove per-run `prepareClinePaths` from `Execute`.
- Stop passing `--data-dir`.
- Remove provider-settings seed/copy helpers from the active path.
- Add native history JSON types and a bounded history command runner.
- Snapshot history before fresh spawn.
- Resolve a unique candidate early and after `Wait`.
- Emit `MessageStatus.SessionID` and set `Result.SessionID`.
- Keep `--data-dir`, `--config`, and `--id` blocked from custom args.
- On resume, retain the known ID instead of looking for a newly created ID.

### Tests

- Fake CLI supports both `--json` and `history --json` commands.
- Fresh run creates one history entry and returns its session ID.
- Concurrent different CWDs resolve independently.
- Same CWD + distinct time/prompt resolves correctly.
- Shared history PID does not cause a wrong match.
- A proven direct PID match can narrow candidates.
- Zero and multiple candidates return an empty ID.
- Resume passes `--id` and retains the known ID.
- History command failure does not fail an otherwise successful task.
- No test resolves or runs an ambient user-installed CLI.

Real internal-fork smoke tests remain behind the repository's
`agentintegration` build tag and explicit opt-in environment variable.

---

## 10. Probe evidence (2026-07-23)

A temporary Go program reproduced Multica's process model with
`exec.CommandContext`, no TTY, stdin/stdout/stderr pipes, and two concurrent
goroutines. Both fresh runs used the same CWD to make correlation harder than
the normal managed-workdir case.

Observed:

- Both fresh `--json` runs completed successfully.
- Neither NDJSON stream contained a resume-capable session ID.
- Native `history --json` returned one unique session for each prompt/time
  combination.
- Go child PIDs and history PIDs differed.
- In a later concurrent run, both history entries used the same long-lived Hub
  PID while their Go child PIDs were different.

This proves that default-home history discovery works under concurrent
headless Go execution and that child PID cannot be a universal hard key for the
open-source launcher/Hub architecture.

The local executable was open-source Cline 3.0.46. Its `--json + --id` behavior
does not represent the internal fork and is not an acceptance result for the
resume path.

---

## 11. Internal fork acceptance checklist

1. Confirm `history --json` exists on the resolved internal executable.
2. Confirm fresh `--json` without `--data-dir` writes a durable history entry.
3. Capture immediate Go child PID, process tree, and history PID semantics.
4. Run two fresh managed-style tasks concurrently in different CWDs.
5. Run an adversarial pair concurrently in one CWD with distinct prompts.
6. Resolve both IDs through the algorithm in section 6 without swapping them.
7. Resume both IDs concurrently with the internal fork's fixed
   `--json + --id` path and verify each recalls only its own prior token.
8. Verify a missing ID produces positive structured rejection if supported.
9. Verify history appears early enough for mid-run pinning.
10. Verify no Multica `--data-dir` or settings seed remains in the launch.

---

## 12. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Native history schema changes | Loose JSON decoding; unknown fields ignored; malformed response disables resume only |
| History output is large | Paginate with a bounded page size and total byte/time limits |
| History command races session commit | Bounded early retry plus final post-`Wait` lookup |
| Shared Hub PID | Do not require PID unless internal semantics are proven |
| Same local CWD | Existing real-path mutex serializes Multica tasks |
| Wrong candidate | Exact CWD/time/delta filters and fail-closed ambiguity |
| Cline home growth | Cline owns its native retention; Multica does not delete Cline sessions in this change |
| User deletes native history | Resume becomes unavailable; never guess another session |
| Internal fork diverges from OSS | Run the acceptance checklist against the configured binary |

---

## 13. Phasing

| Phase | Deliverable | Status |
| --- | --- | --- |
| S0 | Document revised design and open-source concurrency evidence | Complete |
| S1 | Run internal-fork acceptance probe | Pending executable access |
| S2 | Implement default-home native-history discovery and unit tests | Not started |
| S3 | Verify early pin and end-to-end internal-fork resume | Not started |

---

## 14. Decision log

| Date | Decision |
| --- | --- |
| 2026-07-15 | Plan 01 used per-run `--data-dir`, settings seed, and manifest discovery |
| 2026-07-23 | Reject per-run `--data-dir` because the next run does not naturally share that state tree |
| 2026-07-23 | Use Cline's default home and native `history --json` as the session-ID source |
| 2026-07-23 | Reject child PID as a universal hard identity after observing wrapper and shared-Hub PID semantics |
| 2026-07-23 | Use history delta + canonical CWD + time as portable hard filters |
| 2026-07-23 | Keep PID as conditional evidence and prompt/task marker as final disambiguation only |
| 2026-07-23 | Do not use ACP; rely on the internal fork's fixed one-shot `--json + --id` path |

---

## 15. Reviewer summary

| Question | Answer |
| --- | --- |
| Does Multica own Cline session files? | No |
| Does Multica pass `--data-dir`? | No |
| How is a fresh ID obtained? | Native history snapshot/delta filtered by canonical CWD and time |
| Is PID required? | Only if the internal fork proves it is the immediate spawn PID |
| Is a prompt token required? | No; it is an optional final tie-breaker |
| How is resume performed? | Existing session ID + exact reused workdir + internal fork `--json --id` |
| What happens on ambiguity? | Empty ID; no guessed resume |
| Why is concurrency safe? | Managed CWD isolation, local-path serialization, time/delta filtering, fail-closed selection |
