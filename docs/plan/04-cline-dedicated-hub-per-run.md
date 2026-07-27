# 04 - Dedicated Cline Hub per Fresh Multica Run

**Status:** Confirmed, public-Hub implementation (2026-07-25); sole source of
truth for the Cline launch, session discovery, and resume design
**Supersedes:** plans 01, 02, and 03. The implemented stdin prompt transport
from plan 02 is retained and consolidated here.
**Scope:** Use a dedicated local Hub only for fresh Cline invocations that need
SessionID discovery. Resume invocations already have an authoritative ID and
use Cline's normal long-lived shared Hub. Both paths keep sessions in Cline's
persistent state root.
**Implementation plan:**
[`04-cline-dedicated-hub-per-run-implementation.md`](./04-cline-dedicated-hub-per-run-implementation.md)

ACP remains out of scope.

This decision assumes that the user does not configure or use Cline cron or
schedule functionality. If that assumption cannot be enforced, the dedicated
Hub design must not be enabled.

---

## 1. Confirmed decisions

1. Every fresh Multica invocation gets a dedicated Cline Hub.
2. Every fresh-run Hub gets a private discovery file and loopback port.
3. Cline continues to use its normal persistent state root. Multica does not
   create a temporary `--data-dir` and does not copy provider settings.
4. Multica forces both paths to use a Hub backend. Fresh runs use the dedicated
   Hub; resume runs use the normal long-lived shared Hub. Silent fallback to a
   local runtime is forbidden.
5. Fresh session discovery uses the dedicated Hub PID as the primary identity,
   strengthened by history delta, canonical workdir, root-session fields, and
   a narrow session-ID timestamp window.
6. Candidate selection is exact-one and fail-closed. Multica never selects the
   newest or closest history entry.
7. Resume uses the previously persisted native session ID and the same workdir,
   with no dedicated-Hub lifecycle or fresh-session discovery.
8. The existing one-shot `--json` NDJSON transport remains in place.
9. The full Multica prompt is sent on stdin. The positional argv prompt is a
   single newline sentinel required by Cline's headless prompt gate.
10. Multica's context owns the wall-clock timeout. The CLI `--timeout` flag is
    not used.

---

## 2. Why the dedicated Hub is the fresh-session identity

An npm-installed Cline command can contain several process layers:

```text
Multica -> Node launcher -> platform binary -> Cline Hub
```

The PID stored in Cline history is the process that owns the session runtime.
With a normal shared Hub, concurrent fresh sessions can therefore have the same
PID, and the Go child PID is not a reliable discovery identity.

When Multica starts one Hub for one invocation, the Hub discovery record gives
Multica the exact PID expected in that invocation's history row:

```text
history.pid == dedicatedHubPID
```

A user's ordinary local Cline process and another concurrent fresh Multica run
use different Hub PIDs and different discovery records. They are rejected
before time or prompt matching is considered. Resume does not need this
identity because Multica already supplies the exact native SessionID.

---

## 3. Required runtime contract

### 3.1 Persistent Cline state

Do not pass `--data-dir` on the task command. Use either:

- Cline's normal default state root; or
- one fixed installation-level `CLINE_DATA_DIR` used for all Multica Cline
  invocations.

The state root must not change between fresh and resumed runs.

The session backend must remain SQLite. In dedicated-Hub mode, SQLite
initialization failure is fatal. Cline must not silently fall back to its file
session index because that index is not a multi-process persistence contract.

### 3.2 Fresh-run Hub environment

For every fresh invocation, create a private runtime directory outside the
project worktree:

```text
<task-temp>/cline-hub/<run-id>/production.json
```

The discovery record contains a local authentication token and must never be
logged. Runtime validation rejects symlinks and unexpected file types, but does
not require POSIX permission bits because they have no equivalent meaning on
Windows.

The Hub and all Multica-owned Cline control commands use one environment:

```text
CLINE_HUB_HOST=127.0.0.1
CLINE_HUB_PORT=<unique-port>
CLINE_HUB_DISCOVERY_PATH=<private-production.json>
CLINE_SESSION_BACKEND_MODE=hub
```

