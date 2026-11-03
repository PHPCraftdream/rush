package server

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// IsLoopbackHost classifies an IP literal or the lexical localhost name.
// DNS resolution is performed by ListenerPolicy, where the resolved snapshot
// can also be used as the concrete bind address.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func allLoopback(addresses []net.IP) bool {
	if len(addresses) == 0 {
		return false
	}
	for _, address := range addresses {
		if !address.IsLoopback() {
			return false
		}
	}
	return true
}

type ListenerSurface string

const (
	SurfaceREST ListenerSurface = "rest"
	SurfaceWS   ListenerSurface = "ws"
	SurfaceUI   ListenerSurface = "ui"
)

// ListenerTLS contains configuration paths and the requested protocol minimum.
// It deliberately contains no TLS behavior.
type ListenerTLS struct {
	Cert       string
	Key        string
	ClientCA   string
	MinVersion string
	SelfSigned bool
	SANs       []string
	DataDir    string
}

// ListenerLimits contains parsed limits. Enforcement belongs to the API layer.
type ListenerLimits struct {
	RPS              float64
	Burst            int
	MaxConnsPerToken int
	MaxBody          int64
}

// DenyAllAuth is the pre-API-4 fail-closed authentication seam for serve.
func DenyAllAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
	})
}

// ListenerPolicy is the immutable-by-convention startup snapshot for one listener.
type ListenerPolicy struct {
	Address          string
	Surfaces         []ListenerSurface
	AllowCleartext   bool
	AllowAdmin       bool
	AllowAutoApprove bool
	AllowedOrigins   []string
	TokenIDs         []string
	Pins             map[string]string
	DeclaredPins     map[string]string
	PinsDeclared     bool
	Workspace        string
	Limits           ListenerLimits
	TLS              ListenerTLS
	Auth             func(next http.Handler) http.Handler
	RESTHandler      http.Handler
	resolver         func(string) ([]net.IP, error)
}

// ListenerPolicyError reports a policy validation failure.
type ListenerPolicyError struct {
	Address string
	Rule    string
	Cause   error
}

func (e *ListenerPolicyError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("listener policy %q: %s: %v", e.Address, e.Rule, e.Cause)
	}
	return fmt.Sprintf("listener policy %q: %s", e.Address, e.Rule)
}

func (e *ListenerPolicyError) Unwrap() error { return e.Cause }

// Validate checks one policy. Use ValidateListenerSet to also reject duplicate
// addresses among listeners that will be served together.
func (p ListenerPolicy) Validate() error {
	if p.TLS.configured() {
		if p.TLS.SelfSigned && (p.TLS.Cert != "" || p.TLS.Key != "") {
			return &ListenerPolicyError{Address: p.Address, Rule: "self-signed TLS cannot be combined with an explicit certificate or key"}
		}
		if (p.TLS.Cert == "") != (p.TLS.Key == "") {
			return &ListenerPolicyError{Address: p.Address, Rule: "TLS certificate and key must be specified together"}
		}
		if p.TLS.MinVersion != "" && p.TLS.Cert == "" && !p.TLS.SelfSigned {
			return &ListenerPolicyError{Address: p.Address, Rule: "TLS min_version requires a server certificate and key"}
		}
		if p.TLS.MinVersion != "" && p.TLS.MinVersion != "1.2" && p.TLS.MinVersion != "1.3" {
			return &ListenerPolicyError{Address: p.Address, Rule: "TLS min_version must be 1.2 or 1.3"}
		}
		if len(p.TLS.ClientCA) > 0 && p.TLS.Cert == "" && !p.TLS.SelfSigned {
			return &ListenerPolicyError{Address: p.Address, Rule: "client_ca requires a server certificate and key"}
		}
	}
	if p.Auth == nil {
		return &ListenerPolicyError{Address: p.Address, Rule: "authentication policy is required"}
	}
	if len(p.Surfaces) == 0 {
		return &ListenerPolicyError{Address: p.Address, Rule: "empty surface list"}
	}
	seen := make(map[ListenerSurface]struct{}, len(p.Surfaces))
	for _, surface := range p.Surfaces {
		if surface != SurfaceREST && surface != SurfaceWS && surface != SurfaceUI {
			return &ListenerPolicyError{Address: p.Address, Rule: "unknown surface " + string(surface)}
		}
		if _, ok := seen[surface]; ok {
			return &ListenerPolicyError{Address: p.Address, Rule: "duplicate surface " + string(surface)}
		}
		seen[surface] = struct{}{}
	}
	_, loopback, err := p.resolvedAddress()
	if err != nil {
		return &ListenerPolicyError{Address: p.Address, Rule: "unparsable address", Cause: err}
	}
	if !loopback && !p.AllowCleartext && !p.TLS.hasCertificatePair() {
		return &ListenerPolicyError{Address: p.Address, Rule: "non-loopback address requires TLS or allow_cleartext"}
	}
	return nil
}

