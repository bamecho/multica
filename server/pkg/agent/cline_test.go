package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"
)

func TestNewReturnsClineBackend(t *testing.T) {
	t.Parallel()
	b, err := New("cline", Config{ExecutablePath: "/nonexistent/cline"})
	if err != nil {
		t.Fatalf("New(cline) error: %v", err)
	}
	if _, ok := b.(*clineBackend); !ok {
		t.Fatalf("expected *clineBackend, got %T", b)
	}
}

func TestBuildClineArgsContract(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	args := buildClineArgs(ExecOptions{
		Cwd:             "/work",
		Model:           "gpt-test",
		ResumeSessionID: "1784085932547_prior",
		SystemPrompt:    "brief here",
		CustomArgs: []string{
			"--json", "--auto-approve", "false",
			"-c", "/evil", "--id", "x",
			"--data-dir", "/evil-data",
			"--config", "/evil-config",
			"-s", "nope", "-t", "9", "-m", "other", "--verbose",
		},
	}, logger)

	joined := strings.Join(args, "\x00")
	// Required fixed flags
	if args[0] != "--json" {
		t.Fatalf("prefix = %v, want --json first", args[:min(1, len(args))])
	}
	if containsArg(args, "--auto-approve") {
		t.Errorf("Multica must not pass --auto-approve (CLI default true); got %v", args)
	}
	if containsArg(args, "--data-dir") {
		t.Errorf("resume must not use a private data-dir: %v", args)
	}
	if containsArg(args, "--config") {
		t.Errorf("Multica must not pass --config (sandbox seeds settings); got %v", args)
	}
	if !containsArgPair(args, "-c", "/work") {
		t.Errorf("missing -c /work in %v", args)
	}
	if !containsArgPair(args, "-m", "gpt-test") {
		t.Errorf("missing -m gpt-test in %v", args)
	}
	if !containsArgPair(args, "--id", "1784085932547_prior") {
		t.Errorf("missing --id in %v", args)
	}
	// Positional prompt is only the newline gate sentinel — never brief/task.
	last := args[len(args)-1]
	if last != clineArgvPromptSentinel {
		t.Errorf("argv prompt sentinel = %q, want %q", last, clineArgvPromptSentinel)
	}
	for _, a := range args {
		if strings.Contains(a, "brief here") || strings.Contains(a, "user task") {
			t.Errorf("payload text leaked onto argv: %q full=%v", a, args)
		}
	}
	// Never pass -s / -t
	for i, a := range args {
		if a == "-s" || a == "--system" || a == "-t" || a == "--timeout" {
			t.Errorf("forbidden flag %q present at index %d in %v", a, i, args)
		}
	}
	// Blocked custom overrides must not appear as user-controlled values
	if strings.Contains(joined, "/evil") || strings.Contains(joined, "/evil-data") || strings.Contains(joined, "/evil-config") {
		t.Errorf("blocked Multica-owned override survived: %v", args)
	}
	// Allowed custom arg survives
	if !containsArg(args, "--verbose") {
		t.Errorf("allowed custom --verbose missing: %v", args)
	}
}

func TestBuildClineStdinPayload(t *testing.T) {
	t.Parallel()
	if got := buildClineStdinPayload("only user", ExecOptions{}); got != "only user" {
		t.Fatalf("task-only payload = %q", got)
	}
	if got := buildClineStdinPayload("user task", ExecOptions{SystemPrompt: "brief here"}); got != "brief here\n\nuser task" {
		t.Fatalf("brief+task payload = %q", got)
	}
	// Whitespace-only system prompt is treated as empty (skipped).
	if got := buildClineStdinPayload("user", ExecOptions{SystemPrompt: "  \n\t"}); got != "user" {
		t.Fatalf("whitespace system prompt payload = %q", got)
	}
}

func TestBuildClineArgsNoSystemPrompt(t *testing.T) {
	t.Parallel()
	args := buildClineArgs(ExecOptions{}, slog.Default())
	if args[len(args)-1] != clineArgvPromptSentinel {
		t.Fatalf("prompt sentinel = %q, want %q", args[len(args)-1], clineArgvPromptSentinel)
	}
	if containsArg(args, "--data-dir") {
		t.Errorf("unexpected --data-dir: %v", args)
	}
	if containsArg(args, "--config") {
		t.Errorf("must not pass --config: %v", args)
	}
	if containsArg(args, "-c") || containsArg(args, "-m") || containsArg(args, "--id") {
		t.Errorf("unexpected optional flags: %v", args)
	}
}

// fakeClineNDJSONScript writes argv to CLINE_ARGS_FILE, stdin to
// CLINE_STDIN_FILE, and emits the NDJSON stream from CLINE_STDOUT_FILE (or a
// built-in success stream). Exit code from CLINE_EXIT_CODE (default 0).
// Stderr from CLINE_STDERR if set.
func fakeClineNDJSONScript() string {
	// Args are NUL-delimited so multi-line values survive round-trip.
	// Stdin is captured fully (may contain newlines) for payload asserts.
	return `#!/bin/sh
if [ -n "$CLINE_ARGS_FILE" ]; then
  printf '%s\0' "$@" > "$CLINE_ARGS_FILE"
fi
if [ -n "$CLINE_ENV_FILE" ]; then
  env | sort > "$CLINE_ENV_FILE"
fi
STDIN_BODY=""
if [ -n "$CLINE_STDIN_FILE" ]; then
  cat > "$CLINE_STDIN_FILE"
  STDIN_BODY=$(cat "$CLINE_STDIN_FILE")
else
  STDIN_BODY=$(cat)
fi
if [ -n "$CLINE_STDERR" ]; then
  printf '%s\n' "$CLINE_STDERR" >&2
fi
if [ -n "$CLINE_STDOUT_FILE" ] && [ -f "$CLINE_STDOUT_FILE" ]; then
  cat "$CLINE_STDOUT_FILE"
else
  printf '%s\n' '{"type":"hook_event","event":{"type":"agent_start","taskId":"conv_not_a_session","agentId":"agent_not_session"}}'
  printf '%s\n' '{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"Working..."}}'
  # Real CLI run_result has no sessionId — do not emit one here.
  printf '%s\n' '{"type":"run_result","finishReason":"completed","text":"Summary","usage":{"inputTokens":10,"outputTokens":5}}'
fi
exit "${CLINE_EXIT_CODE:-0}"
`
}

