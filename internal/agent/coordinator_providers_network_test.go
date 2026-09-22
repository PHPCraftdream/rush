package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/log"
	"github.com/stretchr/testify/require"
)

// probeTargetBody is the fixed payload the plain target double serves;
// matching it proves the response really traversed the whole chain.
const probeTargetBody = "hello from target"

// probeChatCompletionBody is the fake openai-compat completion served by
// the end-to-end target server in test C.
const probeChatCompletionBody = `{"id":"c1","object":"chat.completion","created":1,"model":"probe-model","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// newNetworkTestStore builds a coordinator config store whose Options
// are exactly opts (Debug:false with no network block when opts is
// nil), so each test controls Debug and the global network settings.
func newNetworkTestStore(t *testing.T, opts *config.Options) *config.ConfigStore {
	t.Helper()
	if opts == nil {
		opts = &config.Options{Debug: false}
	}
	return config.NewLibraryStore(&config.Config{Options: opts}, t.TempDir())
}

// hostOnlyOfAuthority strips a trailing :port from an authority,
// returning the input unchanged when it carries no port.
func hostOnlyOfAuthority(authority string) string {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		return authority
	}
	return host
}

// hostPortOfURL parses rawURL and returns its host:port authority.
func hostPortOfURL(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}

// targetDouble is a plain loopback HTTP server that counts hits under a
// mutex and answers every request with a fixed status, content type and
// body.
type targetDouble struct {
	ts          *httptest.Server
	mu          sync.Mutex
	hits        int
	status      int
	contentType string
	body        string
}

// startTargetDouble starts the target double and registers its cleanup.
func startTargetDouble(t *testing.T, status int, contentType, body string) *targetDouble {
	t.Helper()
	td := &targetDouble{status: status, contentType: contentType, body: body}
	td.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		td.mu.Lock()
		td.hits++
		td.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(td.ts.Close)
	return td
}

// hitCount returns a snapshot of the recorded hit count.
func (td *targetDouble) hitCount() int {
	td.mu.Lock()
	defer td.mu.Unlock()
	return td.hits
}

// proxyDouble is a minimal local HTTP forward proxy: it records the
// authority of every request it sees and "resolves" non-IP-literal
// hosts proxy-side by forwarding them to the fixed fallback target, so
// a .invalid hostname can only succeed by traversing the proxy.
type proxyDouble struct {
	ts          *httptest.Server
	fallback    string
	mu          sync.Mutex
	authorities []string
}

// startProxyDouble starts the proxy double and registers its cleanup.
func startProxyDouble(t *testing.T, fallbackTarget string) *proxyDouble {
	t.Helper()
	p := &proxyDouble{fallback: fallbackTarget}
	p.ts = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.ts.Close)
	return p
}

// URL returns the proxy's http://127.0.0.1:port base URL.
func (p *proxyDouble) URL() string { return p.ts.URL }

// seen returns a snapshot of the recorded authorities in arrival order.
func (p *proxyDouble) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.authorities...)
}

// record appends one authority under the mutex.
func (p *proxyDouble) record(authority string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorities = append(p.authorities, authority)
}

// proxyHopByHopHeaders are stripped from relayed proxy responses.
var proxyHopByHopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive"}

// handle records the request authority and relays the absolute-form
// proxy request: IP-literal hosts are dialed as-is, names go to the
// fixed fallback target.
func (p *proxyDouble) handle(w http.ResponseWriter, r *http.Request) {
	p.record(r.Host)
	// Clone shares Body; RoundTrip consumes it exactly once.
	out := r.Clone(r.Context())
	// Server-received requests carry RequestURI, which client
	// transports reject; clear it before re-sending.
	out.RequestURI = ""
	if net.ParseIP(hostOnlyOfAuthority(out.URL.Host)) == nil {
		out.URL.Host = p.fallback
	}
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, hop := range proxyHopByHopHeaders {
		resp.Header.Del(hop)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func TestResolveProviderHTTPClient(t *testing.T) {
	t.Parallel()

	t.Run("nothing configured returns nil client", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{ID: "probe"})
		require.NoError(t, err)
		require.Nil(t, client)
	})

	t.Run("proxy only builds http proxy transport", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{Proxy: "http://127.0.0.1:1"},
		})
		require.NoError(t, err)
		require.NotNil(t, client)
		t.Cleanup(client.CloseIdleConnections)
		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, tr.Proxy)
		// R6-1 (round-7 audit): the proxy-only HTTP transport now carries
		// a bounded DialContext for the initial TCP connect to the proxy
		// itself -- stdlib's zero-value dialer used to leave that dial
		// unbounded. nil was the pre-fix expectation.
		require.NotNil(t, tr.DialContext)
	})

	t.Run("doh only builds resolver transport", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{DoHURL: "https://cloudflare-dns.com/dns-query"},
		})
		require.NoError(t, err)
		require.NotNil(t, client)
		t.Cleanup(client.CloseIdleConnections)
		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, tr.DialContext)
		require.Nil(t, tr.Proxy)
	})

	t.Run("debug only wraps default transport", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, &config.Options{Debug: true})}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{ID: "probe"})
		require.NoError(t, err)
		require.NotNil(t, client)
		t.Cleanup(client.CloseIdleConnections)
		retry, ok := client.Transport.(*log.RetryTransport)
		require.True(t, ok)
		logger, ok := retry.Transport.(*log.HTTPRoundTripLogger)
		require.True(t, ok)
		require.Equal(t, http.DefaultTransport, logger.Transport)
	})

	t.Run("debug and network compose into one chain", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, &config.Options{Debug: true})}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{Proxy: "http://127.0.0.1:1"},
		})
		require.NoError(t, err)
		require.NotNil(t, client)
		t.Cleanup(client.CloseIdleConnections)
		retry, ok := client.Transport.(*log.RetryTransport)
		require.True(t, ok)
		logger, ok := retry.Transport.(*log.HTTPRoundTripLogger)
		require.True(t, ok)
		tr, ok := logger.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, tr.Proxy)
	})

	t.Run("provider fields cascade per field over globals", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, &config.Options{
			Network: &config.NetworkConfig{DNSServer: "1.1.1.1"},
		})}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{Proxy: "http://127.0.0.1:1"},
		})
		require.NoError(t, err)
		require.NotNil(t, client)
		t.Cleanup(client.CloseIdleConnections)
		// Resolver active plus proxy set means tunneled-dial mode: a
		// struct-level override would produce the opposite shape.
		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, tr.DialContext)
		require.Nil(t, tr.Proxy)
	})

	t.Run("malformed proxy config is a wrapped error", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{Proxy: "ftp://example.com:21"},
		})
		require.Error(t, err)
		require.Nil(t, client)
		require.Contains(t, err.Error(), "probe")
	})

	t.Run("malformed doh url is a wrapped error", func(t *testing.T) {
		t.Parallel()
		coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{DoHURL: "https://[::1"},
		})
		require.Error(t, err)
		require.Nil(t, client)
		require.Contains(t, err.Error(), "probe")
	})
}

// TestRefreshOAuth2TokenRefusesOnNetworkClientBuildFailure proves the R5-2
// residual fix (2026-09-22 round-8 audit): when the provider's configured
// network policy fails to build, refreshOAuth2Token must return an error
// instead of silently falling back to the default-route client. inference
// was already built from a prior working snapshot; if the network config
// changes to something invalid before the next 401 refresh, silently
// exchanging the token over the default route would send it over a
// transport the operator never authorized for this provider -- the same
// class of policy violation build-failure already causes for inference
// requests via resolveProviderHTTPClient's own wrapped error.
func TestRefreshOAuth2TokenRefusesOnNetworkClientBuildFailure(t *testing.T) {
	t.Parallel()

	coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
	providerCfg := config.ProviderConfig{
		ID: "probe",
		// An unsupported proxy scheme makes the underlying
		// nettransport.BuildHTTPClient fail deterministically (see
		// resolveConfig's scheme switch) -- the same fixture the
		// "malformed proxy config is a wrapped error" subtest above uses.
		Network: &config.NetworkConfig{Proxy: "ftp://example.com:21"},
	}

	err := coord.refreshOAuth2Token(t.Context(), providerCfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolve provider network client")
	require.Contains(t, err.Error(), "probe")
}

func TestResolveProviderHTTPClientComposedRequest(t *testing.T) {
	t.Parallel()

	t.Run("network only request traverses the proxy", func(t *testing.T) {
		t.Parallel()
		target := startTargetDouble(t, http.StatusOK, "text/plain", probeTargetBody)
		proxy := startProxyDouble(t, hostPortOfURL(t, target.ts.URL))
		coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{Proxy: proxy.URL()},
		})
		require.NoError(t, err)
		require.NotNil(t, client)
		client.Timeout = 10 * time.Second
		t.Cleanup(client.CloseIdleConnections)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.ts.URL, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, probeTargetBody, string(body))

		require.Contains(t, proxy.seen(), hostPortOfURL(t, target.ts.URL))
		require.GreaterOrEqual(t, target.hitCount(), 1)
	})

	t.Run("debug and network compose over the proxy", func(t *testing.T) {
		t.Parallel()
		target := startTargetDouble(t, http.StatusOK, "text/plain", probeTargetBody)
		proxy := startProxyDouble(t, hostPortOfURL(t, target.ts.URL))
		coord := &coordinator{cfg: newNetworkTestStore(t, &config.Options{Debug: true})}
		client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{Proxy: proxy.URL()},
		})
		require.NoError(t, err)
		require.NotNil(t, client)
		_, ok := client.Transport.(*log.RetryTransport)
		require.True(t, ok)
		client.Timeout = 10 * time.Second
		t.Cleanup(client.CloseIdleConnections)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.ts.URL, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, probeTargetBody, string(body))

		require.Contains(t, proxy.seen(), hostPortOfURL(t, target.ts.URL))
		require.GreaterOrEqual(t, target.hitCount(), 1)
	})
}

func TestBuildProviderRoutesThroughConfiguredProxy(t *testing.T) {
	t.Parallel()
	target := startTargetDouble(t, http.StatusOK, "application/json", probeChatCompletionBody)
	proxy := startProxyDouble(t, hostPortOfURL(t, target.ts.URL))

	store := newNetworkTestStore(t, nil)
	providerCfg := config.ProviderConfig{
		ID:      "probe-provider",
		Type:    openaicompat.Name,
		APIKey:  "test-key",
		BaseURL: "http://probe.invalid/v1",
		Network: &config.NetworkConfig{Proxy: proxy.URL()},
	}
	coord := &coordinator{cfg: store}
	provider, err := coord.buildProvider(coord.cfg.Config(), providerCfg, config.SelectedModel{}, false)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lm, err := provider.LanguageModel(ctx, "probe-model")
	require.NoError(t, err)
	_, err = lm.Generate(ctx, fantasy.Call{})
	require.NoError(t, err)

	// probe.invalid cannot resolve in DNS, so the request can only
	// succeed by traversing the proxy to the fallback target.
	authorities := proxy.seen()
	var sawProbeHost bool
	for _, authority := range authorities {
		if hostOnlyOfAuthority(authority) == "probe.invalid" {
			sawProbeHost = true
			break
		}
	}
	require.True(t, sawProbeHost, "proxy saw authorities %v", authorities)
	require.GreaterOrEqual(t, target.hitCount(), 1)
}

func TestRedactNetworkURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "userinfo stripped scheme and host kept",
			in:   `unsupported proxy scheme: "http://user:secret@proxy.example.com:8080"`,
			want: `unsupported proxy scheme: "http://proxy.example.com:8080"`,
		},
		{
			name: "query dropped",
			in:   "DoH lookup failed for https://dns.example.com/query?token=s3cret&x=1 endpoint",
			want: "DoH lookup failed for https://dns.example.com/query endpoint",
		},
		{
			name: "userinfo and query together keep path",
			in:   "dial failed via http://alice:hunter2@10.0.0.1:9/path/here?api-key=k",
			want: "dial failed via http://10.0.0.1:9/path/here",
		},
		{
			name: "trailing sentence punctuation survives redaction",
			in:   "connection refused (http://user:secret@proxy.example.com:8080).",
			want: "connection refused (http://proxy.example.com:8080).",
		},
		{
			name: "ipv6 host kept",
			in:   "connect http://user:pw@[::1]:8080/x?q=1 refused",
			want: "connect http://[::1]:8080/x refused",
		},
		{
			name: "text without urls untouched",
			in:   "plain failure with no url",
			want: "plain failure with no url",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, redactNetworkURLs(tc.in))
		})
	}
}

func TestRedactNetworkErrorPreservesUnwrap(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("underlying: http://user:secret@host:1/x?token=k")
	wrapped := redactNetworkError(sentinel)

	require.ErrorIs(t, wrapped, sentinel)
	require.NotContains(t, wrapped.Error(), "user:secret")
	require.NotContains(t, wrapped.Error(), "token=k")
	require.Contains(t, wrapped.Error(), "http://host:1/x")
}
