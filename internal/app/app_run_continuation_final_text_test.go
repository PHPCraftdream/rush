package app

// F3 (2026-09-21 weekly audit): a continuation after a partial response
// must return BOTH halves of the answer. The audit repro, end to end:
// the provider streams "Section 1: Introduction" then the connection
// dies mid-stream (transient), the coordinator's continuation retry
// re-asks, and the continuation attempt returns "Section 2: Conclusion"
// with a clean finish. The run's RETURNED final_text (the JSON/SDK
// envelope) must contain both sections — before the fix it carried only
// the continuation's tail.
//
// The stub needs three behaviors because the openai provider stack
// retries internally (fantasy MaxRetries=2) before the coordinator's
// own continuation loop engages: request 1 streams the partial text and
// aborts, requests 2-3 (fantasy's internal step retries) answer 500 so
// the step ultimately fails, and request 4 (the coordinator's
// continuation) completes cleanly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	contPartialText = "Section 1: Introduction"
	contFinalText   = "Section 2: Conclusion"
	contReviewModel = "reviewer-model"
	contReviewText  = "REVIEWER VERDICT: work verified."
)

func sseChunk(content string) string {
	contentJSON, _ := json.Marshal(content)
	return "data: " + fmt.Sprintf(`{"id":"cont","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%s},"finish_reason":null}]}`+"\n\n", contentJSON)
}

