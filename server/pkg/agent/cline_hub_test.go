package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCreateClineHubRuntimeIsScopedAndUnique(t *testing.T) {
	t.Parallel()
	taskTemp := t.TempDir()
	first, err := createClineHubRuntime(taskTemp)
	if err != nil {
		t.Fatal(err)
	}
	second, err := createClineHubRuntime(taskTemp)
	if err != nil {
		t.Fatal(err)
	}
	if first.RuntimeDir == second.RuntimeDir || first.DiscoveryPath == second.DiscoveryPath {
		t.Fatalf("fresh runs reused runtime paths: %+v %+v", first, second)
	}
	if filepath.Dir(filepath.Dir(first.RuntimeDir)) != filepath.Clean(taskTemp) || filepath.Base(first.DiscoveryPath) != "production.json" {
		t.Fatalf("runtime escaped task TMPDIR: %+v", first)
	}
}

func TestCreateClineHubRuntimeRejectsSymlinkRoot(t *testing.T) {
	t.Parallel()
	taskTemp := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(taskTemp, "cline-hub")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := createClineHubRuntime(taskTemp); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink root error = %v", err)
	}
}

func TestReadAndValidateClineHubIdentity(t *testing.T) {
	t.Parallel()
	runtimeInfo, err := createClineHubRuntime(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	launchStartedAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	raw := clineHubJSONRecord{
		HubID:           "hub-run-1",
		ProtocolVersion: "v1",
		AuthToken:       "secret-not-for-logs",
		Host:            "127.0.0.1",
		Port:            32145,
		URL:             "ws://127.0.0.1:32145/hub",
		PID:             4242,
		StartedAt:       launchStartedAt.Add(time.Millisecond).Format(time.RFC3339Nano),
		StartToken:      "process-start-1",
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeInfo.DiscoveryPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	discovery, err := readClineHubRecord(runtimeInfo.DiscoveryPath, true)
	if err != nil {
		t.Fatal(err)
	}
	raw.AuthToken = ""
	statusData, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	status, err := parseClineHubRecord(statusData, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClineHubIdentity(discovery, status, launchStartedAt); err != nil {
		t.Fatal(err)
	}

	status.PID++
	if err := validateClineHubIdentity(discovery, status, launchStartedAt); err == nil {
		t.Fatal("mismatched authenticated status was accepted")
	}
}

func TestValidateClineHubIdentityAcceptsPublicRecordWithoutStartToken(t *testing.T) {
	t.Parallel()
	startedAt := time.Now().Add(time.Millisecond)
	discovery := clineHubRecord{
		HubID:           "hub-public",
		ProtocolVersion: "v1",
		Host:            "127.0.0.1",
		Port:            32145,
		URL:             "ws://127.0.0.1:32145/hub",
		PID:             4242,
		StartedAt:       startedAt,
	}
	if err := validateClineHubIdentity(discovery, discovery, startedAt.Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
}

func TestStartClineHubUsesPublicPortZeroContract(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake executable is Unix-only")
	}
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	argsPath := filepath.Join(tempDir, "hub-args")
	writeTestExecutable(t, fakePath, []byte(`#!/bin/sh
printf '%s\n' "$*" > "$CLINE_TEST_HUB_ARGS"
printf '{"hubId":"hub-public","protocolVersion":"v1","authToken":"test-token","host":"127.0.0.1","port":32145,"url":"ws://127.0.0.1:32145/hub","pid":%s,"startedAt":"%s"}' "$CLINE_TEST_PID" "$CLINE_TEST_STARTED_AT" > "$CLINE_HUB_DISCOVERY_PATH"
chmod 600 "$CLINE_HUB_DISCOVERY_PATH"
printf '%s\n' 'ws://127.0.0.1:32145/hub'
`))

	client := &http.Client{Transport: clineRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/status" || request.Header.Get("Authorization") != "Bearer test-token" {
			return clineTestHTTPResponse(http.StatusUnauthorized, "unauthorized"), nil
		}
		raw, err := os.ReadFile(request.Context().Value(clineHubDiscoveryContextKey{}).(string))
		if err != nil {
			return nil, err
		}
		var status clineHubJSONRecord
		if err := json.Unmarshal(raw, &status); err != nil {
			return nil, err
		}
		status.AuthToken = ""
		body, err := json.Marshal(status)
		if err != nil {
			return nil, err
		}
		return clineTestHTTPResponse(http.StatusOK, string(body)), nil
	})}

	// The transport needs the private discovery path created inside start. Wrap
	// it after creation by locating the sole production.json under task TMPDIR.
	baseTransport := client.Transport
	client.Transport = clineRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		matches, err := filepath.Glob(filepath.Join(tempDir, "cline-hub", "*", "production.json"))
		if err != nil || len(matches) != 1 {
			return nil, fmt.Errorf("discovery matches = %v, error = %v", matches, err)
		}
		request = request.WithContext(context.WithValue(request.Context(), clineHubDiscoveryContextKey{}, matches[0]))
		return baseTransport.RoundTrip(request)
	})

	hub, err := startClineHubWithClient(context.Background(), fakePath, tempDir, map[string]string{
		"TMPDIR":                tempDir,
		"CLINE_TEST_PID":        strconv.Itoa(os.Getpid()),
		"CLINE_TEST_STARTED_AT": time.Now().Add(time.Second).Format(time.RFC3339Nano),
		"CLINE_TEST_HUB_ARGS":   argsPath,
	}, tempDir, client)
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(args)); got != "hub --host 127.0.0.1 --port 0 start" {
		t.Fatalf("hub args = %q", got)
	}
	if hub.discovery.StartToken != "" || hub.process.PID != os.Getpid() || hub.env == nil {
		t.Fatalf("public hub identity = %+v process = %+v", hub.discovery, hub.process)
	}
	hub.stopFn = func(context.Context) error { return os.RemoveAll(hub.runtime.RuntimeDir) }
	if err := hub.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartClineHubReportsStderr(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake executable is Unix-only")
	}
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nprintf 'hub failed visibly' >&2\nexit 1\n"))

	_, err := startClineHubWithClient(context.Background(), fakePath, tempDir, map[string]string{"TMPDIR": tempDir}, tempDir, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "hub failed visibly") {
		t.Fatalf("start error = %v", err)
	}
}

