package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/version"
)

const serveProcessHelperEnv = "RUSH_TEST_SERVE_PROCESS_HELPER"

func TestExecuteServeAPIProcessHelper(t *testing.T) {
	if os.Getenv(serveProcessHelperEnv) != "1" {
		t.Skip("subprocess helper; exercised by TestExecuteServeAPIRealProcess")
	}
	serveSignals = func() (<-chan os.Signal, func()) {
		signals := make(chan os.Signal, 1)
		go func() {
			var b [1]byte
			if _, err := os.Stdin.Read(b[:]); err == nil {
				signals <- os.Interrupt
			}
		}()
		return signals, func() {}
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(serveProcessHelperEnv+"_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"rush"}, args...)
	Execute()
	os.Exit(0)
}

func TestExecuteServeAPIRealProcess(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "global")
	dataDir := filepath.Join(base, "data")
	configDir := filepath.Join(base, "config")
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	token, _, err := config.GlobalAPITokenStore().CreateAPIToken("serve-process", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"serve", "--cwd", cwd, "--listen", "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), serveTestWait)
	defer cancel()
	cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteServeAPIProcessHelper$")
	cmd.Dir = cwd
	cmd.Env = cleanExecuteHelperEnv(os.Environ())
	cmd.Env = append(cmd.Env,
		serveProcessHelperEnv+"=1", serveProcessHelperEnv+"_ARGS="+string(args),
		"RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+dataDir, "RUSH_GLOBAL_CONFIG="+configDir,
		"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(base, "xdg-data"),
		"HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "userprofile"),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var stderrText strings.Builder
	ready := make(chan string, 1)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			stderrText.WriteString(line + "\n")
			if strings.HasPrefix(line, "rush serve: listening on ") {
				select {
				case ready <- strings.TrimPrefix(line, "rush serve: listening on "):
				default:
				}
			}
		}
	}()
	outDone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stdout); close(outDone) }()
	exit := make(chan error, 1)
	go func() { exit <- cmd.Wait() }()
	var address string
	select {
	case address = <-ready:
	case err := <-exit:
		select {
		case <-stderrDone:
		case <-ctx.Done():
			t.Fatal("timed out draining stderr after early exit")
		}
		t.Fatalf("serve exited before readiness: %v; stderr=%q", err, stderrText.String())
	case <-ctx.Done():
		t.Fatal("timed out waiting for serve readiness")
	}
	client := &http.Client{Timeout: serveTestWait}
	request := func(uri, bearer string) (int, []byte) {
		t.Helper()
		requestCtx, requestCancel := context.WithTimeout(ctx, serveTestWait)
		defer requestCancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+address+uri, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close response: %v/%v", readErr, closeErr)
		}
		return resp.StatusCode, body
	}
	if status, body := request("/api/v1/health", ""); status != http.StatusOK || string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("health = %d %q", status, body)
	}
	status, body := request("/api/v1/version", token)
	var versionResponse struct {
		Version string `json:"version"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &versionResponse) != nil || versionResponse.Version != version.VersionLine() {
		t.Fatalf("version = %d %q, expected version %q", status, body, version.VersionLine())
	}
	if status, _ := request("/api/v1/version", ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated version status = %d", status)
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exit:
		select {
		case <-stderrDone:
		case <-ctx.Done():
			t.Fatal("timed out draining stderr after failed exit")
		}
		if err != nil {
			t.Fatalf("serve exit: %v; stderr=%q", err, stderrText.String())
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for clean serve exit")
	}
	_ = stdin.Close()
	select {
	case <-stderrDone:
	case <-ctx.Done():
		t.Fatal("timed out draining stderr")
	}
	select {
	case <-outDone:
	case <-ctx.Done():
		t.Fatal("timed out draining stdout")
	}
	if strings.Contains(stderrText.String(), token) || strings.Contains(stderrText.String(), token[strings.IndexByte(token, '.')+1:]) {
		t.Fatal("token plaintext or secret appeared on stderr")
	}
}
