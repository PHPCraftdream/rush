package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decodeJSONObject(t *testing.T, text string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &object); err != nil {
		t.Fatalf("not a JSON object: %v: %q", err, text)
	}
	return object
}

func findTokenFiles(t *testing.T, root, name string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == name {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// TestAPICLIEndToEndFlow drives `rush api` in real processes against a real
// `rush serve` process: profile management, the session commands, raw access,
// scope refusal, profile selection precedence and secret hygiene.
func TestAPICLIEndToEndFlow(t *testing.T) {
	f := newAPIProcessFixture(t)
	runToken := f.mintToken(t, "flow-run", "run")
	readToken := f.mintToken(t, "flow-read", "read")
	url := f.startServe(t)
	var outputs []string
	run := func(env []string, stdin string, args ...string) (string, string, int) {
		stdout, stderr, exit := f.runWithEnv(env, stdin, args...)
		outputs = append(outputs, stdout, stderr)
		return stdout, stderr, exit
	}
	mustRun := func(env []string, stdin string, args ...string) string {
		t.Helper()
		stdout, stderr, exit := run(env, stdin, args...)
		if exit != 0 {
			t.Fatalf("rush %v exit %d\nstdout=%q\nstderr=%q", args, exit, stdout, stderr)
		}
		return stdout
	}

	// Profile management.
	out := mustRun(nil, runToken+"\n", "api", "remote", "add", "main", "--url", url, "--token-stdin", "--default")
	if !strings.Contains(out, "Added remote main") {
		t.Fatalf("remote add output = %q", out)
	}
	if _, stderr, exit := run(nil, runToken+"\n", "api", "remote", "add", "main", "--url", url, "--token-stdin"); exit != 2 || strings.Contains(stderr, runToken) {
		t.Fatalf("duplicate add without --replace: exit %d stderr=%q", exit, stderr)
	}
	if _, _, exit := run(nil, "not-a-token\n", "api", "remote", "add", "bad", "--url", url, "--token-stdin"); exit != 2 {
		t.Fatalf("malformed stdin token exit = %d, want 2", exit)
	}
	mustRun(nil, "", "api", "remote", "add", "ro", "--url", url, "--token-env", "RUSH_FLOW_READ_TOKEN")
	table := mustRun(nil, "", "api", "remote", "list")
	if !strings.Contains(table, "main") || !strings.Contains(table, "ro") || !strings.Contains(table, "environment:RUSH_FLOW_READ_TOKEN") {
		t.Fatalf("remote list = %q", table)
	}
	lines := strings.Split(strings.TrimSpace(mustRun(nil, "", "api", "remote", "list", "--json")), "\n")
	if len(lines) != 2 {
		t.Fatalf("remote list --json lines = %d: %q", len(lines), lines)
	}
	defaults := 0
	for _, line := range lines {
		object := decodeJSONObject(t, line)
		if object["default"] == true {
			defaults++
			if object["name"] != "main" || object["credential_source"] != "file" {
				t.Fatalf("default row = %v", object)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("default rows = %d", defaults)
	}
	show := mustRun(nil, "", "api", "remote", "show", "main")
	if !strings.Contains(show, "Credential source: file") || !strings.Contains(show, url) {
		t.Fatalf("remote show = %q", show)
	}
	if _, _, exit := run(nil, "", "api", "remote", "show", "missing"); exit != 4 {
		t.Fatalf("show of a missing remote exit = %d, want 4", exit)
	}
	test := mustRun(nil, "", "api", "remote", "test")
	if !strings.Contains(test, "healthy") || !strings.Contains(test, "Server version:") {
		t.Fatalf("remote test = %q", test)
	}

	// Sessions.
	created := decodeJSONObject(t, mustRun(nil, "", "api", "sessions", "create", "--title", "flow-title", "--json"))
	id, _ := created["id"].(string)
	if id == "" || created["title"] != "flow-title" {
		t.Fatalf("created session = %v", created)
	}
	if listing := mustRun(nil, "", "api", "sessions", "list", "--json"); !strings.Contains(listing, id) {
		t.Fatalf("sessions list --json lacks %s: %q", id, listing)
	}
	if listing := mustRun(nil, "", "api", "sessions", "list"); !strings.Contains(listing, "flow-title") || !strings.Contains(listing, "ID") {
		t.Fatalf("sessions list table = %q", listing)
	}
	if listing := mustRun(nil, "", "api", "sessions", "list", "--all", "--json"); !strings.Contains(listing, id) {
		t.Fatalf("sessions list --all lacks %s: %q", id, listing)
	}
	got := decodeJSONObject(t, mustRun(nil, "", "api", "sessions", "get", id, "--json"))
	if got["id"] != id || got["title"] != "flow-title" {
		t.Fatalf("sessions get = %v", got)
	}
	forked := decodeJSONObject(t, mustRun(nil, "", "api", "sessions", "fork", id, "--title", "forked", "--json"))
	forkID, _ := forked["id"].(string)
	if forkID == "" || forkID == id || forked["title"] != "forked" {
		t.Fatalf("forked session = %v", forked)
	}
	if messages := mustRun(nil, "", "api", "messages", id, "--json"); strings.TrimSpace(messages) != "" {
		t.Fatalf("messages of an empty session = %q", messages)
	}
	if _, _, exit := run(nil, "", "api", "messages", "no-such-session"); exit != 4 {
		t.Fatalf("messages of a missing session exit = %d, want 4", exit)
	}
	mustRun(nil, "", "api", "models")

	// Raw access and the OpenAPI document.
	var document map[string]any
	if err := json.Unmarshal([]byte(mustRun(nil, "", "api", "openapi")), &document); err != nil || document["openapi"] == nil {
		t.Fatalf("openapi output is not an OpenAPI document: %v", err)
	}
	rawOut, rawErr, exit := run(nil, "", "api", "raw", "GET", "/sessions")
	if exit != 0 || !strings.Contains(rawOut, id) || !strings.Contains(rawErr, "HTTP 200") {
		t.Fatalf("raw GET exit %d stdout=%q stderr=%q", exit, rawOut, rawErr)
	}
	rawOut, rawErr, exit = run(nil, `{"title":"raw-created"}`, "api", "raw", "POST", "/sessions", "--data", "-")
	if exit != 0 || !strings.Contains(rawOut, "raw-created") || !strings.Contains(rawErr, "HTTP 2") {
		t.Fatalf("raw POST exit %d stdout=%q stderr=%q", exit, rawOut, rawErr)
	}
	if _, _, exit := run(nil, "", "api", "raw", "TRACE", "/sessions"); exit != 2 {
		t.Fatalf("raw with an unsupported method exit = %d, want 2", exit)
	}

	// Scope refusal and profile selection precedence.
	roEnv := []string{"RUSH_FLOW_READ_TOKEN=" + readToken}
	if listing, _, exit := run(roEnv, "", "api", "sessions", "list", "--remote", "ro", "--json"); exit != 0 || !strings.Contains(listing, id) {
		t.Fatalf("read token listing exit %d: %q", exit, listing)
	}
	if _, _, exit := run(roEnv, "", "api", "sessions", "delete", forkID, "--remote", "ro"); exit != 3 {
		t.Fatalf("read token delete exit = %d, want 3", exit)
	}
	if _, _, exit := run(append(roEnv, "RUSH_REMOTE=ro"), "", "api", "sessions", "create"); exit != 3 {
		t.Fatalf("RUSH_REMOTE=ro must select the read-only profile over the default: exit %d, want 3", exit)
	}
	if _, _, exit := run(append(roEnv, "RUSH_REMOTE=ro"), "", "api", "sessions", "create", "--remote", "main", "--json"); exit != 0 {
		t.Fatalf("--remote must beat RUSH_REMOTE: exit %d, want 0", exit)
	}
	if _, _, exit := run(roEnv, "", "api", "sessions", "get", forkID, "--remote", "ro"); exit != 0 {
		t.Fatal("the refused delete removed the session")
	}

	// Deletion, quiet mode and profile removal.
	if quiet, stderr, exit := run(nil, "", "api", "sessions", "delete", forkID, "--quiet"); exit != 0 || quiet != "" || stderr != "" {
		t.Fatalf("quiet delete exit %d stdout=%q stderr=%q", exit, quiet, stderr)
	}
	if out := mustRun(nil, "", "api", "sessions", "delete", id); !strings.Contains(out, "Deleted session "+id) {
		t.Fatalf("delete output = %q", out)
	}
	if _, _, exit := run(nil, "", "api", "sessions", "get", id); exit != 4 {
		t.Fatalf("get after delete exit = %d, want 4", exit)
	}
	if len(findTokenFiles(t, f.root, "main.token")) != 1 {
		t.Fatal("managed token file of the main profile is missing")
	}
	mustRun(nil, "", "api", "remote", "rm", "main")
	if left := findTokenFiles(t, f.root, "main.token"); len(left) != 0 {
		t.Fatalf("remote rm left the managed token file: %v", left)
	}
	if _, stderr, exit := run(nil, "", "api", "sessions", "list"); exit != 2 || !strings.Contains(stderr, "no remote selected") {
		t.Fatalf("no default after rm: exit %d stderr=%q", exit, stderr)
	}

	f.assertNoSecrets(t, []string{runToken, readToken}, outputs...)
}
