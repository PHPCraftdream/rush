package serve

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/spf13/cobra"
)

// InjectShutdownFailure installs the private seams for the real CLI subprocess.
func InjectShutdownFailure(t *testing.T) {
	t.Helper()
	oldShutdown, oldFactory, oldListen, oldSignals := serveAppShutdown, serveAppFactory, serveListen, serveSignals
	t.Cleanup(func() {
		serveAppShutdown, serveAppFactory, serveListen, serveSignals = oldShutdown, oldFactory, oldListen, oldSignals
	})
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
}

// InjectStdinInterrupt lets the TLS subprocess request a graceful shutdown.
func InjectStdinInterrupt(t *testing.T) {
	t.Helper()
	oldSignals := serveSignals
	t.Cleanup(func() { serveSignals = oldSignals })
	serveSignals = func() (<-chan os.Signal, func()) {
		signals := make(chan os.Signal, 1)
		go func() {
			var input [1]byte
			if _, err := os.Stdin.Read(input[:]); err == nil {
				signals <- os.Interrupt
			}
		}()
		return signals, func() {}
	}
}
