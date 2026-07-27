//go:build linux

package agent

import (
	"os"
	"testing"
)

func TestClineProcessIdentityDetectsTokenChange(t *testing.T) {
	t.Parallel()
	identity, err := captureClineProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if identity.StartToken == "" || identity.Executable == "" || !sameClineProcessIdentity(identity) {
		t.Fatalf("identity = %+v", identity)
	}
	identity.StartToken += "-reused"
	if sameClineProcessIdentity(identity) {
		t.Fatal("changed process start token was accepted")
	}
}
