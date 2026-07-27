//go:build !linux && !darwin && !windows

package agent

import "fmt"

func captureClineProcessIdentity(pid int) (clineProcessIdentity, error) {
	return clineProcessIdentity{}, fmt.Errorf("cline Hub process identity is unsupported on this platform")
}
