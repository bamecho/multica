package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// clineArgvPromptSentinel is the positional prompt passed on argv for headless
// --json runs. Cline 3.x gates on truthiness of o.prompt (empty string fails);
// a single newline passes the gate and is trimmed out of the session user
// message so the model only sees the full Multica payload on stdin.
// The confirmed contract is consolidated in
// docs/plan/04-cline-dedicated-hub-per-run.md.
const clineArgvPromptSentinel = "\n"

const clineTaskTerminateGrace = 2 * time.Second

// clineBlockedArgs are flags owned by the daemon for the Cline 3.x NDJSON
// control plane (形态 B). Users must not override protocol transport, cwd,
// session isolation/resume, system/timeout flags (v1 never passes -s/-t),
// or model. --data-dir and --config remain blocked because both fresh and
// resume runs use Cline's persistent state; fresh isolation is provided by a
// dedicated private Hub endpoint rather than a second state root.
//
// --auto-approve is not passed on argv (CLI default is true for headless
// tool approval) but stays blocked so CustomArgs cannot force false and hang
// a daemon run on interactive approval.
var clineBlockedArgs = map[string]blockedArgMode{
	"--json":         blockedStandalone,
	"--auto-approve": blockedWithValue,
	"-c":             blockedWithValue,
	"--cwd":          blockedWithValue,
	"--id":           blockedWithValue,
	"--data-dir":     blockedWithValue,
	"--config":       blockedWithValue,
	"-s":             blockedWithValue,
	"--system":       blockedWithValue,
	"-t":             blockedWithValue,
	"--timeout":      blockedWithValue,
	"-m":             blockedWithValue,
	"--model":        blockedWithValue,
	"--zen":          blockedStandalone,
}

