package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestListenerPolicyLocalhostResolver(t *testing.T) {
	tests := []struct {
		name       string
		addresses  []net.IP
		resolveErr error
		wantAddr   string
		wantErr    bool
	}{
		{name: "ipv4", addresses: []net.IP{net.ParseIP("127.0.0.1")}, wantAddr: "127.0.0.1:8080"},
		{name: "ipv6", addresses: []net.IP{net.ParseIP("::1")}, wantAddr: "[::1]:8080"},
		{name: "mixed", addresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}, wantErr: true},
		{name: "resolver error", resolveErr: errors.New("lookup failed"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			policy := ListenerPolicy{
				Address: "localhost:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth,
				resolver: func(host string) ([]net.IP, error) {
					calls++
					if host != "localhost" {
						t.Errorf("resolver host = %q, want localhost", host)
					}
					return test.addresses, test.resolveErr
				},
			}
			address, err := policy.ResolvedAddress()
			if test.wantErr {
				if err == nil {
					t.Fatal("ResolvedAddress() error = nil, want refusal")
				}
			} else if err != nil || address != test.wantAddr {
				t.Fatalf("ResolvedAddress() = %q, %v; want %q", address, err, test.wantAddr)
			}
			if calls != 1 {
				t.Fatalf("resolver calls = %d, want 1", calls)
			}
		})
	}
}

func TestStartResolvesLocalhostOnceAndBindsResolvedIP(t *testing.T) {
	calls := 0
	server := NewWithPolicy(nil, ListenerPolicy{
		Address: "localhost:0", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth,
		resolver: func(string) ([]net.IP, error) {
			calls++
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		},
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- server.Start(ctx, func(address string) { ready <- address }) }()
	select {
	case address := <-ready:
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" {
			cancel()
			t.Fatalf("bound address = %q, %v; want IPv4 loopback", address, err)
		}
	case err := <-errCh:
		cancel()
		t.Fatalf("Start() error = %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not become ready")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start() after shutdown = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
}

func TestStartRefusesUnsafeLocalhostResolution(t *testing.T) {
	for _, test := range []struct {
		name       string
		addresses  []net.IP
		resolveErr error
	}{
		{name: "mixed", addresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}},
		{name: "resolver error", resolveErr: errors.New("lookup failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := NewWithPolicy(nil, ListenerPolicy{
				Address: "localhost:0", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth,
				resolver: func(string) ([]net.IP, error) {
					calls++
					return test.addresses, test.resolveErr
				},
			}, nil)
			err := server.Start(t.Context(), nil)
			if err == nil || !strings.Contains(err.Error(), "unparsable address") {
				t.Fatalf("Start() error = %v, want unresolved localhost refusal", err)
			}
			if calls != 1 {
				t.Fatalf("resolver calls = %d, want 1", calls)
			}
		})
	}
}
