package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

func TestNewServeAPIListenerPinsProductionOwnershipAndLogging(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	app := newAttachmentsTestApp(t, workingDir, dataDir)
	store := config.GlobalAPITokenStore()
	own, err := app.Sessions.Create(t.Context(), "own")
	if err != nil {
		t.Fatal(err)
	}
	child, err := app.Sessions.CreateTaskSession(t.Context(), "pin-child", own.ID, "child")
	if err != nil {
		t.Fatal(err)
	}
	foreignRoot := filepath.Join(t.TempDir(), "other-checkout")
	foreignService := session.NewServiceWithWorkspace(db.New(app.DB()), app.DB(), nil, nil, foreignRoot, foreignRoot, false)
	foreign, err := foreignService.Create(t.Context(), "foreign")
	if err != nil {
		t.Fatal(err)
	}

	token, record, err := store.CreateAPIToken("production pin", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignToken, foreignRecord, err := store.CreateAPITokenWithBinding("foreign workspace token", []string{"read"}, "ask", nil, config.APITokenBinding{
		Kind: "session", Pin: &config.APITokenPinBinding{Session: own.ID, Workspace: foreignRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, missingRecordInfo, err := store.CreateAPITokenWithBinding("missing record pin", []string{"read"}, "ask", nil, config.APITokenBinding{
		Kind: "session", Pin: &config.APITokenPinBinding{Session: "missing-record-session", Workspace: workingDir},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST},
		Pins:         map[string]string{record.ID: own.ID, foreignRecord.ID: own.ID},
		DeclaredPins: map[string]string{record.ID: own.ID, foreignRecord.ID: own.ID}, PinsDeclared: true,
		Workspace: workingDir,
	}
	if err := ValidateEffectivePins(t.Context(), app, []config.APIToken{record, foreignRecord}, []ListenerPolicy{policy}, nil); err != nil {
		t.Fatalf("validate effective pins through real App startup state: %v", err)
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
	request := func(bearer string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
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

	if got := request(token); got != http.StatusOK {
		t.Fatalf("own top-level listener pin status=%d, want 200", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	foreignReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignReq.Header.Set("Authorization", "Bearer "+foreignToken)
	foreignResp, err := (&http.Client{Timeout: 5 * time.Second}).Do(foreignReq)
	if err != nil {
		t.Fatal(err)
	}
	foreignBody, readErr := io.ReadAll(foreignResp.Body)
	closeErr := foreignResp.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close foreign response: %v/%v", readErr, closeErr)
	}
	if foreignResp.StatusCode != http.StatusForbidden || !strings.Contains(string(foreignBody), "token-not-for-this-workspace") {
		t.Fatalf("foreign workspace response=%d %q", foreignResp.StatusCode, foreignBody)
	}
	for _, tc := range []struct {
		name string
		id   string
	}{
		{name: "missing", id: "missing-session"},
		{name: "child", id: child.ID},
		{name: "foreign workspace", id: foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badToken, badRecord, err := store.CreateAPIToken("listener pin "+tc.name, []string{"read"}, "ask", nil)
			if err != nil {
				t.Fatal(err)
			}
			badPolicy := ListenerPolicy{Pins: map[string]string{badRecord.ID: tc.id}, DeclaredPins: map[string]string{badRecord.ID: tc.id}}
			if err := ValidateEffectivePins(t.Context(), app, []config.APIToken{badRecord}, []ListenerPolicy{badPolicy}, nil); err == nil {
				t.Fatalf("pin %q should be refused at startup", tc.id)
			}
			if got := request(badToken); got != http.StatusUnauthorized {
				t.Fatalf("undeclared listener token status=%d, want 401", got)
			}
		})
	}

	if err := ValidateEffectivePins(t.Context(), app, []config.APIToken{missingRecordInfo}, nil, slog.Default()); err != nil {
		t.Fatalf("missing record pin should warn instead of refusing: %v", err)
	}
	if !strings.Contains(logs.String(), "missing-record-session") {
		t.Fatalf("missing record pin warning absent from logs: %q", logs.String())
	}
	for _, secretToken := range []string{token, foreignToken} {
		secret := secretToken[strings.IndexByte(secretToken, '.')+1:]
		if strings.Contains(logs.String(), secretToken) || strings.Contains(logs.String(), secret) {
			t.Fatal("production listener pin logs contain plaintext token secret")
		}
	}
}

func TestNewServeAPIListenerPinsUnpinnedRequestRefusedThroughProductionChain(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := config.GlobalAPITokenStore()
	pinned, pinnedRecord, err := store.CreateAPIToken("pinned", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	unpinned, _, err := store.CreateAPIToken("unpinned", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	own, err := app.Sessions.Create(t.Context(), "own")
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST},
		Pins: map[string]string{pinnedRecord.ID: own.ID}, PinsDeclared: true,
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
	request := func(bearer string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := request(pinned); got != http.StatusOK {
		t.Fatalf("pinned token status=%d, want 200", got)
	}
	if got := request(unpinned); got != http.StatusUnauthorized {
		t.Fatalf("unpinned token status=%d, want 401", got)
	}
}
