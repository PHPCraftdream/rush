package server

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestEffectiveListenerPinsNarrowBindings(t *testing.T) {
	tokens := []config.APIToken{
		{ID: "11111111", Scopes: []string{"run"}, Permission: "ask", Binding: config.APITokenBinding{Kind: "none"}},
		{ID: "22222222", Scopes: []string{"read"}, Permission: "readonly", Binding: config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: "session-a", Workspace: t.TempDir()}}},
	}
	pins, err := EffectiveListenerPins(map[string]string{"11111111": "session-b"}, tokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pins["11111111"] != "session-b" || pins["22222222"] != "session-a" {
		t.Fatalf("effective pins=%v", pins)
	}
}

func TestEffectiveListenerPinsRejectConflictAndAdmin(t *testing.T) {
	workspace := t.TempDir()
	cases := []struct {
		name  string
		token config.APIToken
		pins  map[string]string
	}{
		{name: "conflict", token: config.APIToken{ID: "11111111", Scopes: []string{"run"}, Binding: config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: "one", Workspace: workspace}}}, pins: map[string]string{"11111111": "two"}},
		{name: "admin", token: config.APIToken{ID: "22222222", Scopes: []string{"admin"}, Binding: config.APITokenBinding{Kind: "none"}}, pins: map[string]string{"22222222": "session"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EffectiveListenerPins(tc.pins, []config.APIToken{tc.token}, nil); err == nil {
				t.Fatal("expected pin policy rejection")
			}
		})
	}
}

func TestEffectiveListenerPinsDeclaredMapLeavesUnpinnedTokensForRequestRejection(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := config.GlobalAPITokenStore()
	pinned, pinnedRecord, err := store.CreateAPIToken("declared", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	unpinned, _, err := store.CreateAPIToken("undeclared", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := app.Sessions.Create(t.Context(), "pinned")
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST},
		Pins: map[string]string{pinnedRecord.ID: sess.ID}, PinsDeclared: true,
	}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	request := func(token string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close response: %v/%v", readErr, closeErr)
		}
		return resp.StatusCode
	}
	if got := request(pinned); got != http.StatusOK {
		t.Fatalf("declared pinned token status=%d, want 200", got)
	}
	if got := request(unpinned); got != http.StatusUnauthorized {
		t.Fatalf("undeclared unpinned token status=%d, want 401", got)
	}
}

func TestValidatePinnedPermissionModesRejectsMismatch(t *testing.T) {
	tokens := []config.APIToken{{ID: "one", Permission: "ask"}, {ID: "two", Permission: "auto"}}
	if err := ValidatePinnedPermissionModes(tokens, map[string]string{"one": "session", "two": "session"}); err == nil {
		t.Fatal("expected mixed permission modes to be rejected")
	}
}