type clineExecCapture struct {
	messages     []Message
	result       Result
	argsFile     string
	stdinFile    string
	envFile      string
	hubStarts    int
	hubStops     int
	historyCalls int
}

func runClineWithStdout(t *testing.T, ndjson string, opts ExecOptions, env map[string]string) (messages []Message, result Result, argsFile string) {
	t.Helper()
	cap := runClineExecute(t, "user prompt", ndjson, opts, env)
	return cap.messages, cap.result, cap.argsFile
}

func runClineExecute(t *testing.T, prompt, ndjson string, opts ExecOptions, env map[string]string) clineExecCapture {
	t.Helper()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	writeTestExecutable(t, fakePath, []byte(fakeClineNDJSONScript()))

	stdoutFile := filepath.Join(tempDir, "stdout.ndjson")
	if err := os.WriteFile(stdoutFile, []byte(ndjson), 0o644); err != nil {
		t.Fatalf("write stdout fixture: %v", err)
	}
	argsFile := filepath.Join(tempDir, "argv.txt")
	stdinFile := filepath.Join(tempDir, "stdin.txt")
	envFile := filepath.Join(tempDir, "env.txt")

	merged := map[string]string{
		"CLINE_ARGS_FILE":   argsFile,
		"CLINE_STDIN_FILE":  stdinFile,
		"CLINE_STDOUT_FILE": stdoutFile,
		"CLINE_ENV_FILE":    envFile,
		"TMPDIR":            tempDir,
	}
	for k, v := range env {
		merged[k] = v
	}

	backend, err := New("cline", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            merged,
	})
	if err != nil {
		t.Fatalf("New(cline): %v", err)
	}
	cline := backend.(*clineBackend)
	var lifecycleMu sync.Mutex
	hubStarts := 0
	hubStops := 0
	cline.startFreshHub = func(_ context.Context, _ string, _ string, extra map[string]string, taskTempDir string) (*clineHub, error) {
		lifecycleMu.Lock()
		hubStarts++
		lifecycleMu.Unlock()
		runtimeInfo := clineHubRuntime{
			RuntimeDir:    filepath.Join(taskTempDir, "cline-hub", "fake-run"),
			DiscoveryPath: filepath.Join(taskTempDir, "cline-hub", "fake-run", "production.json"),
		}
		record := clineHubRecord{
			HubID:           "fake-hub",
			ProtocolVersion: "v1",
			Host:            "127.0.0.1",
			Port:            32145,
			URL:             "ws://127.0.0.1:32145/hub",
			PID:             4242,
			StartedAt:       time.Now(),
		}
		return &clineHub{
			runtime:   runtimeInfo,
			discovery: record,
			status:    record,
			env:       buildClineFreshHubEnv(extra, runtimeInfo.DiscoveryPath, record),
			stopFn: func(context.Context) error {
				lifecycleMu.Lock()
				hubStops++
				lifecycleMu.Unlock()
				return nil
			},
		}, nil
	}
	var historyMu sync.Mutex
	historyCalls := 0
	cline.runFreshHistory = func(context.Context, string, string, []string) ([]clineHistoryEntry, error) {
		historyMu.Lock()
		defer historyMu.Unlock()
		historyCalls++
		if env["CLINE_TEST_HISTORY_ERROR"] == "1" {
			return nil, fmt.Errorf("test history failure")
		}
		sessionID := strings.TrimSpace(env["CLINE_TEST_HISTORY_SESSION_ID"])
		if sessionID == "" || historyCalls == 1 {
			return nil, nil
		}
		startedMs, ok := parseClineSessionIDTimestamp(sessionID)
		if !ok {
			return nil, fmt.Errorf("invalid test SessionID %q", sessionID)
		}
		startedAt := time.UnixMilli(startedMs)
		cli := "cli"
		falseValue := false
		return []clineHistoryEntry{{
			SessionID:   sessionID,
			PID:         4242,
			Source:      &cli,
			Interactive: &falseValue,
			IsSubagent:  &falseValue,
			Cwd:         opts.Cwd,
			StartedAt:   &startedAt,
		}}, nil
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := backend.Execute(ctx, prompt, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var messages []Message
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range session.Messages {
			messages = append(messages, m)
		}
	}()
	result := <-session.Result
	<-done
	lifecycleMu.Lock()
	starts, stops := hubStarts, hubStops
	lifecycleMu.Unlock()
	historyMu.Lock()
	completedHistoryCalls := historyCalls
	historyMu.Unlock()
	return clineExecCapture{
		messages:     messages,
		result:       result,
		argsFile:     argsFile,
		stdinFile:    stdinFile,
		envFile:      envFile,
		hubStarts:    starts,
		hubStops:     stops,
		historyCalls: completedHistoryCalls,
	}
}