Multica must remove `CLINE_VCR` and any inherited setting that forces a local
or remote session backend. The task must fail if the dedicated Hub cannot be
used; it must not continue through Cline's automatic local-runtime fallback.

Use the verified public `--port 0` contract, then read the actual bound port
from the authenticated discovery/status record. Do not preallocate ports.

### 3.3 Public-Hub isolation boundary

Multica does not modify or fork Cline. Isolation therefore uses the strongest
controls available in the public Hub:

- a cryptographically unique private discovery directory per fresh run
- an OS-assigned loopback port and discovery authentication token
- forced Hub backend mode with no Multica fallback
- one Multica task CLI per dedicated Hub
- authenticated shutdown followed by PID-reuse-safe process cleanup
- `--zen` blocked from custom arguments

The public Hub does not expose owner lease, single-root admission,
abort-owned-root, wait-drained, or tool-child environment scrubbing. A tool
that deliberately starts another Cline while inheriting the private Hub
environment can therefore attach to that Hub, and a hard daemon crash can
leave a detached Hub until external process/temp cleanup. These are accepted
residual risks; normal completion, cancellation and timeout remain bounded and
cleaned up by Multica.

### 3.4 No-schedule product assumption

This plan assumes all of the following:

- no enabled file cron specifications
- no enabled Hub schedules
- no queued or lease-expired schedule runs
- no schedule is created while a Multica task is running

Completed, failed, and cancelled one-off schedule records are inert. A
completed execution of a recurring schedule does not mean that the schedule
itself is disabled.

If these conditions cannot be guaranteed, dedicated-Hub fresh execution is not
supported.

---

## 4. Fresh-run lifecycle

### 4.1 Allocate and start the Hub

1. Generate a cryptographically unique Multica run ID.
2. Create the private discovery directory and path.
3. Start `cline hub start` with the dedicated environment.
4. Poll readiness with a short bounded timeout.
5. Read the discovery record and authenticated Hub status.
6. Record the Hub PID, Hub ID, actual port, start time, and a platform process
   start token used to detect PID reuse.

Validate all of the following:

- host is loopback
- port is the expected or OS-assigned port
- PID is positive and live
- discovery and authenticated status agree on Hub ID, PID, URL, and port
- process start time is later than the Multica Hub launch attempt
- process command identity belongs to the configured Cline executable

The later task wrapper's `cmd.Process.Pid` is not the Hub PID.

### 4.2 Snapshot history

Before starting the task CLI, run the same configured executable against the
same persistent state root:

```bash
cline history --json --limit <bounded-limit>
```

Record the existing root session IDs. A history failure does not prevent the
agent task from running, but it disables fresh SessionID discovery for that
run. Multica must not weaken the filters when the snapshot is unavailable.

History calls require their own short timeout and bounded output. Do not poll
history continuously: the command opens the shared persistence backend and can
perform stale-session reconciliation.

### 4.3 Start the NDJSON task

Immediately before `cmd.Start`, record:

```text
cmdStartWallMs
cmdStartMonotonic
```

The wall-clock value must be captured before `cmd.Start`; a timestamp recorded
inside the stdout goroutine is too late to be the lower bound.

Launch:

```bash
cline --json \
  -c <workdir> \
  [-m <model>] \
  $'\n'
```

Write the full payload to stdin and close it for EOF:

```text
SystemPrompt + "\n\n" + userPrompt
```

Drain stdout concurrently with the stdin write so neither pipe can deadlock.

### 4.4 Freeze the session creation window

While parsing stdout, identify the first valid Cline protocol record:

- valid JSON
- top-level type is `hook_event`, `agent_event`, or `run_result`
- top-level `ts` is present and parseable according to the pinned public contract

Freeze this value once:

```text
firstProtocolTimestamp
```

Normalize it to Unix epoch milliseconds before comparing it with the native
session ID prefix.

Also record the parent process's monotonic receipt time for diagnostics. Do not
replace the frozen upper bound with task completion time, provider response
time, or `cmd.Wait` time.

