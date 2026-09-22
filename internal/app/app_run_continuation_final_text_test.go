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

// sseToolCallChunk streams one complete OpenAI-style function tool call
// as a single delta: fantasy's openai provider emits the whole tool call
// as soon as the accumulated arguments parse as valid JSON.
func sseToolCallChunk(name, args string) string {
	nameJSON, _ := json.Marshal(name)
	argsJSON, _ := json.Marshal(args)
	return "data: " + fmt.Sprintf(`{"id":"cont","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`+"\n\n", nameJSON, argsJSON)
}

// sseToolCallsFinishChunk ends a streaming step that produced tool calls.
func sseToolCallsFinishChunk() string {
	return "data: " + `{"id":"cont","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"
}

// newContinuationApp builds a real App whose provider stub replays the
// audit repro. withReviewer additionally configures a reviewer slot on
// the same stub (model contReviewModel) so the scoping test can run the
// reviewer pass after the continuation.
func newContinuationApp(t *testing.T, withReviewer bool) *App {
	t.Helper()
	return newContinuationAppWithStub(t, withReviewer, defaultContinuationSmartStub)
}

// defaultContinuationSmartStub is the audit-repro provider behavior for
// the smart slot: attempt 1 streams the partial text and dies mid-stream,
// requests 2-3 fail fast (fantasy's internal step retries), and request 4
// — the coordinator's continuation attempt — completes cleanly.
func defaultContinuationSmartStub(n int, w http.ResponseWriter, _ *http.Request) {
	switch n {
	case 1:
		// The turn's first attempt: stream the partial text, give the
		// client a moment to read it, then kill the connection
		// mid-stream (transient EOF).
		_, _ = fmt.Fprint(w, sseChunk(contPartialText))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
		panic(http.ErrAbortHandler)
	case 2, 3:
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
}

// newContinuationAppWithStub builds the same app as newContinuationApp
// but lets a test replace the smart slot's scripted responses. n is the
// 1-based request count for the smart model within the test run.
func newContinuationAppWithStub(t *testing.T, withReviewer bool, smartStub func(n int, w http.ResponseWriter, r *http.Request)) *App {
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
		smartStub(n, w, r)
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
			want:       "Section 1: IntroductionSection 2: Conclusion",
		},
		{
			name: "walk stops at an unrelated clean assistant message",
			assistants: []message.Message{
				newMsg("a0", message.TextContent{Text: "Unrelated earlier turn"}, okFinish),
				newMsg("a1", message.TextContent{Text: "Section 1"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "Section 2"}, okFinish),
			},
			terminalID: "a2",
			want:       "Section 1Section 2",
		},
		{
			name: "multiple failed attempts combine in chronological order",
			assistants: []message.Message{
				newMsg("a1", message.TextContent{Text: "Part 1"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "Part 2"}, errFinish()),
				newMsg("a3", message.TextContent{Text: "Part 3"}, okFinish),
			},
			terminalID: "a3",
			want:       "Part 1Part 2Part 3",
		},
		{
			name: "empty failed attempts are skipped but do not break the chain",
			assistants: []message.Message{
				newMsg("a1", errFinish()),
				newMsg("a2", message.TextContent{Text: "Part 2"}, errFinish()),
				newMsg("a3", message.TextContent{Text: "Part 3"}, okFinish),
			},
			terminalID: "a3",
			want:       "Part 2Part 3",
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

// TestContinuationChainTextAcrossToolStepsAndUserTurns pins the R2-2
// walk rules over WHOLE messages, not just assistants: a tool call +
// tool result pair belonging to the continuation attempt itself must not
// break the chain, a genuine user turn must, and a boundary cut mid-JSON
// string must reassemble byte-for-byte instead of getting a separator
// injected.
func TestContinuationChainTextAcrossToolStepsAndUserTurns(t *testing.T) {
	t.Parallel()

	errFinish := func() message.ContentPart {
		return message.Finish{Reason: message.FinishReasonError, Message: "Stream stalled"}
	}
	okFinish := message.Finish{Reason: message.FinishReasonEndTurn}
	toolFinish := message.Finish{Reason: message.FinishReasonToolUse}
	contPrompt := func(id string) message.Message {
		return message.Message{ID: id, Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "Your previous response to the request `write a long report` was interrupted by a transient provider error (e.g. a stream stall or rate limit) partway through. Here is what you had written so far:\n\npartial\n\nContinue exactly where you left off. Do not repeat the text above and do not restart the task from scratch."},
		}}
	}
	realPrompt := func(id, text string) message.Message {
		return message.Message{ID: id, Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: text},
		}}
	}
	newMsg := func(id string, role message.MessageRole, parts ...message.ContentPart) message.Message {
		return message.Message{ID: id, Role: role, Parts: parts}
	}

	tests := []struct {
		name       string
		run        []message.Message
		terminalID string
		want       string
	}{
		{
			name: "tool step inside the continuation attempt keeps the chain",
			run: []message.Message{
				newMsg("u1", message.User, message.TextContent{Text: "write a long report"}),
				newMsg("a1", message.Assistant, message.TextContent{Text: "Section 1: Introduction"}, errFinish()),
				contPrompt("u2"),
				newMsg("a2", message.Assistant,
					message.ToolCall{ID: "call-1", Name: "view", Input: `{"file_path":"notes.md"}`}, toolFinish),
				newMsg("t2", message.Tool, message.ToolResult{ToolCallID: "call-1", Name: "view", Content: "ok"}),
				newMsg("a3", message.Assistant, message.TextContent{Text: "Section 2: Conclusion"}, okFinish),
			},
			terminalID: "a3",
			want:       "Section 1: IntroductionSection 2: Conclusion",
		},
		{
			name: "real user turn between attempts breaks the chain",
			run: []message.Message{
				newMsg("a1", message.Assistant, message.TextContent{Text: "Part 1"}, errFinish()),
				contPrompt("u2"),
				newMsg("a2", message.Assistant, message.TextContent{Text: "Part 2"}, errFinish()),
				realPrompt("u3", "actually, start over"),
				newMsg("a3", message.Assistant, message.TextContent{Text: "Part 3"}, okFinish),
			},
			terminalID: "a3",
			want:       "",
		},
		{
			name: "unrelated completed turn before the chain stays out",
			run: []message.Message{
				newMsg("a0", message.Assistant, message.TextContent{Text: "Unrelated earlier turn"}, okFinish),
				realPrompt("u1", "write a long report"),
				newMsg("a1", message.Assistant, message.TextContent{Text: "Section 1"}, errFinish()),
				contPrompt("u2"),
				newMsg("a2", message.Assistant, message.TextContent{Text: "Section 2"}, okFinish),
			},
			terminalID: "a2",
			want:       "Section 1Section 2",
		},
		{
			name: "mid-JSON-string interruption joins byte-for-byte",
			run: []message.Message{
				newMsg("u1", message.User, message.TextContent{Text: "emit json"}),
				newMsg("a1", message.Assistant, message.TextContent{Text: `{"text":"hello`}, errFinish()),
				contPrompt("u2"),
				newMsg("a2", message.Assistant, message.TextContent{Text: ` world"}`}, okFinish),
			},
			terminalID: "a2",
			want:       `{"text":"hello world"}`,
		},
		{
			name: "every attempt in a multi-attempt chain may carry a tool step",
			run: []message.Message{
				newMsg("a1", message.Assistant, message.TextContent{Text: "Part 1"}, errFinish()),
				contPrompt("u2"),
				newMsg("a2", message.Assistant, message.TextContent{Text: "Part 2"}, errFinish()),
				contPrompt("u3"),
				newMsg("a3", message.Assistant,
					message.ToolCall{ID: "call-1", Name: "view", Input: `{}`}, toolFinish),
				newMsg("t3", message.Tool, message.ToolResult{ToolCallID: "call-1", Name: "view", Content: "ok"}),
				newMsg("a4", message.Assistant, message.TextContent{Text: "Part 3"}, okFinish),
			},
			terminalID: "a4",
			want:       "Part 1Part 2Part 3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var terminal message.Message
			for _, m := range tt.run {
				if m.ID == tt.terminalID {
					terminal = m
				}
			}
			require.NotNil(t, terminal)
			assert.Equal(t, tt.want, continuationChainText(tt.run, terminal))
		})
	}
}

