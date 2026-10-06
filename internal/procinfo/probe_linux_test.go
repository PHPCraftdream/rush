//go:build linux

package procinfo

import "testing"

func TestProbeTriState(t *testing.T) {
	alive, token, known := Probe(0)
	if alive || token != "" || !known {
		t.Fatalf("invalid pid: %v %q %v", alive, token, known)
	}
	alive, token, known = Probe(2147483647)
	if alive || token != "" || !known {
		t.Fatalf("definitely-dead pid: %v %q %v", alive, token, known)
	}
	// High unmapped pid: ESRCH => known-dead.
	alive, token, known = Probe(1 << 30)
	if alive || token != "" || !known {
		t.Fatalf("high unmapped pid: %v %q %v", alive, token, known)
	}
}
