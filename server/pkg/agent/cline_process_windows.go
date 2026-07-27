//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func captureClineProcessIdentity(pid int) (clineProcessIdentity, error) {
	if pid <= 0 {
		return clineProcessIdentity{}, fmt.Errorf("invalid cline Hub PID %d", pid)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return clineProcessIdentity{}, os.ErrNotExist
		}
		return clineProcessIdentity{}, fmt.Errorf("open cline Hub process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var createdAt, exitAt, kernelTime, userTime windows.Filetime
	if err := windows.GetProcessTimes(handle, &createdAt, &exitAt, &kernelTime, &userTime); err != nil {
		return clineProcessIdentity{}, fmt.Errorf("read cline Hub process creation time: %w", err)
	}
	path := make([]uint16, 32768)
	pathLen := uint32(len(path))
	if err := windows.QueryFullProcessImageName(handle, 0, &path[0], &pathLen); err != nil {
		return clineProcessIdentity{}, fmt.Errorf("read cline Hub executable: %w", err)
	}
	return clineProcessIdentity{
		PID:        pid,
		StartToken: fmt.Sprintf("windows:%d", createdAt.Nanoseconds()),
		Executable: filepath.Clean(windows.UTF16ToString(path[:pathLen])),
	}, nil
}