The preferred early-lookup trigger is the first semantic
`agent_event.iteration_start`. Cline generates and persists the session before
that event and emits the event before making the provider request. Provider
queueing therefore does not widen the session creation window.

If no valid protocol timestamp is emitted, Multica must not use the possibly
much later process end time as a substitute. Fresh discovery returns an empty
SessionID unless a later public version exposes an equivalent native correlation field.

### 4.5 Timestamp condition

For the pinned public version, parse the millisecond prefix from the native
session ID. The canonical condition is:

```text
cmdStartWallMs
    <= sessionIdTimestamp
    <= firstProtocolTimestampMs
```

If public-version stress tests prove that a platform can step wall clock across
processes during startup, the implementation may apply one fixed, small
compatibility allowance:

```text
cmdStartWallMs - clockSkew
    <= sessionIdTimestamp
    <= firstProtocolTimestampMs + clockSkew
```

Use a small bounded `clockSkew` established by public-version stress tests. It
exists only for wall-clock adjustment and timestamp precision; it must not grow
with provider queue duration or total task duration.

`startedAt` must be consistent with the same creation interval. Session-ID
format and timestamp parsing are a version-pinned compatibility contract. If the
format changes, discovery fails closed rather than accepting arbitrary IDs.

### 4.6 Exact candidate filters

A fresh history candidate must satisfy every condition:

1. `sessionId` is non-empty and absent from the pre-run snapshot.
2. `pid == dedicatedHubPID`.
3. `source == CLI` when source is present.
4. `interactive == false` when the field is present.
5. `isSubagent == false` and `parentSessionId` is empty.
6. Canonical `cwd` or `workspaceRoot` equals the canonical Multica workdir.
7. `sessionIdTimestamp` is inside the frozen creation interval.
8. `startedAt` is inside and consistent with that interval.

Decision:

```text
exactly one candidate -> accept and pin its sessionId
zero candidates       -> empty SessionID + warning/metric
multiple candidates   -> empty SessionID + warning/metric
```

Never select the newest record, nearest timestamp, highest score, hook
`taskId`, or hook `agentId`. Prompt comparison may be logged as a diagnostic
signal but is not allowed to turn multiple candidates into a winner.

### 4.7 Early pin and final confirmation

After the first semantic event, perform a small bounded history retry. Once a
unique candidate is found, emit a running status message containing the native
session ID.

The backend treats the first discovered non-empty ID as immutable for that run
and emits it once through the existing status-pin path. This Cline adapter does
not modify the shared daemon pin implementation. A terminal callback carries
the same `Result.SessionID`, so normal completion still persists the ID when an
early pin request fails.

After `cmd.Wait`, perform one final lookup when necessary:

- repeat the same frozen interval; never widen it to process end time
- if early pin succeeded, final lookup may confirm but must not replace it
- a different final candidate is an invariant violation and must not overwrite
  the persisted ID
- history failure must not change an otherwise authoritative task result

---

## 5. Resume lifecycle

When `ExecOptions.ResumeSessionID` is non-empty:

1. Require exact workdir reuse through the existing daemon gate.
2. Use Cline's normal long-lived shared Hub and persistent state root. Do not
   create a private discovery path, allocate a private port, start a dedicated
   Hub or stop a Hub for this invocation.
3. Force Hub backend mode without replacing the shared Hub's normal
   discovery/connection configuration.
4. Pass `--id <ResumeSessionID>` to `cline --json`.
5. Skip the entire fresh discovery path: no pre-run history snapshot, session
   creation timestamps, candidate matching, early lookup, or final lookup.
6. Continue streaming NDJSON execution events through Multica's existing task
   message pipeline while the resumed task runs.
7. Preserve the prior ID in the result unless Cline positively reports that
   the session was rejected or replaced.
8. Network, provider queue, quota, authentication, and provider 5xx failures
   are not resume rejection.
9. Multica cancellation terminates only the resumed task CLI. The attached
   `--id` client must abort its current turn on disconnect; Multica does not
   call shared-Hub status/abort/stop APIs and must never signal or clean up the
   long-lived shared Hub.

