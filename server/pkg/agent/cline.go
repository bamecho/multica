package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// clineArgvPromptSentinel is the positional prompt passed on argv for headless
// --json runs. Cline 3.x gates on truthiness of o.prompt (empty string fails);
// a single newline passes the gate and is trimmed out of the session user
// message so the model only sees the full Multica payload on stdin.
// See docs/plan/02-cline-prompt-stdin-hybrid.md.
const clineArgvPromptSentinel = "\n"

// clineBlockedArgs are flags owned by the daemon for the Cline 3.x NDJSON
// control plane (形态 B). Users must not override protocol transport, cwd,
// session isolation/resume, system/timeout flags (v1 never passes -s/-t),
// or model. Multica owns --data-dir for per-run isolation + disk SessionID
// discovery (see plan 01). --config is blocked but not passed: --data-dir
// enables CLI sandbox mode which forces provider settings under data-dir
// (CLINE_PROVIDER_SETTINGS_PATH), so Multica seeds ~/.cline-sr settings into
// the isolated tree instead of relying on --config.
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
}

// clineBackend implements Backend by spawning a Cline-compatible CLI with
// `--json` and parsing Cline 3.x NDJSON (形态 B) from stdout: agent_event /
// hook_event / run_result. See docs/cline-ndjson-multica-adapter-plan.md.
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

	payload := buildClineStdinPayload(prompt, opts)
	paths, pathErr := prepareClinePaths(opts)
	if pathErr != nil {
		cancel()
		return nil, pathErr
	}
	args := buildClineArgs(opts, paths, b.cfg.Logger)
	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	// Never log full stdin body at info — only size + argv sentinel label.
	b.cfg.Logger.Info("agent command",
		"exec", execPath,
		"args", args,
		"argv_prompt", "newline_sentinel",
		"stdin_bytes", len(payload),
		"data_dir", paths.DataDir,
		"settings_source", paths.SettingsSource,
	)
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
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cline stdin pipe: %w", err)
	}
	stderrTail := newStderrTail(newLogWriter(b.cfg.Logger, "[cline:stderr] "), agentStderrTailBytes)
	cmd.Stderr = stderrTail

	if err := cmd.Start(); err != nil {
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
		"data_dir", paths.DataDir,
		"settings_source", paths.SettingsSource,
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

		// Resume-capable session id lives on disk under --data-dir, not in
		// real CLI NDJSON. Never treat stream sessionId / taskId / agentId as
		// Result.SessionID (see docs/plan/01-cline-session-id-data-dir.md).
		sessionID := discoverClineSessionID(paths.DataDir, clineSessionMatchHints{
			PID:       childPID,
			Cwd:       opts.Cwd,
			Prompt:    payload,
			StartTime: startTime,
			EndTime:   endTime,
		}, b.cfg.Logger)

		b.cfg.Logger.Info("cline finished",
			"pid", childPID,
			"status", scanResult.status,
			"duration", duration.Round(time.Millisecond).String(),
			"session_id", sessionID,
			"data_dir", paths.DataDir,
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
			Status:     scanResult.status,
			Output:     scanResult.output,
			Error:      scanResult.errMsg,
			DurationMs: duration.Milliseconds(),
			SessionID:  sessionID,
			Usage:      usage,
		}
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
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

// clinePaths holds Multica-owned filesystem locations for one Cline run.
type clinePaths struct {
	// DataDir is the isolated --data-dir root (sessions live under data/sessions).
	// Provider auth is seeded into this tree before spawn (sandbox mode).
	DataDir string
	// SettingsSource is the user CLI settings directory copied into DataDir
	// (default: ~/.cline-sr/data/settings). Not passed as --config.
	SettingsSource string
}

// clineSettingsSourceDirOverride is set only by tests to a fixture settings
// directory. Production always uses ~/.cline-sr/data/settings.
var clineSettingsSourceDirOverride string

// defaultClineSettingsSourceDir returns the Multica Cline CLI home settings
// path (~/.cline-sr/data/settings) used as the seed source for sandbox runs.
func defaultClineSettingsSourceDir() (string, error) {
	if v := strings.TrimSpace(clineSettingsSourceDirOverride); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cline: resolve home for settings seed: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("cline: empty home directory for settings seed")
	}
	return filepath.Join(home, ".cline-sr", "data", "settings"), nil
}

