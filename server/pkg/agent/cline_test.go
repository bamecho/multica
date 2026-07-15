package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	paths := clinePaths{
		DataDir:   "/tmp/multica-cline-data-test",
		ConfigDir: "/home/user/.cline-sr/data/settings",
	}

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
	}, paths, logger)

	joined := strings.Join(args, "\x00")
	// Required fixed flags
	if args[0] != "--json" {
		t.Fatalf("prefix = %v, want --json first", args[:min(1, len(args))])
	}
	if containsArg(args, "--auto-approve") {
		t.Errorf("Multica must not pass --auto-approve (CLI default true); got %v", args)
	}
	if !containsArgPair(args, "--data-dir", paths.DataDir) {
		t.Errorf("missing Multica --data-dir in %v", args)
	}
	if !containsArgPair(args, "--config", paths.ConfigDir) {
		t.Errorf("missing Multica --config in %v", args)
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
	args := buildClineArgs(ExecOptions{}, clinePaths{
		DataDir:   "/tmp/d",
		ConfigDir: "/tmp/settings",
	}, slog.Default())
	if args[len(args)-1] != clineArgvPromptSentinel {
		t.Fatalf("prompt sentinel = %q, want %q", args[len(args)-1], clineArgvPromptSentinel)
	}
	if !containsArgPair(args, "--data-dir", "/tmp/d") {
		t.Errorf("expected --data-dir even without optional opts: %v", args)
	}
	if !containsArgPair(args, "--config", "/tmp/settings") {
		t.Errorf("expected --config even without optional opts: %v", args)
	}
	if containsArg(args, "-c") || containsArg(args, "-m") || containsArg(args, "--id") {
		t.Errorf("unexpected optional flags: %v", args)
	}
}

func TestPrepareClinePathsPinsHomeConfig(t *testing.T) {
	// Cannot Parallel: t.Setenv mutates process env for this test.
	home := t.TempDir()
	t.Setenv("HOME", home)

	paths, err := prepareClinePaths(ExecOptions{})
	if err != nil {
		t.Fatalf("prepareClinePaths: %v", err)
	}
	if paths.DataDir == "" {
		t.Fatal("expected isolated data-dir")
	}
	wantConfig := filepath.Join(home, ".cline-sr", "data", "settings")
	if paths.ConfigDir != wantConfig {
		t.Fatalf("ConfigDir = %q, want %q", paths.ConfigDir, wantConfig)
	}
	// data-dir must not be nested under the home settings tree
	if strings.HasPrefix(paths.DataDir, wantConfig) {
		t.Errorf("data-dir %q must not live under config %q", paths.DataDir, wantConfig)
	}
}

