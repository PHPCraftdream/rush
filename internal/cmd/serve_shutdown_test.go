package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/spf13/cobra"
)

func TestRunServeShutdownCallbackAfterServerExitForSignals(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(signal.String(), func(t *testing.T) {
			globalDir, cwd := serveTestSetup(t)
			if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
				t.Fatal(err)
			}
			oldListen, oldApp, oldShutdown, oldSignals, oldConsume, oldAuth := serveListen, serveAppFactory, serveAppShutdown, serveSignals, serveConsumeBootstrapToken, serveAuthFactory
			t.Cleanup(func() {
				serveListen, serveAppFactory, serveAppShutdown, serveSignals, serveConsumeBootstrapToken, serveAuthFactory = oldListen, oldApp, oldShutdown, oldSignals, oldConsume, oldAuth
			})
			listenCtx, listenCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer listenCancel()
			listener, err := (&net.ListenConfig{}).Listen(listenCtx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveListen = func(string) (net.Listener, error) { return listener, nil }
			serveAppFactory = func(*cobra.Command) (*app.App, error) { return nil, nil }
			serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
				return config.APIToken{}, false, nil
			}
			signalCh := make(chan os.Signal, 1)
			serveSignals = func() (<-chan os.Signal, func()) { return signalCh, func() {} }
			var mu sync.Mutex
			var events []string
			handlerEntered := make(chan struct{})
			requestDone := make(chan struct{})
			serveAppShutdown = func(*app.App) error {
				mu.Lock()
				events = append(events, "app shutdown")
				mu.Unlock()
				return nil
			}
			serveAuthFactory = func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/wait" {
						close(handlerEntered)
						<-r.Context().Done()
						mu.Lock()
						events = append(events, "handler completed")
						mu.Unlock()
						w.WriteHeader(http.StatusNoContent)
						return
					}
					next.ServeHTTP(w, r)
				})
			}
			cmd := serveTestCommand()
			_ = cmd.Flags().Set("cwd", cwd)
			_ = cmd.Flags().Set("global-dir", globalDir)
			runDone := make(chan error, 1)
			go func() { runDone <- runServe(cmd, nil) }()
			requestClient := http.Client{Timeout: 3 * time.Second}
			go func() {
				defer close(requestDone)
				request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/api/v1/wait", nil)
				if requestErr != nil {
					return
				}
				response, err := requestClient.Do(request)
				if err == nil {
					_ = response.Body.Close()
				}
			}()
			select {
			case <-handlerEntered:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not start")
			}
			signalCh <- signal
			select {
			case err := <-runDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runServe did not stop")
			}
			select {
			case <-requestDone:
			case <-time.After(3 * time.Second):
				t.Fatal("request did not complete")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(events) != 2 || events[0] != "handler completed" || events[1] != "app shutdown" {
				t.Fatalf("shutdown sequence=%v", events)
			}
		})
	}
}

func TestRunServeReturnsInjectedShutdownFailure(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	oldListen, oldApp, oldShutdown, oldSignals, oldConsume, oldAuth := serveListen, serveAppFactory, serveAppShutdown, serveSignals, serveConsumeBootstrapToken, serveAuthFactory
	t.Cleanup(func() {
		serveListen, serveAppFactory, serveAppShutdown, serveSignals, serveConsumeBootstrapToken, serveAuthFactory = oldListen, oldApp, oldShutdown, oldSignals, oldConsume, oldAuth
	})
	listenCtx, listenCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer listenCancel()
	listener, err := (&net.ListenConfig{}).Listen(listenCtx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveListen = func(string) (net.Listener, error) { return listener, nil }
	serveAppFactory = func(*cobra.Command) (*app.App, error) { return nil, nil }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	signals := make(chan os.Signal, 1)
	serveSignals = func() (<-chan os.Signal, func()) { return signals, func() {} }
	serveAppShutdown = func(*app.App) error { return errors.New("injected shutdown failure") }
	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	signals <- os.Interrupt
	err = runServe(cmd, nil)
	if code, ok := tokenCLIExitCode(err); !ok || code != 1 || err.Error() != "injected shutdown failure" {
		t.Fatalf("error=%v, exit=(%d,%t), want injected shutdown failure with code 1", err, code, ok)
	}
}

func TestExecuteServeShutdownFailureExitCode(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global"))
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"serve", "--cwd", cwd})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	process := platform.Command(ctx, executable, "-test.run=^TestServeShutdownFailureHelperProcess$")
	process.Env = cleanExecuteHelperEnv(os.Environ())
	process.Env = append(process.Env,
		"RUSH_TEST_SERVE_SHUTDOWN_HELPER=1",
		"RUSH_TEST_SERVE_SHUTDOWN_ARGS="+string(args),
		"RUSH_GLOBAL_DIR="+filepath.Join(base, "global"),
		"RUSH_GLOBAL_DATA="+filepath.Join(base, "data"),
		"RUSH_GLOBAL_CONFIG="+filepath.Join(base, "config"),
		"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
		"XDG_DATA_HOME="+filepath.Join(base, "xdg-data"),
		"HOME="+filepath.Join(base, "home"),
		"USERPROFILE="+filepath.Join(base, "userprofile"),
	)
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	processErr := process.Run()
	if exit, ok := processErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("process error=%v, stdout=%q stderr=%q; want exit 1", processErr, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(normalizedServeError(strings.ToLower(stderr.String())), normalizedServeError("injected shutdown failure")) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestServeShutdownFailureHelperProcess(t *testing.T) {
	if os.Getenv("RUSH_TEST_SERVE_SHUTDOWN_HELPER") != "1" {
		return
	}
	serveAppShutdown = func(*app.App) error { return errors.New("injected shutdown failure") }
	serveAppFactory = func(*cobra.Command) (*app.App, error) { return nil, nil }
	serveListen = func(string) (net.Listener, error) {
		listenCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return (&net.ListenConfig{}).Listen(listenCtx, "tcp", "127.0.0.1:0")
	}
	serveSignals = func() (<-chan os.Signal, func()) {
		signals := make(chan os.Signal, 1)
		signals <- os.Interrupt
		return signals, func() {}
	}
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("process", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("RUSH_TEST_SERVE_SHUTDOWN_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"rush"}, args...)
	Execute()
	os.Exit(0)
}
