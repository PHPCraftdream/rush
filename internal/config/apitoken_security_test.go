package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolateAPITokenGlobals(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	restoreKtav := SetKtavAutoMigrateForTest(false)
	t.Cleanup(restoreKtav)
	t.Setenv(GlobalDirEnv, "")
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "global-data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(root, "global-config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "xdg-cache"))
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("RUSH_SERVE_TOKEN_SHA256", "")
	t.Setenv("RUSH_SERVE_TOKEN_SCOPE", "")
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local-app-data"))
	return root
}

// Mutant check: accepting corrupt versions, malformed files, unreadable files, or insecure modes violates fail-closed behavior.
func TestAPITokenSecurityFailClosed(t *testing.T) {
	root := isolateAPITokenGlobals(t)
	path := filepath.Join(root, "tokens.json")
	store := &APITokenStore{Path: path}
	plain, _, err := store.CreateAPIToken("candidate", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, data string
		mode       os.FileMode
	}{
		{"wrong version", `{"version":99,"tokens":[]}`, 0o600},
		{"malformed content", `{broken`, 0o600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.data), tc.mode); err != nil {
				t.Fatal(err)
			}
			assertAPITokenStoreFailsClosed(t, store, plain)
		})
	}
	t.Run("unreadable file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix permission bits are not enforced or meaningful on Windows")
		}
		if err := os.WriteFile(path, []byte(`{"version":1,"tokens":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		assertAPITokenStoreFailsClosed(t, store, plain)
	})
	t.Run("group or other permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix permission bits are not enforced or meaningful on Windows")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"version":1,"tokens":[]}`), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		assertAPITokenStoreFailsClosed(t, store, plain)
	})
	t.Run("missing file is empty", func(t *testing.T) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		items, err := store.ListAPITokens()
		if err != nil || len(items) != 0 {
			t.Fatalf("List = %v, %v; want empty", items, err)
		}
		if _, ok, err := store.VerifyAPIToken(plain, time.Now()); err == nil || ok {
			t.Fatalf("Verify = %v, %v; want invalid-token error", ok, err)
		}
	})
}

func assertAPITokenStoreFailsClosed(t *testing.T, store *APITokenStore, plain string) {
	t.Helper()
	if _, ok, err := store.VerifyAPIToken(plain, time.Now()); err == nil || ok {
		t.Fatalf("Verify = %v, %v; want failure", ok, err)
	}
	if _, err := store.ListAPITokens(); err == nil {
		t.Fatal("List unexpectedly succeeded")
	}
}

// Mutant check: parsing any project/data-dir token file (or deriving the store path from it) must not affect the global store.
func TestAPITokenSecurityProjectDirectoryIsolation(t *testing.T) {
	root := isolateAPITokenGlobals(t)
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(cwd, ".rush"), 0o700); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	global := filepath.Join(root, "global-data", apiTokenFileName)
	globalDir := filepath.Join(root, "flag-global")
	dataDir := filepath.Join(root, "explicit-data-dir")
	validToken := "rush_01020304." + strings.Repeat("A", 43)
	hash := sha256.Sum256([]byte(validToken))
	validDoc := fmt.Sprintf(`{"version":1,"tokens":[{"id":"01020304","name":"decoy","scopes":["read"],"permission":"ask","created":"2025-01-01T00:00:00Z","sha256":"%s"}]}`, hex.EncodeToString(hash[:]))
	for _, path := range []string{filepath.Join(cwd, apiTokenFileName), filepath.Join(cwd, ".rush", apiTokenFileName), filepath.Join(dataDir, apiTokenFileName)} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(validDoc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := GlobalAPITokenStore()
	if store.Path != global {
		t.Fatalf("store path = %q, want %q", store.Path, global)
	}
	if _, ok, err := store.VerifyAPIToken(validToken, time.Now()); err == nil || ok {
		t.Fatalf("global Verify = %v, %v; project decoy unexpectedly verified", ok, err)
	}
	if _, err := store.ListAPITokens(); err != nil {
		t.Fatalf("project token file consulted: %v", err)
	}
	t.Setenv(GlobalDirEnv, globalDir) // Mirrors the --global-dir root selected by its flag handler.
	flagStore := GlobalAPITokenStore()
	want := filepath.Join(globalDir, "data", apiTokenFileName)
	if flagStore.Path != want {
		t.Fatalf("--global-dir path = %q, want %q", flagStore.Path, want)
	}
	if _, ok, err := flagStore.VerifyAPIToken(validToken, time.Now()); err == nil || ok {
		t.Fatalf("--global-dir Verify = %v, %v; project/data-dir decoy unexpectedly verified", ok, err)
	}
	if _, err := flagStore.ListAPITokens(); err != nil {
		t.Fatalf("project/data-dir token file consulted: %v", err)
	}
}

