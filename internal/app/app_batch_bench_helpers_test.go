// D11 benchmark helpers. Timed calls are real App.AgentCoordinator.Run turns.
// A hermetic provider calls production `view` against the same tiny fixture
// on each revision; there is no injected fake leaf or hand-built runtime.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
	modernsqlite "modernc.org/sqlite"
)

const (
	d11WarmupCalls               = 2
	d11LatencySamples            = 40
	d11ThroughputSessions        = 8
	d11ThroughputCallsPerSession = 20
	d11MeasuredRounds            = 2
	d11FixtureName               = "d11-fixture.txt"
)

type d11RunEntry struct {
	Phase         string    `json:"phase"`
	Samples       int       `json:"samples"`
	Attempts      int       `json:"attempts"`
	Errors        int       `json:"errors"`
	P50Ms         float64   `json:"p50_ms"`
	P95Ms         float64   `json:"p95_ms"`
	P99Ms         float64   `json:"p99_ms"`
	CallsPerSec   float64   `json:"calls_per_sec"`
	Commits       int64     `json:"commits"`
	SubmitWrites  int       `json:"submit_writes"`
	StartedWrites int       `json:"started_writes"`
	SettledWrites int       `json:"settled_writes"`
	RawMs         []float64 `json:"raw_ms"`
}

type d11Summary struct {
	Mode               string  `json:"mode"`
	CandidateSHA       string  `json:"candidate_sha"`
	BaselineSHA        string  `json:"baseline_sha"`
	P50Ms              float64 `json:"p50_ms"`
	P95Ms              float64 `json:"p95_ms"`
	P99Ms              float64 `json:"p99_ms"`
	P95ContentionMs    float64 `json:"p95_contention_ms"`
	ThroughputCPS      float64 `json:"throughput_cps"`
	Errors             int     `json:"errors"`
	Commits            int64   `json:"commits"`
	CommitsPerCall     int64   `json:"commits_per_call"`
	SequentialAttempts int     `json:"sequential_attempts"`
	ContentionAttempts int     `json:"contention_attempts"`
	SubmitWrites       int     `json:"submit_writes"`
	StartedWrites      int     `json:"started_writes"`
	SettledWrites      int     `json:"settled_writes"`
	Durability         struct {
		JournalMode string `json:"journal_mode"`
		Synchronous string `json:"synchronous"`
	} `json:"durability"`
	Runs []d11RunEntry `json:"runs"`
}

type d11TransitionCounts struct {
	mu                                            sync.Mutex
	committed, pending                            int64
	submit, started, settled                      int
	pendingSubmit, pendingStarted, pendingSettled int
	currentDirty                                  bool
	currentSubmit, currentStarted, currentSettled int
}

type d11Provider struct {
	server   *httptest.Server
	fixture  string
	requests atomic.Int64
	results  sync.Map // request ID -> *atomic.Int64 successful fixture-backed result responses
}

type d11Env struct {
	t           *testing.T
	app         *App
	provider    *d11Provider
	transitions d11TransitionCounts
	sessions    []string
}

func (d *d11TransitionCounts) snapshot() (int64, int, int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.committed, d.submit, d.started, d.settled
}

