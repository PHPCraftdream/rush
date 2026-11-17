package apitoken

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/slots"
)

func TestAPITokenKtavRoundTripAndMalformedCreate(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	for _, ktav := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "ktav"}[ktav], func(t *testing.T) {
			restore := slots.SetKtavAutoMigrateForTest(ktav)
			defer restore()
			path := filepath.Join(t.TempDir(), apiTokenFileName)
			store := &APITokenStore{Path: path}
			plain, _, err := store.CreateAPIToken("roundtrip", []string{"read"}, "ask", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := (&APITokenStore{Path: path}).VerifyAPIToken(plain, time.Now()); err != nil || !ok {
				t.Fatalf("roundtrip: %v %v", ok, err)
			}
			path, err = store.selectedPath()
			if err != nil {
				t.Fatal(err)
			}
			if ktav && !strings.HasSuffix(path, ".ktav") {
				t.Fatal("expected Ktav path")
			}
		})
	}
	restore := slots.SetKtavAutoMigrateForTest(true)
	defer restore()
	path := filepath.Join(t.TempDir(), apiTokenKtavFileName)
	if err := os.WriteFile(path, []byte("not valid = ="), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&APITokenStore{Path: path}).ListAPITokens(); err == nil {
		t.Fatal("malformed Ktav accepted")
	}
	untouched, _ := os.ReadFile(path)
	if _, _, err := (&APITokenStore{Path: path}).CreateAPIToken("cannot convert", []string{"read"}, "ask", nil); err == nil {
		t.Fatal("unrepresentable write accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(untouched) {
		t.Fatal("failed conversion changed file")
	}
}

func TestAPITokenKtavCreateRejectsUnrepresentableRecord(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	restore := slots.SetKtavAutoMigrateForTest(true)
	defer restore()
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	if _, _, err := store.CreateAPIToken("line\rbreak", []string{"read"}, "ask", nil); err == nil {
		t.Fatal("Ktav create accepted a record containing an unrepresentable CR")
	}
	selected, err := store.selectedPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(selected); !os.IsNotExist(err) {
		t.Fatalf("failed Ktav create left a file: %v", err)
	}
}

func TestAPITokenStateUnreadableFailsFlush(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced consistently on Windows")
	}
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	state := filepath.Join(filepath.Dir(path), "api-tokens.state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store.Touch("01020304")
	if err := store.Flush(); err == nil {
		t.Fatal("unreadable state flush succeeded")
	}
}

func TestAPITokenVerifyMalformedVersionFailsClosed(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	if err := os.WriteFile(path, []byte(`{"version":2,"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &APITokenStore{Path: path}
	if _, ok, err := store.VerifyAPIToken("rush_01020304."+strings.Repeat("A", 43), time.Now()); err == nil || ok {
		t.Fatal("wrong version did not fail closed")
	}
	if _, err := store.ListAPITokens(); err == nil {
		t.Fatal("wrong version listed")
	}
}

func TestConsumeServeBootstrapTokenRejectsCollisionAndBadDigestLength(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := &APITokenStore{Path: path}
	if _, _, err := store.CreateAPIToken("collision", []string{"read"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	id := items[0].ID
	for _, digest := range []string{strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		tmp := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
		_, ok, err := ConsumeServeBootstrapToken(tmp, func(k string) string {
			if k == "RUSH_SERVE_TOKEN_SHA256" {
				return id + ":" + digest
			}
			return ""
		}, func(string) error { return nil }, time.Now())
		if err == nil || ok {
			t.Fatalf("bad digest length accepted: %d", len(digest))
		}
	}
	collision := &APITokenStore{Path: path}
	_, ok, err := ConsumeServeBootstrapToken(collision, func(k string) string {
		if k == "RUSH_SERVE_TOKEN_SHA256" {
			return id + ":" + strings.Repeat("a", 64)
		}
		return ""
	}, func(string) error { return nil }, time.Now())
	if err == nil || ok {
		t.Fatal("stored id collision accepted")
	}
}
