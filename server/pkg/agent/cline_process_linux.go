//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func captureClineProcessIdentity(pid int) (clineProcessIdentity, error) {
	if pid <= 0 {
		return clineProcessIdentity{}, fmt.Errorf("invalid cline Hub PID %d", pid)
	}
	stat, err := os.ReadFile(filepath.Join("/proc", fmt.Sprintf("%d", pid), "stat"))
	if err != nil {
		return clineProcessIdentity{}, fmt.Errorf("read cline Hub process stat: %w", err)
	}
	// comm is parenthesized and may contain spaces or parentheses. Field 22
	// (starttime) is index 19 after the final closing parenthesis.
	endComm := strings.LastIndexByte(string(stat), ')')
	if endComm < 0 {
		return clineProcessIdentity{}, fmt.Errorf("invalid cline Hub process stat")
	}
	fields := strings.Fields(string(stat[endComm+1:]))
	if len(fields) <= 19 || fields[19] == "" {
		return clineProcessIdentity{}, fmt.Errorf("cline Hub process stat has no start token")
	}
	executable, err := os.Readlink(filepath.Join("/proc", fmt.Sprintf("%d", pid), "exe"))
	if err != nil {
		return clineProcessIdentity{}, fmt.Errorf("read cline Hub executable: %w", err)
	}
	return clineProcessIdentity{
		PID:        pid,
		StartToken: "linux:" + fields[19],
		Executable: filepath.Clean(executable),
	}, nil
}
