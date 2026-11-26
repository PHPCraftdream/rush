package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
)

const executeHelperEnv = "RUSH_TEST_EXECUTE_HELPER"

func TestExecuteHelperProcess(t *testing.T) {
	if os.Getenv(executeHelperEnv) != "1" {
		base := t.TempDir()
		t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global-dir"))
		t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
		t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(executeHelperEnv+"_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"rush"}, args...)
	Execute()
	os.Exit(0)
}

func TestExecuteTokenExitAndJournal(t *testing.T) {
	base := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global-dir"))
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"not found", []string{"tokens", "show", "deadbeef"}, 4},
		{"usage error", []string{"tokens", "create", "--name", "x", "--scope", "bogus"}, 2},
		{"success", []string{"tokens", "list"}, 0},
		{"verify broken input", []string{"verify", "guards", "", "--json"}, 3},
		{"lock run child status passes through", []string{"lock", "run", "verify-exit-test", "--", "git", "--definitely-invalid-option"}, 129},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			globalDir := filepath.Join(base, "global-dir")
			dataDir := filepath.Join(base, "data")
			configDir := filepath.Join(base, "config")
			cwd := filepath.Join(base, "cwd")
			tmpDir := filepath.Join(base, "tmp")
			for _, dir := range []string{globalDir, dataDir, configDir, cwd, tmpDir} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string(nil), tc.args...)
			if tc.name == "verify broken input" {
				args[2] = filepath.Join(base, "nonexistent-repo")
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			absExecutable, err := filepath.Abs(executable)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := platform.Command(ctx, absExecutable, "-test.run=^TestExecuteHelperProcess$")
			cmd.Dir = cwd
			cmd.Env = cleanExecuteHelperEnv(os.Environ())
			cmd.Env = append(cmd.Env,
				executeHelperEnv+"=1",
				executeHelperEnv+"_ARGS="+string(argsJSON),
				"RUSH_GLOBAL_DIR="+globalDir,
				"RUSH_GLOBAL_DATA="+dataDir,
				"RUSH_GLOBAL_CONFIG="+configDir,
				"TMPDIR="+tmpDir,
				"TMP="+tmpDir,
				"TEMP="+tmpDir,
				"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
				"XDG_DATA_HOME="+filepath.Join(base, "xdg-data"),
				"HOME="+filepath.Join(base, "home"),
				"USERPROFILE="+filepath.Join(base, "userprofile"),
			)
			err = cmd.Run()
			got := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					got = exit.ExitCode()
				} else {
					t.Fatalf("helper process: %v", err)
				}
			}
			if got != tc.code {
				t.Fatalf("Execute exit=%d, want %d", got, tc.code)
			}
			journalDir := filepath.Join(globalDir, "data")
			matches, err := filepath.Glob(filepath.Join(journalDir, "audit-*.jsonl"))
			if err != nil {
				t.Fatalf("journal files in %q: %v", journalDir, err)
			}
			sort.Strings(matches)
			endCount := 0
			var endCode any
			for _, match := range matches {
				f, err := os.Open(match)
				if err != nil {
					t.Fatal(err)
				}
				scanner := bufio.NewScanner(f)
				for scanner.Scan() {
					var rec map[string]any
					if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
						_ = f.Close()
						t.Fatal(err)
					}
					if rec["kind"] == "command_end" {
						endCount++
						endCode = rec["exit"]
					}
				}
				scanErr := scanner.Err()
				closeErr := f.Close()
				if scanErr != nil {
					t.Fatal(scanErr)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if endCount != 1 || endCode != float64(tc.code) {
				t.Fatalf("journal end records=%d, last exit=%v, want exactly one with exit %d", endCount, endCode, tc.code)
			}
		})
	}
}