func newD11Provider(t *testing.T, fixture string) *d11Provider {
	t.Helper()
	p := &d11Provider{fixture: fixture}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Messages []struct {
				Role      string          `json:"role"`
				Name      string          `json:"name"`
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil || len(request.Messages) == 0 {
			http.Error(w, "invalid chat request", http.StatusBadRequest)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		var requestID string
		lastUser := -1
		for i := len(request.Messages) - 1; i >= 0; i-- {
			if request.Messages[i].Role == "user" {
				lastUser = i
				text, err := d11ContentText(request.Messages[i].Content)
				if err != nil {
					http.Error(w, "invalid user content: "+err.Error(), http.StatusBadRequest)
					return
				}
				if index := strings.LastIndex(text, "request="); index >= 0 {
					requestID = strings.TrimSpace(text[index+len("request="):])
				}
				break
			}
		}
		if requestID == "" {
			http.Error(w, "user request id missing", http.StatusBadRequest)
			return
		}
		id := p.requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(chunks ...string) {
			for _, chunk := range chunks {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
				if fl != nil {
					fl.Flush()
				}
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		if last.Role == "tool" {
			count := 0
			for _, item := range request.Messages[lastUser+1:] {
				if item.Role != "tool" {
					continue
				}
				count++
				result, err := d11ContentText(item.Content)
				if err != nil {
					http.Error(w, "invalid tool-result content: "+err.Error(), http.StatusInternalServerError)
					return
				}
				if !strings.Contains(result, "d11 fixture sentinel") {
					http.Error(w, "production view did not return fixture", http.StatusInternalServerError)
					return
				}
			}
			if count != 1 {
				http.Error(w, "expected exactly one new tool result", http.StatusInternalServerError)
				return
			}
			counterValue, _ := p.results.LoadOrStore(requestID, &atomic.Int64{})
			counterValue.(*atomic.Int64).Add(1)
			write(`{"id":"d11-final","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"fixture read"},"finish_reason":null}]}`, `{"id":"d11-final","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
			return
		}
		if last.Role != "user" {
			http.Error(w, "unexpected request turn role", http.StatusBadRequest)
			return
		}
		args, _ := json.Marshal(map[string]string{"file_path": p.fixture})
		calls := 0
		for _, item := range request.Messages[lastUser+1:] {
			for _, call := range item.ToolCalls {
				calls++
				if call.Function.Name != "view" {
					http.Error(w, "unexpected tool requested", http.StatusInternalServerError)
					return
				}
			}
		}
		if calls != 0 {
			http.Error(w, "unexpected historical/current tool call", http.StatusInternalServerError)
			return
		}
		tool, _ := json.Marshal(map[string]any{"index": 0, "id": fmt.Sprintf("d11-view-%d", id), "type": "function", "function": map[string]string{"name": "view", "arguments": string(args)}})
		chunk, _ := json.Marshal(map[string]any{"id": "d11-tool", "object": "chat.completion.chunk", "created": 1, "model": "probe", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{json.RawMessage(tool)}}, "finish_reason": nil}}})
		write(string(chunk), `{"id":"d11-tool","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	}))
	t.Cleanup(p.server.Close)
	return p
}

// modernc CommitHook executes before physical commit. Keep each transaction's
// row/event writes in current*, promote them to pending only in CommitHook,
// discard them on RollbackHook. A subsequent successful writer query (phase
// snapshot) flushes pending into committed counts; because it uses the same
// max-one-connection App writer, all prior returned SQL operations completed.
// Thus SQL errors/rollback cannot be counted as committed evidence.
func d11InstallHooks(t *testing.T, conn *sql.DB, state *d11TransitionCounts) {
	t.Helper()
	c, err := conn.Conn(context.Background())
	require.NoError(t, err)
	err = c.Raw(func(raw any) error {
		hooks, ok := raw.(modernsqlite.HookRegisterer)
		if !ok {
			return fmt.Errorf("SQLite hook API unavailable on %T", raw)
		}
		hooks.RegisterPreUpdateHook(func(change modernsqlite.SQLitePreUpdateData) {
			state.mu.Lock()
			defer state.mu.Unlock()
			// A writer transaction cannot start until the preceding one ends.
			// Finalize its commit-hook observation only at this successful write boundary.
			state.committed += state.pending
			state.submit += state.pendingSubmit
			state.started += state.pendingStarted
			state.settled += state.pendingSettled
			state.pending = 0
			state.pendingSubmit, state.pendingStarted, state.pendingSettled = 0, 0, 0
			state.currentDirty = true
			if change.TableName == "batch_runner_checkpoints" && (change.Op == 18 || change.Op == 23) {
				var columns [7]any
				if change.New(columns[:]...) == nil {
					if event, ok := columns[3].(string); ok {
						switch event {
						case "submit":
							state.currentSubmit++
						case "started":
							state.currentStarted++
						case "settled":
							state.currentSettled++
						}
					}
				}
			}
		})
		hooks.RegisterCommitHook(func() int32 {
			state.mu.Lock()
			defer state.mu.Unlock()
			// Reaching a later commit proves the earlier hook's transaction
			// completed; keep only one not-yet-confirmed commit observation.
			state.committed += state.pending
			state.submit += state.pendingSubmit
			state.started += state.pendingStarted
			state.settled += state.pendingSettled
			state.pending, state.pendingSubmit, state.pendingStarted, state.pendingSettled = 0, 0, 0, 0
			if state.currentDirty {
				state.pending = 1
				state.pendingSubmit = state.currentSubmit
				state.pendingStarted = state.currentStarted
				state.pendingSettled = state.currentSettled
			}
			state.currentDirty = false
			state.currentSubmit, state.currentStarted, state.currentSettled = 0, 0, 0
			return 0
		})
		hooks.RegisterRollbackHook(func() {
			state.mu.Lock()
			state.currentDirty = false
			state.currentSubmit, state.currentStarted, state.currentSettled = 0, 0, 0
			state.pending, state.pendingSubmit, state.pendingStarted, state.pendingSettled = 0, 0, 0, 0
			state.mu.Unlock()
		})
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, c.Close())
}

func newD11Env(t *testing.T, mode string) *d11Env {
	t.Helper()
	dir := t.TempDir()
	for key, name := range map[string]string{"RUSH_GLOBAL_DATA": "global-data", "RUSH_GLOBAL_CONFIG": "global-config", "XDG_DATA_HOME": "xdg-data", "XDG_CONFIG_HOME": "xdg-config"} {
		t.Setenv(key, filepath.Join(dir, name))
	}
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	if mode == "candidate" {
		t.Setenv("RUSH_BATCH_RUNTIME", "1")
	} else {
		t.Setenv("RUSH_BATCH_RUNTIME", "0")
	}
	fixture := filepath.Join(dir, d11FixtureName)
	require.NoError(t, os.WriteFile(fixture, []byte("d11 fixture sentinel\n"), 0o600))
	provider := newD11Provider(t, fixture)
	store, err := config.Init(dir, dir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{ID: "openaicompat", Type: openaicompat.Name, BaseURL: provider.server.URL, APIKey: "probe", Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}}})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()
	conn, err := db.Connect(context.Background(), dir)
	require.NoError(t, err)
	a, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(a.Shutdown)
	e := &d11Env{t: t, app: a, provider: provider}
	d11InstallHooks(t, a.DB(), &e.transitions)
	for i := 0; i <= d11ThroughputSessions; i++ {
		sess, err := a.Sessions.Create(context.Background(), fmt.Sprintf("d11-%s-%d", mode, i))
		require.NoError(t, err)
		a.Permissions.AutoApproveSession(sess.ID)
		e.sessions = append(e.sessions, sess.ID)
		_, err = a.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "seed"}}})
		require.NoError(t, err)
	}
	return e
}

func d11ContentText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("expected text string or text-part array: %w", err)
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type != "text" {
			return "", fmt.Errorf("unsupported content part type %q", part.Type)
		}
		result.WriteString(part.Text)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("empty text-part array")
	}
	return result.String(), nil
}

