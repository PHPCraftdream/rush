package cmd

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCancelSignals(t *testing.T) {
	tests := []struct {
		name string
		sig  os.Signal
		want bool
	}{
		{name: "interrupt", sig: os.Interrupt, want: true},
		{name: "terminate", sig: syscall.SIGTERM, want: true},
		{name: "kill", sig: os.Kill, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found := false
			for _, sig := range cancelSignals {
				if sig == tt.sig {
					found = true
					break
				}
			}
			require.Equal(t, tt.want, found)
		})
	}
}

func TestCancelSignalsCatchSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows console processes do not receive SIGTERM")
	}

	// Keep the revert-check mutant safe if it omits SIGTERM from our context.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	ctx, stop := signal.NotifyContext(context.Background(), cancelSignals...)
	defer stop()
	proc, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, proc.Signal(syscall.SIGTERM))

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled by SIGTERM")
	}
}

func TestRunAndLoginSignalCallsExcludeSIGKILL(t *testing.T) {
	callWithKill := regexp.MustCompile(`(?s)signal\.Notify(?:Context)?\s*\((?:[^()]|\([^()]*\))*os\.Kill`)
	for _, name := range []string{"run.go", "login.go"} {
		t.Run(name, func(t *testing.T) {
			contents, err := os.ReadFile(filepath.Clean(name))
			require.NoError(t, err)
			require.NotRegexp(t, callWithKill, string(contents))
		})
	}
}
