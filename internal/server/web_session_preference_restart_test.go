package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// This exercises the browser's real model-save and send handlers around an
// App/SQLite reopen. The provider is an actual HTTP OpenAI-compatible endpoint,
// so the assertion covers call construction and delivery, not a fake coordinator.
func TestWebSessionModelPreferenceSurvivesRestartAndSends(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	workingDir, dataDir := t.TempDir(), t.TempDir()
	providerRequests := make(chan []byte, 2)
	providerRelease := make(chan struct{})
	var releaseProvider sync.Once
	releaseProviderCalls := func() { releaseProvider.Do(func() { close(providerRelease) }) }
	t.Cleanup(releaseProviderCalls)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Background title generation runs on the fast slot; it is not a turn.
		if bytes.Contains(body, []byte("Generate a concise title")) {
			_, _ = w.Write([]byte("data: {\"id\":\"title\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"web-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"title\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		providerRequests <- body
		<-providerRelease
		_, _ = w.Write([]byte("data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"web-test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"provider reply\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(func() {
		releaseProviderCalls()
		provider.Close()
	})

	store, err := config.Init(workingDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("web-test", config.ProviderConfig{ID: "web-test", Type: openaicompat.Name, BaseURL: provider.URL, APIKey: "test-key", Models: []catwalk.Model{{ID: "configured-model", Name: "Configured", ContextWindow: 32768, DefaultMaxTokens: 512}, {ID: "saved-model", Name: "Saved", ContextWindow: 32768, DefaultMaxTokens: 512}}})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "web-test", Model: "configured-model"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "web-test", Model: "configured-model"})
	store.SetupAgents()
	resolvedDataDir := store.Config().Options.DataDirectory
	require.NoError(t, os.MkdirAll(resolvedDataDir, 0o700))
	conn, err := db.Connect(t.Context(), resolvedDataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseAll(resolvedDataDir) })
	a, err := app.New(t.Context(), conn, store)
	require.NoError(t, err)
	t.Cleanup(a.Shutdown)
	sess, err := a.Sessions.Create(t.Context(), "web preference restart")
	require.NoError(t, err)

	client := newClient(newHub(), nil)
	payload, err := json.Marshal(SetSessionModelsPayload{SessionID: sess.ID, SmartModel: &ModelOverrideWire{Provider: "web-test", Model: "saved-model"}})
	require.NoError(t, err)
	handleSetSessionModels(t.Context(), a, client, WSMessage{ID: "save", Type: CmdSetSessionModels, Payload: payload})
	saved, err := a.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "web-test", saved.SmartModelProvider)
	require.Equal(t, "saved-model", saved.SmartModelID)

	// Close and reopen the DB-backed app, then send without a payload override:
	// handlers_agent.go must resolve the saved session preference.
	a.Shutdown()
	require.NoError(t, db.ReleaseAll(resolvedDataDir))
	conn2, err := db.Connect(t.Context(), resolvedDataDir)
	require.NoError(t, err)
	a2, err := app.New(t.Context(), conn2, store)
	require.NoError(t, err)
	t.Cleanup(a2.Shutdown)
	t.Cleanup(releaseProviderCalls)
	client2 := newClient(newHub(), nil)
	send, err := json.Marshal(SendMessagePayload{SessionID: sess.ID, Content: "please call the configured web provider"})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleSendMessage(t.Context(), a2, client2, WSMessage{ID: "send", Type: CmdSendMessage, Payload: send})
	}()
	select {
	case raw := <-providerRequests:
		var req map[string]any
		require.NoError(t, json.Unmarshal(raw, &req))
		require.Equal(t, "saved-model", req["model"])
		messages, ok := req["messages"].([]any)
		require.True(t, ok)
		encoded, _ := json.Marshal(messages)
		require.True(t, strings.Contains(string(encoded), "please call the configured web provider"))
	case <-time.After(10 * time.Second):
		t.Fatal("web send did not reach the configured HTTP provider")
	}
	// The first provider request is still blocked, so a second handler must
	// enqueue its real web send in the session agent's mailbox before release.
	queuedPrompt := "please use the saved session preference"
	queuedSend, err := json.Marshal(SendMessagePayload{SessionID: sess.ID, Content: queuedPrompt})
	require.NoError(t, err)
	queuedDone := make(chan struct{})
	go func() {
		defer close(queuedDone)
		handleSendMessage(t.Context(), a2, client2, WSMessage{ID: "queued-send", Type: CmdSendMessage, Payload: queuedSend})
	}()
	select {
	case <-queuedDone:
	case <-time.After(10 * time.Second):
		t.Fatal("queued web send handler did not finish")
	}
	require.Equal(t, 1, a2.AgentCoordinator.QueuedPrompts(sess.ID))
	releaseProviderCalls()
	select {
	case raw := <-providerRequests:
		var req map[string]any
		require.NoError(t, json.Unmarshal(raw, &req))
		require.Equal(t, "saved-model", req["model"])
		encoded, _ := json.Marshal(req["messages"])
		require.Contains(t, string(encoded), queuedPrompt)
	case <-time.After(10 * time.Second):
		t.Fatal("queued web turn did not reach the configured HTTP provider")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("web send handler did not finish")
	}
	msgs, _, err := a2.Messages.ListWithWatermark(t.Context(), sess.ID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == message.User && strings.Contains(m.FullText(), "please call the configured web provider") {
			found = true
			require.Equal(t, message.OriginWeb, m.Origin)
		}
	}
	require.True(t, found, "web prompt must be persisted in the session")
}
