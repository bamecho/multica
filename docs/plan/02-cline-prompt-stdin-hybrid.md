# 02 — Cline Long Prompt via Stdin + Newline Argv Sentinel

**Status:** **Obsolete as a standalone plan (2026-07-24). Do not use as a
source of truth.** The implemented stdin transport is retained and consolidated
in [`04-cline-dedicated-hub-per-run.md`](./04-cline-dedicated-hub-per-run.md).
**Scope:** Avoid OS command-line length limits when launching Cline with Multica runtime brief + user prompt  
**Related:**

- [`docs/plan/04-cline-dedicated-hub-per-run.md`](./04-cline-dedicated-hub-per-run.md) — confirmed launch, prompt, session, and resume design
- [`docs/plan/01-cline-session-id-data-dir.md`](./01-cline-session-id-data-dir.md) — historical data-dir session approach
- [`docs/cline-ndjson-multica-adapter-plan.md`](../cline-ndjson-multica-adapter-plan.md) — current adapter puts combined prompt on argv
- Backend: `server/pkg/agent/cline.go` (`buildClineArgs`)
- Daemon: `providerNeedsInlineSystemPrompt("cline")` → `ExecOptions.SystemPrompt = runtimeBrief`

---

## 1. Problem

Multica currently builds:

```text
combined = SystemPrompt + "\n\n" + userPrompt
argv     = [ --json, --auto-approve, true, -c …, combined ]
```

Large `runtimeBrief` (meta skill / agent instructions / repo lists) plus a long task prompt can exceed OS limits:

| Platform | Typical failure | Rough limit |
| --- | --- | --- |
| **Windows** | “The command is too long” / spawn failure | `CreateProcess` ~**32 767** chars; `cmd.exe` ~**8 191** |
| **Linux** | `argument list too long` / `E2BIG` | Total `ARG_MAX` ~MB; **single arg** often `MAX_ARG_STRLEN` = **128 KiB** |
| **macOS** | same class as Linux (`E2BIG`) | `ARG_MAX` often tighter than Linux in practice |

Cline’s npm entry is a **Node wrapper** that `spawnSync`s the real binary with the same argv — long prompts are subject to the limit **twice** on the way in.

This is independent of session-id discovery (plan 01). A run can fail **before** any NDJSON is produced.

---

## 2. Goal

1. Keep Multica able to deliver full runtime brief + user task without depending on argv capacity.
2. Stay compatible with Cline 3.x headless `--json` on Linux, macOS, and Windows.
3. Prefer mechanisms already supported by the CLI (stdin / flags), not shell-specific tricks.
4. **Do not inject a meaningful short sentence into the model’s user prompt** solely to satisfy the CLI gate (avoids changing task semantics).
5. Fail with a clear Multica error if the chosen delivery path is unsupported on the installed binary.

---

## 3. Why not pure stdin / why not a short English sentinel?

### 3.1 What actually helps for OS length

| Mechanism | Avoids argv limit? | Notes |
| --- | --- | --- |
| Shell `cat file \| cline …` | Yes for payload | Demo only; Multica must not shell out |
| Go `cmd.Stdin = reader` / `StdinPipe()` | **Yes** | Correct daemon implementation |
| Keep huge string as last argv | **No** | Current path |

Pipe means **put large text on the child’s stdin FD**, not `sh -c "… | cline"`.

### 3.2 Official docs vs real `--json` gate (Cline 3.0.40)

README / help suggest prompt **or** piped stdin. Real binary check under `--json` is approximately:

```js
if (o.outputMode === "json" && (o.interactive || !o.prompt)) {
  // "JSON output mode requires a prompt argument or piped stdin ..."
  exit 1;
}
```

Important details:

1. Gate only tests **truthiness of `o.prompt`** (argv positional). It does **not** check whether stdin is piped or has bytes.
2. Error text says “or piped stdin”, but **piped stdin alone does not pass** this branch.
3. After the gate, stdin is read when non-TTY + FIFO/file (`stdinHasPipedInput`), then roughly:
   `final = prompt ? prompt + "\n" + stdin : stdin` (then wrapped / trimmed into `<user_input>`).

| Launch shape | Result (3.0.40) |
| --- | --- |
| No argv prompt, stdin piped | **Gate fail** — same error even when stdin has content |
| Empty string `""` as prompt + stdin | **Gate fail** — `!""` is true |
| Meaningful short argv + stdin | OK — session user message is `argv + "\n" + stdin` |
| **Single `"\n"` (or `" "` / `"\t"`) argv + full stdin** | **OK** — gate passes; session user message is **stdin only** (whitespace trimmed away) |
| Full combined text on argv only | Hits OS length limits as size grows |

Documented human pattern (`git diff | cline "review these changes"`) still works, but Multica **does not** want the short argv string to become part of the model prompt when the real task already lives on stdin.

### 3.3 Chosen delivery: **newline sentinel argv + full payload on stdin**

