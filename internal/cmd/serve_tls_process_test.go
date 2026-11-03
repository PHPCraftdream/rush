package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/platform"
)

const tlsProcessHelperEnv = "RUSH_TEST_SERVE_TLS_PROCESS_HELPER"

type synchronizedTLSProcessCapture struct {
	mu sync.Mutex
	strings.Builder
}

func (capture *synchronizedTLSProcessCapture) Write(data []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.Builder.Write(data)
}

func (capture *synchronizedTLSProcessCapture) String() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.Builder.String()
}

func TestExecuteServeTLSProcessHelper(t *testing.T) {
	if os.Getenv(tlsProcessHelperEnv) != "1" {
		return
	}
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
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(tlsProcessHelperEnv+"_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"rush"}, args...)
	Execute()
	os.Exit(0)
}

func TestExecuteServeTLSRealProcessPinsAndBadPair(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	globalDir, dataDir, configDir := filepath.Join(base, "global"), filepath.Join(base, "data"), filepath.Join(base, "config")
	certPath, keyPath := filepath.Join(base, "bad-cert.pem"), filepath.Join(base, "bad-key.pem")
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "xdg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "xdg-data"))
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("USERPROFILE", filepath.Join(base, "profile"))
	token, _, err := config.GlobalAPITokenStore().CreateAPIToken("tls-child-process", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"serve", "--cwd", cwd, "--tls-self-signed", "--tls-san", "localhost", "--listen", "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	child := platform.Command(ctx, executable, "-test.run=^TestExecuteServeTLSProcessHelper$")
	child.Dir = cwd
	child.Env = cleanExecuteHelperEnv(os.Environ())
	child.Env = append(child.Env, tlsProcessHelperEnv+"=1", tlsProcessHelperEnv+"_ARGS="+string(args), "RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+dataDir, "RUSH_GLOBAL_CONFIG="+configDir, "XDG_CONFIG_HOME="+filepath.Join(base, "xdg"), "XDG_DATA_HOME="+filepath.Join(base, "xdg-data"), "HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "profile"))
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	var stderrText synchronizedTLSProcessCapture
	ready := make(chan string, 2)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if _, err := stderrText.Write([]byte(line + "\n")); err != nil {
				return
			}
			if strings.HasPrefix(line, "rush serve: listening on ") || strings.HasPrefix(line, "rush serve: TLS public-key pin: ") {
				select {
				case ready <- line:
				default:
				}
			}
		}
	}()
	stdoutDone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stdout); close(stdoutDone) }()
	exit := make(chan error, 1)
	go func() { exit <- child.Wait() }()
	var address string
	var pinLine string
	for address == "" || pinLine == "" {
		select {
		case line := <-ready:
			if strings.HasPrefix(line, "rush serve: listening on ") {
				address = strings.TrimPrefix(line, "rush serve: listening on ")
			}
			if strings.HasPrefix(line, "rush serve: TLS public-key pin: ") {
				pinLine = line
			}
		case err := <-exit:
			t.Fatalf("serve exited before TLS readiness: %v; stderr=%q", err, stderrText.String())
		case <-ctx.Done():
			t.Fatalf("timed out waiting for TLS readiness; stderr=%q", stderrText.String())
		}
	}
	pinMatch := regexp.MustCompile(`^rush serve: TLS public-key pin: ([0-9a-f]{64}) \(curl: sha256//([A-Za-z0-9+/=]+)\); leaf fingerprint: cert-sha256:[0-9a-f]{64}$`).FindStringSubmatch(pinLine)
	if len(pinMatch) != 3 {
		t.Fatalf("TLS pin line not documented form: %q", pinLine)
	}
	decoded, err := base64.StdEncoding.DecodeString(pinMatch[2])
	if err != nil || fmt.Sprintf("%x", decoded) != pinMatch[1] {
		t.Fatalf("hex/curl pins disagree: %q", pinLine)
	}
	pinnedTransport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("no certificate")
		}
		cert, parseErr := x509.ParseCertificate(rawCerts[0])
		if parseErr != nil {
			return parseErr
		}
		spki, marshalErr := x509.MarshalPKIXPublicKey(cert.PublicKey)
		if marshalErr != nil {
			return marshalErr
		}
		hash := sha256.Sum256(spki)
		if fmt.Sprintf("%x", hash) != pinMatch[1] {
			return fmt.Errorf("SPKI pin mismatch")
		}
		return nil
	}}}
	pinnedClient := &http.Client{Transport: pinnedTransport, Timeout: 10 * time.Second}
	resp, err := pinnedClient.Get("https://" + address + "/api/v1/health")
	if err != nil {
		t.Fatalf("pinned HTTPS request: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("pinned response = %d %q, read error %v", resp.StatusCode, body, readErr)
	}
	strictClient := &http.Client{Timeout: 10 * time.Second}
	if _, err := strictClient.Get("https://" + address + "/api/v1/health"); err == nil {
		t.Fatal("unpinned strict client accepted self-signed server")
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exit:
		if err != nil {
			t.Fatalf("serve shutdown: %v; stderr=%q", err, stderrText.String())
		}
	case <-ctx.Done():
		t.Fatal("serve did not stop")
	}
	select {
	case <-stderrDone:
	case <-ctx.Done():
		t.Fatal("timed out draining TLS child stderr")
	}
	select {
	case <-stdoutDone:
	case <-ctx.Done():
		t.Fatal("timed out draining TLS child stdout")
	}
	if strings.Contains(stderrText.String(), token) || strings.Contains(stderrText.String(), token[strings.IndexByte(token, '.')+1:]) || strings.Contains(string(body), token) {
		t.Fatal("plaintext API token appeared in TLS response or stderr")
	}
	certPath, keyPath = filepath.Join(base, "bad-cert.pem"), filepath.Join(base, "bad-key.pem")
	secret := "PRIVATE_KEY_MATERIAL_MUST_NOT_LEAK"
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	badArgs, _ := json.Marshal([]string{"serve", "--cwd", cwd, "--tls-cert", certPath, "--tls-key", keyPath, "--listen", "127.0.0.1:0"})
	bad := platform.Command(ctx, executable, "-test.run=^TestExecuteServeTLSProcessHelper$")
	bad.Dir = cwd
	bad.Env = cleanExecuteHelperEnv(os.Environ())
	bad.Env = append(bad.Env, tlsProcessHelperEnv+"=1", tlsProcessHelperEnv+"_ARGS="+string(badArgs), "RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+dataDir, "RUSH_GLOBAL_CONFIG="+configDir)
	var badOut, badErr strings.Builder
	bad.Stdout, bad.Stderr = &badOut, &badErr
	badExit := 0
	if err := bad.Run(); err != nil {
		if processErr, ok := err.(*exec.ExitError); ok {
			badExit = processErr.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	if badExit == 0 || !strings.Contains(strings.ToLower(badErr.String()), "certificate") {
		t.Fatalf("bad TLS pair exit=%d stderr=%q", badExit, badErr.String())
	}
	if strings.Contains(badErr.String(), secret) {
		t.Fatal("TLS key material leaked to stderr")
	}
}