// Mutant check: formatting tokens/documents or logging/returning errors with sensitive fields must not expose credentials or digests.
func TestAPITokenSecurityNoPlaintextOrDigestLeaks(t *testing.T) {
	root := isolateAPITokenGlobals(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store := &APITokenStore{Path: filepath.Join(root, "tokens.json")}
	plain, token, err := store.CreateAPIToken("secret-name", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.SplitN(plain, ".", 2)[1]
	digest := token.Hash
	doc := apiTokenDocument{Version: apiTokenStoreVersion, Tokens: []APIToken{token}}
	for _, value := range []any{token, doc} {
		for _, format := range []string{"%v", "%+v", "%s"} {
			assertNoAPITokenSecrets(t, fmt.Sprintf(format, value), plain, secret, digest)
		}
	}
	_ = slog.Default().Handler().Enabled(context.Background(), slog.LevelInfo)
	_, _, createErr := store.CreateAPIToken(plain, []string{secret}, "ask", nil)
	_, _, permissionErr := store.CreateAPIToken("invalid", []string{"read"}, secret, nil)
	mutatedSecret := secret[:len(secret)-1] + map[bool]string{true: "B", false: "A"}[secret[len(secret)-1] == 'A']
	mutatedToken := strings.SplitN(plain, ".", 2)[0] + "." + mutatedSecret
	_, _, verifyErr := store.VerifyAPIToken(mutatedToken, time.Now())
	revokeErr := store.RevokeAPIToken(plain)
	badBootstrap := plain
	_, _, bootstrapErr := ConsumeServeBootstrapToken(&APITokenStore{Path: filepath.Join(root, "bootstrap.json")}, func(string) string { return badBootstrap }, func(string) error { return nil }, time.Now())
	_ = store.RevokeAPIToken(token.ID)
	for _, err := range []error{createErr, permissionErr, verifyErr, revokeErr, bootstrapErr} {
		if err == nil {
			t.Fatal("expected invalid credential input to return an error")
		}
		assertNoAPITokenSecrets(t, err.Error(), plain, secret, digest)
	}
	assertNoAPITokenSecrets(t, logs.String(), plain, secret, digest)
}

func assertNoAPITokenSecrets(t *testing.T, output, plain, secret, digest string) {
	t.Helper()
	if strings.Contains(output, plain) || strings.Contains(output, secret) || strings.Contains(output, digest) {
		t.Fatalf("sensitive token data leaked in %q", output)
	}
}

// Mutant check: removing the inter-instance write lock is covered deterministically by
// TestAPITokenConcurrentCreateLockIsDeterministic; this simultaneous-start test remains probabilistic.
func TestAPITokenSecurityConcurrentCreateAcrossStores(t *testing.T) {
	root := isolateAPITokenGlobals(t)
	path := filepath.Join(root, "tokens.json")
	stores := []*APITokenStore{{Path: path}, {Path: path}}
	const count = 2
	var wg sync.WaitGroup
	plains := make(chan string, count)
	errs := make(chan error, count)
	start := make(chan struct{})
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			plain, _, err := stores[i].CreateAPIToken(fmt.Sprintf("worker-%d", i), []string{"read"}, "ask", nil)
			plains <- plain
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(plains)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	items, err := stores[0].ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != count {
		t.Fatalf("record count = %d, want %d", len(items), count)
	}
	for plain := range plains {
		if _, ok, err := stores[1].VerifyAPIToken(plain, time.Now()); err != nil || !ok {
			t.Fatalf("created token failed verification: ok=%v err=%v", ok, err)
		}
	}
}
