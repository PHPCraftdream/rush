package modelkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/diag/version"
	"github.com/stretchr/testify/require"
)

type seenRequest struct {
	session   string
	userAgent string
}

func openCodeProbe(t *testing.T) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenRequest{session: r.Header.Get(OpenCodeSessionHeader), userAgent: r.Header.Get("User-Agent")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func doOpenCodeRequest(t *testing.T, client *http.Client, url string, ctx context.Context, mutate func(*http.Request)) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	require.NoError(t, err)
	if mutate != nil {
		mutate(req)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

// REVERT-CHECK: dropping the wrapper (or the header) fails every case below.
func TestOpenCodeHTTPClientSendsConversationSession(t *testing.T) {
	for _, provider := range []string{"opencode-go", "opencode-zen"} {
		t.Run(provider, func(t *testing.T) {
			srv, seen := openCodeProbe(t)
			client := WrapOpenCodeHTTPClient(provider, srv.Client())
			ctxA := context.WithValue(t.Context(), tools.SessionIDContextKey, "session-a")
			ctxB := context.WithValue(t.Context(), tools.SessionIDContextKey, "session-b")
			doOpenCodeRequest(t, client, srv.URL, ctxA, nil)
			doOpenCodeRequest(t, client, srv.URL, ctxA, nil)
			doOpenCodeRequest(t, client, srv.URL, ctxB, nil)
			got := seen()
			require.Len(t, got, 3)
			require.Equal(t, "session-a", got[0].session, "a turn carries its conversation id")
			require.Equal(t, got[0].session, got[1].session, "the id is stable across the turns of one conversation")
			require.Equal(t, "session-b", got[2].session, "another conversation gets its own id")
		})
	}
}

func TestOpenCodeHTTPClientFallsBackToProcessSession(t *testing.T) {
	srv, seen := openCodeProbe(t)
	client := WrapOpenCodeHTTPClient("opencode-go", nil)
	doOpenCodeRequest(t, client, srv.URL, t.Context(), nil)
	doOpenCodeRequest(t, client, srv.URL, t.Context(), nil)
	got := seen()
	require.Len(t, got, 2)
	require.NotEmpty(t, got[0].session, "a request without a conversation (ping, probes) still carries the header")
	require.Equal(t, OpenCodeProcessSessionID(), got[0].session)
	require.Equal(t, got[0].session, got[1].session)
}

func TestOpenCodeHTTPClientKeepsUserHeaderAndAgent(t *testing.T) {
	srv, seen := openCodeProbe(t)
	client := WrapOpenCodeHTTPClient("opencode-go", srv.Client())
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session-a")
	doOpenCodeRequest(t, client, srv.URL, ctx, func(r *http.Request) {
		r.Header.Set(OpenCodeSessionHeader, "from-config")
		r.Header.Set("User-Agent", "my-agent/2.0")
	})
	doOpenCodeRequest(t, client, srv.URL, ctx, func(r *http.Request) {
		r.Header.Set("User-Agent", "OpenAI/Go 3.1.0")
	})
	got := seen()
	require.Len(t, got, 2)
	require.Equal(t, "from-config", got[0].session, "extra_headers wins over the generated id")
	require.Equal(t, "my-agent/2.0", got[0].userAgent, "a configured agent string is kept")
	require.Equal(t, "rush/"+version.Version, got[1].userAgent, "an SDK default agent string is replaced by rush's own")
}

func TestOpenCodeHTTPClientLeavesOtherProvidersAlone(t *testing.T) {
	srv, seen := openCodeProbe(t)
	base := srv.Client()
	require.Same(t, base, WrapOpenCodeHTTPClient("anthropic", base), "other providers get the very same client")
	doOpenCodeRequest(t, base, srv.URL, t.Context(), nil)
	require.Empty(t, seen()[0].session)
	require.Nil(t, WrapOpenCodeHTTPClient("openai", nil))
}

func TestWithOpenCodeSessionHeader(t *testing.T) {
	got := WithOpenCodeSessionHeader("opencode-go", map[string]string{"X-Other": "1"})
	require.Equal(t, OpenCodeProcessSessionID(), got[OpenCodeSessionHeader])
	require.Equal(t, "1", got["X-Other"])
	kept := WithOpenCodeSessionHeader("opencode-go", map[string]string{"X-OpenCode-Session": "mine"})
	require.Equal(t, "mine", kept["X-OpenCode-Session"], "a user value is kept, whatever its case")
	require.NotContains(t, kept, OpenCodeSessionHeader)
	require.Equal(t, map[string]string{"a": "b"}, WithOpenCodeSessionHeader("openai", map[string]string{"a": "b"}))
	require.Equal(t, OpenCodeProcessSessionID(), WithOpenCodeSessionHeader("opencode-zen", nil)[OpenCodeSessionHeader])
}