// clineBackend implements Backend by spawning a Cline-compatible CLI with
// `--json` and parsing Cline 3.x NDJSON (形态 B) from stdout: agent_event /
// hook_event / run_result. See docs/cline-ndjson-multica-adapter-plan.md.
type clineBackend struct {
	cfg Config

	startFreshHub   func(context.Context, string, string, map[string]string, string) (*clineHub, error)
	runFreshHistory func(context.Context, string, string, []string) ([]clineHistoryEntry, error)
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

	payload := buildClineStdinPayload(prompt, opts)
	resume := opts.ResumeSessionID != ""
	var hub *clineHub
	var lookup *clineSessionLookupCoordinator
	history := b.runFreshHistory
	if history == nil {
		history = runClineHistory
	}
	if !resume {
		startHub := b.startFreshHub
		if startHub == nil {
			startHub = startClineHub
		}
		taskTempDir := strings.TrimSpace(b.cfg.Env["TMPDIR"])
		var hubErr error
		hub, hubErr = startHub(runCtx, execPath, opts.Cwd, b.cfg.Env, taskTempDir)
		if hubErr != nil {
			cancel()
			return nil, hubErr
		}

		canonicalWorkDir, canonicalErr := canonicalClineWorkDir(opts.Cwd)
		preHistory, historyErr := history(runCtx, execPath, opts.Cwd, hub.env)
		if canonicalErr != nil {
			b.cfg.Logger.Warn("cline: fresh SessionID discovery disabled", "reason", "canonical workdir", "error", canonicalErr)
		} else if historyErr != nil {
			b.cfg.Logger.Warn("cline: fresh SessionID discovery disabled", "reason", "pre-history", "error", historyErr)
		} else {
			lookup = newClineSessionLookupCoordinator(clineHistoryMatch{
				PreRunSessionIDs: clineRootSessionIDs(preHistory),
				DedicatedHubPID:  hub.discovery.PID,
				CanonicalWorkDir: canonicalWorkDir,
			})
		}
	}
	args := buildClineArgs(opts, b.cfg.Logger)
	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	configureProcessGroup(cmd)
	// Cline task cancellation owns only the attached CLI process tree. In the
	// resume branch the long-lived shared Hub is a separate process and must
	// never be signalled by Multica.
	cmd.Cancel = func() error { return nil }
	// Never log full stdin body at info — only size + argv sentinel label.
	b.cfg.Logger.Info("agent command",
		"exec", execPath,
		"args", args,
		"argv_prompt", "newline_sentinel",
		"stdin_bytes", len(payload),
		"dedicated_hub", !resume,
	)
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	if resume {
		cmd.Env = buildClineEnv(b.cfg.Env, true)
	} else {
		cmd.Env = hub.env
	}
	stopHubOnSetupError := func() {
		if hub == nil {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), clineHubControlTimeout+clineHubExitTimeout+time.Second)
		defer stopCancel()
		if stopErr := hub.stop(stopCtx); stopErr != nil {
			b.cfg.Logger.Warn("cline: dedicated hub setup cleanup failed", "error", stopErr)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stopHubOnSetupError()
		cancel()
		return nil, fmt.Errorf("cline stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		stopHubOnSetupError()
		cancel()
		return nil, fmt.Errorf("cline stdin pipe: %w", err)
	}
	stderrTail := newStderrTail(newLogWriter(b.cfg.Logger, "[cline:stderr] "), agentStderrTailBytes)
	cmd.Stderr = stderrTail

	// The wall component is the lower bound for fresh SessionID correlation;
	// keep this immediately adjacent to Start so setup time cannot widen it.
	cmdStartTime := time.Now()
	if lookup != nil {
		lookup.setCommandStart(cmdStartTime.UnixMilli())
	}
	if err := cmd.Start(); err != nil {
		stopHubOnSetupError()
		cancel()
		return nil, fmt.Errorf("start cline: %w", err)
	}

	childPID := 0
	if cmd.Process != nil {
		childPID = cmd.Process.Pid
	}
	b.cfg.Logger.Info("cline started",
		"pid", childPID,
		"cwd", opts.Cwd,
		"model", opts.Model,
		"stdin_bytes", len(payload),
		"hub_pid", func() int {
			if hub != nil {
				return hub.discovery.PID
			}
			return 0
		}(),
	)

	// Deliver full Multica payload on stdin and close for EOF. Concurrent with
	// stdout drain so a large write cannot deadlock against a chatty child.
	go func() {
		defer func() { _ = stdin.Close() }()
		if payload == "" {
			return
		}
		if _, werr := io.WriteString(stdin, payload); werr != nil {
			if b.cfg.Logger != nil {
				b.cfg.Logger.Debug("cline: stdin write failed", "error", werr)
			}
		}
	}()

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	procDone := make(chan struct{})
	lookupCtx, cancelLookup := context.WithCancel(runCtx)
	earlyDone := make(chan struct{})
	if lookup != nil {
		go func() {
			defer close(earlyDone)
			lookup.runEarly(lookupCtx, func(historyCtx context.Context) ([]clineHistoryEntry, error) {
				return history(historyCtx, execPath, opts.Cwd, hub.env)
			}, clineHistoryRetryDelays, func(id string) {
				_ = sendClineMessage(lookupCtx, msgCh, Message{Type: MessageStatus, Status: "running", SessionID: id})
			})
		}()
	} else {
		close(earlyDone)
	}
	var finalLookupMu sync.Mutex
	finalLookupAttempted := false
	runFinalLookup := func() {
		if lookup == nil {
			return
		}
		finalLookupMu.Lock()
		defer finalLookupMu.Unlock()
		if finalLookupAttempted {
			return
		}
		finalLookupAttempted = true

		beforeFinal := lookup.sessionID()
		finalCtx, finalCancel := context.WithTimeout(context.Background(), clineHistoryTimeout+time.Second)
		finalID, lookupErr := lookup.finalLookup(finalCtx, func(historyCtx context.Context) ([]clineHistoryEntry, error) {
			return history(historyCtx, execPath, opts.Cwd, hub.env)
		})
		finalCancel()
		if lookupErr != nil {
			b.cfg.Logger.Warn("cline: final SessionID lookup failed", "error", lookupErr)
		}
		if beforeFinal == "" && finalID != "" {
			notifyCtx, notifyCancel := context.WithTimeout(context.Background(), time.Second)
			_ = sendClineMessage(notifyCtx, msgCh, Message{Type: MessageStatus, Status: "running", SessionID: finalID})
			notifyCancel()
		}
	}

	go func() {
		select {
		case <-procDone:
			return
		case <-runCtx.Done():
		}
		if hub != nil {
			cancelLookup()
			<-earlyDone
			runFinalLookup()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), clineHubControlTimeout+clineHubExitTimeout+time.Second)
			if stopErr := hub.stop(stopCtx); stopErr != nil {
				b.cfg.Logger.Warn("cline: dedicated hub cancellation cleanup failed", "error", stopErr)
			}
			stopCancel()
		}
		if cmd.Process != nil {
			signalProcessGroup(cmd.Process, syscall.SIGTERM)
			select {
			case <-procDone:
			case <-time.After(clineTaskTerminateGrace):
				signalProcessGroup(cmd.Process, syscall.SIGKILL)
			}
		}
		_ = stdout.Close()
	}()

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := cmdStartTime
		var observe func(clineProtocolObservation)
		if lookup != nil {
			observe = lookup.observe
		}
		scanResult := b.processEventsObserved(runCtx, stdout, msgCh, observe)

		exitErr := cmd.Wait()
		close(procDone)
		cancelLookup()
		<-earlyDone
		endTime := time.Now()
		duration := endTime.Sub(startTime)
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

		resumeRejected := opts.ResumeSessionID != "" && scanResult.resumeRejected
		sessionID := opts.ResumeSessionID
		if resumeRejected {
			sessionID = ""
		} else if lookup != nil {
			runFinalLookup()
			sessionID = lookup.sessionID()
		}

		if hub != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), clineHubControlTimeout+clineHubExitTimeout+time.Second)
			if stopErr := hub.stop(stopCtx); stopErr != nil {
				b.cfg.Logger.Warn("cline: dedicated hub cleanup failed", "error", stopErr)
			}
			stopCancel()
		}

		b.cfg.Logger.Info("cline finished",
			"pid", childPID,
			"status", scanResult.status,
			"duration", duration.Round(time.Millisecond).String(),
			"session_id", sessionID,
		)

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
			Status:         scanResult.status,
			Output:         scanResult.output,
			Error:          scanResult.errMsg,
			DurationMs:     duration.Milliseconds(),
			SessionID:      sessionID,
			Usage:          usage,
			ResumeRejected: resumeRejected,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