When the public CLI exposes structured `session_not_found` or replacement evidence,
map it to `ResumeRejected=true`; only that positive signal may trigger the
daemon's fresh-session fallback.

---

## 6. Cancellation, timeout, shutdown, and crash recovery

For a fresh run, the task CLI exiting is not sufficient evidence that the
dedicated Hub session has stopped. A disconnected client must not leave that
owned session running.

Fresh-run normal completion:

1. wait for the task CLI
2. finish or confirm SessionID discovery
3. request authenticated graceful Hub shutdown
4. wait for the recorded process identity to exit
5. remove only the private runtime artifacts for that run

Fresh-run cancellation or timeout:

1. request authenticated shutdown of the dedicated Hub
2. terminate the task CLI process tree
3. use a stronger process termination only after revalidating PID, start token,
   command identity, Hub ID, port, and discovery path

Daemon crash:

- the public Hub has no owner lease, so a hard daemon crash can leave the
  dedicated Hub running
- normal completion/cancel/timeout cleanup still verifies the private
  discovery identity and OS process start token before stronger termination
- cleanup must never stop the user's default Hub or another Multica run

Resume completion:

1. wait for the task CLI and drain its NDJSON events
2. preserve the supplied SessionID unless positive rejection/replacement was
   reported
3. do not run history discovery, Hub drain, Hub stop, PID validation, or private
   runtime cleanup

Resume cancellation or timeout:

1. terminate only the Multica task CLI process tree
2. rely on the attached `--id` CLI contract to abort that session's current
   turn when its client disconnects
3. never call shared-Hub control APIs or stop/signal the long-lived shared Hub

Resume relies on the public attached `--id` client behavior. Multica never
controls the shared Hub, because doing so would risk unrelated local sessions.

---

## 7. Concurrency behavior

| Scenario | Result |
| --- | --- |
| User runs ordinary local Cline during fresh run | Different Hub PID; rejected |
| Two fresh Multica runs | Different discovery files, ports, Hub PIDs, and normally workdirs |
| Two tasks target one local directory | Existing real-path mutex serializes them |
| Follow-up/resume run | Prior native ID plus exact workdir reuse on the long-lived shared Hub |
| Provider queues for a long time | No wider discovery window; first iteration occurs before provider request |
| Agent spawns subagents | Root-session filter excludes children |
| Project script invokes Cline during fresh run | Public Hub may accept it if private variables are inherited; accepted residual risk |
| Multiple unexplained candidates | Empty SessionID; never guess |
| History or pin service is unavailable | Task may finish, but resume capability is reported unavailable |

Fresh-run Hubs and the long-lived shared Hub still share settings, session
SQLite files, caches, and other global Cline metadata. SQLite WAL/busy handling
reduces ordinary contention but does not replace concurrent fresh/resume stress
testing.

---

## 8. Failure policy

| Failure | Required behavior |
| --- | --- |
| Fresh Hub start/readiness failure | Do not start the fresh task CLI |
| Fresh or shared Hub connection failure | Fail; never fall back to local runtime |
| SQLite backend initialization failure | Fail; never use file session fallback |
| Pre-run history failure | Run task, disable fresh SessionID discovery |
| Zero or multiple candidates | Empty SessionID; preserve task result |
| Early pin HTTP failure | Preserve the ID in `Result.SessionID`; terminal callback performs the final persistence attempt |
| No valid first protocol timestamp | Empty fresh SessionID; do not use process end time |
| Resume rejected with positive evidence | Empty SessionID + `ResumeRejected=true` |
| Provider/network/auth failure during resume | Preserve prior SessionID |
| Fresh Hub graceful stop timeout | Revalidate identity, then bounded stronger cleanup or stale sweep |
| Resume task CLI disconnect | Shared Hub aborts only that session's current turn; Multica preserves the ID and does not call Hub controls |

The preferred error mode is loss of resume availability, never attachment to a
guessed session.

---

## 9. Public Cline acceptance gate

The configured public Cline CLI must provide and support:

1. `hub start/status/stop` or one equivalent ephemeral attached-run command for
   fresh invocations.