type clineHubDiscoveryContextKey struct{}

func TestClineRealHubLifecycle(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_CLINE_HUB_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_CLINE_HUB_SMOKE=1 to run")
	}
	execPath, err := exec.LookPath("cline")
	if err != nil {
		t.Skipf("cline not installed: %v", err)
	}
	taskTempDir := t.TempDir()
	hub, err := startClineHub(context.Background(), execPath, taskTempDir, map[string]string{"TMPDIR": taskTempDir}, taskTempDir)
	if err != nil {
		t.Fatal(err)
	}
	if hub.discovery.StartToken != "" {
		t.Logf("Cline discovery supplied start token %q", hub.discovery.StartToken)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), clineHubControlTimeout+clineHubExitTimeout+time.Second)
	defer cancel()
	if err := hub.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hub.runtime.RuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("runtime still exists after stop: %v", err)
	}
}

func TestClineRealConcurrentHubLifecycle(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_CLINE_HUB_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_CLINE_HUB_SMOKE=1 to run")
	}
	execPath, err := exec.LookPath("cline")
	if err != nil {
		t.Skipf("cline not installed: %v", err)
	}
	type startResult struct {
		hub *clineHub
		err error
	}
	results := make(chan startResult, 2)
	for range 2 {
		taskTempDir := t.TempDir()
		go func() {
			hub, startErr := startClineHub(context.Background(), execPath, taskTempDir, map[string]string{"TMPDIR": taskTempDir}, taskTempDir)
			results <- startResult{hub: hub, err: startErr}
		}()
	}
	hubs := make([]*clineHub, 0, 2)
	defer func() {
		for _, hub := range hubs {
			stopCtx, cancel := context.WithTimeout(context.Background(), clineHubControlTimeout+clineHubExitTimeout+time.Second)
			_ = hub.stop(stopCtx)
			cancel()
		}
	}()
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		hubs = append(hubs, result.hub)
	}
	if hubs[0].discovery.PID == hubs[1].discovery.PID || hubs[0].discovery.Port == hubs[1].discovery.Port || hubs[0].runtime.DiscoveryPath == hubs[1].runtime.DiscoveryPath {
		t.Fatalf("concurrent Hubs were not isolated: first=%+v second=%+v", hubs[0].discovery, hubs[1].discovery)
	}
}