// TestExecuteRunContinuationChainWithToolStepReturnsFullText is R2-2
// mechanism A end to end: the continuation attempt legitimately starts
// with a tool call (view) before completing the answer. Before the fix
// the chain walk stopped at the tool-use row and the returned envelope
// carried only the tail.
func TestExecuteRunContinuationChainWithToolStepReturnsFullText(t *testing.T) {
	viewTarget := filepath.Join(t.TempDir(), "notes.md")
	require.NoError(t, os.WriteFile(viewTarget, []byte("some notes"), 0o644))
	toolArgs, err := json.Marshal(map[string]string{"file_path": viewTarget})
	require.NoError(t, err)

	app := newContinuationAppWithStub(t, false, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, sseChunk(contPartialText))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(300 * time.Millisecond)
			panic(http.ErrAbortHandler)
		case 2, 3:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"message":"transient stub failure","type":"server_error"}}`)
		case 4:
			// The continuation attempt first calls a tool — a clean
			// tool-loop round, not a failure.
			_, _ = fmt.Fprint(w, sseToolCallChunk("view", string(toolArgs)))
			_, _ = fmt.Fprint(w, sseToolCallsFinishChunk())
			_, _ = fmt.Fprint(w, sseDone)
		default:
			// The round after the view result: clean finish.
			_, _ = fmt.Fprint(w, sseChunk(contFinalText))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		}
	})

	result, err := runContinuationExecuteRun(t, app)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, contPartialText+contFinalText, result.FinalText,
		"the chain with an intervening tool step must return the full combined text in the envelope")
}

// TestExecuteRunContinuationMidJSONStringJoinsWithoutSeparator is R2-2
// mechanism B end to end: the stream dies INSIDE a JSON string value and
// the continuation completes it. The combined final_text must be the
// exact bytes the uninterrupted stream would have produced — valid JSON
// with no injected separator — or --format json callers get invalid
// output from a semantically correct continuation.
func TestExecuteRunContinuationMidJSONStringJoinsWithoutSeparator(t *testing.T) {
	const partial = `{"text":"hello`
	const tail = ` world"}`

	app := newContinuationAppWithStub(t, false, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, sseChunk(partial))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(300 * time.Millisecond)
			panic(http.ErrAbortHandler)
		case 2, 3:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"message":"transient stub failure","type":"server_error"}}`)
		default:
			_, _ = fmt.Fprint(w, sseChunk(tail))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		}
	})

	result, err := runContinuationExecuteRun(t, app)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, `{"text":"hello world"}`, result.FinalText,
		"a mid-JSON-string interruption must reassemble byte-for-byte, without any injected separator")
	assert.True(t, json.Valid([]byte(result.FinalText)),
		"combined output must remain valid JSON")
}

// TestContinuationChainTextJoinsByteExact pins the R2-2 / F3 join
// contract at unit level: continuation fragments are byte-exact pieces
// of one interrupted stream, so the join never synthesizes a separator.
// A cut with no whitespace on either side is the common mid-token and
// mid-structure case, and a guessed "\n\n" corrupts the payload — into
// invalid JSON for the JSON-string and JSON-number shapes.
func TestContinuationChainTextJoinsByteExact(t *testing.T) {
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
		run        []message.Message
		terminalID string
		want       string
		wantJSON   bool
	}{
		{
			name: "mid-JSON-string cut with no whitespace on either side",
			run: []message.Message{
				newMsg("a1", message.TextContent{Text: `{"text":"hel`}, errFinish()),
				newMsg("a2", message.TextContent{Text: `lo"}`}, okFinish),
			},
			terminalID: "a2",
			want:       `{"text":"hello"}`,
			wantJSON:   true,
		},
		{
			name: "plain mid-word cut",
			run: []message.Message{
				newMsg("a1", message.TextContent{Text: "hel"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "lo"}, okFinish),
			},
			terminalID: "a2",
			want:       "hello",
		},
		{
			name: "JSON number cut",
			run: []message.Message{
				newMsg("a1", message.TextContent{Text: `{"n":1`}, errFinish()),
				newMsg("a2", message.TextContent{Text: `2}`}, okFinish),
			},
			terminalID: "a2",
			want:       `{"n":12}`,
			wantJSON:   true,
		},
		{
			name: "several attempts cut mid-word reassemble in order",
			run: []message.Message{
				newMsg("a1", message.TextContent{Text: "Hel"}, errFinish()),
				newMsg("a2", message.TextContent{Text: "lo"}, errFinish()),
				newMsg("a3", message.TextContent{Text: ", world"}, okFinish),
			},
			terminalID: "a3",
			want:       "Hello, world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var terminal message.Message
			for _, m := range tt.run {
				if m.ID == tt.terminalID {
					terminal = m
				}
			}
			require.NotNil(t, terminal)
			got := continuationChainText(tt.run, terminal)
			assert.Equal(t, tt.want, got)
			if tt.wantJSON {
				assert.True(t, json.Valid([]byte(got)),
					"joined JSON payload must stay valid")
			}
		})
	}
}

