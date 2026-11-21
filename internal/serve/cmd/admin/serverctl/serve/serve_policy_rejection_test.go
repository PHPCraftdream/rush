package serve

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/spf13/cobra"
)

func writeServePolicyConfig(t *testing.T, globalDir, body string) {
	t.Helper()
	configPath := filepath.Join(globalDir, "config", "rush.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServeDuplicateListenerAddressesRejectedBeforeSideEffects(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	writeServePolicyConfig(t, globalDir, `{"server":{"listeners":[{"addr":"127.0.0.1:8181"},{"addr":"127.0.0.1:8181"}]}}`)
	oldListen, oldApp, oldConsume := serveListen, serveAppFactory, serveConsumeBootstrapToken
	t.Cleanup(func() { serveListen, serveAppFactory, serveConsumeBootstrapToken = oldListen, oldApp, oldConsume })
	binds, apps := 0, 0
	serveListen = func(string) (net.Listener, error) { binds++; return nil, errors.New("unexpected bind") }
	serveAppFactory = func(*cobra.Command) (*app.App, error) { apps++; return nil, nil }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	err := runServe(cmd, nil)
	if code, ok := cliout.ErrorExitCode(err, "tokens"); !ok || code != 2 || !strings.Contains(err.Error(), "duplicate address") || !strings.Contains(err.Error(), "127.0.0.1:8181") {
		t.Fatalf("error=%v exit=(%d,%t), want duplicate-address usage error", err, code, ok)
	}
	if binds != 0 || apps != 0 {
		t.Fatalf("listener factory calls=%d app factory calls=%d, want zero", binds, apps)
	}
}

func TestServeUISurfaceRejectedBeforeSideEffects(t *testing.T) {
	for _, surfaces := range []string{`["rest","ui"]`, `["ui"]`} {
		t.Run(surfaces, func(t *testing.T) {
			globalDir, cwd := serveTestSetup(t)
			if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
				t.Fatal(err)
			}
			writeServePolicyConfig(t, globalDir, `{"server":{"listeners":[{"addr":"127.0.0.1:8182","surfaces":`+surfaces+`}]}}`)
			oldListen, oldApp, oldConsume := serveListen, serveAppFactory, serveConsumeBootstrapToken
			t.Cleanup(func() { serveListen, serveAppFactory, serveConsumeBootstrapToken = oldListen, oldApp, oldConsume })
			binds, apps := 0, 0
			serveListen = func(string) (net.Listener, error) { binds++; return nil, errors.New("unexpected bind") }
			serveAppFactory = func(*cobra.Command) (*app.App, error) { apps++; return nil, nil }
			serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
				return config.APIToken{}, false, nil
			}
			cmd := serveTestCommand()
			_ = cmd.Flags().Set("cwd", cwd)
			_ = cmd.Flags().Set("global-dir", globalDir)
			err := runServe(cmd, nil)
			if code, ok := cliout.ErrorExitCode(err, "tokens"); !ok || code != 2 || !strings.Contains(err.Error(), "UI surface") {
				t.Fatalf("error=%v exit=(%d,%t), want UI-surface usage error", err, code, ok)
			}
			if binds != 0 || apps != 0 {
				t.Fatalf("listener factory calls=%d app factory calls=%d, want zero", binds, apps)
			}
		})
	}
}
