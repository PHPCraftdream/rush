package modelkit

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/diag/version"
)

// OpenCodeSessionHeader is the header the OpenCode Go and Zen gateways require
// on every inference request: a stable id per conversation, used for routing
// and prompt caching. Requests without it are rejected with a 400.
const OpenCodeSessionHeader = "x-opencode-session"

// IsOpenCodeProvider reports whether the provider id is an OpenCode gateway
// that requires OpenCodeSessionHeader.
func IsOpenCodeProvider(providerID string) bool {
	return providerID == string(catwalk.InferenceProviderOpenCodeGo) ||
		providerID == string(catwalk.InferenceProviderOpenCodeZen)
}

var (
	processSessionOnce sync.Once
	processSessionID   string
)

// OpenCodeProcessSessionID is the session id used for requests that carry no
// conversation (ping, probes): random, fixed for the life of the process.
func OpenCodeProcessSessionID() string {
	processSessionOnce.Do(func() {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			processSessionID = "rush-process"
			return
		}
		processSessionID = "rush-" + hex.EncodeToString(b[:])
	})
	return processSessionID
}

// WithOpenCodeSessionHeader adds the process-wide session header to a static
// header map for callers that cannot attach a per-request transport (ping).
// A value the user configured is kept.
func WithOpenCodeSessionHeader(providerID string, headers map[string]string) map[string]string {
	if !IsOpenCodeProvider(providerID) {
		return headers
	}
	for k := range headers {
		if strings.EqualFold(k, OpenCodeSessionHeader) {
			return headers
		}
	}
	if headers == nil {
		headers = make(map[string]string, 1)
	}
	headers[OpenCodeSessionHeader] = OpenCodeProcessSessionID()
	return headers
}

// WrapOpenCodeHTTPClient makes every request of an OpenCode gateway provider
// carry OpenCodeSessionHeader: the conversation's session id from the request
// context, else the process session id. A header the user set through
// extra_headers is kept. The gateway also asks clients to identify with their
// own user agent, so an SDK-default agent string is replaced by "rush/<version>".
// Other providers get the client back unchanged.
func WrapOpenCodeHTTPClient(providerID string, client *http.Client) *http.Client {
	if !IsOpenCodeProvider(providerID) {
		return client
	}
	wrapped := &http.Client{}
	base := http.DefaultTransport
	if client != nil {
		*wrapped = *client
		if client.Transport != nil {
			base = client.Transport
		}
	}
	wrapped.Transport = openCodeSessionTransport{base: base}
	return wrapped
}

type openCodeSessionTransport struct{ base http.RoundTripper }

func (t openCodeSessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	if out.Header.Get(OpenCodeSessionHeader) == "" {
		id := tools.GetSessionFromContext(req.Context())
		if id == "" {
			id = OpenCodeProcessSessionID()
		}
		out.Header.Set(OpenCodeSessionHeader, id)
	}
	if isSDKDefaultUserAgent(out.Header.Get("User-Agent")) {
		out.Header.Set("User-Agent", "rush/"+version.Version)
	}
	return t.base.RoundTrip(out)
}

// isSDKDefaultUserAgent matches the generic strings the provider SDKs and the
// Go HTTP stack send when nothing sets an agent.
func isSDKDefaultUserAgent(ua string) bool {
	if ua == "" {
		return true
	}
	lower := strings.ToLower(ua)
	for _, prefix := range []string{"openai/", "anthropic/", "go-http-client", "fantasy"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}
