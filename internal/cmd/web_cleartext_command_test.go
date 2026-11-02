package cmd

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/server"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type cleartextCommandCoordinator struct {
	agent.Coordinator
}

func (cleartextCommandCoordinator) SetPersistentMode(bool) {}
func (cleartextCommandCoordinator) CancelAll() bool        { return false }

func TestWebRefusesNonLoopbackBeforeAppSetup(t *testing.T) {
	for _, args := range [][]string{{"--host", "0.0.0.0"}, {"web", "--host", "0.0.0.0"}} {
		base := t.TempDir()
		code, stdout, stderr := runWebExecuteProcess(t, base, args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "--allow-cleartext") || !strings.Contains(stderr, "--host localhost") || !strings.Contains(stderr, "SSH port forwarding") {
			t.Fatalf("Execute(%v): code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		assertWebRefusalHasNoArtifacts(t, filepath.Join(base, "global"))
	}
}

func resetWebCleartextCommandFlags(t *testing.T) {
	t.Helper()
	for _, flags := range []*pflag.FlagSet{
		rootCmd.Flags(), rootCmd.PersistentFlags(),
		webCmd.Flags(), webCmd.PersistentFlags(),
	} {
		flags.VisitAll(func(flag *pflag.Flag) {
			if err := flag.Value.Set(flag.DefValue); err != nil {
				t.Errorf("reset %s flag: %v", flag.Name, err)
				return
			}
			flag.Changed = false
		})
	}
}

func TestWebCleartextOptInWarnsAndBindsThroughRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global"))
	called := 0
	oldFactory, oldListen := webAppFactory, server.Listen
	webAppFactory = func(*cobra.Command) (*app.App, error) {
		called++
		return &app.App{AgentCoordinator: cleartextCommandCoordinator{}}, nil
	}
	var gotAddress string
	server.Listen = func(_ context.Context, _, address string) (net.Listener, error) {
		gotAddress = address
		return nil, errors.New("stop before real server starts")
	}
	resetWebCleartextCommandFlags(t)
	t.Cleanup(func() {
		webAppFactory, server.Listen = oldFactory, oldListen
		resetWebCleartextCommandFlags(t)
		rootCmd.SetArgs(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetContext(context.Background())
	})
	var stderr strings.Builder
	rootCmd.SetErr(&stderr)
	rootCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rootCmd.SetContext(rootCtx)
	rootCmd.SetArgs([]string{"web", "--host", "0.0.0.0", "--allow-cleartext", "--no-open"})
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "stop before real server starts") {
		t.Fatalf("Execute() = %v, want injected listener stop", err)
	}
	if called != 1 || gotAddress != "0.0.0.0:0" {
		t.Fatalf("factory calls=%d listener address=%q", called, gotAddress)
	}
	if !strings.Contains(stderr.String(), "WARNING: rush web is serving unencrypted HTTP") {
		t.Fatalf("stderr = %q, want cleartext warning", stderr.String())
	}
}
