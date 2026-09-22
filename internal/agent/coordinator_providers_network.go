package agent

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/nettransport"
)

// resolveProviderHTTPClient builds the *http.Client this provider's
// outbound connections should use, composing the resolved proxy/DNS/DoH
// network config with the existing Options.Debug request/response
// logging. Returns nil when NEITHER is configured, so callers fall back
// to the provider SDK's own default client unchanged.
//
// cfg MUST be the SAME pinned *config.Config snapshot the caller used to
// resolve providerCfg (F6): this used to read the global network defaults
// via its own separate, later c.cfg.Config() call, so a reload landing
// between the caller's snapshot and this read paired a provider built
// from config generation A with network policy from generation B — a
// mixed pairing that then persisted for the rest of the call. Threading
// the caller's snapshot down makes both reads one atomic read.
func (c *coordinator) resolveProviderHTTPClient(cfg *config.Config, providerCfg config.ProviderConfig) (*http.Client, error) {
	resolved := nettransport.ResolveNetworkConfig(cfg.Options.Network, providerCfg.Network)
	networkClient, err := nettransport.BuildHTTPClient(resolved)
	if err != nil {
		// R2-5: the inner error text can embed the raw proxy/DoH URL
		// from config (userinfo, query secrets included); redact before
		// this message is wrapped and persisted. Unwrap still reaches
		// the original error so errors.Is/As classification is intact.
		return nil, fmt.Errorf("provider %q: build network client: %w", providerCfg.ID, redactNetworkError(err))
	}
	if !cfg.Options.Debug {
		return networkClient, nil
	}
	base := http.RoundTripper(http.DefaultTransport)
	if networkClient != nil {
		base = networkClient.Transport
	}
	return log.NewHTTPClientWithTransport(base), nil
}

// copilotBaseTransport extracts the base round tripper from the optional
// resolved HTTP client so the Copilot initiator transport can layer on top
// of the proxy/DNS-configured transport. Nil means "nothing configured".
func copilotBaseTransport(httpClient *http.Client) http.RoundTripper {
	if httpClient == nil {
		return nil
	}
	return httpClient.Transport
}

// networkURLPattern matches scheme-qualified URL-looking tokens inside
// error text so each can be redacted before the message is persisted.
var networkURLPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"<>]+`)

// trailingPunctuation is trimmed off a matched token before URL parsing:
// prose often appends sentence punctuation or a closing bracket directly
// to the URL, and a trailing "…8080)." fails to parse — which would
// leave the secrets in place unredacted. The trimmed suffix is appended
// back after redaction.
const trailingPunctuation = ".,;:!?)\\]}>"

// redactNetworkURLs rewrites every URL-looking token in s (R2-5) so
// diagnostic strings that end up persisted in finish details or the
// transcript cannot carry URL secrets: userinfo is stripped, scheme and
// host are kept, path is kept, and query/fragment are dropped. Tokens
// that do not parse as URLs with a host are left untouched.
func redactNetworkURLs(s string) string {
	return networkURLPattern.ReplaceAllStringFunc(s, func(token string) string {
		body := strings.TrimRight(token, trailingPunctuation)
		suffix := token[len(body):]
		u, err := url.Parse(body)
		if err != nil || u.Host == "" {
			return token
		}
		return u.Scheme + "://" + u.Host + u.Path + suffix
	})
}

// redactedNetworkError wraps err with a redacted message while keeping
// the original error reachable via Unwrap, so errors.Is/As chains that
// classify network failures keep working after redaction (R2-5).
type redactedNetworkError struct {
	msg string
	err error
}

func (e *redactedNetworkError) Error() string { return e.msg }

func (e *redactedNetworkError) Unwrap() error { return e.err }

// redactNetworkError returns err with any URL secrets in its message
// redacted (R2-5); nil passes through unchanged.
func redactNetworkError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedNetworkError{msg: redactNetworkURLs(err.Error()), err: err}
}