// fakeClineNDJSONScript writes argv to CLINE_ARGS_FILE, stdin to
// CLINE_STDIN_FILE, and emits the NDJSON stream from CLINE_STDOUT_FILE (or a
// built-in success stream). Exit code from CLINE_EXIT_CODE (default 0).
// Stderr from CLINE_STDERR if set.
//
// When CLINE_WRITE_SESSION=1, writes a real-shaped session JSON under the
// --data-dir passed on argv (data/sessions/<id>/<id>.json) using this shell's
// PID so Multica post-exit discovery can recover a resume-capable session_id.
// Session id defaults to CLINE_SESSION_ID or 1784999999999_testhost.
func fakeClineNDJSONScript() string {
	// Args are NUL-delimited so multi-line values survive round-trip.
	// Stdin is captured fully (may contain newlines) for payload asserts.
	return `#!/bin/sh
if [ -n "$CLINE_ARGS_FILE" ]; then
  printf '%s\0' "$@" > "$CLINE_ARGS_FILE"
fi
STDIN_BODY=""
if [ -n "$CLINE_STDIN_FILE" ]; then
  cat > "$CLINE_STDIN_FILE"
  STDIN_BODY=$(cat "$CLINE_STDIN_FILE")
else
  STDIN_BODY=$(cat)
fi
# Parse --data-dir and -c from argv for optional session file write.
DATA_DIR=""
CWD=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--data-dir" ]; then DATA_DIR="$a"; fi
  if [ "$prev" = "-c" ] || [ "$prev" = "--cwd" ]; then CWD="$a"; fi
  prev="$a"
done
if [ "$CLINE_WRITE_SESSION" = "1" ] && [ -n "$DATA_DIR" ]; then
  SID="${CLINE_SESSION_ID:-1784999999999_testhost}"
  SDIR="$DATA_DIR/data/sessions/$SID"
  mkdir -p "$SDIR"
  # Escape JSON string content for prompt (minimal).
  PROMPT_ESC=$(printf '%s' "$STDIN_BODY" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr '\n' ' ')
  STARTED=$(date -u +"%Y-%m-%dT%H:%M:%S.000Z" 2>/dev/null || date -u +"%Y-%m-%dT%H:%M:%SZ")
  printf '%s\n' "{
  \"version\": 1,
  \"session_id\": \"$SID\",
  \"source\": \"cli\",
  \"pid\": $$,
  \"started_at\": \"$STARTED\",
  \"ended_at\": \"$STARTED\",
  \"exit_code\": 0,
  \"status\": \"completed\",
  \"cwd\": \"$CWD\",
  \"workspace_root\": \"$CWD\",
  \"prompt\": \"<user_input mode=\\\"act\\\">$PROMPT_ESC</user_input>\",
  \"provider\": \"openai-compatible\",
  \"model\": \"test-model\"
}" > "$SDIR/$SID.json"
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
	messages []Message
	result   Result
	argsFile string
	stdinFile string
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

	merged := map[string]string{
		"CLINE_ARGS_FILE":   argsFile,
		"CLINE_STDIN_FILE":  stdinFile,
		"CLINE_STDOUT_FILE": stdoutFile,
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
	return clineExecCapture{
		messages:  messages,
		result:    result,
		argsFile:  argsFile,
		stdinFile: stdinFile,
	}
}

func TestClineExecuteSuccessTextAndUsage(t *testing.T) {
	t.Parallel()
	// Synthetic NDJSON sessionId is not the real CLI contract and must not
	// become Result.SessionID. Resume id comes only from disk discovery.
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
	// Cannot Parallel: t.Setenv mutates process env so --config resolves to a
	// known home settings path for this test.
	ndjson := `{"type":"run_result","finishReason":"completed","text":"ok"}` + "\n"
	userPrompt := "user prompt with unique task token TASK_SECRET_42"
	brief := "RUNTIME BRIEF with unique brief token BRIEF_SECRET_99"
	wantPayload := brief + "\n\n" + userPrompt

	home := t.TempDir()
	t.Setenv("HOME", home)
	wantConfig := filepath.Join(home, ".cline-sr", "data", "settings")

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
	if !containsArgPair(lines, "--config", wantConfig) {
		t.Errorf("missing Multica --config %q in %v", wantConfig, lines)
	}
	if !containsArg(lines, "--data-dir") {
		t.Errorf("missing Multica --data-dir: %v", lines)
	}
	// data-dir must be Multica-owned temp, not user override
	if dataDir := argValueAfter(lines, "--data-dir"); dataDir == "" || dataDir == "/evil-data" {
		t.Errorf("data-dir = %q, want isolated temp path", dataDir)
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

// TestClineExecuteDiscoversSessionIDFromDataDir drives the real Execute path
// with a fake CLI that writes real-shaped session JSON under Multica's
// --data-dir and asserts Result.SessionID equals that resume id.
func TestClineExecuteDiscoversSessionIDFromDataDir(t *testing.T) {
	t.Parallel()
	// Real CLI shape: no sessionId on NDJSON; id only on disk.
	ndjson := strings.Join([]string{
		`{"type":"hook_event","event":{"type":"agent_start","taskId":"conv_abc","agentId":"agent_xyz"}}`,
		`{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"hi"}}`,
		`{"type":"run_result","finishReason":"completed","text":"done","usage":{"inputTokens":1,"outputTokens":1}}`,
	}, "\n")
	wantID := "1784999999999_testhost"
	cwd := t.TempDir()
	cap := runClineExecute(t, "discover me UNIQUE_DISK_PROMPT", ndjson, ExecOptions{
		Cwd:   cwd,
		Model: "m-disk",
	}, map[string]string{
		"CLINE_WRITE_SESSION": "1",
		"CLINE_SESSION_ID":    wantID,
	})
	if cap.result.Status != "completed" {
		t.Fatalf("status=%q error=%q", cap.result.Status, cap.result.Error)
	}
	if cap.result.SessionID != wantID {
		t.Fatalf("SessionID=%q, want disk session %q (not empty, not conv_/agent_)", cap.result.SessionID, wantID)
	}
	if strings.HasPrefix(cap.result.SessionID, "conv_") || strings.HasPrefix(cap.result.SessionID, "agent_") {
		t.Fatalf("SessionID looks like hook task/agent id: %q", cap.result.SessionID)
	}
	// Confirm Multica passed --data-dir and fake wrote under it.
	raw, err := os.ReadFile(cap.argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	lines := splitNULArgs(raw)
	dataDir := argValueAfter(lines, "--data-dir")
	if dataDir == "" {
		t.Fatalf("missing --data-dir in argv: %v", lines)
	}
	sessionPath := filepath.Join(dataDir, "data", "sessions", wantID, wantID+".json")
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("expected fake CLI session file at %s: %v", sessionPath, err)
	}
	if cap.result.Output != "done" {
		t.Errorf("output=%q, want done", cap.result.Output)
	}
}

// TestClineExecuteEmptySessionIDWithoutDiskSession asserts that when the
// fake CLI emits only NDJSON (no session JSON under data-dir), Result.SessionID
// stays empty without panic — even if NDJSON carries synthetic sessionId.
func TestClineExecuteEmptySessionIDWithoutDiskSession(t *testing.T) {
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
		t.Fatalf("SessionID=%q, want empty when no disk session file", result.SessionID)
	}
	if result.Output != "ok" {
		t.Errorf("output=%q", result.Output)
	}
	// Still passed isolated --data-dir (empty tree).
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if !containsArg(splitNULArgs(raw), "--data-dir") {
		t.Errorf("expected --data-dir even when no session written: %v", splitNULArgs(raw))
	}
}

func TestDiscoverClineSessionIDUnit(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	sid := "1784085932547_muen6"
	cwd := "/work/project"
	sessionDir := filepath.Join(dataDir, "data", "sessions", sid)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "session_id": "` + sid + `",
  "pid": 4242,
  "cwd": "` + cwd + `",
  "workspace_root": "` + cwd + `",
  "prompt": "<user_input mode=\"act\">hello task</user_input>",
  "started_at": "2026-07-15T12:00:00.000Z",
  "ended_at": "2026-07-15T12:00:01.000Z",
  "status": "completed"
}`
	if err := os.WriteFile(filepath.Join(sessionDir, sid+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	got := discoverClineSessionID(dataDir, clineSessionMatchHints{
		PID:       4242,
		Cwd:       cwd,
		Prompt:    "hello task",
		StartTime: start,
		EndTime:   start.Add(2 * time.Second),
	}, slog.Default())
	if got != sid {
		t.Fatalf("discover = %q, want %q", got, sid)
	}

	// Reject conv_/agent_ prefixes even if present as session_id field.
	badDir := t.TempDir()
	badSID := "conv_not_resume"
	badSessionDir := filepath.Join(badDir, "data", "sessions", badSID)
	if err := os.MkdirAll(badSessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	badBody := `{"session_id":"conv_not_resume","pid":1,"cwd":"/x","prompt":"p","status":"completed"}`
	if err := os.WriteFile(filepath.Join(badSessionDir, badSID+".json"), []byte(badBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := discoverClineSessionID(badDir, clineSessionMatchHints{PID: 1}, slog.Default()); got != "" {
		t.Fatalf("expected empty for conv_ id, got %q", got)
	}

	// Empty data-dir → empty id.
	if got := discoverClineSessionID(t.TempDir(), clineSessionMatchHints{}, slog.Default()); got != "" {
		t.Fatalf("empty tree: got %q", got)
	}
}

func argValueAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
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
	res := b.processEvents(strings.NewReader(ndjson), ch)
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
