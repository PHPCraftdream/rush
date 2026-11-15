package cmd

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestAPIRemoteCredentialSourceNeverContainsSecret(t *testing.T) {
	for _, tc := range []struct {
		remote config.APIRemote
		want   string
	}{
		{config.APIRemote{TokenEnv: "RUSH_SECRET"}, "environment:RUSH_SECRET"},
		{config.APIRemote{TokenFile: "/secret/path"}, "file"},
		{config.APIRemote{}, "none"},
	} {
		if got := remoteCredentialSource(tc.remote); got != tc.want {
			t.Errorf("remoteCredentialSource() = %q, want %q", got, tc.want)
		}
	}
	const secret = "rush_01234567.0123456789012345678901234567890123456789012"
	if got := remoteCredentialSource(config.APIRemote{TokenFile: secret}); strings.Contains(got, secret) {
		t.Fatalf("credential source exposed secret %q", got)
	}
}

func TestAPIRemoteTLSFacts(t *testing.T) {
	for _, tc := range []struct{ url, pin, tls, pinFact string }{
		{"https://api.example.test", "", "HTTPS certificate verification enabled (unless --insecure-skip-verify is used)", "not configured"},
		{"http://localhost:8080", "sha256//x", "HTTP cleartext", "configured"},
	} {
		if got := remoteTLSFact(tc.url); got != tc.tls {
			t.Errorf("remoteTLSFact(%q) = %q", tc.url, got)
		}
		if got := pinFact(tc.pin); got != tc.pinFact {
			t.Errorf("pinFact(%q) = %q", tc.pin, got)
		}
	}
}

func TestSortedRemoteNames(t *testing.T) {
	got := sortedRemoteNames(map[string]config.APIRemote{"z": {}, "a": {}, "m": {}})
	if strings.Join(got, ",") != "a,m,z" {
		t.Fatalf("sortedRemoteNames() = %v", got)
	}
}
