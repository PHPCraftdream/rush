package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAPITokenBindingValidation(t *testing.T) {
	cases := []struct {
		name    string
		binding APITokenBinding
		scopes  []string
		valid   bool
	}{
		{"none", APITokenBinding{Kind: "none"}, []string{"run"}, true},
		{"session", APITokenBinding{Kind: "session", Pin: &APITokenPinBinding{Session: "s", Workspace: "/w"}}, []string{"run"}, true},
		{"batch", APITokenBinding{Kind: "batch", Batch: &APITokenBatchBinding{Tools: []string{"agent"}, MaxRoots: 2, MaxParallel: 2}}, []string{"run"}, true},
		{"admin", APITokenBinding{Kind: "session", Pin: &APITokenPinBinding{Session: "s", Workspace: "/w"}}, []string{"admin"}, false},
		{"both", APITokenBinding{Kind: "session", Pin: &APITokenPinBinding{Session: "s", Workspace: "/w"}, Batch: &APITokenBatchBinding{}}, []string{"run"}, false},
		{"missing pin", APITokenBinding{Kind: "session"}, []string{"run"}, false},
		{"unknown kind", APITokenBinding{Kind: "other"}, []string{"run"}, false},
		{"negative limits", APITokenBinding{Kind: "batch", Batch: &APITokenBatchBinding{MaxRoots: -1}}, []string{"run"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.binding.Validate(tc.scopes) == nil; got != tc.valid {
				t.Fatalf("valid = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestAPITokenBindingJSONCompatibility(t *testing.T) {
	var old APIToken
	if err := json.Unmarshal([]byte(`{"id":"12345678","name":"old","scopes":["run"],"permission":"ask","created":"2026-01-01T00:00:00Z","sha256":"digest"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Binding.Normalized().Kind != "none" {
		t.Fatalf("old record binding = %#v", old.Binding)
	}
	binding := APITokenBinding{Kind: "session", Pin: &APITokenPinBinding{Session: "sess", Workspace: "/workspace", Create: true}}
	data, err := json.Marshal(APIToken{Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip APIToken
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Binding.Kind != "session" || roundTrip.Binding.Pin == nil || !roundTrip.Binding.Pin.Create {
		t.Fatalf("round trip binding = %#v", roundTrip.Binding)
	}
	if !strings.Contains(string(data), `"binding"`) {
		t.Fatalf("serialized record omitted binding: %s", data)
	}
	forward := []byte(`{"id":"87654321","name":"future","scopes":["run"],"permission":"ask","created":"2026-01-01T00:00:00Z","sha256":"digest","binding":{"kind":"batch","batch":{"tools":["agent"],"max_roots":3,"max_parallel":2},"future_policy":"preserve-on-read"},"future_record_field":true}`)
	var future APIToken
	if err := json.Unmarshal(forward, &future); err != nil {
		t.Fatal(err)
	}
	if future.Binding.Kind != "batch" || future.Binding.Batch == nil || future.Binding.Batch.MaxRoots != 3 {
		t.Fatalf("forward binding = %#v", future.Binding)
	}
	forwardRoundTrip, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	var forwardJSON map[string]json.RawMessage
	if err := json.Unmarshal(forwardRoundTrip, &forwardJSON); err != nil {
		t.Fatal(err)
	}
	if forwardJSON["binding"] == nil || forwardJSON["future_record_field"] != nil {
		t.Fatalf("forward-compatible encoding = %s", forwardRoundTrip)
	}
}

func TestAPITokenBindingCreateValidationAndClone(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
	if _, _, err := store.CreateAPITokenWithBinding("admin", []string{"admin"}, "ask", nil, APITokenBinding{Kind: "session", Pin: &APITokenPinBinding{Session: "s", Workspace: "/w"}}); err == nil {
		t.Fatal("admin binding accepted")
	}
	binding := APITokenBinding{Kind: "batch", Batch: &APITokenBatchBinding{Tools: []string{"agent"}, MaxRoots: 2, MaxParallel: 1}}
	_, created, err := store.CreateAPITokenWithBinding("batch", []string{"run"}, "ask", nil, binding)
	if err != nil {
		t.Fatal(err)
	}
	created.Binding.Batch.Tools[0] = "changed"
	loaded, err := store.ShowAPIToken(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Binding.Batch.Tools[0] != "agent" {
		t.Fatalf("stored binding mutated through returned token: %#v", loaded.Binding)
	}
}

func TestCanonicalAPIWorkspace(t *testing.T) {
	dir := t.TempDir()
	resolved, err := CanonicalAPIWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("canonical path is not absolute: %q", resolved)
	}
	if runtime.GOOS == "windows" && resolved != strings.ToLower(resolved) {
		t.Fatalf("Windows path not case-folded: %q", resolved)
	}
	if _, err := CanonicalAPIWorkspace(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("nonexistent path resolved")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err == nil {
		viaLink, err := CanonicalAPIWorkspace(link)
		if err != nil || !strings.EqualFold(viaLink, resolved) {
			t.Fatalf("symlink canonical = %q, %v; want %q", viaLink, err, resolved)
		}
	}
}