func (e *d11Env) call(ctx context.Context, slot int, id string) (float64, error) {
	started := time.Now()
	_, err := e.app.AgentCoordinator.Run(ctx, e.sessions[slot], "Read fixture. request="+id)
	countValue, ok := e.provider.results.Load(id)
	var count int64
	if ok {
		count, ok = countValue.(*atomic.Int64).Load(), true
	}
	if err == nil && (!ok || count != 1) {
		err = fmt.Errorf("expected exactly one fixture-backed view result for %s, got %d", id, count)
	}
	if err != nil {
		e.t.Logf("D11 invocation %s failed: %v", id, err)
	}
	return float64(time.Since(started).Microseconds()) / 1000, err
}

func (e *d11Env) writeSnapshot() (int64, int, int, int) {
	var one int
	require.NoError(e.t, e.app.DB().QueryRow(`SELECT 1`).Scan(&one))
	e.transitions.mu.Lock()
	e.transitions.committed += e.transitions.pending
	e.transitions.submit += e.transitions.pendingSubmit
	e.transitions.started += e.transitions.pendingStarted
	e.transitions.settled += e.transitions.pendingSettled
	e.transitions.pending, e.transitions.pendingSubmit, e.transitions.pendingStarted, e.transitions.pendingSettled = 0, 0, 0, 0
	e.transitions.mu.Unlock()
	return e.transitions.snapshot()
}

func d11Durability(t *testing.T, conn *sql.DB) (string, string) {
	t.Helper()
	var journal string
	require.NoError(t, conn.QueryRow(`PRAGMA journal_mode`).Scan(&journal))
	var value int
	require.NoError(t, conn.QueryRow(`PRAGMA synchronous`).Scan(&value))
	return journal, map[int]string{0: "OFF", 1: "NORMAL", 2: "FULL", 3: "EXTRA"}[value]
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	// Nearest-rank percentile: ceil(p*N)-1 avoids understating the tail.
	return ordered[int(math.Ceil(p*float64(len(ordered))/100))-1]
}

func d11PrintSummary(t *testing.T, s d11Summary) {
	t.Helper()
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	fmt.Printf("===D11-SUMMARY-BEGIN===\n%s\n===D11-SUMMARY-END===\n", raw)
}