```text
argv:  cline --json
         --data-dir <isolated>          # Multica-owned; seeds settings (plan 01)
         -c <workdir>
         [-m model] [--id prior]
         $'\n'                          # single newline — gate only; no semantic content

stdin: SystemPrompt + "\n\n" + userPrompt   # full Multica payload
         EOF after write

# Not on argv: --config (sandbox ignores it for providers; settings are seeded),
#              --auto-approve (CLI default true; still blocked in CustomArgs)
```

| Channel | Content |
| --- | --- |
| **argv last element** | Exactly one newline character: `"\n"`. **Not** `""`. **Not** a short English instruction. |
| **stdin** | Full combined payload: `ExecOptions.SystemPrompt` (runtime brief) + separator + `BuildPrompt` user task. |
| **Not on argv** | Any large brief or task text |

**Why `"\n"` instead of a short sentence**

| Option | Gate | Model user message | Semantic risk |
| --- | --- | --- | --- |
| `""` | Fail | — | — |
| `"Execute the Multica task on stdin."` | Pass | sentinel **prepended** to stdin | Short sentence may bias / dilute task |
| **`"\n"`** | Pass (truthy string) | **stdin only** after trim | Minimal — preferred |
| `" "` / `"\t"` | Pass (same class) | stdin only after trim | Same idea; prefer `"\n"` as the fixed constant |

**Why not brief→stdin / task→argv hybrid as default**

That split also avoids length limits and matches README demos, but:

- User task on argv still competes with OS limits if the task itself is huge.
- Two channels make framing harder (“is the real instruction argv or stdin?”).
- Multica product preference (2026-07-15): **one full payload on stdin**, argv only satisfies the CLI gate with a **meaningless** character.

Hybrid (task argv + brief stdin) remains a **documented alternative** if a future binary rejects whitespace-only prompts (e.g. gate becomes `!prompt.trim()`).

---

## 4. OS-specific notes

### 4.1 Linux

- `execve` + pipe; Go `os/exec` is fine.
- Single-arg limit (~128 KiB) is the usual cliff for Multica’s combined string on argv.
- Env size counts toward `ARG_MAX` total; Multica already injects many `MULTICA_*` vars — another reason to keep argv small.

### 4.2 macOS

- Same Unix model as Linux.
- Treat **stdin payload as default on all Unix**, not only when length exceeds a Linux-specific constant.

### 4.3 Windows

- Strictest practical limit (~32 KiB CreateProcess). Multica briefs can exceed this easily → **Windows is a primary motivation**.
- Implement with `StdinPipe` / assigned `io.Reader`, **never** `cmd.exe /c "echo … | cline …"`.
- UTF-8: write stdin as UTF-8 bytes; avoid locale code-page conversion.
- Node wrapper: once payload leaves argv, the wrapper’s `spawnSync` argv stays short — fixes double-spawn blowups.
- Pass argv newline as a real `"\n"` string in `os/exec` (not shell-escaped `$'\n'`).

### 4.4 Shared rules

1. Write stdin fully, then **Close** (EOF). Leaving the pipe open can hang the CLI.
2. Start the process before or while writing per Go best practices (`Start` + write + `Close` + drain stdout).
3. Do not put multi-megabyte briefs on `-s` / `--system` either — still argv.
4. Shell `|` is for humans; daemon uses **FDs**.
5. Log `argv_prompt_repr` as e.g. `"\\n"` / `newline_sentinel`, never dump full stdin body into info logs.

---

## 5. Fallback chain

Ordered:

1. **Newline sentinel + full stdin (this plan)** — default.
2. **Space / tab sentinel** — same class as newline if a platform mishandles `"\n"` in argv (unlikely with Go `exec`).
3. **Semantic hybrid** — user task on argv, brief on stdin — only if whitespace sentinel fails gate on a target binary (`!trim(prompt)` style check).
4. **AGENTS.md / workdir files** — already written by `InjectRuntimeConfig` for cline; can reduce stdin brief if binary reliably reads them.
5. **Hard fail with clear error** — if even the sentinel path cannot start, or if Multica detects an empty payload when a task was expected.

Do **not** invent `@file` prompt flags without verification on the target CLI.

---

## 6. Implementation sketch

| Area | Change |
| --- | --- |
| `buildClineArgs` | Stop appending full `SystemPrompt + prompt`. Append **`"\n"`** as the last positional argument when using the stdin payload path. |
| `clineBackend.Execute` | Build `payload := SystemPrompt + "\n\n" + prompt` (skip empty sides cleanly). `stdin, err := cmd.StdinPipe()`; after `Start`, write payload; `Close`. If payload empty, still pass `"\n"` argv and close stdin immediately (or define no-op behavior). |
| Logging | Log flags without full payload; log `stdin_bytes` and `argv_prompt=newline_sentinel`. |
| Blocked args | Unchanged for protocol flags; do not reintroduce huge `-s`. |
| Tests | Fake CLI records argv **and** stdin; assert last argv == `"\n"`; assert stdin == full payload; assert brief/task not on argv. |
| Docs | Cross-link plan 01; update adapter plan §5 when implemented. |