func TestClineExecuteHubStartFailureDoesNotStartTask(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	startedPath := filepath.Join(tempDir, "task-started")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nprintf started > \"$CLINE_TASK_STARTED\"\n"))
	backend := &clineBackend{
		cfg: Config{
			ExecutablePath: fakePath,
			Logger:         slog.Default(),
			Env: map[string]string{
				"TMPDIR":             tempDir,
				"CLINE_TASK_STARTED": startedPath,
			},
		},
		startFreshHub: func(context.Context, string, string, map[string]string, string) (*clineHub, error) {
			return nil, fmt.Errorf("readiness failed")
		},
	}
	if _, err := backend.Execute(context.Background(), "prompt", ExecOptions{}); err == nil || !strings.Contains(err.Error(), "readiness failed") {
		t.Fatalf("Execute error = %v", err)
	}
	if _, err := os.Stat(startedPath); !os.IsNotExist(err) {
		t.Fatalf("task executable ran after Hub failure: %v", err)
	}
}

func TestClineExecuteHistoryFailureDoesNotFailTask(t *testing.T) {
	t.Parallel()
	cap := runClineExecute(t, "prompt", `{"type":"run_result","ts":1784999999999,"finishReason":"completed","text":"ok"}`+"\n", ExecOptions{}, map[string]string{
		"CLINE_TEST_HISTORY_ERROR": "1",
	})
	if cap.result.Status != "completed" || cap.result.Output != "ok" || cap.result.SessionID != "" {
		t.Fatalf("result = %+v", cap.result)
	}
	if cap.hubStarts != 1 || cap.hubStops != 1 || cap.historyCalls != 1 {
		t.Fatalf("lifecycle: starts=%d stops=%d history=%d", cap.hubStarts, cap.hubStops, cap.historyCalls)
	}
}

func TestClineExecuteSuccessTextAndUsage(t *testing.T) {
	t.Parallel()
	// Synthetic NDJSON sessionId is not the real CLI contract and must not
	// become Result.SessionID. Fresh IDs come only from exact Hub history.
	ndjson := strings.Join([]string{
		`{"type":"hook_event","event":{"type":"agent_start"},"sessionId":"ses_early"}`,
		`{"type":"agent_event","event":{"type":"iteration_start","iteration":1}}`,
		`{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"Hello stream"}}`,
		`{"type":"agent_event","event":{"type":"usage","inputTokens":100,"outputTokens":20}}`,
		`{"type":"agent_event","event":{"type":"done","reason":"completed","text":"mid done"}}`,
		`{"type":"run_result","finishReason":"completed","text":"Final summary","usage":{"inputTokens":120,"outputTokens":40},"sessionId":"ses_final"}`,
		``,
	}, "\n")

	messages, result, _ := runClineWithStdout(t, ndjson, ExecOptions{Model: "m1"}, nil)

	if result.Status != "completed" {
		t.Fatalf("status=%q error=%q", result.Status, result.Error)
	}
	if result.Output != "Final summary" {
		t.Errorf("output=%q, want Final summary (last run_result)", result.Output)
	}
	if result.SessionID != "" {
		t.Errorf("session id = %q, want empty when no disk session (NDJSON sessionId is not authoritative)", result.SessionID)
	}
	u, ok := result.Usage["m1"]
	if !ok {
		t.Fatalf("usage missing for m1: %#v", result.Usage)
	}
	if u.InputTokens != 120 || u.OutputTokens != 40 {
		t.Errorf("usage = %+v, want run_result authoritative 120/40", u)
	}

	var sawText bool
	for _, m := range messages {
		if m.Type == MessageText && strings.Contains(m.Content, "Hello stream") {
			sawText = true
		}
		if m.Type == MessageStatus && m.SessionID != "" {
			t.Errorf("unexpected stream SessionID pin on status message: %+v", m)
		}
	}
	if !sawText {
		t.Errorf("expected text message; messages=%+v", messages)
	}
}

func TestClineExecuteToolHooks(t *testing.T) {
	t.Parallel()
	ndjson := strings.Join([]string{
		`{"type":"hook_event","event":{"type":"tool_call"}}`,
		`{"type":"hook_event","event":{"type":"tool_result"}}`,
		`{"type":"hook_event","event":{"type":"tool_call","name":"bash","callId":"c1","input":{"command":"pwd"}}}`,
		`{"type":"hook_event","event":{"type":"tool_result","callId":"c1","output":"/tmp\n"}}`,
		`{"type":"run_result","finishReason":"completed","text":"ok"}`,
	}, "\n")

	messages, result, _ := runClineWithStdout(t, ndjson, ExecOptions{}, nil)
	if result.Status != "completed" {
		t.Fatalf("status=%q error=%q", result.Status, result.Error)
	}

	var toolUses, toolResults []Message
	for _, m := range messages {
		switch m.Type {
		case MessageToolUse:
			toolUses = append(toolUses, m)
		case MessageToolResult:
			toolResults = append(toolResults, m)
		}
	}
	if len(toolUses) != 2 {
		t.Fatalf("tool uses = %d, want 2; %+v", len(toolUses), toolUses)
	}
	if toolUses[0].Tool != "tool" {
		t.Errorf("unnamed tool_call Tool=%q, want generic tool", toolUses[0].Tool)
	}
	if toolUses[0].CallID == "" {
		t.Error("expected synthetic call id for unnamed tool_call")
	}
	if toolUses[1].Tool != "bash" || toolUses[1].CallID != "c1" {
		t.Errorf("named tool_use = %+v", toolUses[1])
	}
	if len(toolResults) != 2 {
		t.Fatalf("tool results = %d, want 2", len(toolResults))
	}
	if toolResults[1].CallID != "c1" || toolResults[1].Output != "/tmp\n" {
		t.Errorf("tool result = %+v", toolResults[1])
	}
}