func buildClineEnv(extra map[string]string, resume bool) []string {
	env := buildEnv(extra)
	if !resume {
		return env
	}
	filtered := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "CLINE_VCR", "CLINE_SESSION_BACKEND_MODE":
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, "CLINE_SESSION_BACKEND_MODE=hub")
}

// buildClineStdinPayload builds the full Multica prompt delivered on process
// stdin (runtime brief + user task). Empty system prompt is skipped; user
// prompt is used as-is so empty-task edge cases stay explicit.
func buildClineStdinPayload(prompt string, opts ExecOptions) string {
	if strings.TrimSpace(opts.SystemPrompt) != "" {
		return opts.SystemPrompt + "\n\n" + prompt
	}
	return prompt
}

// buildClineArgs assembles argv for a headless Cline 3.x NDJSON run.
// The positional prompt is only the newline gate sentinel; the real Multica
// payload is written to stdin (see buildClineStdinPayload). No -s.
// Timeout is owned by Multica runContext; -t is never passed (v1).
// --data-dir and --config are never passed; both fresh and resume runs use
// Cline's persistent state while Multica owns only the fresh Hub endpoint.
// --auto-approve is omitted (CLI default true).
func buildClineArgs(opts ExecOptions, logger *slog.Logger) []string {
	args := []string{"--json"}
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
	args = append(args, clineArgvPromptSentinel)
	return args
}

// ── NDJSON (形态 B) ──

type clineScanResult struct {
	status                   string
	errMsg                   string
	output                   string
	usage                    TokenUsage
	sawRunResult             bool
	doneReason               string
	doneText                 string
	resumeRejected           bool
	firstProtocolTimestampMs int64
	sawIterationStart        bool
}

type clineProtocolObservation struct {
	FirstTimestampMs int64
	IterationStarted bool
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
	Code           string          `json:"code"`
	ErrorCode      string          `json:"errorCode"`
	TS             json.RawMessage `json:"ts"`
}