func TestParseClineHubRecordRejectsUnsafeEndpoint(t *testing.T) {
	t.Parallel()
	base := clineHubJSONRecord{
		HubID:           "hub-run-1",
		ProtocolVersion: "v1",
		AuthToken:       "secret",
		Host:            "127.0.0.1",
		Port:            32145,
		URL:             "ws://127.0.0.1:32145/hub",
		PID:             4242,
		StartedAt:       "2026-07-25T12:00:00Z",
	}
	tests := map[string]func(*clineHubJSONRecord){
		"non-loopback": func(record *clineHubJSONRecord) {
			record.Host = "192.0.2.1"
			record.URL = "ws://192.0.2.1:32145/hub"
		},
		"host mismatch": func(record *clineHubJSONRecord) { record.URL = "ws://127.0.0.2:32145/hub" },
		"port mismatch": func(record *clineHubJSONRecord) { record.URL = "ws://127.0.0.1:32146/hub" },
		"credentials":   func(record *clineHubJSONRecord) { record.URL = "ws://user@127.0.0.1:32145/hub" },
		"query":         func(record *clineHubJSONRecord) { record.URL += "?token=bad" },
		"missing token": func(record *clineHubJSONRecord) { record.AuthToken = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := base
			mutate(&record)
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseClineHubRecord(data, true); err == nil {
				t.Fatal("unsafe record was accepted")
			}
		})
	}
}

func TestClineHubAuthenticatedStatusAndShutdown(t *testing.T) {
	t.Parallel()
	const token = "local-secret"
	launchStartedAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var statusRequests, shutdownRequests int
	status := clineHubJSONRecord{
		HubID:           "hub-run-1",
		ProtocolVersion: "v1",
		Host:            "127.0.0.1",
		Port:            32145,
		URL:             "ws://127.0.0.1:32145/hub",
		PID:             4242,
		StartedAt:       launchStartedAt.Add(time.Millisecond).Format(time.RFC3339Nano),
		StartToken:      "process-start-1",
	}
	client := &http.Client{Transport: clineRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			return clineTestHTTPResponse(http.StatusUnauthorized, "unauthorized"), nil
		}
		switch request.URL.Path {
		case "/status":
			if request.Method != http.MethodGet {
				t.Errorf("status method = %s", request.Method)
			}
			statusRequests++
			body, err := json.Marshal(status)
			if err != nil {
				return nil, err
			}
			return clineTestHTTPResponse(http.StatusOK, string(body)), nil
		case "/shutdown":
			if request.Method != http.MethodPost {
				t.Errorf("shutdown method = %s", request.Method)
			}
			shutdownRequests++
			return clineTestHTTPResponse(http.StatusNoContent, ""), nil
		default:
			return clineTestHTTPResponse(http.StatusNotFound, "not found"), nil
		}
	})}
	statusData, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	expectedStatus, err := parseClineHubRecord(statusData, false)
	if err != nil {
		t.Fatal(err)
	}
	discovery := expectedStatus
	discovery.AuthToken = token
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdownClineHubWithClient(ctx, discovery, expectedStatus, launchStartedAt, client); err != nil {
		t.Fatal(err)
	}
	if statusRequests != 1 || shutdownRequests != 1 {
		t.Fatalf("control requests: status=%d shutdown=%d", statusRequests, shutdownRequests)
	}

	status.PID++
	if err := shutdownClineHubWithClient(ctx, discovery, expectedStatus, launchStartedAt, client); err == nil {
		t.Fatal("shutdown accepted a changed Hub identity")
	}
	if shutdownRequests != 1 {
		t.Fatal("identity mismatch still sent shutdown")
	}
}

func TestClineHubConcurrentStopRunsCleanupOnce(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	hub := &clineHub{stopFn: func(context.Context) error {
		calls++
		close(entered)
		<-release
		return nil
	}}

	errs := make(chan error, 2)
	go func() { errs <- hub.stop(context.Background()) }()
	<-entered
	go func() { errs <- hub.stop(context.Background()) }()
	close(release)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", calls)
	}
}