const (
	sseStopChunk = "data: " + `{"id":"cont","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"
	sseDone      = "data: [DONE]\n\n"
)

// newContinuationApp builds a real App whose provider stub replays the
// audit repro. withReviewer additionally configures a reviewer slot on
// the same stub (model contReviewModel) so the scoping test can run the
// reviewer pass after the continuation.
func newContinuationApp(t *testing.T, withReviewer bool) *App {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	var mu sync.Mutex
	requestsByModel := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		mu.Lock()
		requestsByModel[req.Model]++
		n := requestsByModel[req.Model]
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if req.Model == contReviewModel {
			_, _ = fmt.Fprint(w, sseChunk(contReviewText))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
			return
		}
		if req.Model != "smart-default" {
			// Title generation and any other fast-slot traffic: always a
			// clean answer, never part of the repro.
			_, _ = fmt.Fprint(w, sseChunk("ok"))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
			return
		}
		switch {
		case n == 1:
			// The turn's first attempt: stream the partial text, give the
			// client a moment to read it, then kill the connection
			// mid-stream (transient EOF).
			_, _ = fmt.Fprint(w, sseChunk(contPartialText))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(300 * time.Millisecond)
			panic(http.ErrAbortHandler)
		case n == 2 || n == 3:
			// fantasy's internal step retries: fail fast so the step
			// ultimately errors out and the coordinator's own
			// continuation retry takes over.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"message":"transient stub failure","type":"server_error"}}`)
		default:
			// The coordinator's continuation attempt: clean finish.
			_, _ = fmt.Fprint(w, sseChunk(contFinalText))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		}
	}))
	t.Cleanup(srv.Close)

	models := `"models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"}
  }`
	providerModels := `{"id":"smart-default","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-default","context_window":200000,"default_max_tokens":1000}`
	if withReviewer {
		models = `"models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"},
    "reviewer": {"provider":"openaicompat","model":"` + contReviewModel + `"}
  }`
		providerModels += `,
        {"id":"` + contReviewModel + `","context_window":200000,"default_max_tokens":1000}`
	}
	dataDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        %s
      ]
    }
  },
  %s
}`, srv.URL, providerModels, models)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	if withReviewer {
		store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: contReviewModel})
	}
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	return application
}

func runContinuationExecuteRun(t *testing.T, application *App) (*RunResult, error) {
	t.Helper()
	return application.ExecuteRun(context.Background(), RunRequest{
		Prompt:      "write a long report",
		Overrides:   RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:        RunModeJSON,
		Stdout:      io.Discard,
		Stderr:      io.Discard,
		HideSpinner: true,
	})
}

// TestExecuteRunContinuationRetryReturnsFullText is the F3 acceptance
// test: the run's RETURNED final text — not just the DB message count —
// must contain the interrupted first half AND the continuation's tail.
func TestExecuteRunContinuationRetryReturnsFullText(t *testing.T) {
	app := newContinuationApp(t, false)

	result, err := runContinuationExecuteRun(t, app)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Contains(t, result.FinalText, contPartialText,
		"the interrupted first half of the answer must survive into the returned final text")
	assert.Contains(t, result.FinalText, contFinalText,
		"the continuation's tail must be in the returned final text")
	assert.LessOrEqual(t, strings.Index(result.FinalText, contPartialText), strings.Index(result.FinalText, contFinalText),
		"the partial text must precede the continuation text")
}

// TestExecuteRunContinuationChainScopedToItsOwnPhase pins the other half
// of F3: the combination is scoped to genuine continuation chains. Here
// the primary turn goes through the SAME continuation chain as above,
// but a reviewer pass then runs as its own phase on the session; the
// review turn's own message is the run's terminal message and must NOT
// have the primary turn's chain text concatenated into the final output.
func TestExecuteRunContinuationChainScopedToItsOwnPhase(t *testing.T) {
	app := newContinuationApp(t, true)

	result, err := runContinuationExecuteRun(t, app)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, contReviewText, result.FinalText,
		"the reviewer pass's own response must be the final output, untouched by the primary turn's continuation chain")
	assert.NotContains(t, result.FinalText, contPartialText)
	assert.NotContains(t, result.FinalText, contFinalText)
}

// TestContinuationChainText pins the walk rules directly: only the
// consecutive failed (FinishReasonError) assistant rows immediately
// before a successful terminal are combined, never an unrelated clean
// assistant message.
func TestContinuationChainText(t *testing.T) {
	t.Parallel()

	errFinish := func() message.ContentPart {
		return message.Finish{Reason: message.FinishReasonError, Message: "Stream stalled"}
	}
	okFinish := message.Finish{Reason: message.FinishReasonEndTurn}
	newMsg := func(id string, parts ...message.ContentPart) message.Message {
		return message.Message{ID: id, Role: message.Assistant, Parts: parts}
	}

	tests := []struct {
		name       string
		assistants []message.Message
		terminalID string
		want       string
	}{
		{
			name: "successful continuation combines the failed attempt",
			assistants: []message.Message{
				newMsg("a1", message.TextContent{Text: "Section 1: Introduction"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "Section 2: Conclusion"}, okFinish),
			},
			terminalID: "a2",
			want:       "Section 1: Introduction\n\nSection 2: Conclusion",
		},
		{
			name: "walk stops at an unrelated clean assistant message",
			assistants: []message.Message{
				newMsg("a0", message.TextContent{Text: "Unrelated earlier turn"}, okFinish),
				newMsg("a1", message.TextContent{Text: "Section 1"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "Section 2"}, okFinish),
			},
			terminalID: "a2",
			want:       "Section 1\n\nSection 2",
		},
		{
			name: "multiple failed attempts combine in chronological order",
			assistants: []message.Message{
				newMsg("a1", message.TextContent{Text: "Part 1"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "Part 2"}, errFinish()),
				newMsg("a3", message.TextContent{Text: "Part 3"}, okFinish),
			},
			terminalID: "a3",
			want:       "Part 1\n\nPart 2\n\nPart 3",
		},
		{
			name: "empty failed attempts are skipped but do not break the chain",
			assistants: []message.Message{
				newMsg("a1", errFinish()),
				newMsg("a2", message.TextContent{Text: "Part 2"}, errFinish()),
				newMsg("a3", message.TextContent{Text: "Part 3"}, okFinish),
			},
			terminalID: "a3",
			want:       "Part 2\n\nPart 3",
		},
		{
			name: "terminal that itself failed is left alone",
			assistants: []message.Message{
				newMsg("a1", message.TextContent{Text: "Part 1"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "Part 2"}, errFinish()),
			},
			terminalID: "a2",
			want:       "",
		},
		{
			name: "clean run without a chain returns empty",
			assistants: []message.Message{
				newMsg("a1", message.TextContent{Text: "Just one answer"}, okFinish),
			},
			terminalID: "a1",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var terminal message.Message
			for _, m := range tt.assistants {
				if m.ID == tt.terminalID {
					terminal = m
				}
			}
			require.NotNil(t, terminal)
			assert.Equal(t, tt.want, continuationChainText(tt.assistants, terminal))
		})
	}
}