func TestClineExecuteAbortedRunResult(t *testing.T) {
	t.Parallel()
	ndjson := `{"type":"run_result","finishReason":"aborted","text":"stopped"}` + "\n"
	_, result, _ := runClineWithStdout(t, ndjson, ExecOptions{}, nil)
	if result.Status != "failed" {
		t.Fatalf("status=%q, want failed for aborted", result.Status)
	}
	if result.Output != "stopped" {
		t.Errorf("output=%q", result.Output)
	}
}

func TestClineExecuteErrorRunResult(t *testing.T) {
	t.Parallel()
	ndjson := `{"type":"run_result","finishReason":"error","text":"boom"}` + "\n"
	_, result, _ := runClineWithStdout(t, ndjson, ExecOptions{}, nil)
	if result.Status != "failed" {
		t.Fatalf("status=%q, want failed", result.Status)
	}
}

func TestClineExecuteMissingRunResultFallsBackToDoneAndExit(t *testing.T) {
	t.Parallel()
	ndjson := strings.Join([]string{
		`{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"partial"}}`,
		`{"type":"agent_event","event":{"type":"done","reason":"completed","text":"from done"}}`,
		`not json at all`,
		`{"type":"unknown_top","foo":1}`,
	}, "\n")
	_, result, _ := runClineWithStdout(t, ndjson, ExecOptions{}, map[string]string{
		"CLINE_EXIT_CODE": "0",
	})
	if result.Status != "completed" {
		t.Fatalf("status=%q error=%q, want completed from done+exit0", result.Status, result.Error)
	}
	if result.Output != "from done" {
		t.Errorf("output=%q, want from done", result.Output)
	}
}

func TestClineExecuteMissingRunResultFailedExit(t *testing.T) {
	t.Parallel()
	ndjson := `{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"x"}}` + "\n"
	_, result, _ := runClineWithStdout(t, ndjson, ExecOptions{}, map[string]string{
		"CLINE_EXIT_CODE": "2",
	})
	if result.Status != "failed" {
		t.Fatalf("status=%q error=%q, want failed on non-zero exit without run_result", result.Status, result.Error)
	}
}

func TestClineExecuteTimeoutFromStderr(t *testing.T) {
	t.Parallel()
	ndjson := `{"type":"run_result","finishReason":"aborted","text":""}` + "\n"
	_, result, _ := runClineWithStdout(t, ndjson, ExecOptions{}, map[string]string{
		"CLINE_STDERR": `{"type":"error","message":"run timed out after 600s"}`,
	})
	if result.Status != "timeout" {
		t.Fatalf("status=%q error=%q, want timeout from stderr", result.Status, result.Error)
	}
}

func TestClineExecuteSkipsBadLines(t *testing.T) {
	t.Parallel()
	ndjson := strings.Join([]string{
		`this is not json`,
		`{broken`,
		`{"type":"run_result","finishReason":"completed","text":"survived"}`,
	}, "\n")
	_, result, _ := runClineWithStdout(t, ndjson, ExecOptions{}, nil)
	if result.Status != "completed" || result.Output != "survived" {
		t.Fatalf("got status=%q output=%q", result.Status, result.Output)
	}
}

func TestClineExecuteArgvSentinelAndStdinPayload(t *testing.T) {
	t.Parallel()
	ndjson := `{"type":"run_result","finishReason":"completed","text":"ok"}` + "\n"
	userPrompt := "user prompt with unique task token TASK_SECRET_42"
	brief := "RUNTIME BRIEF with unique brief token BRIEF_SECRET_99"
	wantPayload := brief + "\n\n" + userPrompt

	cap := runClineExecute(t, userPrompt, ndjson, ExecOptions{
		Cwd:             t.TempDir(),
		Model:           "model-x",
		ResumeSessionID: "1784085932547_prior",
		SystemPrompt:    brief,
		CustomArgs: []string{
			"--json", "-t", "99", "-s", "no",
			"--data-dir", "/evil-data",
			"--config", "/evil-config",
			"--extra", "keep",
		},
	}, nil)
	if cap.result.Status != "completed" {
		t.Fatalf("status=%q error=%q", cap.result.Status, cap.result.Error)
	}
	if cap.hubStarts != 0 || cap.hubStops != 0 || cap.historyCalls != 0 {
		t.Fatalf("resume touched fresh lifecycle: starts=%d stops=%d history=%d", cap.hubStarts, cap.hubStops, cap.historyCalls)
	}
	raw, err := os.ReadFile(cap.argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	lines := splitNULArgs(raw)
	if len(lines) < 1 || lines[0] != "--json" {
		t.Fatalf("argv prefix = %v, want --json first", lines)
	}
	if containsArg(lines, "--auto-approve") {
		t.Errorf("must not pass --auto-approve: %v", lines)
	}
	joined := strings.Join(lines, " ")
	for _, line := range lines {
		if line == "-s" || line == "--system" || line == "-t" || line == "--timeout" || line == "99" {
			t.Errorf("banned flag/value in argv: %q full=%v", line, lines)
		}
		if strings.Contains(line, "RUNTIME BRIEF") || strings.Contains(line, "TASK_SECRET") || strings.Contains(line, "BRIEF_SECRET") {
			t.Errorf("payload text leaked onto argv element %q full=%v", line, lines)
		}
		if line == "/evil-data" || line == "/evil-config" {
			t.Errorf("blocked Multica-owned override survived: %v", lines)
		}
	}
	if containsArg(lines, "--config") {
		t.Errorf("must not pass --config (settings seeded into data-dir): %v", lines)
	}
	if containsArg(lines, "--data-dir") {
		t.Errorf("resume must not use --data-dir: %v", lines)
	}
	if !containsArgPair(lines, "-m", "model-x") {
		t.Errorf("missing -m: %v", lines)
	}
	if !containsArgPair(lines, "--id", "1784085932547_prior") {
		t.Errorf("missing --id: %v", lines)
	}
	last := lines[len(lines)-1]
	if last != clineArgvPromptSentinel {
		t.Errorf("argv last = %q (len=%d), want newline sentinel", last, len(last))
	}
	if !strings.Contains(joined, "--extra") || !strings.Contains(joined, "keep") {
		t.Errorf("allowed custom args missing: %v", lines)
	}

	stdinRaw, err := os.ReadFile(cap.stdinFile)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if string(stdinRaw) != wantPayload {
		t.Errorf("stdin payload = %q, want %q", string(stdinRaw), wantPayload)
	}
}

func TestBuildClineResumeEnvUsesSharedHub(t *testing.T) {
	t.Setenv("CLINE_HUB_HOST", "127.0.0.1")
	t.Setenv("CLINE_HUB_PORT", "7777")
	t.Setenv("CLINE_HUB_DISCOVERY_PATH", "/shared/production.json")
	t.Setenv("CLINE_SESSION_BACKEND_MODE", "local")
	t.Setenv("CLINE_VCR", "1")

	env := buildClineEnv(map[string]string{
		"CLINE_SESSION_BACKEND_MODE": "file",
		"CLINE_VCR":                  "2",
	}, true)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"CLINE_HUB_HOST=127.0.0.1",
		"CLINE_HUB_PORT=7777",
		"CLINE_HUB_DISCOVERY_PATH=/shared/production.json",
		"CLINE_SESSION_BACKEND_MODE=hub",
	} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("missing %q in env", want)
		}
	}
	if strings.Contains(joined, "CLINE_VCR=") || strings.Contains(joined, "CLINE_SESSION_BACKEND_MODE=local") || strings.Contains(joined, "CLINE_SESSION_BACKEND_MODE=file") {
		t.Fatalf("managed resume env was not sanitized: %s", joined)
	}
}

