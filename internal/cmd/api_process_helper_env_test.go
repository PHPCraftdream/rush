package cmd

import (
	"os"
	"testing"
)

// The API process fixtures must never let a command reach the network for the
// provider catalogue: that stalled TestAPICLIRunInterruptCancelsRemoteRunAndExits130
// for ~20 s on a cold isolated data dir.
func TestAPIProcessFixtureForbidsProviderNetworkFetch(t *testing.T) {
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "")
	newAPIProcessFixture(t)
	if got := os.Getenv("RUSH_PROVIDER_CACHE_ONLY"); got != "1" {
		t.Fatalf("RUSH_PROVIDER_CACHE_ONLY = %q, want 1", got)
	}
}
