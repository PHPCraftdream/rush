package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const apiFaultToken = "rush_01234567.0123456789012345678901234567890123456789012"

var apiFaultEnv = []string{"RUSH_FAULT_TOKEN=" + apiFaultToken}

// addFaultRemote stores a default profile for url that takes its credential
// from RUSH_FAULT_TOKEN.
func addFaultRemote(t *testing.T, f *apiProcessFixture, url string, extra ...string) {
	t.Helper()
	args := append([]string{"api", "remote", "add", "fault", "--url", url, "--token-env", "RUSH_FAULT_TOKEN", "--default"}, extra...)
	if stdout, stderr, exit := f.run("", args...); exit != 0 {
		t.Fatalf("remote add exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

func TestAPICLIFaultExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		wantExit     int
		wantRequests func(int32) bool
	}{
		{"unauthorized", http.StatusUnauthorized, 3, func(n int32) bool { return n == 1 }},
		{"forbidden", http.StatusForbidden, 3, func(n int32) bool { return n == 1 }},
		{"not found", http.StatusNotFound, 4, func(n int32) bool { return n == 1 }},
		{"unavailable is retried", http.StatusServiceUnavailable, 1, func(n int32) bool { return n > 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIProcessFixture(t)
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/problem+json")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"title":"injected"}`))
			}))
			defer srv.Close()
			addFaultRemote(t, f, srv.URL)
			stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "sessions", "list")
			if exit != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", exit, tc.wantExit, stdout, stderr)
			}
			if !tc.wantRequests(requests.Load()) {
				t.Fatalf("server saw %d requests", requests.Load())
			}
			f.assertNoSecrets(t, []string{apiFaultToken}, stdout, stderr)
		})
	}
}

func TestAPICLIWholeCommandTimeout(t *testing.T) {
	f := newAPIProcessFixture(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	addFaultRemote(t, f, srv.URL)
	start := time.Now()
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "sessions", "list", "--timeout", "300ms")
	if exit != 5 {
		t.Fatalf("exit = %d, want 5; stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the command was not bounded by --timeout: %s", elapsed)
	}
	if _, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "sessions", "list", "--timeout", "-1s"); exit != 2 {
		t.Fatalf("negative --timeout exit = %d, want 2; stderr=%q", exit, stderr)
	}
}

func TestAPICLITLSFailureAndInsecureSkipVerify(t *testing.T) {
	f := newAPIProcessFixture(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/health"):
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = w.Write([]byte(`{"version":"fault-test"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	addFaultRemote(t, f, srv.URL)

	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "remote", "test")
	if exit != 1 {
		t.Fatalf("untrusted certificate exit = %d, want 1; stdout=%q stderr=%q", exit, stdout, stderr)
	}
	f.assertNoSecrets(t, []string{apiFaultToken}, stdout, stderr)

	for i := 0; i < 2; i++ {
		stdout, stderr, exit = f.runWithEnv(apiFaultEnv, "", "api", "remote", "test", "--insecure-skip-verify")
		if exit != 0 || !strings.Contains(stdout, "fault-test") {
			t.Fatalf("run %d: exit %d stdout=%q stderr=%q", i, exit, stdout, stderr)
		}
		if !strings.Contains(stderr, "WARNING: TLS certificate verification is disabled") {
			t.Fatalf("run %d: no warning on stderr: %q", i, stderr)
		}
	}
	// Never persisted: the profile still verifies certificates.
	if _, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "remote", "test"); exit != 1 {
		t.Fatalf("--insecure-skip-verify was persisted: exit %d", exit)
	}
}

func TestAPICLIInsecureSkipVerifyRefusedForNonLoopback(t *testing.T) {
	f := newAPIProcessFixture(t)
	addFaultRemote(t, f, "https://203.0.113.7:9")
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "sessions", "list", "--insecure-skip-verify", "--timeout", "5s")
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

func TestAPICLIRefusesTokenShapedInputWithoutRequests(t *testing.T) {
	f := newAPIProcessFixture(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	addFaultRemote(t, f, srv.URL)
	var outputs []string
	for _, args := range [][]string{
		{"api", "sessions", "get", apiFaultToken},
		{"api", "messages", apiFaultToken},
		{"api", "sessions", "list", "--remote", apiFaultToken},
		{"api", "sessions", "list", "--after", apiFaultToken},
		{"api", "sessions", "create", "--title", apiFaultToken},
		{"api", "sessions", "list", "--ca", apiFaultToken},
		{"api", "remote", "add", "x", "--url", srv.URL, "--token-env", apiFaultToken},
		{"api", "remote", "show", apiFaultToken},
	} {
		stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", args...)
		outputs = append(outputs, stdout, stderr)
		if exit != 2 {
			t.Errorf("rush %s: exit %d, want 2", strings.Join(args[:3], " "), exit)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("token-shaped input reached the network: %d requests", requests.Load())
	}
	f.assertNoSecrets(t, []string{apiFaultToken}, outputs...)
}

func TestAPICLICreatesNoProjectState(t *testing.T) {
	f := newAPIProcessFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"next_after":null}`))
	}))
	defer srv.Close()
	addFaultRemote(t, f, srv.URL)
	if stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "sessions", "list", "--json"); exit != 0 {
		t.Fatalf("exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if entries, err := os.ReadDir(f.cwd); err != nil || len(entries) != 0 {
		t.Fatalf("cwd not empty after an API command: %v %v", entries, err)
	}
	_ = filepath.WalkDir(f.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := entry.Name()
		if base == ".rush" || base == "rush.db" || strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".db-wal") {
			t.Errorf("API command created project state: %s", path)
		}
		return nil
	})
}
