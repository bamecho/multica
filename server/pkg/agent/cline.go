package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// clineBlockedArgs are flags owned by the daemon for the Cline 3.x NDJSON
// control plane (形态 B). Users must not override protocol transport, cwd,
// session resume, system/timeout flags (v1 never passes -s/-t), or model.
var clineBlockedArgs = map[string]blockedArgMode{
	"--json":         blockedStandalone,
	"--auto-approve": blockedWithValue,
	"-c":             blockedWithValue,
	"--cwd":          blockedWithValue,
	"--id":           blockedWithValue,
	"-s":             blockedWithValue,
	"--system":       blockedWithValue,
	"-t":             blockedWithValue,
	"--timeout":      blockedWithValue,
	"-m":             blockedWithValue,
	"--model":        blockedWithValue,
}

// clineBackend implements Backend by spawning a Cline-compatible CLI with
// `--json --auto-approve true` and parsing Cline 3.x NDJSON (形态 B) from
// stdout: agent_event / hook_event / run_result. See
// docs/cline-ndjson-multica-adapter-plan.md.
type clineBackend struct {
	cfg Config
}

func (b *clineBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "cline"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("cline executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := buildClineArgs(prompt, opts, b.cfg.Logger)
	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	b.cfg.Logger.Info("agent command", "exec", execPath, "args", args)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cline stdout pipe: %w", err)
	}
	stderrTail := newStderrTail(newLogWriter(b.cfg.Logger, "[cline:stderr] "), agentStderrTailBytes)
	cmd.Stderr = stderrTail

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start cline: %w", err)
	}

	b.cfg.Logger.Info("cline started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	go func() {
		<-runCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		scanResult := b.processEvents(stdout, msgCh)

		exitErr := cmd.Wait()
		duration := time.Since(startTime)
		stderr := stderrTail.Tail()

		// Wall-clock context owns timeout in v1 (CLI -t is never passed).
		if runCtx.Err() == context.DeadlineExceeded {
			scanResult.status = "timeout"
			scanResult.errMsg = fmt.Sprintf("cline timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			scanResult.status = "aborted"
			scanResult.errMsg = "execution cancelled"
		} else {
			// Prefer last run_result; fall back to done + exit code when absent.
			if !scanResult.sawRunResult {
				if scanResult.doneReason != "" {
					scanResult.status = clineStatusFromFinishReason(scanResult.doneReason)
					if scanResult.output == "" {
						scanResult.output = scanResult.doneText
					}
					if scanResult.status != "completed" && scanResult.errMsg == "" {
						scanResult.errMsg = fmt.Sprintf("cline finished with reason %q", scanResult.doneReason)
					}
				}
				if exitErr != nil && scanResult.status == "completed" {
					scanResult.status = "failed"
					scanResult.errMsg = withAgentStderr(fmt.Sprintf("cline exited with error: %v", exitErr), "cline", stderr)
				}
			} else if exitErr != nil && scanResult.status == "completed" {
				// run_result says completed but process failed — keep completed
				// (authoritative run_result) unless stderr indicates timeout.
				if clineStderrLooksLikeTimeout(stderr) {
					scanResult.status = "timeout"
					scanResult.errMsg = withAgentStderr("cline run timed out", "cline", stderr)
				}
			}
			// Promote aborted/error run_result to timeout when stderr says so.
			if scanResult.status != "completed" && clineStderrLooksLikeTimeout(stderr) {
				scanResult.status = "timeout"
				if scanResult.errMsg == "" {
					scanResult.errMsg = withAgentStderr("cline run timed out", "cline", stderr)
				} else {
					scanResult.errMsg = withAgentStderr(scanResult.errMsg, "cline", stderr)
				}
			} else if scanResult.status == "failed" && scanResult.errMsg != "" && stderr != "" && !strings.Contains(scanResult.errMsg, "stderr:") {
				scanResult.errMsg = withAgentStderr(scanResult.errMsg, "cline", stderr)
			}
		}

		b.cfg.Logger.Info("cline finished", "pid", cmd.Process.Pid, "status", scanResult.status, "duration", duration.Round(time.Millisecond).String())

		var usage map[string]TokenUsage
		u := scanResult.usage
		if u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
			model := opts.Model
			if model == "" {
				model = "unknown"
			}
			usage = map[string]TokenUsage{model: u}
		}

		resCh <- Result{
			Status:     scanResult.status,
			Output:     scanResult.output,
			Error:      scanResult.errMsg,
			DurationMs: duration.Milliseconds(),
			SessionID:  scanResult.sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// buildClineArgs assembles argv for a headless Cline 3.x NDJSON run.
// System/runtime brief is prepended into the final prompt arg (no -s).
// Timeout is owned by Multica runContext; -t is never passed (v1).
func buildClineArgs(prompt string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{"--json", "--auto-approve", "true"}
	if opts.Cwd != "" {
		args = append(args, "-c", opts.Cwd)
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--id", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.CustomArgs, clineBlockedArgs, logger)...)

	combined := prompt
	if strings.TrimSpace(opts.SystemPrompt) != "" {
		combined = opts.SystemPrompt + "\n\n" + prompt
	}
	args = append(args, combined)
	return args
}

// ── NDJSON (形态 B) ──

type clineScanResult struct {
	status       string
	errMsg       string
	output       string
	sessionID    string
	usage        TokenUsage
	sawRunResult bool
	doneReason   string
	doneText     string
}

// Loose top-level envelope. Nested event payloads vary by type; we re-parse
// .event when present rather than forcing a single nested struct.
type clineLine struct {
	Type           string          `json:"type"`
	Event          json.RawMessage `json:"event"`
	FinishReason   string          `json:"finishReason"`
	Text           string          `json:"text"`
	SessionID      string          `json:"sessionId"`
	ID             string          `json:"id"`
	DurationMs     int64           `json:"durationMs"`
	Usage          *clineUsageBlob `json:"usage"`
	AggregateUsage *clineUsageBlob `json:"aggregateUsage"`
}

type clineUsageBlob struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

type clineNestedEvent struct {
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	ContentType      string          `json:"contentType"`
	Reason           string          `json:"reason"`
	InputTokens      int64           `json:"inputTokens"`
	OutputTokens     int64           `json:"outputTokens"`
	CacheReadTokens  int64           `json:"cacheReadTokens"`
	CacheWriteTokens int64           `json:"cacheWriteTokens"`
	Name             string          `json:"name"`
	Tool             string          `json:"tool"`
	ToolName         string          `json:"toolName"`
	CallID           string          `json:"callId"`
	ToolCallID       string          `json:"toolCallId"`
	ID               string          `json:"id"`
	SessionID        string          `json:"sessionId"`
	Input            json.RawMessage `json:"input"`
	Params           json.RawMessage `json:"params"`
	Parameters       json.RawMessage `json:"parameters"`
	Output           json.RawMessage `json:"output"`
	Result           json.RawMessage `json:"result"`
	Content          json.RawMessage `json:"content"`
}

// processEvents reads Cline 3.x NDJSON lines from r and maps them to Multica
// Messages / terminal Result fields. Extracted for fixture-driven unit tests.
func (b *clineBackend) processEvents(r io.Reader, ch chan<- Message) clineScanResult {
	var streamOut strings.Builder
	var sessionID string
	var usage TokenUsage
	finalStatus := "completed"
	var finalError string
	var sawRunResult bool
	var runOutput string
	var doneReason, doneText string
	toolSeq := 0
	var lastToolCallID string
	sessionPinned := false

	pinSession := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		sessionID = id
		if !sessionPinned {
			sessionPinned = true
			trySend(ch, Message{Type: MessageStatus, Status: "running", SessionID: id})
		}
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var env clineLine
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			if b.cfg.Logger != nil {
				b.cfg.Logger.Debug("cline: skip non-JSON stdout line", "line", truncateForLog(line, 200))
			}
			continue
		}

		// Prefer explicit sessionId fields only — bare "id" is ambiguous on
		// non-run_result lines (tool call ids, etc.).
		pinSession(env.SessionID)

		switch env.Type {
		case "agent_event":
			var ev clineNestedEvent
			if len(env.Event) > 0 {
				if err := json.Unmarshal(env.Event, &ev); err != nil {
					continue
				}
			}
			pinSession(ev.SessionID)

			switch ev.Type {
			case "content_start", "content_update", "content_end":
				text := strings.TrimSpace(ev.Text)
				if text == "" {
					text = extractClineTextFromContent(ev.Content)
				}
				// Prefer complete blocks; still stream updates when they carry text.
				if text != "" && (ev.Type == "content_end" || ev.Type == "content_update") {
					if ev.ContentType == "" || ev.ContentType == "text" {
						streamOut.WriteString(text)
						trySend(ch, Message{Type: MessageText, Content: text})
					}
				}
			case "usage":
				usage.InputTokens += ev.InputTokens
				usage.OutputTokens += ev.OutputTokens
				usage.CacheReadTokens += ev.CacheReadTokens
				usage.CacheWriteTokens += ev.CacheWriteTokens
			case "done":
				doneReason = firstNonEmpty(ev.Reason, doneReason)
				if t := strings.TrimSpace(ev.Text); t != "" {
					doneText = t
				}
			case "error":
				if msg := strings.TrimSpace(ev.Text); msg != "" {
					trySend(ch, Message{Type: MessageError, Content: msg})
					if finalError == "" {
						finalError = msg
					}
				}
			case "notice", "iteration_start", "iteration_end":
				// Lifecycle only — ignore for Multica messages.
			default:
				// Unknown nested types: skip (do not abort scan).
			}

		case "hook_event":
			var ev clineNestedEvent
			if len(env.Event) > 0 {
				if err := json.Unmarshal(env.Event, &ev); err != nil {
					continue
				}
			}
			pinSession(ev.SessionID)

			switch ev.Type {
			case "tool_call":
				toolSeq++
				callID := firstNonEmpty(ev.CallID, ev.ToolCallID, ev.ID)
				if callID == "" {
					callID = fmt.Sprintf("cline-tool-%d", toolSeq)
				}
				lastToolCallID = callID
				name := firstNonEmpty(ev.Name, ev.Tool, ev.ToolName)
				if name == "" {
					name = "tool"
				}
				input := parseClineToolInput(ev)
				trySend(ch, Message{
					Type:   MessageToolUse,
					Tool:   name,
					CallID: callID,
					Input:  input,
				})
			case "tool_result":
				callID := firstNonEmpty(ev.CallID, ev.ToolCallID, ev.ID, lastToolCallID)
				out := extractClineToolOutput(ev)
				trySend(ch, Message{
					Type:   MessageToolResult,
					CallID: callID,
					Output: out,
				})
			case "agent_start", "agent_end":
				// Lifecycle only.
			}

		case "run_result":
			sawRunResult = true
			reason := strings.TrimSpace(env.FinishReason)
			finalStatus = clineStatusFromFinishReason(reason)
			if t := strings.TrimSpace(env.Text); t != "" {
				runOutput = t
			}
			if finalStatus != "completed" {
				if finalError == "" {
					finalError = fmt.Sprintf("cline finishReason=%q", reason)
				}
			} else {
				// A later completed run_result clears a prior non-success.
				finalError = ""
			}
			if u := env.Usage; u != nil {
				// run_result usage is authoritative for the whole run; replace.
				usage = TokenUsage{
					InputTokens:      u.InputTokens,
					OutputTokens:     u.OutputTokens,
					CacheReadTokens:  u.CacheReadTokens,
					CacheWriteTokens: u.CacheWriteTokens,
				}
			} else if u := env.AggregateUsage; u != nil {
				usage = TokenUsage{
					InputTokens:      u.InputTokens,
					OutputTokens:     u.OutputTokens,
					CacheReadTokens:  u.CacheReadTokens,
					CacheWriteTokens: u.CacheWriteTokens,
				}
			}
			// run_result may surface session as sessionId or id.
			pinSession(firstNonEmpty(env.SessionID, env.ID))

		default:
			// Unknown top-level type: skip.
			if b.cfg.Logger != nil {
				b.cfg.Logger.Debug("cline: skip unknown stdout type", "type", env.Type)
			}
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		if b.cfg.Logger != nil {
			b.cfg.Logger.Warn("cline stdout scanner error", "error", scanErr)
		}
		if finalStatus == "completed" && !sawRunResult {
			finalStatus = "failed"
			finalError = fmt.Sprintf("stdout read error: %v", scanErr)
		}
	}

	output := runOutput
	if output == "" {
		output = doneText
	}
	if output == "" {
		output = streamOut.String()
	}

	// Empty Output with completed is valid (side-effect-only multica work).
	return clineScanResult{
		status:       finalStatus,
		errMsg:       finalError,
		output:       output,
		sessionID:    sessionID,
		usage:        usage,
		sawRunResult: sawRunResult,
		doneReason:   doneReason,
		doneText:     doneText,
	}
}

func clineStatusFromFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "completed", "end_turn", "success", "":
		// Empty finishReason on a present run_result is treated as completed.
		return "completed"
	case "aborted", "cancelled", "canceled":
		return "failed"
	case "timeout", "timed_out":
		return "timeout"
	default:
		// max_iterations, mistake_limit, error, …
		return "failed"
	}
}

func clineStderrLooksLikeTimeout(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "timed out") || strings.Contains(s, "timeout")
}

func parseClineToolInput(ev clineNestedEvent) map[string]any {
	for _, raw := range []json.RawMessage{ev.Input, ev.Params, ev.Parameters} {
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err == nil && len(m) > 0 {
			return m
		}
	}
	return nil
}

func extractClineToolOutput(ev clineNestedEvent) string {
	if t := strings.TrimSpace(ev.Text); t != "" {
		return t
	}
	for _, raw := range []json.RawMessage{ev.Output, ev.Result, ev.Content} {
		if len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		// Non-string JSON: keep compact form for the UI.
		return string(raw)
	}
	return ""
}

func extractClineTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// content blocks: [{type:"text", text:"..."}]
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Text != "" {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func truncateForLog(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