### Concurrency / drain

Stdout NDJSON scanning continues as today. Stdin write is one-shot; brief size is typically tens–hundreds of KB — fine in one write after `Start`.

### Interaction with plan 01 (`--data-dir` + settings seed)

Orthogonal and complementary (both implemented):

```text
prep:  seed ~/.cline-sr/data/settings → <data-dir>/settings (+ data/settings)
argv:  --json --data-dir <isolated> -c … [-m …] [--id …] "\n"
stdin: full Multica payload
disk:  session_id discovery after Wait (under data-dir)
```

Together they cover **start** (length / semantics + auth under sandbox) and **finish** (session id).

---

## 7. Risks

| Risk | Mitigation |
| --- | --- |
| Upstream gate becomes `!prompt.trim()` | Detect gate error string; fall back to hybrid or documented failure |
| Internal CLI ignores stdin | Probe matrix; AGENTS.md fallback |
| Whitespace argv mishandled by shell wrappers | Daemon uses `os/exec` args slice, not shell |
| Resume (`--id`) + stdin | Separate regression; plan 01 P1 |
| Double-close / early close of stdin | Single closer; tests |
| Windows encoding | UTF-8 only |
| Future reader treats empty-looking user message oddly | Session probe shows stdin-only `<user_input>` works on 3.0.40 |

---

## 8. Verification

### 8.1 Already probed (open-source Cline 3.0.40, 2026-07-15)

| Case | Result |
| --- | --- |
| `printf '…' \| cline --json …` (no argv prompt) | Gate fail |
| `argv=""` + stdin | Gate fail |
| `argv="\n"` + stdin full task containing secret token | **Gate pass**; session user = stdin only; model returned secret (`NEWLINE_OK_*`); `finish=completed` |
| `argv=" "` / `"\t"` + stdin | Gate pass (auth-limited runs still showed stdin-only user message) |
| Control: normal argv `"…pong…"` | Model returned `pong` (same env) |

### 8.2 Unit (fake CLI) when implementing

1. Last argv element is exactly `"\n"`.
2. Stdin equals full Multica payload (brief + task).
3. Existing NDJSON parsers still pass.

### 8.3 Real CLI matrix (manual / opt-in)

| Case | Linux | macOS | Windows |
| --- | --- | --- | --- |
| `"\n"` + stdin ~50–100 KiB | required | required | **required** |
| `"\n"` + empty stdin | document | document | document |
| Stdin-only, no argv prompt | expect fail | expect fail | expect fail |
| Argv combined ~40 KiB | optional | optional | expect fail |

### 8.4 Regression

- Provider env / model flags unchanged.
- No shell dependency in daemon code paths.
- Plan 01 session discovery still works with short argv.

---

## 9. Decision log

| Date | Decision |
| --- | --- |
| 2026-07-15 | Argv combined prompt is the root of “command too long” class failures |
| 2026-07-15 | Use process stdin (Go `StdinPipe`), not shell `\|` |
| 2026-07-15 | Cline 3.0.40: pure stdin under `--json` fails gate; error text “or piped stdin” does **not** match implementation |
| 2026-07-15 | Gate is truthiness of argv prompt; `"\n"` / space / tab pass; `""` does not |
| 2026-07-15 | Prefer **no semantic short argv sentence** — avoid changing model prompt meaning |
| 2026-07-15 | **Default plan: argv = `"\n"`, stdin = full `SystemPrompt + userPrompt`** |
| 2026-07-15 | Real model E2E: newline sentinel + stdin secret task completed successfully on 3.0.40 |
| 2026-07-15 | Hybrid (task argv + brief stdin) kept as fallback if whitespace gate is tightened |
| 2026-07-15 | Windows CreateProcess limit remains a primary production motivator |
| 2026-07-15 | Orthogonal to session data-dir plan (01) |
| 2026-07-15 | Plan 01 auth is settings **seed**, not `--config` on argv |

---

## 10. Open questions

1. Confirm the same `"\n"` + stdin behavior on the **internal renamed CLI** fork (not only open-source 3.0.40).
2. Exact payload framing: `SystemPrompt + "\n\n" + userPrompt` vs labeled sections (`## Multica runtime` / `## Task`) so the model prioritizes Multica rules.
3. Is open-source `-s` usable on some versions for small briefs? (Design docs said internal `-s` is unusable — reconfirm per binary; still not for large payloads.)
4. If gate is ever tightened to `trim`, should Multica auto-fall back to hybrid and log a one-line warning?

---

## 11. Summary

**Deliver the full Multica prompt on process stdin. Keep argv’s positional prompt as a single newline (`"\n"`) so Cline 3.x `--json` accepts the run without injecting meaningful text into the model’s user message.**

- Go `StdinPipe` + close-after-write on all major OSes.
- Empty argv prompt and pure-stdin-without-argv both fail the real 3.0.40 gate; `"\n"` is verified end-to-end with a live model.
- Implement in `cline.go` with tests asserting argv sentinel vs stdin payload; keep plan 01 for session ids.
