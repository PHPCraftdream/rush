package cmd

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/spf13/cobra"
)

// serveAPITestSetup runs runServe without a real App (as the other serve
// tests do): a real App holds rush.log and its database open in the test's
// TempDir, which Windows cannot remove. Log setup is claimed file-less first.
func serveAPITestSetup(t *testing.T) (string, string) {
	t.Helper()
	rushlog.Setup("", false)
	oldApp, oldShutdown := serveAppFactory, serveAppShutdown
	t.Cleanup(func() { serveAppFactory, serveAppShutdown = oldApp, oldShutdown })
	serveAppFactory = func(*cobra.Command) (*app.App, error) { return nil, nil }
	serveAppShutdown = func(*app.App) error { return nil }
	return serveTestSetup(t)
}

type serveReadinessBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	ready chan struct{}
}

func (b *serveReadinessBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.Buffer.Write(p)
	ready := strings.Contains(b.Buffer.String(), "rush serve: listening on ")
	b.mu.Unlock()
	if ready {
		select {
		case b.ready <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (b *serveReadinessBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func waitServeReadiness(t *testing.T, stderr *serveReadinessBuffer) {
	t.Helper()
	select {
	case <-stderr.ready:
	case <-time.After(serveTestWait):
		t.Fatal("timed out waiting for serve readiness")
	}
}

func startServeForReadinessTest(t *testing.T, cmd *cobra.Command, signals chan os.Signal) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- runServe(cmd, nil)
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case signals <- os.Interrupt:
		default:
		}
		select {
		case <-done:
		case <-time.After(serveTestWait):
			t.Error("runServe did not stop within the test bound")
		}
	})
	return done
}

func waitServeStopped(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe: %v", err)
		}
	case <-time.After(serveTestWait):
		t.Fatal("runServe did not stop within the test bound")
	}
}

func TestServeReadinessLineUsesBoundAddress(t *testing.T) {
	globalDir, cwd := serveAPITestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("ready", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume, oldSignals := serveListen, serveConsumeBootstrapToken, serveSignals
	t.Cleanup(func() { serveListen, serveConsumeBootstrapToken, serveSignals = oldListen, oldConsume, oldSignals })
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveListen = func(string) (net.Listener, error) { return listener, nil }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	signals := make(chan os.Signal, 1)
	serveSignals = func() (<-chan os.Signal, func()) { return signals, func() {} }
	cmd := serveTestCommand()
	cmd.SetContext(context.Background())
	stderr := &serveReadinessBuffer{ready: make(chan struct{}, 1)}
	cmd.SetErr(stderr)
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	done := startServeForReadinessTest(t, cmd, signals)
	waitServeReadiness(t, stderr)
	want := "rush serve: listening on " + listener.Addr().String() + "\n"
	if stderr.String() != want {
		t.Fatalf("readiness=%q, want %q", stderr.String(), want)
	}
	signals <- os.Interrupt
	waitServeStopped(t, done)
}

func TestServeFactorySeamOverridesProductionAuthWhenSet(t *testing.T) {
	globalDir, cwd := serveAPITestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("ready", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume, oldAuth, oldSignals := serveListen, serveConsumeBootstrapToken, serveAuthFactory, serveSignals
	t.Cleanup(func() {
		serveListen, serveConsumeBootstrapToken, serveAuthFactory, serveSignals = oldListen, oldConsume, oldAuth, oldSignals
	})
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveListen = func(string) (net.Listener, error) { return listener, nil }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	var mu sync.Mutex
	factoryCalls := 0
	serveAuthFactory = func(next http.Handler) http.Handler {
		mu.Lock()
		factoryCalls++
		mu.Unlock()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Seam", "1")
			next.ServeHTTP(w, r)
		})
	}
	signals := make(chan os.Signal, 1)
	serveSignals = func() (<-chan os.Signal, func()) { return signals, func() {} }
	cmd := serveTestCommand()
	cmd.SetContext(context.Background())
	stderr := &serveReadinessBuffer{ready: make(chan struct{}, 1)}
	cmd.SetErr(stderr)
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	done := startServeForReadinessTest(t, cmd, signals)
	waitServeReadiness(t, stderr)
	requestCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+listener.Addr().String()+"/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := http.Client{Timeout: serveTestWait}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Seam") != "1" {
		t.Fatalf("health response status=%d X-Seam=%q", response.StatusCode, response.Header.Get("X-Seam"))
	}
	mu.Lock()
	calls := factoryCalls
	mu.Unlock()
	if calls < 2 {
		t.Fatalf("auth factory calls=%d, want policy and REST handler wiring", calls)
	}
	signals <- os.Interrupt
	waitServeStopped(t, done)
}

func TestServeNilAuthFactoryUsesDenyAllForInitialValidation(t *testing.T) {
	_, cwd := serveTestSetup(t)
	oldAuth := serveAuthFactory
	serveAuthFactory = nil
	t.Cleanup(func() { serveAuthFactory = oldAuth })

	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("print-config", "true")
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	if err := runServe(cmd, nil); err != nil {
		t.Fatalf("run serve with nil auth factory: %v", err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("address: 127.0.0.1:8080")) {
		t.Fatalf("print-config output missing default address: %q", stdout.String())
	}
}

// serveTestWait only bounds how long a genuine failure takes to be reported:
// a passing run never waits for it. It is generous because a loaded Windows
// runner can stall file I/O for several seconds.
const serveTestWait = 45 * time.Second
