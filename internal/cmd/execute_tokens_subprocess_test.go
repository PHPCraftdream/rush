package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			globalDir := filepath.Join(base, "global-dir")
			dataDir := filepath.Join(base, "data")
			configDir := filepath.Join(base, "config")
			cwd := filepath.Join(base, "cwd")
			for _, dir := range []string{globalDir, dataDir, configDir, cwd} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			argsJSON, err := json.Marshal(tc.args)
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
			cmd := exec.CommandContext(ctx, absExecutable, "-test.run=^TestExecuteHelperProcess$")
			cmd.Dir = cwd
			cmd.Env = cleanExecuteHelperEnv(os.Environ())
			cmd.Env = append(cmd.Env,
				executeHelperEnv+"=1",
				executeHelperEnv+"_ARGS="+string(argsJSON),
				"RUSH_GLOBAL_DIR="+globalDir,
				"RUSH_GLOBAL_DATA="+dataDir,
				"RUSH_GLOBAL_CONFIG="+configDir,
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
		cmd := exec.CommandContext(ctx, absExecutable, "-test.run=^TestExecuteHelperProcess$")
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
