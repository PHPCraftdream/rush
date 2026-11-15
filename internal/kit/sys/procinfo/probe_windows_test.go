//go:build windows

package procinfo

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestAccessDeniedProbeRemainsUnknown(t *testing.T) {
	prev := openProcess
	openProcess = func(uint32, bool, uint32) (windows.Handle, error) {
		return 0, windows.ERROR_ACCESS_DENIED
	}
	t.Cleanup(func() { openProcess = prev })
	alive, token, known := Probe(4)
	if !alive || token != "" || known {
		t.Fatalf("access-denied pid should be conservatively unknown: %v %q %v", alive, token, known)
	}
}

// The System process is alive whether or not this runner may open it.
func TestSystemPidAlive(t *testing.T) {
	if !Alive(4) {
		t.Fatal("system pid 4 reported dead")
	}
}
