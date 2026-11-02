package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListenerPolicyValidateRules(t *testing.T) {
	tests := []struct {
		name   string
		policy ListenerPolicy
		want   string
	}{
		{name: "empty surfaces", policy: ListenerPolicy{Address: "127.0.0.1:8080", Auth: DenyAllAuth}, want: "empty surface list"},
		{name: "unknown surface", policy: ListenerPolicy{Address: "127.0.0.1:8080", Surfaces: []ListenerSurface{"graphql"}, Auth: DenyAllAuth}, want: "unknown surface"},
		{name: "duplicate surface", policy: ListenerPolicy{Address: "127.0.0.1:8080", Surfaces: []ListenerSurface{SurfaceREST, SurfaceREST}, Auth: DenyAllAuth}, want: "duplicate surface"},
		{name: "non-loopback address requires secure transport", policy: ListenerPolicy{Address: "example.com:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}, want: "requires TLS"},
		{name: "invalid port", policy: ListenerPolicy{Address: "127.0.0.1:nope", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}, want: "unparsable address"},
		{name: "wildcard ipv4 requires secure transport", policy: ListenerPolicy{Address: "0.0.0.0:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}, want: "requires TLS"},
		{name: "wildcard ipv6 requires secure transport", policy: ListenerPolicy{Address: "[::]:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}, want: "requires TLS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.policy.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
			var policyErr *ListenerPolicyError
			if !errors.As(err, &policyErr) {
				t.Fatalf("error type = %T, want *ListenerPolicyError", err)
			}
		})
	}
	mapped := ListenerPolicy{Address: "[::ffff:127.0.0.1]:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}
	if err := mapped.Validate(); err != nil {
		t.Fatalf("mapped loopback Validate() error = %v", err)
	}
}

func TestListenerPolicyRequiresAuth(t *testing.T) {
	policy := ListenerPolicy{Address: "127.0.0.1:8080", Surfaces: []ListenerSurface{SurfaceREST}}
	if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), "authentication policy is required") {
		t.Fatalf("Validate() error = %v, want missing-auth refusal", err)
	}
}

func TestListenerPolicyValidateLoopbackAndCleartext(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8080", "127.9.8.7:8080", "[::1]:8443"} {
		t.Run(address, func(t *testing.T) {
			policy := ListenerPolicy{Address: address, Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}
			if err := policy.Validate(); err != nil {
				t.Fatalf("loopback Validate() error = %v", err)
			}
		})
	}
	policy := ListenerPolicy{Address: "0.0.0.0:8080", Surfaces: []ListenerSurface{SurfaceREST}, AllowCleartext: true, Auth: DenyAllAuth}
	if err := policy.Validate(); err != nil {
		t.Fatalf("explicit cleartext Validate() error = %v", err)
	}
	policy.AllowCleartext = false
	policy.TLS.Cert = "server.pem"
	if err := policy.Validate(); err != nil {
		var policyErr *ListenerPolicyError
		if !errors.As(err, &policyErr) {
			t.Fatalf("configured TLS error type = %T, want *ListenerPolicyError", err)
		}
		if policyErr.Rule != "TLS is not implemented yet" {
			t.Fatalf("configured TLS error Rule = %q, want %q", policyErr.Rule, "TLS is not implemented yet")
		}
	} else {
		t.Fatal("configured TLS Validate() error = nil, want refusal")
	}
}

func TestListenerPolicyValidateLocalhost(t *testing.T) {
	policy := ListenerPolicy{Address: "localhost:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}
	if err := policy.Validate(); err != nil {
		t.Fatalf("localhost Validate() error = %v", err)
	}
	resolved, err := policy.ResolvedAddress()
	if err != nil {
		t.Fatalf("ResolvedAddress() error = %v", err)
	}
	if strings.HasPrefix(resolved, "localhost:") {
		t.Fatalf("ResolvedAddress() = %q; must bind a concrete address", resolved)
	}
}

func TestValidateListenerSetDuplicateAddresses(t *testing.T) {
	policies := []ListenerPolicy{
		{Address: "127.0.0.1:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth},
		{Address: "127.0.0.1:8080", Surfaces: []ListenerSurface{SurfaceWS}, Auth: DenyAllAuth},
	}
	err := ValidateListenerSet(policies)
	if err == nil || !strings.Contains(err.Error(), "duplicate address") {
		t.Fatalf("ValidateListenerSet() error = %v, want duplicate address", err)
	}
	var policyErr *ListenerPolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error type = %T, want *ListenerPolicyError", err)
	}
}

func TestListenerPolicyAuthSeam(t *testing.T) {
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	DenyAllAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("downstream handler invoked")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", recorder.Header().Get("WWW-Authenticate"))
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("response body = %q, want empty", recorder.Body.String())
	}
	policy := ListenerPolicy{Auth: DenyAllAuth}
	if policy.Auth == nil {
		t.Fatal("Auth seam was not retained")
	}
}
