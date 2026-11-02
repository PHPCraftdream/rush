package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/platform"
)

func TestTokenBindingJSONPresentation(t *testing.T) {
	binding := config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: "sess", Workspace: filepath.Clean("/tmp/work")}}
	got := tokenBindingPresentation(binding)
	if !strings.Contains(got, "session=sess") || !strings.Contains(got, "workspace="+filepath.Clean("/tmp/work")) {
		t.Fatalf("binding presentation = %q", got)
	}
}

func TestTokenBindingCreateNewShowListProcess(t *testing.T) {
	base := t.TempDir()
	globalDir, dataDir, configDir := filepath.Join(base, "global"), filepath.Join(base, "data"), filepath.Join(base, "config")
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	cwd := filepath.Join(base, "workspace")
	for _, dir := range []string{globalDir, dataDir, configDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(args ...string) (string, string, int) {
		t.Helper()
		argsJSON, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteHelperProcess$")
		cmd.Dir = cwd
		cmd.Env = cleanExecuteHelperEnv(os.Environ())
		cmd.Env = append(cmd.Env, executeHelperEnv+"=1", executeHelperEnv+"_ARGS="+string(argsJSON), "RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+dataDir, "RUSH_GLOBAL_CONFIG="+configDir, "HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "userprofile"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("execute %v: %v", args, err)
			}
			code = exit.ExitCode()
		}
		return stdout.String(), stderr.String(), code
	}
	createOut, createErr, code := invoke("tokens", "create", "--name", "pinned", "--pin-session", "new", "--cwd", cwd, "--json")
	if code != 0 || createErr == "" {
		t.Fatalf("create code=%d stdout=%q stderr=%q", code, createOut, createErr)
	}
	var created struct {
		Token  string          `json:"token"`
		Record config.APIToken `json:"record"`
	}
	if err := json.Unmarshal([]byte(createOut), &created); err != nil {
		t.Fatalf("create JSON: %v", err)
	}
	if created.Token == "" || created.Record.Binding.Kind != "session" || created.Record.Binding.Pin == nil || !created.Record.Binding.Pin.Create || created.Record.Binding.Pin.Session == "new" {
		t.Fatalf("created record = %#v", created.Record)
	}
	canonical, err := config.CanonicalAPIWorkspace(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if created.Record.Binding.Pin.Workspace != canonical {
		t.Fatalf("workspace=%q want %q", created.Record.Binding.Pin.Workspace, canonical)
	}
	showOut, showErr, code := invoke("tokens", "show", created.Record.ID)
	if code != 0 || showErr != "" || !strings.Contains(showOut, "session="+created.Record.Binding.Pin.Session) || !strings.Contains(showOut, "workspace="+canonical) {
		t.Fatalf("show code=%d stdout=%q stderr=%q", code, showOut, showErr)
	}
	listOut, listErr, code := invoke("tokens", "list")
	if code != 0 || listErr != "" || !strings.Contains(listOut, "session="+created.Record.Binding.Pin.Session) || !strings.Contains(listOut, canonical) {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, listOut, listErr)
	}
	persisted, err := config.GlobalAPITokenStore().ShowAPIToken(created.Record.ID)
	if err != nil || persisted.Binding.Pin == nil || persisted.Binding.Pin.Session != created.Record.Binding.Pin.Session {
		t.Fatalf("persisted record=%#v err=%v", persisted, err)
	}
}

func TestTokenCreatePinSessionErrorExitProcess(t *testing.T) {
	base := t.TempDir()
	globalDir, dataDir, configDir := filepath.Join(base, "global"), filepath.Join(base, "data"), filepath.Join(base, "config")
	cwd := filepath.Join(base, "cwd")
	for _, dir := range []string{globalDir, dataDir, configDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	argsJSON, _ := json.Marshal([]string{"tokens", "create", "--name", "valid", "--pin-session", "new"})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteHelperProcess$")
	cmd.Dir = cwd
	cmd.Env = append(cleanExecuteHelperEnv(os.Environ()), executeHelperEnv+"=1", executeHelperEnv+"_ARGS="+string(argsJSON), "RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+dataDir, "RUSH_GLOBAL_CONFIG="+configDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires --cwd") {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}
