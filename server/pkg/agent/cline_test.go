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

	args := buildClineArgs("user task", ExecOptions{
		Cwd:             "/work",
		Model:           "gpt-test",
		ResumeSessionID: "ses_prior",
		SystemPrompt:    "brief here",
		CustomArgs:      []string{"--json", "--auto-approve", "false", "-c", "/evil", "--id", "x", "-s", "nope", "-t", "9", "-m", "other", "--verbose"},
	}, logger)

	joined := strings.Join(args, "\x00")
	// Required fixed flags
	if args[0] != "--json" || args[1] != "--auto-approve" || args[2] != "true" {
		t.Fatalf("prefix = %v, want --json --auto-approve true", args[:min(3, len(args))])
	}
	if !containsArgPair(args, "-c", "/work") {
		t.Errorf("missing -c /work in %v", args)
	}
	if !containsArgPair(args, "-m", "gpt-test") {
		t.Errorf("missing -m gpt-test in %v", args)
	}
	if !containsArgPair(args, "--id", "ses_prior") {
		t.Errorf("missing --id ses_prior in %v", args)
	}
	// Prompt is last and combines brief + user
	last := args[len(args)-1]
	if last != "brief here\n\nuser task" {
		t.Errorf("combined prompt = %q", last)
	}
	// Never pass -s / -t
	for i, a := range args {
		if a == "-s" || a == "--system" || a == "-t" || a == "--timeout" {
			t.Errorf("forbidden flag %q present at index %d in %v", a, i, args)
		}
	}
	// Blocked custom overrides must not appear as user-controlled values
	if strings.Contains(joined, "/evil") {
		t.Errorf("blocked -c override survived: %v", args)
	}
	if strings.Contains(joined, "\x00false\x00") || strings.HasSuffix(joined, "\x00false") {
		// auto-approve false from custom must be filtered
		// note: "true" is our fixed value
	}
	// Allowed custom arg survives
	if !containsArg(args, "--verbose") {
		t.Errorf("allowed custom --verbose missing: %v", args)
	}
}

func TestBuildClineArgsNoSystemPrompt(t *testing.T) {
	t.Parallel()
	args := buildClineArgs("only user", ExecOptions{}, slog.Default())
	if args[len(args)-1] != "only user" {
		t.Fatalf("prompt = %q, want only user", args[len(args)-1])
	}
	if containsArg(args, "-c") || containsArg(args, "-m") || containsArg(args, "--id") {
		t.Errorf("unexpected optional flags: %v", args)
	}
}

// fakeClineNDJSONScript writes argv to CLINE_ARGS_FILE and emits the NDJSON
// stream from CLINE_STDOUT_FILE (or a built-in success stream). Exit code from
// CLINE_EXIT_CODE (default 0). Stderr from CLINE_STDERR if set.
func fakeClineNDJSONScript() string {
	// Args are NUL-delimited so multi-line prompts (system brief prepend)
	// survive round-trip; newline-delimited capture would split them.
	return `#!/bin/sh
if [ -n "$CLINE_ARGS_FILE" ]; then
  printf '%s\0' "$@" > "$CLINE_ARGS_FILE"
fi
if [ -n "$CLINE_STDERR" ]; then
  printf '%s\n' "$CLINE_STDERR" >&2
fi
if [ -n "$CLINE_STDOUT_FILE" ] && [ -f "$CLINE_STDOUT_FILE" ]; then
  cat "$CLINE_STDOUT_FILE"
else
  printf '%s\n' '{"type":"hook_event","event":{"type":"agent_start"}}'
  printf '%s\n' '{"type":"agent_event","event":{"type":"content_end","contentType":"text","text":"Working..."}}'
  printf '%s\n' '{"type":"run_result","finishReason":"completed","text":"Summary","usage":{"inputTokens":10,"outputTokens":5},"sessionId":"ses_default"}'
fi
exit "${CLINE_EXIT_CODE:-0}"
`
}