// prepareClinePaths chooses an isolated data-dir under the process temp base
// (daemon sets TMPDIR per task) and seeds provider settings into it so sandbox
// mode still has auth/model without --config.
func prepareClinePaths(_ ExecOptions) (clinePaths, error) {
	// os.MkdirTemp honors TMPDIR; the daemon sets a per-task TMPDIR so this
	// stays outside the user's project worktree (local_directory mode).
	// P1 may reuse a durable data-dir when ResumeSessionID is set.
	dataDir, err := os.MkdirTemp("", "multica-cline-data-*")
	if err != nil {
		return clinePaths{}, fmt.Errorf("cline: create data-dir: %w", err)
	}
	src, err := defaultClineSettingsSourceDir()
	if err != nil {
		return clinePaths{}, err
	}
	if err := seedClineSettingsIntoDataDir(dataDir, src); err != nil {
		return clinePaths{}, err
	}
	return clinePaths{DataDir: dataDir, SettingsSource: src}, nil
}

// seedClineSettingsIntoDataDir copies authenticated CLI settings into the
// isolated data-dir. --data-dir enables Cline sandbox mode, which forces
// CLINE_PROVIDER_SETTINGS_PATH under the data-dir and ignores a separate
// --config for provider auth. We seed both common layouts:
//   - <data-dir>/settings          (OSS sandbox env path)
//   - <data-dir>/data/settings     (nested tree when data-dir is ~/.cline root)
// providers.json is required; other files are copied when present.
func seedClineSettingsIntoDataDir(dataDir, srcSettings string) error {
	srcSettings = strings.TrimSpace(srcSettings)
	if srcSettings == "" {
		return fmt.Errorf("cline: empty settings source for seed")
	}
	providers := filepath.Join(srcSettings, "providers.json")
	st, err := os.Stat(providers)
	if err != nil {
		return fmt.Errorf("cline: missing providers.json under %s (run cline auth first): %w", srcSettings, err)
	}
	if st.IsDir() {
		return fmt.Errorf("cline: providers.json under %s is a directory", srcSettings)
	}

	dests := []string{
		filepath.Join(dataDir, "settings"),
		filepath.Join(dataDir, "data", "settings"),
	}
	for _, dest := range dests {
		if err := copyClineSettingsDir(srcSettings, dest); err != nil {
			return fmt.Errorf("cline: seed settings into %s: %w", dest, err)
		}
	}
	return nil
}

// copyClineSettingsDir recursively copies settings files from src to dst.
// Files are written with 0o600 (may contain API keys). Directories 0o700.
func copyClineSettingsDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		// Skip non-regular files (symlinks to secrets are still opened via Open).
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyClineFile(path, target, 0o600)
	})
}

func copyClineFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// buildClineArgs assembles argv for a headless Cline 3.x NDJSON run.
// The positional prompt is only the newline gate sentinel; the real Multica
// payload is written to stdin (see buildClineStdinPayload). No -s.
// Timeout is owned by Multica runContext; -t is never passed (v1).
// paths.DataDir is always passed as --data-dir for isolation + session discovery.
// --config is never passed (sandbox ignores it for providers; settings are seeded).
// --auto-approve is omitted (CLI default true).
func buildClineArgs(opts ExecOptions, paths clinePaths, logger *slog.Logger) []string {
	args := []string{"--json"}
	if paths.DataDir != "" {
		args = append(args, "--data-dir", paths.DataDir)
	}
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

// clineSessionMatchHints scores session JSON files under a data-dir after exit.
type clineSessionMatchHints struct {
	PID       int
	Cwd       string
	Prompt    string // Multica stdin payload (may be wrapped by Cline on disk)
	StartTime time.Time
	EndTime   time.Time
}

// clineSessionFile is the subset of Cline disk session JSON used for discovery.
// Real CLI (3.0.40) writes data/sessions/<id>/<id>.json with these fields.
type clineSessionFile struct {
	SessionID     string `json:"session_id"`
	PID           int    `json:"pid"`
	Cwd           string `json:"cwd"`
	WorkspaceRoot string `json:"workspace_root"`
	Prompt        string `json:"prompt"`
	StartedAt     string `json:"started_at"`
	EndedAt       string `json:"ended_at"`
	Status        string `json:"status"`
}

// discoverClineSessionID scans dataDir for a resume-capable session_id.
// Real Cline NDJSON does not emit sessionId; disk is the source of truth for
// --id. Returns "" when no acceptable match is found (no panic).
// Never returns taskId/agentId-style values from hooks — only session_id from
// session JSON files.
func discoverClineSessionID(dataDir string, hints clineSessionMatchHints, logger *slog.Logger) string {
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	candidates := listClineSessionFiles(dataDir)
	if len(candidates) == 0 {
		if logger != nil {
			logger.Warn("cline: no session files under data-dir for SessionID discovery",
				"data_dir", dataDir)
		}
		return ""
	}

	bestID := ""
	bestScore := -1
	for _, path := range candidates {
		meta, err := readClineSessionFile(path)
		if err != nil || meta == nil {
			continue
		}
		id := strings.TrimSpace(meta.SessionID)
		if id == "" {
			// Fall back to parent directory name (sessions/<id>/<id>.json).
			id = filepath.Base(filepath.Dir(path))
		}
		if !isClineResumeSessionID(id) {
			continue
		}
		score := scoreClineSessionMatch(meta, hints)
		if score > bestScore {
			bestScore = score
			bestID = id
		}
	}
	if bestID == "" {
		if logger != nil {
			logger.Warn("cline: session files present but none matched discovery hints",
				"data_dir", dataDir, "candidates", len(candidates))
		}
		return ""
	}
	// Require a positive score so an unrelated leftover session is not picked
	// when hints cannot match (isolated data-dir usually has only this run).
	if bestScore <= 0 && len(candidates) > 1 {
		if logger != nil {
			logger.Warn("cline: ambiguous session match with non-positive score",
				"data_dir", dataDir, "best_score", bestScore)
		}
		return ""
	}
	// Single candidate under an isolated data-dir is always the run's session
	// even when pid/cwd hints are weak (e.g. process already reaped).
	if bestScore <= 0 && len(candidates) == 1 {
		return bestID
	}
	if bestScore <= 0 {
		return ""
	}
	return bestID
}

// isClineResumeSessionID rejects values that look like hook taskId/agentId
// rather than Cline history / --id session identifiers.
func isClineResumeSessionID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "conv_") || strings.HasPrefix(lower, "agent_") {
		return false
	}
	// Synthetic NDJSON-only fixtures used "ses_*"; real disk ids are
	// timestamp_random (e.g. 1784085932547_muen6). We accept any non-empty
	// id that is not an obvious hook id — do not require a prefix.
	return true
}

func listClineSessionFiles(dataDir string) []string {
	// Observed default: <data-dir>/data/sessions/<id>/<id>.json
	// Tolerate <data-dir>/sessions/<id>/<id>.json as well.
	roots := []string{
		filepath.Join(dataDir, "data", "sessions"),
		filepath.Join(dataDir, "sessions"),
	}
	var out []string
	seen := map[string]struct{}{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			sessionDir := filepath.Join(root, ent.Name())
			// Prefer <id>/<id>.json; also accept any non-messages *.json.
			primary := filepath.Join(sessionDir, ent.Name()+".json")
			if st, err := os.Stat(primary); err == nil && !st.IsDir() {
				if _, ok := seen[primary]; !ok {
					seen[primary] = struct{}{}
					out = append(out, primary)
				}
				continue
			}
			files, err := os.ReadDir(sessionDir)
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				name := f.Name()
				if !strings.HasSuffix(name, ".json") {
					continue
				}
				if strings.HasSuffix(name, ".messages.json") {
					continue
				}
				p := filepath.Join(sessionDir, name)
				if _, ok := seen[p]; ok {
					continue
				}
				seen[p] = struct{}{}
				out = append(out, p)
			}
		}
	}
	return out
}

