package cmd

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

func TestRunServeNilContextRequiresCwdWithoutPanicking(t *testing.T) {
	serveTestSetup(t)
	cmd := &cobra.Command{Use: "serve"}
	cmd.Flags().String("cwd", "", "")
	cmd.Flags().String("global-dir", "", "")
	cmd.Flags().String("listen", "", "")
	cmd.Flags().Bool("allow-cleartext", false, "")
	cmd.Flags().Bool("print-config", false, "")

	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("runServe panicked without a command context: %v", recovered)
			}
		}()
		err = runServe(cmd, nil)
	}()
	if code, ok := tokenCLIExitCode(err); !ok || code != 2 {
		t.Fatalf("error=%v exit=(%d,%t), want usage exit 2", err, code, ok)
	}
}

func TestRunServeNilContextStartsAndStopsOnInjectedSignal(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	oldListen, oldApp, oldShutdown, oldSignals, oldConsume := serveListen, serveAppFactory, serveAppShutdown, serveSignals, serveConsumeBootstrapToken
	t.Cleanup(func() {
		serveListen, serveAppFactory, serveAppShutdown, serveSignals, serveConsumeBootstrapToken = oldListen, oldApp, oldShutdown, oldSignals, oldConsume
	})
	listener := newServeStartupListener(8088)
	serveListen = func(string) (net.Listener, error) { return listener, nil }
	serveAppFactory = func(*cobra.Command) (*app.App, error) { return nil, nil }
	serveAppShutdown = func(*app.App) error { return nil }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	serveSignals = func() (<-chan os.Signal, func()) { return signals, func() {} }
	cmd := serveTestCommand()
	cmd.SetContext(nil) //nolint:staticcheck // A nil context is the point of this test: runServe must tolerate it.
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("runServe panicked: %v", recovered)
			}
		}()
		done <- runServe(cmd, nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe with nil context: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not stop after injected signal")
	}
}