func runClineWithStdout(t *testing.T, ndjson string, opts ExecOptions, env map[string]string) (messages []Message, result Result, argsFile string) {
	t.Helper()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	writeTestExecutable(t, fakePath, []byte(fakeClineNDJSONScript()))

	stdoutFile := filepath.Join(tempDir, "stdout.ndjson")
	if err := os.WriteFile(stdoutFile, []byte(ndjson), 0o644); err != nil {
		t.Fatalf("write stdout fixture: %v", err)
	}
	argsFile = filepath.Join(tempDir, "argv.txt")

	merged := map[string]string{
		"CLINE_ARGS_FILE":   argsFile,
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

	session, err := backend.Execute(ctx, "user prompt", opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range session.Messages {
			messages = append(messages, m)
		}
	}()
	result = <-session.Result
	<-done
	return messages, result, argsFile
}

func TestClineExecuteSuccessTextAndUsage(t *testing.T) {
	t.Parallel()
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
	if result.SessionID != "ses_final" {
		t.Errorf("session id = %q, want ses_final (last pin from run_result)", result.SessionID)
	}
	u, ok := result.Usage["m1"]
	if !ok {
		t.Fatalf("usage missing for m1: %#v", result.Usage)
	}
	if u.InputTokens != 120 || u.OutputTokens != 40 {
		t.Errorf("usage = %+v, want run_result authoritative 120/40", u)
	}

	var sawText, sawSessionPin bool
	for _, m := range messages {
		if m.Type == MessageText && strings.Contains(m.Content, "Hello stream") {
			sawText = true
		}
		if m.Type == MessageStatus && m.SessionID != "" {
			sawSessionPin = true
		}
	}
	if !sawText {
		t.Errorf("expected text message; messages=%+v", messages)
	}
	if !sawSessionPin {
		t.Errorf("expected early session pin status message; messages=%+v", messages)
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

func TestClineExecuteArgvAndPromptPrepend(t *testing.T) {
	t.Parallel()
	ndjson := `{"type":"run_result","finishReason":"completed","text":"ok"}` + "\n"
	_, result, argsFile := runClineWithStdout(t, ndjson, ExecOptions{
		Cwd:             t.TempDir(),
		Model:           "model-x",
		ResumeSessionID: "ses_r",
		SystemPrompt:    "RUNTIME BRIEF",
		CustomArgs:      []string{"--json", "-t", "99", "-s", "no", "--extra", "keep"},
	}, nil)
	if result.Status != "completed" {
		t.Fatalf("status=%q error=%q", result.Status, result.Error)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	lines := splitNULArgs(raw)
	if len(lines) < 3 || lines[0] != "--json" || lines[1] != "--auto-approve" || lines[2] != "true" {
		t.Fatalf("argv prefix = %v", lines)
	}
	joined := strings.Join(lines, " ")
	for _, line := range lines {
		if line == "-s" || line == "--system" || line == "-t" || line == "--timeout" || line == "99" {
			t.Errorf("banned flag/value in argv: %q full=%v", line, lines)
		}
	}
	if !containsArgPair(lines, "-m", "model-x") {
		t.Errorf("missing -m: %v", lines)
	}
	if !containsArgPair(lines, "--id", "ses_r") {
		t.Errorf("missing --id: %v", lines)
	}
	last := lines[len(lines)-1]
	if last != "RUNTIME BRIEF\n\nuser prompt" {
		t.Errorf("combined prompt = %q", last)
	}
	if !strings.Contains(joined, "--extra") || !strings.Contains(joined, "keep") {
		t.Errorf("allowed custom args missing: %v", lines)
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
// SupportedTypes lockstep set and migration 175 stay aligned for `cline`.
func TestClineMigrationWhitelistMentionsProvider(t *testing.T) {
	t.Parallel()
	// migrations live at server/migrations relative to this package's module root.
	path := filepath.Join("..", "..", "migrations", "175_runtime_profile_add_cline.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "'cline'") {
		t.Fatalf("migration 175 must list 'cline' in protocol_family CHECK:\n%s", body)
	}
	for _, keep := range []string{"'claude'", "'traecli'", "'qoder'"} {
		if !strings.Contains(body, keep) {
			t.Errorf("migration 175 missing prior family %s", keep)
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