func readClineSessionFile(path string) (*clineSessionFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta clineSessionFile
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func scoreClineSessionMatch(meta *clineSessionFile, hints clineSessionMatchHints) int {
	if meta == nil {
		return 0
	}
	score := 0
	// pid match is strongest (unique per concurrent run under isolated data-dir).
	if hints.PID > 0 && meta.PID == hints.PID {
		score += 100
	}
	cwd := strings.TrimSpace(hints.Cwd)
	if cwd != "" {
		if samePathLoose(meta.Cwd, cwd) || samePathLoose(meta.WorkspaceRoot, cwd) {
			score += 40
		}
	}
	if promptMatchesClineSession(meta.Prompt, hints.Prompt) {
		score += 20
	}
	if started, ok := parseClineSessionTime(meta.StartedAt); ok {
		// Allow small clock skew around the Multica-observed run window.
		windowStart := hints.StartTime.Add(-2 * time.Minute)
		windowEnd := hints.EndTime.Add(2 * time.Minute)
		if !hints.EndTime.IsZero() && started.After(windowStart) && started.Before(windowEnd) {
			score += 15
		} else if hints.EndTime.IsZero() && !hints.StartTime.IsZero() && started.After(windowStart) {
			score += 10
		}
	}
	// Prefer completed-looking sessions when scores tie via tiny bias.
	if strings.EqualFold(strings.TrimSpace(meta.Status), "completed") {
		score += 1
	}
	return score
}

func samePathLoose(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	// Best-effort clean comparison without requiring paths to exist.
	return filepath.Clean(a) == filepath.Clean(b)
}

func promptMatchesClineSession(diskPrompt, multicaPayload string) bool {
	diskPrompt = strings.TrimSpace(diskPrompt)
	multicaPayload = strings.TrimSpace(multicaPayload)
	if diskPrompt == "" || multicaPayload == "" {
		return false
	}
	// Cline wraps user input: <user_input mode="act">…</user_input>
	inner := diskPrompt
	if i := strings.Index(diskPrompt, ">"); i >= 0 && strings.Contains(diskPrompt, "<user_input") {
		rest := diskPrompt[i+1:]
		if j := strings.LastIndex(rest, "</user_input>"); j >= 0 {
			inner = strings.TrimSpace(rest[:j])
		}
	}
	if inner == multicaPayload || strings.Contains(diskPrompt, multicaPayload) {
		return true
	}
	// Prefix match for long prompts truncated on disk.
	const prefixN = 64
	prefix := multicaPayload
	if len(prefix) > prefixN {
		prefix = prefix[:prefixN]
	}
	return strings.Contains(diskPrompt, prefix) || strings.Contains(inner, prefix)
}

func parseClineSessionTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// Real files use RFC3339 / RFC3339Nano with Z.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ── NDJSON (形态 B) ──

type clineScanResult struct {
	status       string
	errMsg       string
	output       string
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
//
// Stream sessionId fields (when present) are intentionally ignored for
// Result.SessionID: real Cline 3.x CLI NDJSON does not emit a resume-capable
// id; disk discovery after Wait owns that field. Synthetic NDJSON sessionId
// fixtures must not green-wash the resume contract.
func (b *clineBackend) processEvents(r io.Reader, ch chan<- Message) clineScanResult {
	var streamOut strings.Builder
	var usage TokenUsage
	finalStatus := "completed"
	var finalError string
	var sawRunResult bool
	var runOutput string
	var doneReason, doneText string
	toolSeq := 0
	var lastToolCallID string

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

		switch env.Type {
		case "agent_event":
			var ev clineNestedEvent
			if len(env.Event) > 0 {
				if err := json.Unmarshal(env.Event, &ev); err != nil {
					continue
				}
			}

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
				// Lifecycle only. hook taskId/agentId are not resume SessionIDs.
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
