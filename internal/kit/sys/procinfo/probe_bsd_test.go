//go:build darwin || freebsd || openbsd || netbsd

package procinfo

import (
	"testing"
)

func TestProbeInvalidPIDKnownDead(t *testing.T) {
	alive, token, known := Probe(-1)
	if alive || token != "" || !known {
		t.Fatalf("%v %q %v", alive, token, known)
	}
}