func TestClineHubStopCanRetryAfterFailure(t *testing.T) {
	t.Parallel()
	attempts := 0
	hub := &clineHub{stopFn: func(context.Context) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("temporary shutdown failure")
		}
		return nil
	}}

	if err := hub.stop(context.Background()); err == nil {
		t.Fatal("first stop unexpectedly succeeded")
	}
	if err := hub.stop(context.Background()); err != nil {
		t.Fatalf("retry stop: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("cleanup attempts = %d, want 2", attempts)
	}
}

type clineRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip clineRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func clineTestHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestReadClineHubRecordRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "record.json")
	if err := os.WriteFile(recordPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "link.json")
	if err := os.Symlink(recordPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readClineHubRecord(linkPath, true); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestBuildClineFreshHubEnvReplacesManagedValues(t *testing.T) {
	t.Setenv("CLINE_HUB_HOST", "192.0.2.1")
	t.Setenv("CLINE_HUB_PORT", "1")
	t.Setenv("CLINE_HUB_DISCOVERY_PATH", "/shared.json")
	t.Setenv("CLINE_SESSION_BACKEND_MODE", "local")
	t.Setenv("CLINE_VCR", "1")
	record := clineHubRecord{Host: "127.0.0.1", Port: 32145}
	env := buildClineFreshHubEnv(map[string]string{
		"CLINE_HUB_PORT":             "2",
		"CLINE_SESSION_BACKEND_MODE": "file",
		"KEEP_ME":                    "yes",
	}, "/private/production.json", record)
	wants := map[string]string{
		"CLINE_HUB_HOST":             "127.0.0.1",
		"CLINE_HUB_PORT":             "32145",
		"CLINE_HUB_DISCOVERY_PATH":   "/private/production.json",
		"CLINE_SESSION_BACKEND_MODE": "hub",
		"KEEP_ME":                    "yes",
	}
	counts := map[string]int{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if want, ok := wants[key]; ok {
			counts[key]++
			if value != want {
				t.Errorf("%s = %q, want %q", key, value, want)
			}
		}
		if key == "CLINE_VCR" {
			t.Fatal("CLINE_VCR survived fresh environment sanitization")
		}
	}
	for key := range wants {
		if counts[key] != 1 {
			t.Errorf("%s count = %d, want 1", key, counts[key])
		}
	}
}

func TestRunClineHistoryUsesBoundedContract(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	argsPath := filepath.Join(tempDir, "args")
	writeTestExecutable(t, fakePath, []byte(`#!/bin/sh
printf '%s\n' "$*" > "$CLINE_HISTORY_ARGS"
printf '%s' '[{"sessionId":"1784999999999_root","source":"cli","pid":4242,"startedAt":"2026-07-25T12:00:00Z","interactive":false,"cwd":"/work","workspaceRoot":"/work","isSubagent":false}]'
`))
	env := append(buildEnv(nil), "CLINE_HISTORY_ARGS="+argsPath)
	entries, err := runClineHistory(context.Background(), fakePath, tempDir, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != "1784999999999_root" {
		t.Fatalf("history entries = %+v", entries)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "history --json --limit " + strconv.Itoa(clineHistoryLimit)
	if strings.TrimSpace(string(args)) != want {
		t.Fatalf("history args = %q, want %q", strings.TrimSpace(string(args)), want)
	}
	ids := clineRootSessionIDs(entries)
	if _, ok := ids["1784999999999_root"]; !ok {
		t.Fatalf("root snapshot = %+v", ids)
	}
}

func TestRunClineHistoryHonorsCallerDeadline(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nsleep 5\n"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := runClineHistory(ctx, fakePath, tempDir, buildEnv(nil)); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("history cancellation took %s", elapsed)
	}
}

func TestClineBoundedBufferRejectsOverflow(t *testing.T) {
	t.Parallel()
	buffer := &clineBoundedBuffer{maxBytes: 4}
	if _, err := buffer.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("5")); err == nil || !buffer.exceeded {
		t.Fatalf("overflow error = %v, exceeded = %v", err, buffer.exceeded)
	}
}

func TestParseClineHistoryJSONContract(t *testing.T) {
	t.Parallel()
	input := `[
		{"sessionId":"1784085932547_root","source":"cli","pid":4242,"startedAt":"2026-07-15T03:25:32.547Z","interactive":false,"cwd":"/work/project","workspaceRoot":"/work/project","isSubagent":false},
		{"sessionId":"bad-pid","pid":"4242","startedAt":"2026-07-15T03:25:32.547Z","isSubagent":false},
		{"sessionId":"bad-time","source":"cli","pid":4242,"startedAt":"not-a-time","interactive":false,"cwd":"/work/project","isSubagent":false}
	]`
	entries, err := parseClineHistoryJSON([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want malformed typed record skipped: %+v", len(entries), entries)
	}
	if entries[0].SessionID != "1784085932547_root" || entries[0].PID != 4242 || entries[0].StartedAt == nil || entries[0].IsSubagent == nil || *entries[0].IsSubagent {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[1].SessionID != "bad-time" || entries[1].StartedAt != nil {
		t.Fatalf("invalid time must remain ineligible: %+v", entries[1])
	}

	if _, err := parseClineHistoryJSON([]byte(`{"sessions":[]}`)); err == nil || !strings.Contains(err.Error(), "history array") {
		t.Fatalf("non-array error = %v", err)
	}
}

func TestParseClineSessionIDTimestamp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id   string
		want int64
		ok   bool
	}{
		{id: "1784085932547_muen6", want: 1784085932547, ok: true},
		{id: "178408593254_muen6"},
		{id: "17840859325470_muen6"},
		{id: "178408593254x_muen6"},
		{id: "1784085932547_"},
		{id: "conv_abc"},
	} {
		got, ok := parseClineSessionIDTimestamp(tc.id)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseClineSessionIDTimestamp(%q) = (%d, %v), want (%d, %v)", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMatchClineHistoryCandidateRequiresEveryIdentityField(t *testing.T) {
	t.Parallel()
	falseValue := false
	cli := "cli"
	start := int64(1784085932540)
	upper := int64(1784085932560)
	startedAt := time.UnixMilli(1784085932550)
	base := clineHistoryEntry{
		SessionID:   "1784085932547_muen6",
		PID:         4242,
		Source:      &cli,
		Interactive: &falseValue,
		IsSubagent:  &falseValue,
		Cwd:         "/work/project",
		StartedAt:   &startedAt,
	}
	match := clineHistoryMatch{
		PreRunSessionIDs:         map[string]struct{}{},
		DedicatedHubPID:          4242,
		CanonicalWorkDir:         "/work/project",
		CmdStartWallMs:           start,
		FirstProtocolTimestampMs: upper,
	}
	if got := matchClineHistoryCandidate([]clineHistoryEntry{base}, match); got != base.SessionID {
		t.Fatalf("valid candidate = %q", got)
	}

	tests := map[string]func(*clineHistoryEntry, *clineHistoryMatch){
		"preexisting": func(_ *clineHistoryEntry, m *clineHistoryMatch) { m.PreRunSessionIDs[base.SessionID] = struct{}{} },
		"pid":         func(e *clineHistoryEntry, _ *clineHistoryMatch) { e.PID++ },
		"source":      func(e *clineHistoryEntry, _ *clineHistoryMatch) { value := "sdk"; e.Source = &value },
		"interactive": func(e *clineHistoryEntry, _ *clineHistoryMatch) { value := true; e.Interactive = &value },
		"subagent":    func(e *clineHistoryEntry, _ *clineHistoryMatch) { value := true; e.IsSubagent = &value },
		"parent":      func(e *clineHistoryEntry, _ *clineHistoryMatch) { e.ParentSessionID = "parent" },
		"cwd":         func(e *clineHistoryEntry, _ *clineHistoryMatch) { e.Cwd = "/other" },
		"id before":   func(e *clineHistoryEntry, _ *clineHistoryMatch) { e.SessionID = "1784085932539_muen6" },
		"id after":    func(e *clineHistoryEntry, _ *clineHistoryMatch) { e.SessionID = "1784085932561_muen6" },
		"started before": func(e *clineHistoryEntry, _ *clineHistoryMatch) {
			value := time.UnixMilli(start - 1)
			e.StartedAt = &value
		},
		"started after": func(e *clineHistoryEntry, _ *clineHistoryMatch) {
			value := time.UnixMilli(upper + 1)
			e.StartedAt = &value
		},
		"started missing": func(e *clineHistoryEntry, _ *clineHistoryMatch) { e.StartedAt = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			entry := base
			candidateMatch := match
			candidateMatch.PreRunSessionIDs = map[string]struct{}{}
			mutate(&entry, &candidateMatch)
			if got := matchClineHistoryCandidate([]clineHistoryEntry{entry}, candidateMatch); got != "" {
				t.Fatalf("candidate = %q, want empty", got)
			}
		})
	}

	other := base
	other.SessionID = "1784085932548_other"
	if got := matchClineHistoryCandidate([]clineHistoryEntry{base, other}, match); got != "" {
		t.Fatalf("multiple candidates = %q, want empty", got)
	}
}

func TestClineSessionLookupCoordinatorPinsEarlyIDAndKeepsIt(t *testing.T) {
	t.Parallel()
	falseValue := false
	cli := "cli"
	startedAt := time.UnixMilli(1784085932547)
	base := clineHistoryEntry{
		SessionID:   "1784085932547_root",
		PID:         4242,
		Source:      &cli,
		Interactive: &falseValue,
		IsSubagent:  &falseValue,
		Cwd:         "/work/project",
		StartedAt:   &startedAt,
	}
	coordinator := newClineSessionLookupCoordinator(clineHistoryMatch{
		PreRunSessionIDs: map[string]struct{}{},
		DedicatedHubPID:  4242,
		CanonicalWorkDir: "/work/project",
		CmdStartWallMs:   1784085932540,
	})
	lookups := 0
	history := func(context.Context) ([]clineHistoryEntry, error) {
		lookups++
		if lookups == 1 {
			return nil, nil
		}
		return []clineHistoryEntry{base}, nil
	}
	found := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		coordinator.runEarly(context.Background(), history, []time.Duration{0, 0}, func(id string) { found <- id })
		close(done)
	}()
	coordinator.observe(clineProtocolObservation{FirstTimestampMs: 1784085932560})
	coordinator.observe(clineProtocolObservation{IterationStarted: true})
	select {
	case id := <-found:
		if id != base.SessionID {
			t.Fatalf("early ID = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("early lookup did not report an ID")
	}
	<-done
	if lookups != 2 || coordinator.sessionID() != base.SessionID {
		t.Fatalf("lookups=%d immutableID=%q", lookups, coordinator.sessionID())
	}

	later := base
	later.SessionID = "1784085932548_other"
	laterStartedAt := time.UnixMilli(1784085932548)
	later.StartedAt = &laterStartedAt
	got, err := coordinator.finalLookup(context.Background(), func(context.Context) ([]clineHistoryEntry, error) {
		return []clineHistoryEntry{later}, nil
	})
	if err == nil || got != base.SessionID || coordinator.sessionID() != base.SessionID {
		t.Fatalf("final lookup = (%q, %v), immutable=%q", got, err, coordinator.sessionID())
	}
}

func TestClineSessionLookupCoordinatorRequiresFrozenTimestamp(t *testing.T) {
	t.Parallel()
	coordinator := newClineSessionLookupCoordinator(clineHistoryMatch{
		PreRunSessionIDs: map[string]struct{}{},
		DedicatedHubPID:  4242,
		CanonicalWorkDir: "/work/project",
		CmdStartWallMs:   1784085932540,
	})
	called := false
	if id, err := coordinator.finalLookup(context.Background(), func(context.Context) ([]clineHistoryEntry, error) {
		called = true
		return nil, nil
	}); err != nil || id != "" || called {
		t.Fatalf("lookup without timestamp = (%q, %v), history called=%v", id, err, called)
	}
}

func TestCanonicalClineWorkDirResolvesSymlink(t *testing.T) {
	t.Parallel()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonical, err := canonicalClineWorkDir(link)
	if err != nil {
		t.Fatal(err)
	}
	if !sameCanonicalClinePath(real, canonical) || !sameCanonicalClinePath(link, canonical) {
		t.Fatalf("canonical path mismatch: real=%q link=%q canonical=%q", real, link, canonical)
	}
}