2. Private discovery-path selection and unique/OS-assigned loopback ports.
3. Authenticated readiness/status containing Hub ID, PID, URL, port, and start
   time.
4. Forced Hub runtime mode with no silent local fallback.
5. Hub PID persisted in root history rows.
6. Concurrent SQLite access with no file-backend fallback.
7. Stable session ID timestamp format or another native correlation field.
8. Native history fields required by the exact candidate filters.
9. `--json + --id` through the normal long-lived shared Hub, including
    attached-client disconnect semantics that abort only the current turn.
10. The existing stdin prompt plus newline argv sentinel contract.

The Cline 3.0.46/core 0.0.65 probe and opt-in real Hub lifecycle smoke pin the
currently used public behavior. Upgrades must rerun these checks.

Execution-log visibility is a Multica adapter responsibility. Capture the exact
Cline build's NDJSON and map all existing text, thinking, tool start/result,
error, usage, and terminal variants through `clineBackend.processEvents` and the
existing task-message/WebSocket pipeline. A Hub event bridge is not part of
this design. Missing public NDJSON semantics remain documented compatibility
limits rather than a reason to modify Cline.

---

## 10. Verification requirements

### Unit tests

- fresh environment forces the dedicated Hub and removes local/VCR overrides
- resume environment forces Hub mode but does not inject a private discovery
  path/port or start/stop a Hub
- argv contains no `--data-dir`, `--config`, `-s`, or `-t`
- stdin carries the full payload; argv carries only the newline sentinel
- lower bound is captured before `cmd.Start`
- first valid protocol timestamp freezes the upper bound
- provider delay does not widen the candidate interval
- PID, root-session, source, interactive, cwd, history-delta, ID timestamp, and
  `startedAt` filters are all mandatory
- zero and multiple candidates return empty SessionID
- an early-pinned ID is never replaced
- resume preserves the prior ID except on positive rejection
- pin state changes only after persistence succeeds and retries transient errors
- history failure does not fail an otherwise valid agent result
- fresh cancellation aborts the dedicated Hub session before cleanup
- resume cancellation only terminates the task CLI; disconnect aborts the
  current turn without any Multica shared-Hub control call
- no default test resolves or executes an ambient user-installed CLI

### Public-Hub integration tests

- two concurrent dedicated Hubs sharing one persistent state root
- local default Hub plus several dedicated Hubs
- many concurrent fresh runs with distinct and identical CWDs
- resume through the existing long-lived shared Hub while fresh dedicated Hubs
  run concurrently
- 30-minute simulated provider queue
- CLI disconnect while the model is active
- port collision and stale discovery/PID reuse
- SQLite busy/migration contention fails safely without file fallback
- authenticated stop never targets the default or another dedicated Hub
- resume never invokes any shared-Hub control command or process termination

Real-agent smoke tests remain behind the repository's `agentintegration` build
tag and explicit account/quota opt-in.

---

## 11. Implementation impact

Primary implementation areas:

- `server/pkg/agent/cline.go`
  - remove temporary data-dir and settings-copy ownership
  - branch fresh dedicated-Hub execution from shared-Hub resume before setup
  - add fresh-run Hub lifecycle and environment sanitization
  - add native history snapshot and exact matching
  - capture and freeze protocol timestamps
  - emit early SessionID status and preserve resume IDs
- existing daemon path
  - keep exact-workdir resume gating and status/terminal SessionID persistence
  - no shared `client.go` / `daemon.go` changes are part of this internal Cline
    adapter
No Cline source modification or internal fork is part of this implementation.
The accepted public-Hub residual risks in section 3.3 are not completion
blockers. Implementation is complete when the Multica changes and applicable
tests above pass.

---

## 12. Obsolete plans

The following documents are historical records only and must not be used to
implement or review current Cline behavior:

- `01-cline-session-id-data-dir.md`
- `02-cline-prompt-stdin-hybrid.md`
- `03-cline-session-id-default-home.md`

Plan 01's per-run data directory and settings seed are retired. Plan 03's shared
default Hub matching is retired. Plan 02's stdin transport remains in force only
as consolidated in this document.
