package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// `providers update --all` lists the enabled providers once, then updates them
// one by one. Each successful write reloads the live config, and that reload
// re-discovers every provider that still has no models: one whose discovery
// fails at that moment is dropped from the live config. The command used to
// look every id up again and so reported `provider "x" not found` for a
// provider that is in the config file, hiding its real fetch error.
//
// Which provider is updated first depends on map order and on whether the host
// has CLI providers installed (their update never touches the network but still
// writes and reloads), so the test fixes the sequence by request count instead:
// requests 1-2 are the two startup discoveries, request 3 is the first update
// fetch (succeeds and triggers the reload), every later request fails.
func TestProvidersUpdateAllReportsFetchErrorOfProviderDroppedByReload(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n := requests.Add(1); n <= 3 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"model-` + string(rune('0'+n)) + `"}]}`))
			return
		}
		http.Error(w, "provider unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	fixture := `{"providers":{` +
		`"alpha":{"name":"Alpha","type":"openai-compat","base_url":"` + server.URL + `/a","models":[]},` +
		`"beta":{"name":"Beta","type":"openai-compat","base_url":"` + server.URL + `/b","models":[]}}}`

	_, _, _, err := runProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, fixture, "--all", "")

	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 502", "the failing provider must report its own fetch error")
	require.False(t, strings.Contains(err.Error(), "not found"),
		"a provider dropped from the live config by an earlier write's reload is still updated: %v", err)
}