func TestClineExecuteResumePreservesIDUnlessStructuredRejection(t *testing.T) {
	t.Parallel()
	const sessionID = "1784085932547_prior"

	_, failed, argsFile := runClineWithStdout(t,
		`{"type":"run_result","finishReason":"error","text":"provider unavailable"}`+"\n",
		ExecOptions{ResumeSessionID: sessionID}, nil)
	if failed.SessionID != sessionID || failed.ResumeRejected {
		t.Fatalf("provider failure result = %+v", failed)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if containsArg(splitNULArgs(raw), "--data-dir") {
		t.Fatalf("resume argv contains --data-dir: %v", splitNULArgs(raw))
	}

	_, rejected, _ := runClineWithStdout(t,
		`{"type":"run_result","finishReason":"error","code":"session_not_found","text":"missing"}`+"\n",
		ExecOptions{ResumeSessionID: sessionID}, nil)
	if rejected.SessionID != "" || !rejected.ResumeRejected {
		t.Fatalf("structured rejection result = %+v", rejected)
	}
}

func TestClineExecuteFreshIgnoresResumeRejectionCode(t *testing.T) {
	t.Parallel()
	sessionTimestamp := time.Now().Add(500 * time.Millisecond).UnixMilli()
	firstProtocolTimestamp := sessionTimestamp + 500
	wantID := fmt.Sprintf("%d_fresh", sessionTimestamp)
	ndjson := strings.Join([]string{
		fmt.Sprintf(`{"type":"agent_event","ts":%d,"event":{"type":"iteration_start"}}`, firstProtocolTimestamp),
		fmt.Sprintf(`{"type":"run_result","ts":%d,"finishReason":"completed","code":"session_replaced","text":"done"}`, firstProtocolTimestamp+1),
	}, "\n")

	cap := runClineExecute(t, "fresh run", ndjson, ExecOptions{Cwd: t.TempDir()}, map[string]string{
		"CLINE_TEST_HISTORY_SESSION_ID": wantID,
	})
	if cap.result.SessionID != wantID || cap.result.ResumeRejected {
		t.Fatalf("fresh result = %+v, want SessionID %q without resume rejection", cap.result, wantID)
	}
}

func TestClineExecuteDiscoversSessionIDFromDedicatedHubHistory(t *testing.T) {
	t.Parallel()
	sessionTimestamp := time.Now().Add(500 * time.Millisecond).UnixMilli()
	firstProtocolTimestamp := sessionTimestamp + 500
	wantID := fmt.Sprintf("%d_testhost", sessionTimestamp)
	ndjson := strings.Join([]string{
		fmt.Sprintf(`{"type":"agent_event","ts":%d,"event":{"type":"iteration_start","iteration":1}}`, firstProtocolTimestamp),
		fmt.Sprintf(`{"type":"agent_event","ts":%d,"event":{"type":"content_end","contentType":"text","text":"hi"}}`, firstProtocolTimestamp+1),
		fmt.Sprintf(`{"type":"run_result","ts":%d,"finishReason":"completed","text":"done","usage":{"inputTokens":1,"outputTokens":1}}`, firstProtocolTimestamp+2),
	}, "\n")
	cwd := t.TempDir()
	cap := runClineExecute(t, "discover me", ndjson, ExecOptions{
		Cwd:   cwd,
		Model: "m-history",
	}, map[string]string{
		"CLINE_TEST_HISTORY_SESSION_ID": wantID,
	})
	if cap.result.Status != "completed" {
		t.Fatalf("status=%q error=%q", cap.result.Status, cap.result.Error)
	}
	if cap.result.SessionID != wantID {
		t.Fatalf("SessionID=%q, want history session %q", cap.result.SessionID, wantID)
	}
	if cap.hubStarts != 1 || cap.hubStops != 1 || cap.historyCalls < 2 {
		t.Fatalf("fresh lifecycle: starts=%d stops=%d history=%d", cap.hubStarts, cap.hubStops, cap.historyCalls)
	}
	raw, err := os.ReadFile(cap.argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	lines := splitNULArgs(raw)
	if containsArg(lines, "--data-dir") {
		t.Fatalf("fresh argv contains --data-dir: %v", lines)
	}
	envRaw, err := os.ReadFile(cap.envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CLINE_HUB_HOST=127.0.0.1",
		"CLINE_HUB_PORT=32145",
		"CLINE_SESSION_BACKEND_MODE=hub",
	} {
		if !strings.Contains(string(envRaw), want+"\n") {
			t.Errorf("fresh task env missing %q", want)
		}
	}
	var pinned bool
	for _, message := range cap.messages {
		if message.Type == MessageStatus && message.SessionID == wantID {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("missing early/final SessionID status: %+v", cap.messages)
	}
	if cap.result.Output != "done" {
		t.Errorf("output=%q, want done", cap.result.Output)
	}
}

func TestClineExecuteEmptySessionIDWithoutExactHistoryCandidate(t *testing.T) {
	t.Parallel()
	ndjson := strings.Join([]string{
		`{"type":"hook_event","event":{"type":"agent_start"},"sessionId":"ses_stream_only"}`,
		`{"type":"run_result","finishReason":"completed","text":"ok","sessionId":"ses_stream_only"}`,
	}, "\n")
	_, result, argsFile := runClineWithStdout(t, ndjson, ExecOptions{Cwd: t.TempDir()}, nil)
	if result.Status != "completed" {
		t.Fatalf("status=%q error=%q", result.Status, result.Error)
	}
	if result.SessionID != "" {
		t.Fatalf("SessionID=%q, want empty without exact history candidate", result.SessionID)
	}
	if result.Output != "ok" {
		t.Errorf("output=%q", result.Output)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if containsArg(splitNULArgs(raw), "--data-dir") {
		t.Errorf("unexpected --data-dir: %v", splitNULArgs(raw))
	}
}

func TestClineExecuteStdinTaskOnlyNoBrief(t *testing.T) {
	t.Parallel()
	ndjson := `{"type":"run_result","finishReason":"completed","text":"ok"}` + "\n"
	userPrompt := "task-only prompt UNIQUE_TASK_ONLY"
	cap := runClineExecute(t, userPrompt, ndjson, ExecOptions{}, nil)
	if cap.result.Status != "completed" {
		t.Fatalf("status=%q error=%q", cap.result.Status, cap.result.Error)
	}
	raw, err := os.ReadFile(cap.argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	lines := splitNULArgs(raw)
	if lines[len(lines)-1] != clineArgvPromptSentinel {
		t.Fatalf("argv last = %q, want newline sentinel", lines[len(lines)-1])
	}
	for _, line := range lines {
		if strings.Contains(line, "UNIQUE_TASK_ONLY") {
			t.Errorf("task text on argv: %q", line)
		}
	}
	stdinRaw, err := os.ReadFile(cap.stdinFile)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if string(stdinRaw) != userPrompt {
		t.Errorf("stdin = %q, want %q", string(stdinRaw), userPrompt)
	}
}

func TestClineProcessEventsUnit(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 32)
	ndjson := strings.Join([]string{
		`{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"A"}}`,
		`{"type":"run_result","finishReason":"max_iterations","text":"stopped early"}`,
		`{"type":"run_result","finishReason":"completed","text":"last wins"}`,
	}, "\n")
	res := b.processEvents(context.Background(), strings.NewReader(ndjson), ch)
	close(ch)
	if !res.sawRunResult {
		t.Fatal("expected sawRunResult")
	}
	if res.status != "completed" {
		t.Errorf("status=%q, last run_result should win", res.status)
	}
	if res.output != "last wins" {
		t.Errorf("output=%q", res.output)
	}
}

func TestClineProcessEventsFreezesFirstProtocolTimestamp(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 1)
	ndjson := strings.Join([]string{
		`{"type":"unknown","ts":1784085932500}`,
		`{"type":"agent_event","ts":1784085932547,"event":{"type":"iteration_start"}}`,
		`{"type":"run_result","ts":1784085999999,"finishReason":"completed"}`,
	}, "\n")
	res := b.processEvents(context.Background(), strings.NewReader(ndjson), ch)
	if res.firstProtocolTimestampMs != 1784085932547 {
		t.Fatalf("first timestamp = %d", res.firstProtocolTimestampMs)
	}
	if !res.sawIterationStart {
		t.Fatal("iteration_start was not observed")
	}
}

func TestClineProcessEventsReportsLookupTriggersBeforeEOF(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	reader, writer := io.Pipe()
	observed := make(chan clineProtocolObservation, 2)
	done := make(chan clineScanResult, 1)
	go func() {
		done <- b.processEventsObserved(context.Background(), reader, make(chan Message, 1), func(observation clineProtocolObservation) {
			observed <- observation
		})
	}()
	if _, err := io.WriteString(writer, `{"type":"agent_event","ts":1784085932547,"event":{"type":"iteration_start","iteration":1}}`+"\n"); err != nil {
		t.Fatal(err)
	}

	first := <-observed
	second := <-observed
	if first.FirstTimestampMs != 1784085932547 || first.IterationStarted {
		t.Fatalf("first observation = %+v", first)
	}
	if !second.IterationStarted || second.FirstTimestampMs != 0 {
		t.Fatalf("second observation = %+v", second)
	}
	select {
	case result := <-done:
		t.Fatalf("scanner returned before EOF: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.firstProtocolTimestampMs != 1784085932547 || !result.sawIterationStart {
		t.Fatalf("scan result = %+v", result)
	}
}

func TestClineProcessEventsContentSnapshotsAndThinking(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 16)
	ndjson := strings.Join([]string{
		`{"type":"agent_event","event":{"type":"content_start","contentType":"thinking","contentBlockId":"r1"}}`,
		`{"type":"agent_event","event":{"type":"content_update","contentType":"thinking","contentBlockId":"r1","snapshot":" reason"}}`,
		`{"type":"agent_event","event":{"type":"content_end","contentType":"thinking","contentBlockId":"r1","snapshot":" reasoned\n"}}`,
		`{"type":"agent_event","event":{"type":"content_start","contentType":"text","contentBlockId":"t1"}}`,
		`{"type":"agent_event","event":{"type":"content_update","contentType":"text","contentBlockId":"t1","delta":" answer"}}`,
		`{"type":"agent_event","event":{"type":"content_end","contentType":"text","contentBlockId":"t1","snapshot":" answer\n"}}`,
	}, "\n")

	res := b.processEvents(context.Background(), strings.NewReader(ndjson), ch)
	close(ch)
	var thinking, text strings.Builder
	for msg := range ch {
		switch msg.Type {
		case MessageThinking:
			thinking.WriteString(msg.Content)
		case MessageText:
			text.WriteString(msg.Content)
		}
	}
	if got := thinking.String(); got != " reasoned\n" {
		t.Fatalf("thinking = %q, want preserved deduplicated snapshot", got)
	}
	if got := text.String(); got != " answer\n" {
		t.Fatalf("text = %q, want preserved delta plus snapshot suffix", got)
	}
	if res.output != " answer\n" {
		t.Fatalf("output = %q", res.output)
	}
}

func TestClineProcessEventsCline3046AgentContract(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 16)
	ndjson := strings.Join([]string{
		`{"type":"agent_event","ts":"2026-07-24T10:00:00.100Z","event":{"type":"content_start","contentType":"reasoning","reasoning":"inspect "}}`,
		`{"type":"agent_event","ts":"2026-07-24T10:00:00.200Z","event":{"type":"content_end","contentType":"reasoning","reasoning":"inspect repo"}}`,
		`{"type":"agent_event","ts":"2026-07-24T10:00:00.300Z","event":{"type":"content_start","contentType":"text","text":"answer "}}`,
		`{"type":"agent_event","ts":"2026-07-24T10:00:00.400Z","event":{"type":"content_end","contentType":"text","text":"answer done"}}`,
		`{"type":"agent_event","event":{"type":"error","error":{"name":"ProviderError","message":"queue unavailable"},"recoverable":true,"iteration":1}}`,
		`{"type":"run_result","finishReason":"completed","text":"answer done"}`,
	}, "\n")

	res := b.processEvents(context.Background(), strings.NewReader(ndjson), ch)
	close(ch)
	var got []Message
	for msg := range ch {
		got = append(got, msg)
	}
	if len(got) != 5 {
		t.Fatalf("messages = %d, want 5: %+v", len(got), got)
	}
	wants := []struct {
		type_   MessageType
		content string
	}{
		{MessageThinking, "inspect "},
		{MessageThinking, "repo"},
		{MessageText, "answer "},
		{MessageText, "done"},
		{MessageError, "queue unavailable"},
	}
	for i, want := range wants {
		if got[i].Type != want.type_ || got[i].Content != want.content {
			t.Errorf("message[%d] = %+v, want type=%s content=%q", i, got[i], want.type_, want.content)
		}
	}
	if res.output != "answer done" || res.firstProtocolTimestampMs != 1784887200100 {
		t.Fatalf("scan result = %+v", res)
	}
}

func TestClineProcessEventsPairsConcurrentToolsByCallID(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 16)
	ndjson := strings.Join([]string{
		`{"type":"hook_event","event":{"type":"tool_call","toolName":"read","toolCallId":"a","input":"{\"path\":\"a.txt\"}"}}`,
		`{"type":"hook_event","event":{"type":"tool_call","toolName":"write","toolCallId":"b","input":{"path":"b.txt"}}}`,
		`{"type":"hook_event","event":{"type":"tool_result","toolCallId":"b","output":{"stdout":"wrote b","stderr":""}}}`,
		`{"type":"hook_event","event":{"type":"tool_result","toolCallId":"a","output":"read a\n"}}`,
	}, "\n")

	b.processEvents(context.Background(), strings.NewReader(ndjson), ch)
	close(ch)
	var got []Message
	for msg := range ch {
		got = append(got, msg)
	}
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(got), got)
	}
	if got[0].CallID != "a" || got[0].Tool != "read" || got[0].Input["path"] != "a.txt" {
		t.Fatalf("first tool use = %+v", got[0])
	}
	if got[2].CallID != "b" || got[2].Tool != "write" || !strings.Contains(got[2].Output, "wrote b") {
		t.Fatalf("first tool result = %+v", got[2])
	}
	if got[3].CallID != "a" || got[3].Tool != "read" || got[3].Output != "read a\n" {
		t.Fatalf("second tool result = %+v", got[3])
	}
}