func TestExecuteTokensStdoutStderr(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "global-dir")
	dataDir := filepath.Join(base, "data")
	configDir := filepath.Join(base, "config")
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	cwd := filepath.Join(base, "cwd")
	for _, dir := range []string{globalDir, dataDir, configDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, string) {
		t.Helper()
		argsJSON, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		absExecutable, err := filepath.Abs(executable)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := platform.Command(ctx, absExecutable, "-test.run=^TestExecuteHelperProcess$")
		cmd.Dir = cwd
		cmd.Env = cleanExecuteHelperEnv(os.Environ())
		cmd.Env = append(cmd.Env,
			executeHelperEnv+"=1", executeHelperEnv+"_ARGS="+string(argsJSON),
			"RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+dataDir,
			"RUSH_GLOBAL_CONFIG="+configDir,
			"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
			"XDG_DATA_HOME="+filepath.Join(base, "xdg-data"),
			"HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "userprofile"),
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("Execute %v: %v; stderr=%q", args, err, stderr.String())
		}
		return stdout.String(), stderr.String()
	}

	tokenPattern := regexp.MustCompile(`^rush_[0-9a-f]{8}\.[A-Za-z0-9_-]{43}$`)
	plainOut, plainErr := run("tokens", "create", "--name", "x")
	plainLines := strings.Split(strings.TrimSuffix(plainOut, "\n"), "\n")
	if !strings.HasSuffix(plainOut, "\n") || len(plainLines) != 1 || !tokenPattern.MatchString(plainLines[0]) {
		t.Fatalf("plain create stdout = %q, want one token line", plainOut)
	}
	plainToken := plainLines[0]
	if strings.Contains(plainErr, plainToken) || plainErr == "" {
		t.Fatalf("plain token stderr invalid: %q", plainErr)
	}
	plainID := plainToken[5:13]
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(plainID) {
		t.Fatalf("plain token has invalid id: %q", plainToken)
	}

	jsonOut, jsonErr := run("tokens", "create", "--name", "y", "--json")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonOut), &fields); err != nil {
		t.Fatalf("JSON create stdout = %q, err=%v", jsonOut, err)
	}
	if len(fields) != 2 || fields["token"] == nil || fields["record"] == nil {
		t.Fatalf("JSON create stdout keys = %v, want exactly token and record", fields)
	}
	var created struct {
		Token  string `json:"token"`
		Record struct {
			ID string `json:"id"`
		} `json:"record"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &created); err != nil || created.Token == "" || created.Record.ID == "" {
		t.Fatalf("JSON create stdout = %q, err=%v", jsonOut, err)
	}
	if strings.Contains(jsonErr, created.Token) || jsonErr == "" {
		t.Fatalf("JSON token stderr invalid: %q", jsonErr)
	}
	listOut, listErr := run("tokens", "list", "--json")
	var records []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(listOut), &records); err != nil {
		t.Fatalf("JSON list stdout = %q, err=%v", listOut, err)
	}
	ids := map[string]bool{}
	for _, record := range records {
		ids[record.ID] = true
	}
	if !ids[plainID] || len(records) != 2 || !ids[created.Record.ID] || listErr != "" {
		t.Fatalf("JSON list records=%v, stderr=%q", records, listErr)
	}
	showOut, showErr := run("tokens", "show", created.Record.ID, "--json")
	var shown map[string]any
	if err := json.Unmarshal([]byte(showOut), &shown); err != nil || showErr != "" || shown["id"] != created.Record.ID {
		t.Fatalf("JSON show stdout=%q stderr=%q err=%v", showOut, showErr, err)
	}
	plainListOut, plainListErr := run("tokens", "list")
	plainListLines := strings.Split(strings.TrimSuffix(plainListOut, "\n"), "\n")
	if !strings.HasSuffix(plainListOut, "\n") || len(plainListLines) != 2 || plainListErr != "" {
		t.Fatalf("plain list stdout=%q stderr=%q", plainListOut, plainListErr)
	}
}

// TestTokensCLIDataDirAndProjectConfigAreIgnored is a subprocess decoy
// test: each subcase seeds exactly one requested input — a decoy API-token
// store passed via the --data-dir flag, a visible rush.json pointing at it,
// a hidden .rush/rush.json pointing at it, or a malformed visible config —
// and the tokens command must ignore all of them.
// Running the real cli.Execute in a fresh helper process keeps the shared
// in-process root command and journal state untouched. Workspace and decoy
// directories are compared with full key membership before/after, so
// deletions (including whole subdirectories) are caught, not just edits.
const executeTokensDecoyToken = "rush_01020304.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func executeTokensSeedDecoy(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(executeTokensDecoyToken))
	doc := `{"version":1,"tokens":[{"id":"01020304","name":"decoy","scopes":["read"],"permission":"ask","created":"2030-01-01T00:00:00Z","sha256":"` + hex.EncodeToString(digest[:]) + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "api-tokens.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// executeTokensDeepSnapshot maps every entry under dir to a stable digest:
// directories to the marker "dir", files to the hex sha256 of their
// contents. Comparing with maps.Equal checks actual key membership, so a
// deleted directory no longer compares equal to an empty one.
func executeTokensDeepSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			snapshot[rel+"/"] = "dir"
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		snapshot[rel] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestTokensCLIDataDirAndProjectConfigAreIgnored(t *testing.T) {
	decoyConfig := func(t *testing.T, path, decoy string) {
		t.Helper()
		doc, err := json.Marshal(map[string]any{"options": map[string]any{"data_directory": decoy}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, doc, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name      string
		configRel string
		args      []string
	}{
		{"data-dir flag", "", []string{"--data-dir", "", "tokens", "list"}},
		{"visible workspace config", "rush.json", []string{"tokens", "list"}},
		{"hidden workspace config", filepath.Join(".rush", "rush.json"), []string{"tokens", "list"}},
		{"malformed visible workspace config", "rush.json", []string{"tokens", "list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			globalDir := filepath.Join(base, "global-dir")
			dataDir := filepath.Join(base, "data")
			configDir := filepath.Join(base, "config")
			cwd := filepath.Join(base, "cwd")
			decoy := filepath.Join(base, "decoy")
			for _, dir := range []string{globalDir, dataDir, configDir, cwd} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			args := tc.args
			if tc.configRel != "" {
				configPath := filepath.Join(cwd, tc.configRel)
				if tc.name == "malformed visible workspace config" {
					if err := os.WriteFile(configPath, []byte("{not-json"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					decoyConfig(t, configPath, decoy)
				}
			} else {
				for i, arg := range args {
					if arg == "--data-dir" {
						args[i+1] = decoy
					}
				}
			}

			executeTokensSeedDecoy(t, decoy)
			beforeWorkspace := executeTokensDeepSnapshot(t, cwd)
			beforeDecoy := executeTokensDeepSnapshot(t, decoy)

			argsJSON, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			absExecutable, err := filepath.Abs(executable)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := platform.Command(ctx, absExecutable, "-test.run=^TestExecuteHelperProcess$")
			cmd.Dir = cwd
			cmd.Env = cleanExecuteHelperEnv(os.Environ())
			cmd.Env = append(cmd.Env,
				executeHelperEnv+"=1", executeHelperEnv+"_ARGS="+string(argsJSON),
				"RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+dataDir,
				"RUSH_GLOBAL_CONFIG="+configDir,
				"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
				"XDG_DATA_HOME="+filepath.Join(base, "xdg-data"),
				"HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "userprofile"),
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("Execute %v: %v; stdout=%q stderr=%q", args, err, stdout.String(), stderr.String())
			}
			if stdout.String() != "" || stderr.String() != "" {
				t.Fatalf("tokens list output not empty: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}

			afterWorkspace := executeTokensDeepSnapshot(t, cwd)
			afterDecoy := executeTokensDeepSnapshot(t, decoy)
			if !maps.Equal(beforeWorkspace, afterWorkspace) {
				t.Fatalf("command changed workspace state: before=%v after=%v", beforeWorkspace, afterWorkspace)
			}
			if !maps.Equal(beforeDecoy, afterDecoy) {
				t.Fatalf("command changed decoy state: before=%v after=%v", beforeDecoy, afterDecoy)
			}

			// Read the global token store from the parent with the same
			// isolated global env the child received.
			t.Setenv("RUSH_GLOBAL_DIR", globalDir)
			t.Setenv("RUSH_GLOBAL_DATA", dataDir)
			t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
			records, err := config.GlobalAPITokenStore().ListAPITokens()
			if err != nil || len(records) != 0 {
				t.Fatalf("global token store=%#v err=%v", records, err)
			}
			if _, ok, err := config.GlobalAPITokenStore().VerifyAPIToken(executeTokensDecoyToken, time.Now()); err == nil || ok {
				t.Fatalf("decoy verified: ok=%v err=%v", ok, err)
			}
		})
	}
}

func cleanExecuteHelperEnv(env []string) []string {
	keys := []string{"RUSH_GLOBAL_DIR", "RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME", "USERPROFILE", executeHelperEnv, executeHelperEnv + "_ARGS"}
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		remove := false
		for _, blocked := range keys {
			if strings.EqualFold(key, blocked) {
				remove = true
				break
			}
		}
		if !remove {
			clean = append(clean, entry)
		}
	}
	return clean
}

type createdToken struct {
	Token  string          `json:"token"`
	Record config.APIToken `json:"record"`
}

func createTokenProcess(t *testing.T, base string, args ...string) createdToken {
	t.Helper()
	code, stdout, stderr := runCLIExecuteProcess(t, base, append([]string{"tokens", "create", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("tokens create %v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	var created createdToken
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("create JSON %q: %v", stdout, err)
	}
	return created
}

func TestTokensCreateBatchBindingProcess(t *testing.T) {
	base := t.TempDir()
	created := createTokenProcess(t, base, "--name", "worker", "--batch", "--tools", "view,grep", "--tools", "grep", "--max-roots", "4", "--max-parallel", "3")
	binding := created.Record.Binding
	if binding.Kind != "batch" || binding.Batch == nil || binding.Pin != nil {
		t.Fatalf("binding = %#v", binding)
	}
	if got := strings.Join(binding.Batch.Tools, ","); got != "view,grep" {
		t.Fatalf("tools = %q, want exact names deduplicated in order", got)
	}
	if binding.Batch.MaxRoots != 4 || binding.Batch.MaxParallel != 3 {
		t.Fatalf("limits = %#v", binding.Batch)
	}

	code, stdout, stderr := runCLIExecuteProcess(t, base, "tokens", "show", created.Record.ID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "batch (tools=view,grep, max_roots=4, max_parallel=3)") {
		t.Fatalf("show code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = runCLIExecuteProcess(t, base, "tokens", "list")
	if code != 0 || !strings.Contains(stdout, "batch (tools=view,grep") {
		t.Fatalf("list code=%d stdout=%q", code, stdout)
	}

	plain := createTokenProcess(t, base, "--name", "plain", "--batch")
	if b := plain.Record.Binding; b.Kind != "batch" || b.Batch == nil || len(b.Batch.Tools) != 0 || b.Batch.MaxRoots != 0 {
		t.Fatalf("a bare --batch must default every limit: %#v", b)
	}
}

func TestTokensCreateBatchUsageErrorsProcess(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"with a session pin", []string{"--batch", "--pin-session", "new"}, "cannot be combined with --pin-session"},
		{"with admin scope", []string{"--batch", "--scope", "admin"}, "cannot be combined with the admin scope"},
		{"wildcard tool", []string{"--batch", "--tools", "mcp_*"}, "no wildcards"},
		{"empty tool entry", []string{"--batch", "--tools", "view,,grep"}, "no wildcards"},
		{"negative roots", []string{"--batch", "--max-roots", "-1"}, "must not be negative"},
		{"negative parallel", []string{"--batch", "--max-parallel", "-2"}, "must not be negative"},
		{"tools without batch", []string{"--tools", "view"}, "require --batch"},
		{"roots without batch", []string{"--max-roots", "2"}, "require --batch"},
		{"parallel without batch", []string{"--max-parallel", "2"}, "require --batch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			args := append([]string{"tokens", "create", "--name", "x"}, tc.args...)
			code, stdout, stderr := runCLIExecuteProcess(t, base, args...)
			if code != 2 || stdout != "" || !strings.Contains(strings.ToLower(stderr), strings.ToLower(tc.want)) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want exit 2 mentioning %q", code, stdout, stderr, tc.want)
			}
			// Nothing was stored.
			code, stdout, _ = runCLIExecuteProcess(t, base, "tokens", "list")
			if code != 0 || strings.TrimSpace(stdout) != "" {
				t.Fatalf("a refused create must not store a token: code=%d stdout=%q", code, stdout)
			}
		})
	}
}

func TestServeRouteFlagProcess(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLIExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config", "--route", "batch")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "route: batch (source: flag)") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = runCLIExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config")
	if code != 0 || !strings.Contains(stdout, "route: full (source: default)") {
		t.Fatalf("default route: code=%d stdout=%q", code, stdout)
	}
	code, stdout, stderr = runCLIExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config", "--route", "everything")
	if code != 2 || stdout != "" || !strings.Contains(strings.ToLower(stderr), `invalid --route "everything"`) {
		t.Fatalf("invalid route: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// A route set in the global config file reaches the policy (and the printed
// configuration) like any other listener field.
func TestServeRouteFromConfigFileProcess(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	configDir := filepath.Join(base, "isolated global", "config")
	for _, dir := range []string{cwd, configDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configDir, "rush.json"), []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8443","route":"batch"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLIExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "route: batch (source: global config)") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// A flag overrides the file.
	code, stdout, _ = runCLIExecuteProcess(t, base, "serve", "--cwd", cwd, "--print-config", "--route", "full")
	if code != 0 || !strings.Contains(stdout, "route: full (source: flag)") {
		t.Fatalf("flag override: code=%d stdout=%q", code, stdout)
	}
}

// route batch and a listener pin exclude each other; the refusal happens
// before anything listens.
func TestServeRouteBatchWithPinsIsRefusedProcess(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	created := createTokenProcess(t, base, "--name", "pinme")
	code, stdout, stderr := runCLIExecuteProcess(t, base, "serve", "--cwd", cwd, "--route", "batch", "--pin-session", created.Record.ID+"=some-session")
	if code != 2 || stdout != "" || !strings.Contains(strings.ToLower(stderr), "route batch cannot be combined with session pins") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
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

func runCLIExecuteProcess(t *testing.T, base string, args ...string) (int, string, string) {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, executable, "-test.run=^TestExecuteHelperProcess$")
	cwd := filepath.Join(base, "cwd")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd.Dir = cwd
	cmd.Env = cleanExecuteHelperEnv(os.Environ())
	globalDir := filepath.Join(base, "isolated global")
	cmd.Env = append(cmd.Env, executeHelperEnv+"=1", executeHelperEnv+"_ARGS="+string(argsJSON), "RUSH_GLOBAL_DIR="+globalDir, "RUSH_GLOBAL_DATA="+filepath.Join(base, "data"), "RUSH_GLOBAL_CONFIG="+filepath.Join(base, "config"), "XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"), "XDG_DATA_HOME="+filepath.Join(base, "xdg-data"), "HOME="+filepath.Join(base, "home"), "USERPROFILE="+filepath.Join(base, "userprofile"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("helper process: %v", err)
		}
	}
	return code, stdout.String(), stderr.String()
}

// TestTokensCommandRegisteredOnRoot pins tokens subtree registration on the
// actual CLI tree after its owner moved to admin/settings.
func TestTokensCommandRegisteredOnRoot(t *testing.T) {
	root := NewRootCmd()
	command, _, err := root.Find([]string{"tokens"})
	if err != nil || command == nil || command.Name() != "tokens" || command.CommandPath() != "rush tokens" {
		t.Fatalf("root lookup for tokens = %v, %v; want registered tokens command", command, err)
	}
}

// TestTokensInvocationIsCommandScoped uses the real command tree to rule out
// matching token text in run arguments or after Cobra's end-of-flags marker.
func TestTokensInvocationIsCommandScoped(t *testing.T) {
	root := NewRootCmd()
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"tokens", "list"}, true},
		{[]string{"run", "tokens"}, false},
		{[]string{"--name", "tokens", "run"}, false},
		{[]string{"--", "tokens"}, false},
	} {
		if got := cliout.IsInvocation(tc.args, "tokens"); got != tc.want {
			t.Errorf("isTokensInvocation(%q) = %v, want %v (root %p)", tc.args, got, tc.want, root)
		}
	}
}