// runByteCutContinuationE2E drives the audit repro with the stream
// severed at an arbitrary byte cut: attempt 1 streams partial and dies
// mid-stream, requests 2-3 fail fast (fantasy's internal step retries),
// and the coordinator's continuation attempt streams tail to a clean
// finish.
func runByteCutContinuationE2E(t *testing.T, partial, tail string) *RunResult {
	t.Helper()

	app := newContinuationAppWithStub(t, false, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, sseChunk(partial))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(300 * time.Millisecond)
			panic(http.ErrAbortHandler)
		case 2, 3:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"message":"transient stub failure","type":"server_error"}}`)
		default:
			_, _ = fmt.Fprint(w, sseChunk(tail))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		}
	})

	result, err := runContinuationExecuteRun(t, app)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

// TestExecuteRunContinuationMidJSONStringWithoutSpaceJoinsExactly is the
// R2-2 / F3 repro end to end: the existing
// TestExecuteRunContinuationMidJSONStringJoinsWithoutSeparator only
// exercises a tail that happens to START with a space, which satisfied
// the old whitespace-edge heuristic. A stream can die at any offset; a
// cut with no whitespace on either side must still reassemble to the
// exact bytes, valid JSON, with no separator injected.
func TestExecuteRunContinuationMidJSONStringWithoutSpaceJoinsExactly(t *testing.T) {
	result := runByteCutContinuationE2E(t, `{"text":"hel`, `lo"}`)
	require.Equal(t, `{"text":"hello"}`, result.FinalText,
		"a mid-JSON-string cut with no boundary whitespace must reassemble byte-for-byte")
	assert.True(t, json.Valid([]byte(result.FinalText)),
		"combined output must remain valid JSON")
}

// TestExecuteRunContinuationMidWordJoinsExactly pins the plain mid-word
// cut outside any structured payload: hel + lo is hello, never a word
// torn apart by an injected separator.
func TestExecuteRunContinuationMidWordJoinsExactly(t *testing.T) {
	result := runByteCutContinuationE2E(t, "hel", "lo")
	require.Equal(t, "hello", result.FinalText,
		"a mid-word cut must reassemble the word exactly")
}

// TestExecuteRunContinuationMidNumberJoinsExactly pins a cut inside a
// JSON number: {"n":1 + 2} is {"n":12}, never invalid JSON with literal
// newlines inside the number.
func TestExecuteRunContinuationMidNumberJoinsExactly(t *testing.T) {
	result := runByteCutContinuationE2E(t, `{"n":1`, `2}`)
	require.Equal(t, `{"n":12}`, result.FinalText,
		"a mid-number cut must reassemble the JSON document exactly")
	assert.True(t, json.Valid([]byte(result.FinalText)),
		"combined output must remain valid JSON")
}
