package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

type helloPayload struct {
	Protocol string   `json:"protocol"`
	TokenID  string   `json:"token_id"`
	Scopes   []string `json:"scopes"`
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func serveOriginAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	if origin == "null" {
		return false
	}
	for _, candidate := range allowed {
		if originMatches(origin, candidate) {
			return true
		}
	}
	return false
}

func originMatches(origin, candidate string) bool {
	a, err := url.Parse(origin)
	if err != nil || a.Scheme == "" || a.Host == "" || a.User != nil || a.Path != "" || a.RawQuery != "" || a.Fragment != "" {
		return false
	}
	b, err := url.Parse(candidate)
	if err != nil || b.Scheme == "" || b.Host == "" || b.User != nil || b.Path != "" || b.RawQuery != "" || b.Fragment != "" {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		if strings.EqualFold(u.Scheme, "http") {
			return "80"
		}
		return ""
	}
	normalHost := func(u *url.URL) string {
		host := strings.ToLower(u.Hostname())
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
		return host
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && normalHost(a) == normalHost(b) && port(a) == port(b)
}

func validateAllowedOrigins(origins []string) error {
	for _, origin := range origins {
		if origin == "*" {
			return fmt.Errorf("wildcard origin is not allowed")
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || origin == "null" {
			return fmt.Errorf("invalid origin %q", origin)
		}
	}
	return nil
}
