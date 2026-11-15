package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

type serveTestListener struct {
	net.Listener
	closed bool
}

func (l *serveTestListener) Close() error {
	l.closed = true
	return nil
}

type serveStartupListener struct {
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
}

func newServeStartupListener(port int) *serveStartupListener {
	return &serveStartupListener{closed: make(chan struct{}), addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}}
}

func (l *serveStartupListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *serveStartupListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *serveStartupListener) Addr() net.Addr { return l.addr }

func serveTestSetup(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	globalDir := filepath.Join(base, "global dir")
	cwd := filepath.Join(base, "workspace")
	for _, dir := range []string{globalDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	return globalDir, cwd
}

func serveTestCommand(args ...string) *cobra.Command {
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(context.Background())
	cmd.Flags().String("cwd", "", "")
	cmd.Flags().String("global-dir", "", "")
	cmd.Flags().String("listen", "", "")
	cmd.Flags().Bool("allow-cleartext", false, "")
	cmd.Flags().Bool("print-config", false, "")
	cmd.SetArgs(args)
	return cmd
}

func normalizedServeError(value string) string {
	return strings.Join(strings.Fields(value), "")
}

func TestServeInjectedSignalsCancelRunContext(t *testing.T) {
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			signals := make(chan os.Signal, 1)
			watchServeSignals(ctx, cancel, signals)
			signals <- signal
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatalf("%s did not cancel serve context", signal)
			}
		})
	}
}

