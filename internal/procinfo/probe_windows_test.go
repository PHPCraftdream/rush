//go:build windows

package procinfo

import "testing"

func TestAccessDeniedProbeRemainsUnknown(t *testing.T) {
	alive, token, known := Probe(4)
	if !alive || token != "" || known {
		t.Fatalf("system pid should be conservatively unknown: %v %q %v", alive, token, known)
	}
}
