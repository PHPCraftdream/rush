package settingsctl

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestParseTokenExpiry(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	now := cmdtest.MustParseTime(t, "2026-11-01T00:00:00Z")
	got, err := parseTokenExpiry("30d", now)
	if err != nil || got == nil || !got.Equal(now.Add(30*24*60*60*1e9)) {
		t.Fatalf("30d expiry = %v, %v", got, err)
	}
	if _, err := parseTokenExpiry("2026-12-01T12:30:00+02:00", now); err != nil {
		t.Fatalf("RFC3339 expiry rejected: %v", err)
	}
	if _, err := parseTokenExpiry("forever", now); err == nil {
		t.Fatal("invalid expiry accepted")
	}
	if _, err := parseTokenExpiry("0s", now); err == nil {
		t.Fatal("non-future expiry accepted")
	}
}

func TestValidateTokenID(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	for _, id := range []string{"deadbeef", "00000000"} {
		if err := validateTokenID(id); err != nil {
			t.Fatalf("valid id %q rejected: %v", id, err)
		}
	}
	for _, id := range []string{"DEADBEEF", "deadbee", "deadbeef0", "deadbeeg", "rush_deadbeef"} {
		err := validateTokenID(id)
		if err == nil || err.Error() != "token ID must be exactly 8 lowercase hexadecimal characters" || strings.Contains(err.Error(), id) {
			t.Fatalf("invalid id %q error = %v", id, err)
		}
	}
}

func TestParseTokenExpiryRFC3339(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	now := cmdtest.MustParseTime(t, "2026-11-01T00:00:00Z")
	got, err := parseTokenExpiry("2026-12-01T12:30:00+02:00", now)
	want := cmdtest.MustParseTime(t, "2026-12-01T10:30:00Z")
	if err != nil || got == nil || !got.Equal(want) {
		t.Fatalf("RFC3339 expiry = %v, %v; want %v", got, err, want)
	}
}