func TestServeNoUsableTokenStopsBeforeSideEffects(t *testing.T) {
	for _, fixture := range []string{"empty", "expired", "revoked", "malformed"} {
		t.Run(fixture, func(t *testing.T) {
			globalDir, cwd := serveTestSetup(t)
			if fixture == "expired" {
				oldNow := serveNow
				fixedNow := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
				serveNow = func() time.Time { return fixedNow }
				t.Cleanup(func() { serveNow = oldNow })
			}
			store := config.GlobalAPITokenStore()
			switch fixture {
			case "expired":
				expires := time.Date(2030, time.January, 2, 2, 4, 5, 0, time.UTC)
				_, record, err := store.CreateAPIToken("expired", []string{"run"}, "ask", &expires)
				if err != nil {
					t.Fatal(err)
				}
				if record.Expires == nil || !record.Expires.Equal(expires) || !record.Expires.Before(serveNow()) {
					t.Fatalf("expired token fixture was not created with a past expiry: %+v", record)
				}
			case "revoked":
				_, record, err := store.CreateAPIToken("revoked", []string{"run"}, "ask", nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.RevokeAPIToken(record.ID); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.Path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			oldListen, oldApp := serveListen, serveAppFactory
			oldConsume := serveConsumeBootstrapToken
			defer func() {
				serveListen, serveAppFactory = oldListen, oldApp
				serveConsumeBootstrapToken = oldConsume
			}()
			binds, apps := 0, 0
			serveListen = func(string) (net.Listener, error) { binds++; return nil, errors.New("unexpected bind") }
			serveAppFactory = func(*cobra.Command) (*app.App, error) { apps++; return nil, nil }
			storePath := config.GlobalAPITokenStore().Path
			serveConsumeBootstrapToken = func(store *config.APITokenStore, now time.Time) (config.APIToken, bool, error) {
				if store.Path != storePath {
					t.Fatalf("token store path=%q, want %q", store.Path, storePath)
				}
				return config.APIToken{}, false, nil
			}
			cmd := serveTestCommand()
			_ = cmd.Flags().Set("cwd", cwd)
			_ = cmd.Flags().Set("global-dir", globalDir)
			err := runServe(cmd, nil)
			if err == nil {
				t.Fatal("expected token-check error")
			}
			wantCode := 2
			if fixture == "malformed" {
				wantCode = 1
			}
			if code, ok := tokenCLIExitCode(err); !ok || code != wantCode {
				t.Fatalf("exit=(%d,%v), want %d: %v", code, ok, wantCode, err)
			}
			wantGuidance := fmt.Sprintf("rush tokens create --name <n> --scope run --cwd %q --global-dir %q", cwd, globalDir)
			if !strings.Contains(normalizedServeError(err.Error()), normalizedServeError(wantGuidance)) {
				t.Fatalf("guidance=%q, want command containing %q", err, wantGuidance)
			}
			if binds != 0 || apps != 0 {
				t.Fatalf("binds=%d app factories=%d", binds, apps)
			}
		})
	}
}

func TestServeRequiresExplicitCwd(t *testing.T) {
	serveTestSetup(t)
	cmd := serveTestCommand()
	err := runServe(cmd, nil)
	if code, ok := tokenCLIExitCode(err); !ok || code != 2 || !strings.Contains(err.Error(), "requires an explicit --cwd") {
		t.Fatalf("error=%v, exit=(%d,%t), want explicit cwd usage error", err, code, ok)
	}
}

func TestServeValidTokenReachesListenerFactory(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume := serveListen, serveConsumeBootstrapToken
	defer func() { serveListen, serveConsumeBootstrapToken = oldListen, oldConsume }()
	calls := 0
	serveListen = func(string) (net.Listener, error) {
		calls++
		return nil, errors.New("listener reached")
	}
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	if err := runServe(cmd, nil); err == nil || !strings.Contains(err.Error(), "listener reached") {
		t.Fatalf("error=%v, want listener factory error", err)
	}
	if calls != 1 {
		t.Fatalf("listener factory calls=%d, want 1", calls)
	}
}

func TestServeExplicitEmptySurfacesExitsBeforeBind(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(globalDir, "config", "rush.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8081","surfaces":[]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume := serveListen, serveConsumeBootstrapToken
	defer func() { serveListen, serveConsumeBootstrapToken = oldListen, oldConsume }()
	binds := 0
	serveListen = func(string) (net.Listener, error) { binds++; return nil, errors.New("unexpected bind") }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	err := runServe(cmd, nil)
	if code, ok := tokenCLIExitCode(err); !ok || code != 2 || !strings.Contains(err.Error(), "empty surface list") {
		t.Fatalf("error=%v exit=(%d,%t), want empty-surface exit 2", err, code, ok)
	}
	if binds != 0 {
		t.Fatalf("bound %d listeners for explicitly empty surfaces", binds)
	}
}

func TestServeMissingSurfacesDefaultsAndResolvesOncePerListener(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(globalDir, "config", "rush.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"server":{"listeners":[{"addr":"localhost:8081"},{"addr":"localhost:8082"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume := serveListen, serveConsumeBootstrapToken
	oldResolver, oldAppFactory, oldShutdown := serveResolver, serveAppFactory, serveAppShutdown
	defer func() {
		serveListen, serveConsumeBootstrapToken = oldListen, oldConsume
		serveResolver, serveAppFactory, serveAppShutdown = oldResolver, oldAppFactory, oldShutdown
	}()
	resolverCalls, binds := 0, 0
	serveResolver = func(host string) ([]net.IP, error) {
		resolverCalls++
		if host != "localhost" {
			t.Errorf("resolver host=%q, want localhost", host)
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	serveListen = func(address string) (net.Listener, error) {
		binds++
		if address != net.JoinHostPort("127.0.0.1", fmt.Sprint(8080+binds)) {
			t.Errorf("bind address=%q", address)
		}
		return newServeStartupListener(8080 + binds), nil
	}
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	serveAppFactory = func(*cobra.Command) (*app.App, error) { return nil, nil }
	serveAppShutdown = func(*app.App) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := serveTestCommand()
	cmd.SetContext(ctx)
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	if err := runServe(cmd, nil); err != nil {
		t.Fatalf("run serve: %v", err)
	}
	if resolverCalls != 2 || binds != 2 {
		t.Fatalf("resolver calls=%d binds=%d, want 2 each", resolverCalls, binds)
	}
}

func TestServeLateBindFailureClosesEarlierListeners(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(globalDir, "config", "rush.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8081"},{"addr":"127.0.0.1:8082"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume := serveListen, serveConsumeBootstrapToken
	defer func() { serveListen, serveConsumeBootstrapToken = oldListen, oldConsume }()
	first := &serveTestListener{}
	calls := 0
	serveListen = func(string) (net.Listener, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return nil, errors.New("late bind failure")
	}
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	if err := runServe(cmd, nil); err == nil || !strings.Contains(err.Error(), "late bind failure") {
		t.Fatalf("error=%v, want late bind failure", err)
	}
	if calls != 2 || !first.closed {
		t.Fatalf("listener calls=%d first closed=%t", calls, first.closed)
	}
}

func TestServeValidateBeforeBind(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("valid", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(globalDir, "config", "rush.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8081"},{"addr":"bad address"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume := serveListen, serveConsumeBootstrapToken
	defer func() { serveListen, serveConsumeBootstrapToken = oldListen, oldConsume }()
	binds := 0
	serveListen = func(string) (net.Listener, error) { binds++; return nil, errors.New("unexpected bind") }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	if err := runServe(cmd, nil); err == nil {
		t.Fatal("expected invalid listener error")
	}
	if binds != 0 {
		t.Fatalf("bound %d listeners before validation", binds)
	}
}

func TestServeAppFactoryPassesMCPRestrictionOption(t *testing.T) {
	options := serveAppOptions()
	if len(options) != 1 || options[0] == nil {
		t.Fatalf("serve app factory configured %d options; want one MCP restriction option", len(options))
	}
	got := reflect.ValueOf(options[0]).Pointer()
	want := reflect.ValueOf(app.RestrictMCPToCLI()).Pointer()
	if got != want {
		t.Fatal("serve app factory option is not app.RestrictMCPToCLI")
	}
}

func TestServeBootstrapTokenCountsAsUsable(t *testing.T) {
	globalDir, cwd := serveTestSetup(t)
	store := config.GlobalAPITokenStore()
	oldListen, oldConsume := serveListen, serveConsumeBootstrapToken
	defer func() { serveListen, serveConsumeBootstrapToken = oldListen, oldConsume }()
	bound := 0
	serveListen = func(string) (net.Listener, error) { bound++; return nil, errors.New("stop after token check") }
	storePath := store.Path
	serveConsumeBootstrapToken = func(got *config.APITokenStore, _ time.Time) (config.APIToken, bool, error) {
		if got.Path != storePath {
			t.Fatalf("token store path=%q, want %q", got.Path, storePath)
		}
		getenv := func(key string) string {
			if key == "RUSH_SERVE_TOKEN_SHA256" {
				return "1234abcd:" + strings.Repeat("a", 64)
			}
			return "run"
		}
		unsetenv := func(string) error { return nil }
		return config.ConsumeServeBootstrapToken(store, getenv, unsetenv, time.Now())
	}
	cmd := serveTestCommand()
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	if err := runServe(cmd, nil); err == nil {
		t.Fatal("expected injected bind failure")
	}
	if bound != 1 {
		t.Fatalf("listener calls=%d, want one", bound)
	}
}

func TestServeGuidanceGlobalDirIsQuoted(t *testing.T) {
	err := tokenStoreGuidance(errors.New("store failure"), `C:\work dir`, `D:\global dir`)
	if !strings.Contains(err.Error(), `rush tokens create --name <n> --scope run --cwd "C:\\work dir" --global-dir "D:\\global dir"`) {
		t.Fatalf("unquoted cwd guidance: %v", err)
	}
}