func TestClineProcessEventsNestedHookPayloads(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	ch := make(chan Message, 8)
	ndjson := strings.Join([]string{
		`{"type":"hook_event","event":{"hookName":"tool_call","tool_call":{"id":"call-1","name":"execute_command","input":{"command":"pwd"}}}}`,
		`{"type":"hook_event","event":{"hookName":"tool_result","tool_result":{"id":"call-1","name":"execute_command","input":{"command":"pwd"},"output":{"stdout":"/work\n","stderr":""},"durationMs":5}}}`,
		`{"type":"hook_event","event":{"hookName":"tool_call","tool_call":{"id":"call-2","name":"read_file","input":"{\"path\":\"a.txt\"}"}}}`,
		`{"type":"hook_event","event":{"hookName":"tool_result","tool_result":{"id":"call-2","name":"read_file","output":null,"error":"permission denied","durationMs":2}}}`,
	}, "\n")
	b.processEvents(context.Background(), strings.NewReader(ndjson), ch)
	close(ch)
	var got []Message
	for msg := range ch {
		got = append(got, msg)
	}
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(got), got)
	}
	if got[0].Type != MessageToolUse || got[0].CallID != "call-1" || got[0].Tool != "execute_command" || got[0].Input["command"] != "pwd" {
		t.Fatalf("tool use = %+v", got[0])
	}
	if got[1].Type != MessageToolResult || got[1].CallID != "call-1" || !strings.Contains(got[1].Output, `"stdout":"/work\n"`) {
		t.Fatalf("tool result = %+v", got[1])
	}
	if got[2].Input["path"] != "a.txt" || got[3].Output != "permission denied" {
		t.Fatalf("string input/error result = %+v / %+v", got[2], got[3])
	}
}

