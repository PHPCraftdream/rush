package serve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
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
	if code, ok := cliout.ErrorExitCode(err, "tokens"); !ok || code != 1 || err.Error() != "injected shutdown failure" {
		t.Fatalf("error=%v, exit=(%d,%t), want injected shutdown failure with code 1", err, code, ok)
	}
}