// ValidateListenerSet validates each policy and rejects duplicate bind addresses.
func ValidateListenerSet(policies []ListenerPolicy) error {
	seen := make(map[string]struct{}, len(policies))
	for _, policy := range policies {
		address, _, err := policy.resolvedAddress()
		if err != nil {
			return &ListenerPolicyError{Address: policy.Address, Rule: "unparsable address", Cause: err}
		}
		resolvedPolicy := policy
		resolvedPolicy.Address = address
		if err := resolvedPolicy.Validate(); err != nil {
			return err
		}
		if _, exists := seen[address]; exists {
			return &ListenerPolicyError{Address: policy.Address, Rule: "duplicate address in listener set"}
		}
		seen[address] = struct{}{}
	}
	return nil
}

func (p ListenerPolicy) WithResolver(resolver func(string) ([]net.IP, error)) ListenerPolicy {
	p.resolver = resolver
	return p
}

// ResolvedAddress returns the concrete bind address. In particular, localhost
// is resolved at validation/bind time and is never returned as a bind hostname.
func (p ListenerPolicy) ResolvedAddress() (string, error) {
	address, _, err := p.resolvedAddress()
	if err != nil {
		return "", &ListenerPolicyError{Address: p.Address, Rule: "unparsable address", Cause: err}
	}
	return address, nil
}

func (p ListenerPolicy) resolvedAddress() (string, bool, error) {
	host, port, err := net.SplitHostPort(p.Address)
	if err != nil {
		return "", false, err
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return "", false, fmt.Errorf("invalid port %q", port)
	}
	if host == "" && !p.TLS.SelfSigned {
		return "", false, fmt.Errorf("empty host")
	}
	if host == "" {
		return net.JoinHostPort("", port), false, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsLoopbackHost(host) {
			return net.JoinHostPort(ip.String(), port), true, nil
		}
		return net.JoinHostPort(ip.String(), port), false, nil
	}
	if strings.EqualFold(host, "localhost") {
		resolver := p.resolver
		if resolver == nil {
			resolver = net.LookupIP
		}
		addresses, err := resolver(host)
		if err != nil || len(addresses) == 0 {
			if err == nil {
				err = fmt.Errorf("localhost resolved to no addresses")
			}
			return "", false, err
		}
		for _, address := range addresses {
			if !IsLoopbackHost(address.String()) {
				return "", false, fmt.Errorf("localhost resolved to non-loopback address %s", address)
			}
		}
		sort.Slice(addresses, func(i, j int) bool { return addresses[i].String() < addresses[j].String() })
		return net.JoinHostPort(addresses[0].String(), port), true, nil
	}
	return net.JoinHostPort(host, port), false, nil
}

func (tls ListenerTLS) configured() bool {
	return tls.Cert != "" || tls.Key != "" || tls.ClientCA != "" || tls.MinVersion != "" || tls.SelfSigned
}

func (tls ListenerTLS) hasCertificatePair() bool {
	return (tls.Cert != "" && tls.Key != "") || tls.SelfSigned
}