func TestClineProcessEventsBackpressureDoesNotDropBurst(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	const count = 600
	var ndjson strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&ndjson, "{\"type\":\"agent_event\",\"event\":{\"type\":\"content_update\",\"contentType\":\"text\",\"contentBlockId\":\"%d\",\"delta\":\"x\"}}\n", i)
	}
	ch := make(chan Message, 1)
	done := make(chan clineScanResult, 1)
	go func() {
		done <- b.processEvents(context.Background(), strings.NewReader(ndjson.String()), ch)
		close(ch)
	}()

	got := 0
	for range ch {
		got++
	}
	<-done
	if got != count {
		t.Fatalf("messages = %d, want %d", got, count)
	}
}

func TestClineProcessEventsCancellationUnblocksBackpressure(t *testing.T) {
	t.Parallel()
	b := &clineBackend{cfg: Config{Logger: slog.Default()}}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan Message)
	done := make(chan clineScanResult, 1)
	go func() {
		done <- b.processEvents(ctx, strings.NewReader(strings.Join([]string{
			`{"type":"agent_event","event":{"type":"usage","inputTokens":12,"outputTokens":3}}`,
			`{"type":"agent_event","event":{"type":"content_update","contentType":"text","delta":"x"}}`,
		}, "\n")), ch)
	}()
	cancel()
	select {
	case result := <-done:
		if result.status != "aborted" || result.usage.InputTokens != 12 || result.usage.OutputTokens != 3 {
			t.Fatalf("cancelled scan result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("processEvents stayed blocked after cancellation")
	}
}

func TestClineExecuteStreamsToolEventBeforeProcessExit(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	releasePath := filepath.Join(tempDir, "release")
	writeTestExecutable(t, fakePath, []byte(`#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"agent_event","event":{"type":"content_update","contentType":"thinking","delta":"checking"}}'
printf '%s\n' '{"type":"hook_event","event":{"type":"tool_call","toolName":"bash","toolCallId":"call-1","input":{"command":"pwd"}}}'
while [ ! -f "$CLINE_RELEASE_FILE" ]; do sleep 0.01; done
printf '%s\n' '{"type":"hook_event","event":{"type":"tool_result","toolName":"bash","toolCallId":"call-1","output":"done"}}'
printf '%s\n' '{"type":"run_result","finishReason":"completed","text":"finished"}'
`))
	backend, err := New("cline", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            map[string]string{"CLINE_RELEASE_FILE": releasePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", ExecOptions{ResumeSessionID: "1784085932547_prior"})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []MessageType{MessageThinking, MessageToolUse} {
		select {
		case message := <-session.Messages:
			if message.Type != want {
				t.Fatalf("message type = %q, want %q: %+v", message.Type, want, message)
			}
		case <-time.After(time.Second):
			t.Fatalf("did not receive %s before process exit", want)
		}
	}
	select {
	case result := <-session.Result:
		t.Fatalf("process exited before release: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	select {
	case result := <-session.Result:
		if result.Status != "completed" || result.Output != "finished" {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("result did not arrive after release")
	}
}

func TestListModelsClineEmptyCatalog(t *testing.T) {
	t.Parallel()
	got, err := ListModels(context.Background(), "cline", "")
	if err != nil {
		t.Fatalf("ListModels(cline): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty catalog, got %#v", got)
	}
}

// TestClineMigrationWhitelistMentionsProvider is a structural guard so the
// SupportedTypes lockstep set and migration 9001 stay aligned for `cline`.
func TestClineMigrationWhitelistMentionsProvider(t *testing.T) {
	t.Parallel()
	// migrations live at server/migrations relative to this package's module root.
	path := filepath.Join("..", "..", "migrations", "9001_runtime_profile_add_cline.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "'cline'") {
		t.Fatalf("migration 9001 must list 'cline' in protocol_family CHECK:\n%s", body)
	}
	for _, keep := range []string{"'claude'", "'traecli'", "'qoder'", "'deveco'"} {
		if !strings.Contains(body, keep) {
			t.Errorf("migration 9001 missing prior family %s", keep)
		}
	}
}

func splitNULArgs(raw []byte) []string {
	parts := strings.Split(string(raw), "\x00")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func containsArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
