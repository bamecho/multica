//go:build darwin

package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func captureClineProcessIdentity(pid int) (clineProcessIdentity, error) {
	if pid <= 0 {
		return clineProcessIdentity{}, fmt.Errorf("invalid cline Hub PID %d", pid)
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return clineProcessIdentity{}, os.ErrNotExist
		}
		return clineProcessIdentity{}, fmt.Errorf("read cline Hub process info: %w", err)
	}
	commandBytes := make([]byte, 0, len(process.Proc.P_comm))
	for _, value := range process.Proc.P_comm {
		if value == 0 {
			break
		}
		commandBytes = append(commandBytes, byte(value))
	}
	command := strings.TrimSpace(string(commandBytes))
	if command == "" {
		return clineProcessIdentity{}, fmt.Errorf("cline Hub process info has no command identity")
	}
	started := process.Proc.P_starttime
	return clineProcessIdentity{
		PID:        pid,
		StartToken: fmt.Sprintf("darwin:%d:%d", started.Sec, started.Usec),
		Executable: command,
	}, nil
}