type clineUsageBlob struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

type clineNestedEvent struct {
	Type             string           `json:"type"`
	HookName         string           `json:"hookName"`
	Text             string           `json:"text"`
	Accumulated      string           `json:"accumulated"`
	Reasoning        string           `json:"reasoning"`
	Message          string           `json:"message"`
	ContentType      string           `json:"contentType"`
	ContentBlockID   string           `json:"contentBlockId"`
	BlockID          string           `json:"blockId"`
	Reason           string           `json:"reason"`
	Code             string           `json:"code"`
	InputTokens      int64            `json:"inputTokens"`
	OutputTokens     int64            `json:"outputTokens"`
	CacheReadTokens  int64            `json:"cacheReadTokens"`
	CacheWriteTokens int64            `json:"cacheWriteTokens"`
	Name             string           `json:"name"`
	Tool             string           `json:"tool"`
	ToolName         string           `json:"toolName"`
	CallID           string           `json:"callId"`
	ToolCallID       string           `json:"toolCallId"`
	ID               string           `json:"id"`
	SessionID        string           `json:"sessionId"`
	Input            json.RawMessage  `json:"input"`
	Params           json.RawMessage  `json:"params"`
	Parameters       json.RawMessage  `json:"parameters"`
	Output           json.RawMessage  `json:"output"`
	Result           json.RawMessage  `json:"result"`
	Content          json.RawMessage  `json:"content"`
	Delta            json.RawMessage  `json:"delta"`
	Snapshot         json.RawMessage  `json:"snapshot"`
	Update           json.RawMessage  `json:"update"`
	Error            json.RawMessage  `json:"error"`
	ToolCall         *clineToolCall   `json:"tool_call"`
	ToolResult       *clineToolResult `json:"tool_result"`
}

type clineToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type clineToolResult struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
}

// processEvents reads Cline 3.x NDJSON lines from r and maps them to Multica
// Messages / terminal Result fields. Extracted for fixture-driven unit tests.
//
// Stream sessionId fields (when present) are intentionally ignored for
// Result.SessionID: real Cline 3.x CLI NDJSON does not emit a resume-capable
// id; exact dedicated-Hub history matching owns that field. Synthetic NDJSON
// sessionId fixtures must not green-wash the resume contract.
func (b *clineBackend) processEvents(ctx context.Context, r io.Reader, ch chan<- Message) clineScanResult {
	return b.processEventsObserved(ctx, r, ch, nil)
}

