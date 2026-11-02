package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/platform"
)

// apiProcessFixture keeps all API CLI global state, the CLI working directory
// and the served workspace beneath one disposable root. HOME and USERPROFILE
// intentionally equal the root: on Windows remote token files must live under
// the user profile without symlinks.
type apiProcessFixture struct {
	root, globalDir, dataDir, configDir, cwd, workspace string
}

func newAPIProcessFixture(t *testing.T) *apiProcessFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &apiProcessFixture{
		root: root, globalDir: filepath.Join(root, "global"),
		dataDir:   filepath.Join(root, "global", "data"),
		configDir: filepath.Join(root, "global", "config"),
		cwd:       filepath.Join(root, "cwd"),
		workspace: filepath.Join(root, "workspace"),
	}
	for _, dir := range []string{f.cwd, f.workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{
		"RUSH_GLOBAL_DIR": f.globalDir, "RUSH_GLOBAL_DATA": f.dataDir, "RUSH_GLOBAL_CONFIG": f.configDir,
		"XDG_CONFIG_HOME": filepath.Join(root, "xdg-config"), "XDG_DATA_HOME": filepath.Join(root, "xdg-data"),
		"HOME": root, "USERPROFILE": root,
	} {
		t.Setenv(key, value)
	}
	return f
}

const apiProcessHelperEnv = "RUSH_TEST_API_PROCESS_HELPER"
const apiProcessArgsEnv = apiProcessHelperEnv + "_ARGS"

// TestAPIProcessHelperProcess re-executes the production Execute() path.
func TestAPIProcessHelperProcess(t *testing.T) {
	if os.Getenv(apiProcessHelperEnv) != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(apiProcessArgsEnv)), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"rush"}, args...)
	Execute()
	os.Exit(0)
}

func (f *apiProcessFixture) baseEnv() []string {
	return append(cleanExecuteHelperEnv(os.Environ()),
		"RUSH_GLOBAL_DIR="+f.globalDir, "RUSH_GLOBAL_DATA="+f.dataDir, "RUSH_GLOBAL_CONFIG="+f.configDir,
		"XDG_CONFIG_HOME="+filepath.Join(f.root, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(f.root, "xdg-data"),
		"HOME="+f.root, "USERPROFILE="+f.root)
}

// run executes `rush <args>` in a fresh process inside the empty cwd.
func (f *apiProcessFixture) run(stdin string, args ...string) (stdout, stderr string, exit int) {
	return f.runWithEnv(nil, stdin, args...)
}

func (f *apiProcessFixture) runWithEnv(extra []string, stdin string, args ...string) (stdout, stderr string, exit int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return "", err.Error(), -1
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", err.Error(), -1
	}
	cmd := platform.Command(ctx, executable, "-test.run=^TestAPIProcessHelperProcess$")
	cmd.Dir = f.cwd
	cmd.Env = append(f.baseEnv(), apiProcessHelperEnv+"=1", apiProcessArgsEnv+"="+string(argsJSON))
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.String(), errOut.String(), 0
	case errors.As(err, &exitErr):
		return out.String(), errOut.String(), exitErr.ExitCode()
	default:
		return out.String(), errOut.String() + err.Error(), -1
	}
}

// mintToken creates a token through the real token store of the fixture's
// global data directory (the same store a started `rush serve` reads).
func (f *apiProcessFixture) mintToken(t *testing.T, name string, scopes ...string) string {
	t.Helper()
	token, _, err := config.GlobalAPITokenStore().CreateAPIToken(name, scopes, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// startServe launches the real `rush serve` command in a child process (the
// helper of serve_api_process_test.go) and returns its REST base URL. Mint
// tokens BEFORE calling it.
func (f *apiProcessFixture) startServe(t *testing.T) string {
	t.Helper()
	args, err := json.Marshal([]string{"serve", "--cwd", f.workspace, "--listen", "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteServeAPIProcessHelper$")
	cmd.Dir = f.workspace
	cmd.Env = append(f.baseEnv(), serveProcessHelperEnv+"=1", serveProcessHelperEnv+"_ARGS="+string(args))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	var stderrText strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			stderrText.WriteString(line + "\n")
			if address, ok := strings.CutPrefix(line, "rush serve: listening on "); ok {
				select {
				case ready <- address:
				default:
				}
			}
		}
	}()
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	exit := make(chan error, 1)
	go func() { exit <- cmd.Wait() }()
	t.Cleanup(func() {
		_, _ = stdin.Write([]byte{1})
		select {
		case <-exit:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-exit
		}
		<-stderrDone
		_ = stdin.Close()
		cancel()
	})
	select {
	case address := <-ready:
		return "http://" + address
	case err := <-exit:
		<-stderrDone
		t.Fatalf("serve exited before readiness: %v; stderr=%q", err, stderrText.String())
	case <-time.After(2 * time.Minute):
		t.Fatalf("serve not ready; stderr=%q", stderrText.String())
	}
	return ""
}

// assertNoSecrets fails when a secret (or the secret half of a token) appears
// in any given output or in any regular file below the fixture root, except
// the managed remote token files, which exist to hold the credential.
func (f *apiProcessFixture) assertNoSecrets(t *testing.T, secrets []string, outputs ...string) {
	t.Helper()
	var needles []string
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		needles = append(needles, secret)
		if _, tail, ok := strings.Cut(secret, "."); ok && tail != "" {
			needles = append(needles, tail)
		}
	}
	for _, output := range outputs {
		for _, needle := range needles {
			if strings.Contains(output, needle) {
				t.Errorf("plaintext secret appeared in process output")
			}
		}
	}
	err := filepath.WalkDir(f.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "remotes" && strings.HasSuffix(path, ".token") {
			return nil
		}
		if info, err := entry.Info(); err != nil || info.Size() > 16<<20 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // locked by the running server; not a leak
		}
		for _, needle := range needles {
			if strings.Contains(string(data), needle) {
				t.Errorf("plaintext secret appeared in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Errorf("scan fixture: %v", err)
	}
}
