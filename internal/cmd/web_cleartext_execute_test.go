package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/server"
	"github.com/spf13/cobra"
)

func TestExecuteWebCleartextHelperProcess(t *testing.T) {
	if os.Getenv(executeHelperEnv+"_WEB") != "1" {
		return
	}
	base := os.Getenv(executeHelperEnv + "_BASE")
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global"))
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	webAppFactory = func(*cobra.Command) (*app.App, error) {
		return &app.App{AgentCoordinator: cleartextCommandCoordinator{}}, nil
	}
	server.Listen = func(context.Context, string, string) (net.Listener, error) {
		return nil, errors.New("injected listener failure")
	}
	os.Args = []string{"rush", "web", "--host", "0.0.0.0", "--allow-cleartext", "--no-open"}
	Execute()
	os.Exit(0)
}

func TestExecuteWebCleartextOptInStartupStreams(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"global", "data", "config", "cwd", "home", "xdg-config", "xdg-data"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteWebCleartextHelperProcess$")
	cmd.Dir = filepath.Join(base, "cwd")
	cmd.Env = cleanExecuteHelperEnv(os.Environ())
	cmd.Env = append(cmd.Env, executeHelperEnv+"_WEB=1", executeHelperEnv+"_BASE="+base,
		"HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(base, "xdg-data"))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("Execute helper unexpectedly succeeded; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(strings.ToLower(stderr.String()), "failed to get user home directory") {
		t.Fatalf("stderr unexpectedly contains home-directory error: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARNING: rush web is serving unencrypted HTTP") {
		t.Fatalf("stderr = %q, want cleartext warning", stderr.String())
	}
	if !strings.Contains(normalizedServeError(strings.ToLower(stderr.String())), normalizedServeError("injected listener failure")) {
		t.Fatalf("stderr = %q, want injected listener failure", stderr.String())
	}
}

func runWebExecuteProcess(t *testing.T, base string, args ...string) (int, string, string) {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"global", "data", "config", "cwd", "home", "userprofile", "xdg-config", "xdg-data"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteHelperProcess$")
	cmd.Dir = filepath.Join(base, "cwd")
	cmd.Env = cleanExecuteHelperEnv(os.Environ())
	cmd.Env = append(cmd.Env, executeHelperEnv+"=1", executeHelperEnv+"_ARGS="+string(argsJSON),
		"RUSH_GLOBAL_DIR="+filepath.Join(base, "global"), "RUSH_GLOBAL_DATA="+filepath.Join(base, "global", "data"),
		"RUSH_GLOBAL_CONFIG="+filepath.Join(base, "global", "config"), "HOME="+filepath.Join(base, "home"),
		"USERPROFILE="+filepath.Join(base, "userprofile"), "XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
		"XDG_DATA_HOME="+filepath.Join(base, "xdg-data"))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("Execute helper exit=%v, stdout=%q stderr=%q", runErr, stdout.String(), stderr.String())
	}
	return exitErr.ExitCode(), stdout.String(), stderr.String()
}

func assertWebRefusalHasNoArtifacts(t *testing.T, globalDir string) {
	t.Helper()
	for _, dir := range []string{globalDir, filepath.Join(globalDir, "data"), filepath.Join(globalDir, "sessions"), filepath.Join(globalDir, "config")} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := strings.ToLower(entry.Name())
			if strings.Contains(name, "db") || strings.Contains(name, "session") || strings.Contains(name, "token") {
				t.Fatalf("refusal created unexpected artifact %q", filepath.Join(dir, entry.Name()))
			}
		}
	}
}

func TestExecuteWebCleartextRefusalExitAndStreams(t *testing.T) {
	for _, args := range [][]string{{"--host", "0.0.0.0"}, {"web", "--host", "0.0.0.0"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			base := t.TempDir()
			code, stdout, stderr := runWebExecuteProcess(t, base, args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "--allow-cleartext") || !strings.Contains(stderr, "--host localhost") || !strings.Contains(stderr, "SSH port forwarding") {
				t.Fatalf("Execute(%v): code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
			}
			assertWebRefusalHasNoArtifacts(t, filepath.Join(base, "global"))
		})
	}
}
