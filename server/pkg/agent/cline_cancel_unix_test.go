//go:build !windows

package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClineResumeCancellationTerminatesOnlyTaskProcessGroup(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	childPIDPath := filepath.Join(tempDir, "child.pid")
	writeTestExecutable(t, fakePath, []byte(`#!/bin/sh
cat >/dev/null
sleep 60 &
child=$!
printf '%s' "$child" > "$CLINE_CHILD_PID_FILE"
printf '%s\n' '{"type":"agent_event","event":{"type":"iteration_start"}}'
wait "$child"
`))
	backend, err := New("cline", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env:            map[string]string{"CLINE_CHILD_PID_FILE": childPIDPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := backend.Execute(ctx, "prompt", ExecOptions{ResumeSessionID: "1784085932547_prior"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}

	var childPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(childPIDPath)
		if readErr == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				cancel()
				t.Fatalf("parse child pid: %v", err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		cancel()
		t.Fatal("fake Cline child did not start")
	}

	cancel()
	for range session.Messages {
	}
	select {
	case result := <-session.Result:
		if result.Status != "aborted" {
			t.Fatalf("status = %q, want aborted", result.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Cline result did not arrive after cancellation")
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task child process %d survived resume cancellation", childPID)
}

func TestClineFreshCancellationStopsDedicatedHubBeforeTaskProcessGroup(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	fakePath := filepath.Join(tempDir, "cline")
	hubStoppedPath := filepath.Join(tempDir, "hub.stopped")
	terminationOrderPath := filepath.Join(tempDir, "termination.order")
	sessionTimestamp := time.Now().Add(2 * time.Second).UnixMilli()
	protocolTimestamp := sessionTimestamp + 1000
	wantSessionID := strconv.FormatInt(sessionTimestamp, 10) + "_cancel"
	writeTestExecutable(t, fakePath, []byte(`#!/bin/sh
trap 'if [ -f "$CLINE_HUB_STOPPED_FILE" ]; then printf yes > "$CLINE_TERMINATION_ORDER_FILE"; else printf no > "$CLINE_TERMINATION_ORDER_FILE"; fi; exit 0' TERM
cat >/dev/null
printf '{"type":"agent_event","ts":%s,"event":{"type":"iteration_start"}}\n' "$CLINE_PROTOCOL_TIMESTAMP"
while :; do sleep 1; done
`))
	backend, err := New("cline", Config{
		ExecutablePath: fakePath,
		Logger:         slog.Default(),
		Env: map[string]string{
			"TMPDIR":                       tempDir,
			"CLINE_HUB_STOPPED_FILE":       hubStoppedPath,
			"CLINE_TERMINATION_ORDER_FILE": terminationOrderPath,
			"CLINE_PROTOCOL_TIMESTAMP":     strconv.FormatInt(protocolTimestamp, 10),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cline := backend.(*clineBackend)
	cline.startFreshHub = func(_ context.Context, _ string, _ string, extra map[string]string, _ string) (*clineHub, error) {
		record := clineHubRecord{Host: "127.0.0.1", Port: 32145, PID: 4242}
		return &clineHub{
			discovery: record,
			env:       buildClineFreshHubEnv(extra, filepath.Join(tempDir, "production.json"), record),
			stopFn: func(context.Context) error {
				return os.WriteFile(hubStoppedPath, []byte("stopped"), 0o600)
			},
		}, nil
	}
	earlyHistoryStarted := make(chan struct{})
	historyCalls := 0
	cline.runFreshHistory = func(ctx context.Context, _ string, _ string, _ []string) ([]clineHistoryEntry, error) {
		historyCalls++
		switch historyCalls {
		case 1:
			return nil, nil
		case 2:
			close(earlyHistoryStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if _, err := os.Stat(hubStoppedPath); err == nil {
			return nil, errors.New("final history lookup ran after hub stop")
		}
		cli := "cli"
		falseValue := false
		startedAt := time.UnixMilli(sessionTimestamp)
		return []clineHistoryEntry{{
			SessionID:   wantSessionID,
			PID:         4242,
			Source:      &cli,
			Interactive: &falseValue,
			IsSubagent:  &falseValue,
			Cwd:         tempDir,
			StartedAt:   &startedAt,
		}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Cwd: tempDir})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-earlyHistoryStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("early history lookup did not start")
	}
	cancel()
	for range session.Messages {
	}
	select {
	case result := <-session.Result:
		if result.Status != "aborted" {
			t.Fatalf("status = %q, want aborted", result.Status)
		}
		if result.SessionID != wantSessionID {
			t.Fatalf("SessionID = %q, want %q from pre-stop final lookup", result.SessionID, wantSessionID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Cline result did not arrive after cancellation")
	}
	raw, err := os.ReadFile(terminationOrderPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "yes" {
		t.Fatalf("task process observed hub stop first = %q", raw)
	}
}
