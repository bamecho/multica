package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	clineHistoryLimit          = 500
	clineHistoryTimeout        = 3 * time.Second
	clineHistoryMaxOutputBytes = 8 << 20
	clineHubRecordMaxBytes     = 64 << 10
	clineHubStartTimeout       = 15 * time.Second
	clineHubReadinessTimeout   = 5 * time.Second
	clineHubControlTimeout     = 3 * time.Second
	clineHubExitTimeout        = 3 * time.Second
)

var clineHistoryRetryDelays = []time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond}

type clineHubRuntime struct {
	RuntimeDir    string
	DiscoveryPath string
}

type clineHub struct {
	runtime         clineHubRuntime
	discovery       clineHubRecord
	status          clineHubRecord
	process         clineProcessIdentity
	launchStartedAt time.Time
	env             []string

	stopMu   sync.Mutex
	stopWait chan struct{}
	stopped  bool
	stopFn   func(context.Context) error
}

type clineProcessIdentity struct {
	PID        int
	StartToken string
	Executable string
}

type clineHubRecord struct {
	HubID           string
	ProtocolVersion string
	AuthToken       string
	Host            string
	Port            int
	URL             string
	PID             int
	StartedAt       time.Time
	StartToken      string
}

type clineHubJSONRecord struct {
	HubID           string `json:"hubId"`
	ProtocolVersion string `json:"protocolVersion"`
	AuthToken       string `json:"authToken"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	URL             string `json:"url"`
	PID             int    `json:"pid"`
	StartedAt       string `json:"startedAt"`
	StartToken      string `json:"startToken"`
}

func sameClineProcessIdentity(expected clineProcessIdentity) bool {
	matches, err := matchClineProcessIdentity(expected)
	return err == nil && matches
}

func matchClineProcessIdentity(expected clineProcessIdentity) (bool, error) {
	if expected.PID <= 0 || expected.StartToken == "" || expected.Executable == "" {
		return false, fmt.Errorf("incomplete cline Hub process identity")
	}
	current, err := captureClineProcessIdentity(expected.PID)
	if err != nil {
		return false, err
	}
	return current.StartToken == expected.StartToken && sameCanonicalClinePath(current.Executable, expected.Executable), nil
}

type clineHistoryJSONEntry struct {
	SessionID       string  `json:"sessionId"`
	PID             int     `json:"pid"`
	Source          *string `json:"source"`
	Interactive     *bool   `json:"interactive"`
	IsSubagent      *bool   `json:"isSubagent"`
	ParentSessionID string  `json:"parentSessionId"`
	Cwd             string  `json:"cwd"`
	WorkspaceRoot   string  `json:"workspaceRoot"`
	StartedAt       *string `json:"startedAt"`
}

type clineHistoryEntry struct {
	SessionID       string
	PID             int
	Source          *string
	Interactive     *bool
	IsSubagent      *bool
	ParentSessionID string
	Cwd             string
	WorkspaceRoot   string
	StartedAt       *time.Time
}

type clineHistoryMatch struct {
	PreRunSessionIDs         map[string]struct{}
	DedicatedHubPID          int
	CanonicalWorkDir         string
	CmdStartWallMs           int64
	FirstProtocolTimestampMs int64
}

type clineSessionLookupCoordinator struct {
	mu               sync.Mutex
	match            clineHistoryMatch
	iterationStarted bool
	earlyTrigger     chan struct{}
	earlyTriggerOnce sync.Once
	immutableID      string
}

func newClineSessionLookupCoordinator(match clineHistoryMatch) *clineSessionLookupCoordinator {
	return &clineSessionLookupCoordinator{
		match:        match,
		earlyTrigger: make(chan struct{}),
	}
}

func (c *clineSessionLookupCoordinator) observe(observation clineProtocolObservation) {
	c.mu.Lock()
	if c.match.FirstProtocolTimestampMs == 0 && observation.FirstTimestampMs > 0 {
		c.match.FirstProtocolTimestampMs = observation.FirstTimestampMs
	}
	if observation.IterationStarted {
		c.iterationStarted = true
	}
	ready := c.iterationStarted && c.match.FirstProtocolTimestampMs > 0
	c.mu.Unlock()
	if ready {
		c.earlyTriggerOnce.Do(func() { close(c.earlyTrigger) })
	}
}

func (c *clineSessionLookupCoordinator) setCommandStart(wallMs int64) {
	c.mu.Lock()
	if c.match.CmdStartWallMs == 0 {
		c.match.CmdStartWallMs = wallMs
	}
	c.mu.Unlock()
}

func (c *clineSessionLookupCoordinator) runEarly(ctx context.Context, history func(context.Context) ([]clineHistoryEntry, error), retryDelays []time.Duration, onFound func(string)) {
	select {
	case <-ctx.Done():
		return
	case <-c.earlyTrigger:
	}
	for _, delay := range retryDelays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		id, err := c.lookup(ctx, history)
		if err != nil || id == "" {
			continue
		}
		if onFound != nil {
			onFound(id)
		}
		return
	}
}

func (c *clineSessionLookupCoordinator) finalLookup(ctx context.Context, history func(context.Context) ([]clineHistoryEntry, error)) (string, error) {
	return c.lookup(ctx, history)
}

func (c *clineSessionLookupCoordinator) lookup(ctx context.Context, history func(context.Context) ([]clineHistoryEntry, error)) (string, error) {
	c.mu.Lock()
	match := c.match
	c.mu.Unlock()
	if match.FirstProtocolTimestampMs == 0 {
		return c.sessionID(), nil
	}
	entries, err := history(ctx)
	if err != nil {
		return c.sessionID(), err
	}
	candidate := matchClineHistoryCandidate(entries, match)
	if candidate == "" {
		return c.sessionID(), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.immutableID == "" {
		c.immutableID = candidate
		return candidate, nil
	}
	if c.immutableID != candidate {
		return c.immutableID, fmt.Errorf("cline SessionID invariant violation: early=%q later=%q", c.immutableID, candidate)
	}
	return c.immutableID, nil
}

func (c *clineSessionLookupCoordinator) sessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.immutableID
}

func createClineHubRuntime(taskTempDir string) (clineHubRuntime, error) {
	taskTempDir = strings.TrimSpace(taskTempDir)
	if taskTempDir == "" {
		return clineHubRuntime{}, fmt.Errorf("cline dedicated hub requires task TMPDIR")
	}
	taskTempDir, err := canonicalClineWorkDir(taskTempDir)
	if err != nil {
		return clineHubRuntime{}, fmt.Errorf("resolve cline task TMPDIR: %w", err)
	}
	root := filepath.Join(taskTempDir, "cline-hub")
	if err := ensurePrivateClineDir(root); err != nil {
		return clineHubRuntime{}, err
	}
	for range 16 {
		random := make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, random); err != nil {
			return clineHubRuntime{}, fmt.Errorf("generate cline hub run id: %w", err)
		}
		runtimeDir := filepath.Join(root, hex.EncodeToString(random))
		if err := os.Mkdir(runtimeDir, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return clineHubRuntime{}, fmt.Errorf("create cline hub runtime dir: %w", err)
		}
		return clineHubRuntime{
			RuntimeDir:    runtimeDir,
			DiscoveryPath: filepath.Join(runtimeDir, "production.json"),
		}, nil
	}
	return clineHubRuntime{}, fmt.Errorf("allocate unique cline hub runtime dir")
}

func ensurePrivateClineDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create cline hub directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect cline hub directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cline hub directory is not a real directory")
	}
	return nil
}

func readClineHubRecord(path string, requireAuthToken bool) (clineHubRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return clineHubRecord{}, fmt.Errorf("inspect cline hub record: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return clineHubRecord{}, fmt.Errorf("cline hub record is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > clineHubRecordMaxBytes {
		return clineHubRecord{}, fmt.Errorf("cline hub record size is outside the allowed range")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return clineHubRecord{}, fmt.Errorf("read cline hub record: %w", err)
	}
	return parseClineHubRecord(data, requireAuthToken)
}

func parseClineHubRecord(data []byte, requireAuthToken bool) (clineHubRecord, error) {
	var raw clineHubJSONRecord
	if err := json.Unmarshal(data, &raw); err != nil {
		return clineHubRecord{}, fmt.Errorf("decode cline hub record: %w", err)
	}
	record := clineHubRecord{
		HubID:           strings.TrimSpace(raw.HubID),
		ProtocolVersion: strings.TrimSpace(raw.ProtocolVersion),
		AuthToken:       strings.TrimSpace(raw.AuthToken),
		Host:            strings.TrimSpace(raw.Host),
		Port:            raw.Port,
		URL:             strings.TrimSpace(raw.URL),
		PID:             raw.PID,
		StartToken:      strings.TrimSpace(raw.StartToken),
	}
	if requireAuthToken && record.AuthToken == "" {
		return clineHubRecord{}, fmt.Errorf("cline hub record is missing auth token")
	}
	if record.HubID == "" || record.ProtocolVersion == "" || record.PID <= 0 || record.Port < 1 || record.Port > 65535 {
		return clineHubRecord{}, fmt.Errorf("cline hub record is missing required identity fields")
	}
	startedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.StartedAt))
	if err != nil {
		return clineHubRecord{}, fmt.Errorf("cline hub record has invalid startedAt: %w", err)
	}
	record.StartedAt = startedAt
	parsedURL, err := url.Parse(record.URL)
	if err != nil || parsedURL.Scheme != "ws" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return clineHubRecord{}, fmt.Errorf("cline hub record has invalid local websocket URL")
	}
	hostIP := net.ParseIP(parsedURL.Hostname())
	recordIP := net.ParseIP(record.Host)
	urlPort, portErr := strconv.Atoi(parsedURL.Port())
	if hostIP == nil || !hostIP.IsLoopback() || recordIP == nil || !recordIP.IsLoopback() || !hostIP.Equal(recordIP) || portErr != nil || urlPort != record.Port {
		return clineHubRecord{}, fmt.Errorf("cline hub record endpoint is not a matching loopback address")
	}
	return record, nil
}

func validateClineHubIdentity(discovery, status clineHubRecord, launchStartedAt time.Time) error {
	if !sameClineHubRecordIdentity(discovery, status) {
		return fmt.Errorf("cline hub discovery and authenticated status disagree")
	}
	if discovery.StartedAt.Before(launchStartedAt) {
		return fmt.Errorf("cline hub predates this launch attempt")
	}
	if discovery.StartToken != "" || status.StartToken != "" {
		if discovery.StartToken == "" || status.StartToken == "" || discovery.StartToken != status.StartToken {
			return fmt.Errorf("cline hub process start token is inconsistent")
		}
	}
	return nil
}

func sameClineHubRecordIdentity(left, right clineHubRecord) bool {
	return left.HubID == right.HubID &&
		left.ProtocolVersion == right.ProtocolVersion &&
		left.PID == right.PID &&
		left.Host == right.Host &&
		left.Port == right.Port &&
		left.URL == right.URL &&
		left.StartedAt.Equal(right.StartedAt)
}

func startClineHub(ctx context.Context, execPath, cwd string, extra map[string]string, taskTempDir string) (*clineHub, error) {
	return startClineHubWithClient(ctx, execPath, cwd, extra, taskTempDir, clineHubHTTPClient())
}

func startClineHubWithClient(ctx context.Context, execPath, cwd string, extra map[string]string, taskTempDir string, client *http.Client) (*clineHub, error) {
	runtimeInfo, err := createClineHubRuntime(taskTempDir)
	if err != nil {
		return nil, err
	}
	cleanupRuntime := true
	hubLaunchAttempted := false
	var launchEnv []string
	defer func() {
		if cleanupRuntime {
			if hubLaunchAttempted {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), clineHubControlTimeout)
				stopCmd := exec.CommandContext(stopCtx, execPath, "hub", "stop")
				hideAgentWindow(stopCmd)
				stopCmd.Dir = cwd
				stopCmd.Env = launchEnv
				stopCmd.Stdout = io.Discard
				stopCmd.Stderr = io.Discard
				_ = stopCmd.Run()
				stopCancel()
			}
			_ = os.RemoveAll(runtimeInfo.RuntimeDir)
		}
	}()

	launchStartedAt := time.Now()
	launchEnv = buildClineFreshHubEnv(extra, runtimeInfo.DiscoveryPath, clineHubRecord{Host: "127.0.0.1"})
	startCtx, cancelStart := context.WithTimeout(ctx, clineHubStartTimeout)
	cmd := exec.CommandContext(startCtx, execPath, "hub", "--host", "127.0.0.1", "--port", "0", "start")
	hideAgentWindow(cmd)
	cmd.Dir = cwd
	cmd.Env = launchEnv
	cmd.WaitDelay = time.Second
	stdout := &clineBoundedBuffer{maxBytes: clineHubRecordMaxBytes}
	stderr := &clineBoundedBuffer{maxBytes: clineHubRecordMaxBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	hubLaunchAttempted = true
	err = cmd.Run()
	startContextErr := startCtx.Err()
	cancelStart()
	if startContextErr != nil {
		return nil, fmt.Errorf("cline dedicated hub start timed out: %w", startContextErr)
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("cline dedicated hub start output exceeded %d bytes", clineHubRecordMaxBytes)
	}
	if err != nil {
		if message := strings.TrimSpace(stderr.buf.String()); message != "" {
			return nil, fmt.Errorf("start cline dedicated hub: %w: %s", err, truncateForLog(message, 500))
		}
		return nil, fmt.Errorf("start cline dedicated hub: %w", err)
	}

	readyCtx, cancelReady := context.WithTimeout(ctx, clineHubReadinessTimeout)
	defer cancelReady()
	var discovery, status clineHubRecord
	for {
		discovery, err = readClineHubRecord(runtimeInfo.DiscoveryPath, true)
		if err == nil {
			statusCtx, cancelStatus := context.WithTimeout(readyCtx, clineHubControlTimeout)
			status, err = fetchClineHubStatusWithClient(statusCtx, discovery, client)
			cancelStatus()
			if err == nil {
				err = validateClineHubIdentity(discovery, status, launchStartedAt)
			}
			if err == nil {
				break
			}
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("cline dedicated hub readiness failed: %w", err)
		case <-timer.C:
		}
	}

	process, err := captureClineProcessIdentity(discovery.PID)
	if err != nil {
		return nil, fmt.Errorf("capture cline dedicated hub process identity: %w", err)
	}
	if discovery.StartToken != "" && discovery.StartToken != process.StartToken {
		return nil, fmt.Errorf("cline dedicated hub process start token disagrees with the operating system")
	}

	hub := &clineHub{
		runtime:         runtimeInfo,
		discovery:       discovery,
		status:          status,
		process:         process,
		launchStartedAt: launchStartedAt,
		env:             buildClineFreshHubEnv(extra, runtimeInfo.DiscoveryPath, discovery),
	}
	cleanupRuntime = false
	return hub, nil
}

func (h *clineHub) stop(ctx context.Context) error {
	for {
		h.stopMu.Lock()
		if h.stopped {
			h.stopMu.Unlock()
			return nil
		}
		if h.stopWait != nil {
			wait := h.stopWait
			h.stopMu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wait:
				continue
			}
		}
		h.stopWait = make(chan struct{})
		wait := h.stopWait
		h.stopMu.Unlock()

		var err error
		if h.stopFn != nil {
			err = h.stopFn(ctx)
		} else {
			err = h.stopDedicated(ctx)
		}

		h.stopMu.Lock()
		if err == nil {
			h.stopped = true
		}
		h.stopWait = nil
		close(wait)
		h.stopMu.Unlock()
		return err
	}
}

func (h *clineHub) stopDedicated(ctx context.Context) error {
	controlCtx, cancelControl := context.WithTimeout(ctx, clineHubControlTimeout)
	shutdownErr := shutdownClineHub(controlCtx, h.discovery, h.status, h.launchStartedAt)
	cancelControl()

	exitCtx, cancelExit := context.WithTimeout(ctx, clineHubExitTimeout)
	exited, waitErr := waitForClineProcessExit(exitCtx, h.process)
	cancelExit()
	if waitErr != nil {
		return fmt.Errorf("verify cline dedicated hub exit: %w", waitErr)
	}
	if !exited {
		current, readErr := readClineHubRecord(h.runtime.DiscoveryPath, true)
		if readErr != nil || !sameClineHubRecordIdentity(current, h.discovery) || !sameClineProcessIdentity(h.process) {
			if shutdownErr != nil {
				return shutdownErr
			}
			return fmt.Errorf("cline dedicated hub did not exit and its identity can no longer be verified")
		}
		process, findErr := os.FindProcess(h.process.PID)
		if findErr != nil {
			return fmt.Errorf("find cline dedicated hub process: %w", findErr)
		}
		if killErr := process.Kill(); killErr != nil {
			return fmt.Errorf("kill verified cline dedicated hub process: %w", killErr)
		}
		killCtx, cancelKill := context.WithTimeout(context.Background(), clineHubExitTimeout)
		exited, waitErr = waitForClineProcessExit(killCtx, h.process)
		cancelKill()
		if waitErr != nil {
			return fmt.Errorf("verify killed cline dedicated hub exit: %w", waitErr)
		}
		if !exited {
			return fmt.Errorf("verified cline dedicated hub process survived termination")
		}
	}
	if shutdownErr != nil && sameClineProcessIdentity(h.process) {
		return shutdownErr
	}
	if err := os.RemoveAll(h.runtime.RuntimeDir); err != nil {
		return fmt.Errorf("remove cline dedicated hub runtime: %w", err)
	}
	return nil
}

func waitForClineProcessExit(ctx context.Context, identity clineProcessIdentity) (bool, error) {
	for {
		matches, err := matchClineProcessIdentity(identity)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			return false, err
		}
		if !matches {
			return true, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, nil
		case <-timer.C:
		}
	}
}

func fetchClineHubStatus(ctx context.Context, discovery clineHubRecord) (clineHubRecord, error) {
	return fetchClineHubStatusWithClient(ctx, discovery, clineHubHTTPClient())
}

func fetchClineHubStatusWithClient(ctx context.Context, discovery clineHubRecord, client *http.Client) (clineHubRecord, error) {
	statusURL, err := clineHubHTTPURL(discovery.URL, "/status")
	if err != nil {
		return clineHubRecord{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return clineHubRecord{}, fmt.Errorf("create cline hub status request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+discovery.AuthToken)
	response, err := client.Do(request)
	if err != nil {
		return clineHubRecord{}, fmt.Errorf("request cline hub status: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return clineHubRecord{}, fmt.Errorf("cline hub status returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, clineHubRecordMaxBytes+1))
	if err != nil {
		return clineHubRecord{}, fmt.Errorf("read cline hub status: %w", err)
	}
	if len(body) > clineHubRecordMaxBytes {
		return clineHubRecord{}, fmt.Errorf("cline hub status exceeded %d bytes", clineHubRecordMaxBytes)
	}
	return parseClineHubRecord(body, false)
}

func shutdownClineHub(ctx context.Context, discovery, expectedStatus clineHubRecord, launchStartedAt time.Time) error {
	return shutdownClineHubWithClient(ctx, discovery, expectedStatus, launchStartedAt, clineHubHTTPClient())
}

func shutdownClineHubWithClient(ctx context.Context, discovery, expectedStatus clineHubRecord, launchStartedAt time.Time, client *http.Client) error {
	status, err := fetchClineHubStatusWithClient(ctx, discovery, client)
	if err != nil {
		return err
	}
	if err := validateClineHubIdentity(discovery, status, launchStartedAt); err != nil {
		return err
	}
	if err := validateClineHubIdentity(discovery, expectedStatus, launchStartedAt); err != nil {
		return err
	}
	shutdownURL, err := clineHubHTTPURL(discovery.URL, "/shutdown")
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, shutdownURL, nil)
	if err != nil {
		return fmt.Errorf("create cline hub shutdown request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+discovery.AuthToken)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request cline hub shutdown: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, clineHubRecordMaxBytes))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("cline hub shutdown returned HTTP %d", response.StatusCode)
	}
	return nil
}

func clineHubHTTPURL(websocketURL, pathname string) (string, error) {
	parsed, err := url.Parse(websocketURL)
	if err != nil || parsed.Scheme != "ws" {
		return "", fmt.Errorf("invalid cline hub websocket URL")
	}
	host := net.ParseIP(parsed.Hostname())
	if host == nil || !host.IsLoopback() || parsed.User != nil {
		return "", fmt.Errorf("cline hub control URL is not loopback")
	}
	parsed.Scheme = "http"
	parsed.Path = pathname
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func clineHubHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func buildClineFreshHubEnv(extra map[string]string, discoveryPath string, record clineHubRecord) []string {
	env := buildEnv(extra)
	filtered := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "CLINE_VCR", "CLINE_HUB_HOST", "CLINE_HUB_PORT", "CLINE_HUB_DISCOVERY_PATH", "CLINE_SESSION_BACKEND_MODE":
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return append(filtered,
		"CLINE_HUB_HOST="+record.Host,
		"CLINE_HUB_PORT="+strconv.Itoa(record.Port),
		"CLINE_HUB_DISCOVERY_PATH="+discoveryPath,
		"CLINE_SESSION_BACKEND_MODE=hub",
	)
}

type clineBoundedBuffer struct {
	buf      bytes.Buffer
	maxBytes int
	exceeded bool
}

func (b *clineBoundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.maxBytes-b.buf.Len() {
		b.exceeded = true
		return 0, fmt.Errorf("cline command output exceeded %d bytes", b.maxBytes)
	}
	return b.buf.Write(p)
}

func runClineHistory(ctx context.Context, execPath, cwd string, env []string) ([]clineHistoryEntry, error) {
	runCtx, cancel := context.WithTimeout(ctx, clineHistoryTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, execPath, "history", "--json", "--limit", strconv.Itoa(clineHistoryLimit))
	hideAgentWindow(cmd)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.WaitDelay = time.Second
	stdout := &clineBoundedBuffer{maxBytes: clineHistoryMaxOutputBytes}
	stderr := &clineBoundedBuffer{maxBytes: clineHistoryMaxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("cline history output exceeded %d bytes", clineHistoryMaxOutputBytes)
	}
	if runCtx.Err() != nil {
		return nil, fmt.Errorf("cline history timed out: %w", runCtx.Err())
	}
	if err != nil {
		message := strings.TrimSpace(stderr.buf.String())
		if message != "" {
			return nil, fmt.Errorf("cline history failed: %w: %s", err, truncateForLog(message, 500))
		}
		return nil, fmt.Errorf("cline history failed: %w", err)
	}
	return parseClineHistoryJSON(stdout.buf.Bytes())
}

func clineRootSessionIDs(entries []clineHistoryEntry) map[string]struct{} {
	ids := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.SessionID)
		if id == "" || entry.IsSubagent == nil || *entry.IsSubagent || strings.TrimSpace(entry.ParentSessionID) != "" {
			continue
		}
		ids[id] = struct{}{}
	}
	return ids
}

func parseClineHistoryJSON(data []byte) ([]clineHistoryEntry, error) {
	var records []json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode cline history array: %w", err)
	}
	entries := make([]clineHistoryEntry, 0, len(records))
	for _, record := range records {
		var raw clineHistoryJSONEntry
		if err := json.Unmarshal(record, &raw); err != nil {
			// A malformed record cannot become a candidate, but it does not
			// invalidate well-formed records from the same bounded snapshot.
			continue
		}
		entry := clineHistoryEntry{
			SessionID:       raw.SessionID,
			PID:             raw.PID,
			Source:          raw.Source,
			Interactive:     raw.Interactive,
			IsSubagent:      raw.IsSubagent,
			ParentSessionID: raw.ParentSessionID,
			Cwd:             raw.Cwd,
			WorkspaceRoot:   raw.WorkspaceRoot,
		}
		if raw.StartedAt != nil {
			if startedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*raw.StartedAt)); err == nil {
				entry.StartedAt = &startedAt
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func matchClineHistoryCandidate(entries []clineHistoryEntry, match clineHistoryMatch) string {
	if match.DedicatedHubPID <= 0 || match.CanonicalWorkDir == "" || match.CmdStartWallMs <= 0 || match.FirstProtocolTimestampMs < match.CmdStartWallMs {
		return ""
	}
	var candidate string
	for _, entry := range entries {
		id := strings.TrimSpace(entry.SessionID)
		if id == "" {
			continue
		}
		if _, existed := match.PreRunSessionIDs[id]; existed {
			continue
		}
		if entry.PID != match.DedicatedHubPID || entry.IsSubagent == nil || *entry.IsSubagent || strings.TrimSpace(entry.ParentSessionID) != "" {
			continue
		}
		if entry.Source != nil && !strings.EqualFold(strings.TrimSpace(*entry.Source), "cli") {
			continue
		}
		if entry.Interactive != nil && *entry.Interactive {
			continue
		}
		if !sameCanonicalClinePath(entry.Cwd, match.CanonicalWorkDir) && !sameCanonicalClinePath(entry.WorkspaceRoot, match.CanonicalWorkDir) {
			continue
		}
		idTimestamp, ok := parseClineSessionIDTimestamp(id)
		if !ok || idTimestamp < match.CmdStartWallMs || idTimestamp > match.FirstProtocolTimestampMs {
			continue
		}
		if entry.StartedAt == nil {
			continue
		}
		startedAt := entry.StartedAt.UnixMilli()
		if startedAt < match.CmdStartWallMs || startedAt > match.FirstProtocolTimestampMs {
			continue
		}
		if candidate != "" {
			return ""
		}
		candidate = id
	}
	return candidate
}

func parseClineSessionIDTimestamp(id string) (int64, bool) {
	prefix, suffix, ok := strings.Cut(strings.TrimSpace(id), "_")
	if !ok || prefix == "" || suffix == "" || len(prefix) != 13 {
		return 0, false
	}
	for _, ch := range prefix {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	ms, err := strconv.ParseInt(prefix, 10, 64)
	return ms, err == nil && ms > 0
}

func canonicalClineWorkDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

func sameCanonicalClinePath(path, canonical string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	canonical = filepath.Clean(strings.TrimSpace(canonical))
	if path == "." || canonical == "." {
		return false
	}
	if path == canonical {
		return true
	}
	left, leftErr := os.Stat(path)
	right, rightErr := os.Stat(canonical)
	return leftErr == nil && rightErr == nil && os.SameFile(left, right)
}