// processEventsObserved reports the two fresh-session lookup triggers while
// stdout is still being drained. observe must return immediately; production
// uses it only for once-style notification into the lookup coordinator.
func (b *clineBackend) processEventsObserved(ctx context.Context, r io.Reader, ch chan<- Message, observe func(clineProtocolObservation)) clineScanResult {
	var streamOut strings.Builder
	var usage TokenUsage
	finalStatus := "completed"
	var finalError string
	var sawRunResult bool
	var runOutput string
	var doneReason, doneText string
	var resumeRejected bool
	var firstProtocolTimestampMs int64
	var sawIterationStart bool
	toolSeq := 0
	contentBlocks := map[string]string{}
	inFlightTools := map[string]string{}
	buildResult := func(status, errMsg string) clineScanResult {
		output := runOutput
		if output == "" {
			output = doneText
		}
		if output == "" {
			output = streamOut.String()
		}
		return clineScanResult{
			status:                   status,
			errMsg:                   errMsg,
			output:                   output,
			usage:                    usage,
			sawRunResult:             sawRunResult,
			doneReason:               doneReason,
			doneText:                 doneText,
			resumeRejected:           resumeRejected,
			firstProtocolTimestampMs: firstProtocolTimestampMs,
			sawIterationStart:        sawIterationStart,
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
		if firstProtocolTimestampMs == 0 && isClineProtocolRecord(env.Type) {
			if timestamp, ok := parseClineProtocolTimestamp(env.TS); ok {
				firstProtocolTimestampMs = timestamp
				if observe != nil {
					observe(clineProtocolObservation{FirstTimestampMs: timestamp})
				}
			}
		}

		switch env.Type {
		case "agent_event":
			var ev clineNestedEvent
			if len(env.Event) > 0 {
				if err := json.Unmarshal(env.Event, &ev); err != nil {
					continue
				}
				if isClineResumeRejectionCode(ev.Code) {
					resumeRejected = true
				}
			}

			switch ev.Type {
			case "content_start", "content_update", "content_end":
				// In the pinned 3.0.46 SDK contract content_update is tool
				// progress only. Keep accepting older text delta variants, but
				// do not turn tool progress into duplicate tool lifecycle rows.
				if strings.EqualFold(ev.ContentType, "tool") {
					continue
				}
				blockID := firstNonEmpty(ev.ContentBlockID, ev.BlockID, ev.ID, ev.ContentType, "default")
				text, isDelta := extractClineContentText(ev)
				previous := contentBlocks[blockID]
				if ev.Type == "content_start" {
					previous = ""
					contentBlocks[blockID] = ""
				}
				if text != "" {
					chunk := text
					if ev.Type == "content_start" {
						contentBlocks[blockID] = text
					} else if !isDelta {
						chunk = clineSnapshotSuffix(previous, text)
						contentBlocks[blockID] = text
					} else {
						contentBlocks[blockID] = previous + text
					}
					if chunk != "" {
						messageType := clineContentMessageType(ev.ContentType)
						if messageType == MessageText {
							streamOut.WriteString(chunk)
						}
						if !sendClineMessage(ctx, ch, Message{Type: messageType, Content: chunk}) {
							return buildResult("aborted", "execution cancelled")
						}
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
				if msg := extractClineError(ev); msg != "" {
					if !sendClineMessage(ctx, ch, Message{Type: MessageError, Content: msg}) {
						return buildResult("aborted", "execution cancelled")
					}
					if finalError == "" {
						finalError = msg
					}
				}
			case "notice", "iteration_start", "iteration_end":
				// Lifecycle only — ignore for Multica messages.
				if ev.Type == "iteration_start" && !sawIterationStart {
					sawIterationStart = true
					if observe != nil {
						observe(clineProtocolObservation{IterationStarted: true})
					}
				}
			default:
				b.logUnknownClineEvent("agent_event", env.Event, ev.Type)
			}

		case "hook_event":
			var ev clineNestedEvent
			if len(env.Event) > 0 {
				if err := json.Unmarshal(env.Event, &ev); err != nil {
					continue
				}
				if isClineResumeRejectionCode(ev.Code) {
					resumeRejected = true
				}
			}

			eventType := firstNonEmpty(ev.Type, ev.HookName)
			switch eventType {
			case "tool_call":
				toolSeq++
				callID := firstNonEmpty(ev.CallID, ev.ToolCallID, ev.ID)
				name := firstNonEmpty(ev.Name, ev.Tool, ev.ToolName)
				input := parseClineToolInput(ev)
				if ev.ToolCall != nil {
					callID = firstNonEmpty(ev.ToolCall.ID, callID)
					name = firstNonEmpty(ev.ToolCall.Name, name)
					input = parseClineToolInputRaw(ev.ToolCall.Input, input)
				}
				if callID == "" {
					callID = fmt.Sprintf("cline-tool-%d", toolSeq)
				}
				if name == "" {
					name = "tool"
				}
				inFlightTools[callID] = name
				if !sendClineMessage(ctx, ch, Message{
					Type:   MessageToolUse,
					Tool:   name,
					CallID: callID,
					Input:  input,
				}) {
					return buildResult("aborted", "execution cancelled")
				}
			case "tool_result":
				callID := firstNonEmpty(ev.CallID, ev.ToolCallID, ev.ID)
				tool := firstNonEmpty(ev.Name, ev.Tool, ev.ToolName)
				out := extractClineToolOutput(ev)
				if ev.ToolResult != nil {
					callID = firstNonEmpty(ev.ToolResult.ID, callID)
					tool = firstNonEmpty(ev.ToolResult.Name, tool)
					out = extractClineToolResultOutput(*ev.ToolResult, out)
				}
				if callID == "" && len(inFlightTools) == 1 {
					for id := range inFlightTools {
						callID = id
					}
				}
				tool = firstNonEmpty(tool, inFlightTools[callID])
				delete(inFlightTools, callID)
				if !sendClineMessage(ctx, ch, Message{
					Type:   MessageToolResult,
					Tool:   tool,
					CallID: callID,
					Output: out,
				}) {
					return buildResult("aborted", "execution cancelled")
				}
			case "agent_start", "agent_end":
				// Lifecycle only. hook taskId/agentId are not resume SessionIDs.
			default:
				b.logUnknownClineEvent("hook_event", env.Event, eventType)
			}

		case "run_result":
			if isClineResumeRejectionCode(firstNonEmpty(env.Code, env.ErrorCode)) {
				resumeRejected = true
			}
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
			// Do not pin SessionID from run_result sessionId/id — real CLI
			// omits them; synthetic values are not resume-capable --id tokens.

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

	// Empty Output with completed is valid (side-effect-only multica work).
	return buildResult(finalStatus, finalError)
}

func isClineProtocolRecord(recordType string) bool {
	switch recordType {
	case "hook_event", "agent_event", "run_result":
		return true
	default:
		return false
	}
}

func parseClineProtocolTimestamp(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		ms, err := strconv.ParseInt(number.String(), 10, 64)
		return ms, err == nil && ms > 0
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	if ms, err := strconv.ParseInt(value, 10, 64); err == nil && ms > 0 {
		return ms, true
	}
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	return timestamp.UnixMilli(), err == nil
}

func isClineResumeRejectionCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "session_not_found", "session_replaced":
		return true
	default:
		return false
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
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err == nil {
			if err := json.Unmarshal([]byte(encoded), &m); err == nil && len(m) > 0 {
				return m
			}
		}
	}
	return nil
}

func parseClineToolInputRaw(raw json.RawMessage, fallback map[string]any) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err == nil {
		return input
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil && json.Unmarshal([]byte(encoded), &input) == nil {
		return input
	}
	return fallback
}

func extractClineToolOutput(ev clineNestedEvent) string {
	if ev.Text != "" {
		return ev.Text
	}
	if ev.Message != "" {
		return ev.Message
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

func extractClineToolResultOutput(result clineToolResult, fallback string) string {
	output := extractClineRawText(result.Output)
	if output == "" {
		output = fallback
	}
	if result.Error == "" {
		return output
	}
	if output == "" {
		return result.Error
	}
	return output + "\n" + result.Error
}

func extractClineRawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	return string(raw)
}

func extractClineError(ev clineNestedEvent) string {
	if msg := firstNonEmpty(ev.Text, ev.Message); msg != "" {
		return msg
	}
	if len(ev.Error) == 0 || string(ev.Error) == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(ev.Error, &value); err == nil {
		return value
	}
	var object struct {
		Message string `json:"message"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(ev.Error, &object); err == nil {
		return firstNonEmpty(object.Message, object.Name)
	}
	return ""
}

func extractClineContentText(ev clineNestedEvent) (string, bool) {
	if len(ev.Delta) > 0 && string(ev.Delta) != "null" {
		return extractClineTextFromContent(ev.Delta), true
	}
	if len(ev.Snapshot) > 0 && string(ev.Snapshot) != "null" {
		return extractClineTextFromContent(ev.Snapshot), false
	}
	if ev.Reasoning != "" {
		return ev.Reasoning, false
	}
	if ev.Text != "" {
		return ev.Text, false
	}
	if ev.Accumulated != "" {
		return ev.Accumulated, false
	}
	return extractClineTextFromContent(ev.Content), false
}

func (b *clineBackend) logUnknownClineEvent(envelope string, raw json.RawMessage, eventType string) {
	if b.cfg.Logger == nil {
		return
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	b.cfg.Logger.Debug("cline: skip unknown event", "envelope", envelope, "event_type", eventType, "keys", keys)
}

func clineSnapshotSuffix(previous, current string) string {
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	return current
}

func clineContentMessageType(contentType string) MessageType {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "thinking", "reasoning", "thought":
		return MessageThinking
	default:
		return MessageText
	}
}

func sendClineMessage(ctx context.Context, ch chan<- Message, msg Message) bool {
	select {
	case ch <- msg:
		return true
	case <-ctx.Done():
		return false
	}
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
