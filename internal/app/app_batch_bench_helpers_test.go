// D11 benchmark helpers. Timed calls are real App.AgentCoordinator.Run turns.
// The fixture workload uses the direct view tool; the wrapped workload uses
// the production bash tool with the same harmless command in both modes.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
	modernsqlite "modernc.org/sqlite"
)

const (
	d11WarmupCalls               = 20  // warmup calls per round, split over the round's sequential sessions
	d11LatencySamples            = 200 // measured sequential samples per round
	d11SeqBatchSessions          = 4   // fresh sequential sessions per round, so per-session history stays bounded
	d11ThroughputSessions        = 8
	d11ThroughputCallsPerSession = 20
	d11ConcSessionWarmupCalls    = 2 // warm calls per contention session before the measured block
	d11MeasuredRounds            = 3
	d11FixtureName               = "d11-fixture.txt"
)

type d11RunEntry struct {
	Workload      string    `json:"workload"`
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
	Mode               string          `json:"mode"`
	Workload           string          `json:"workload"`
	CallTransactions   []d11CallCommit `json:"call_transactions"`
	MeasuredCallIDs    []string        `json:"measured_call_ids"`
	CandidateSHA       string          `json:"candidate_sha"`
	BaselineSHA        string          `json:"baseline_sha"`
	P50Ms              float64         `json:"p50_ms"`
	P95Ms              float64         `json:"p95_ms"`
	P99Ms              float64         `json:"p99_ms"`
	P95ContentionMs    float64         `json:"p95_contention_ms"`
	ThroughputCPS      float64         `json:"throughput_cps"`
	Errors             int             `json:"errors"`
	Commits            int64           `json:"commits"`
	CommitsPerCall     int64           `json:"commits_per_call"`
	SequentialAttempts int             `json:"sequential_attempts"`
	ContentionAttempts int             `json:"contention_attempts"`
	SubmitWrites       int             `json:"submit_writes"`
	StartedWrites      int             `json:"started_writes"`
	SettledWrites      int             `json:"settled_writes"`
	Durability         struct {
		JournalMode string `json:"journal_mode"`
		Synchronous string `json:"synchronous"`
	} `json:"durability"`
	HealthViolations int           `json:"health_violations"`
	OffWaitPath      string        `json:"off_wait_path"`
	Runs             []d11RunEntry `json:"runs"`
	// Workload configuration, exported so the artifact is self-describing:
	// a consumer can recompute and verify the measured call counts from the
	// JSON alone. None of these fields use `omitempty`: their zero values
	// are meaningful configuration (0 sessions, 0 rounds, or 0 warmup calls
	// would be real workload settings, not absent data), so every field must
	// always appear explicitly in the summary JSON.
	MeasuredRounds            int `json:"measured_rounds"`
	SequentialSessions        int `json:"sequential_sessions"`
	LatencySamplesPerRound    int `json:"latency_samples_per_round"`
	ContentionSessions        int `json:"contention_sessions"`
	ContentionCallsPerSession int `json:"contention_calls_per_session"`
	WarmupCallsPerRound       int `json:"warmup_calls_per_round"`
	ConcSessionWarmupCalls    int `json:"conc_session_warmup_calls"`
}

type d11TransitionCounts struct {
	mu                 sync.Mutex
	commits            int64
	nextTx             int64
	calls              map[string]*d11CallEvidence
	currentTx          int64
	currentEvents      map[string]string
	currentEventCounts map[string]int
	currentRootCalls   map[string]string
	rootCalls          map[string]string
	pendingTx          int64
	pendingEvents      map[string]string
	pendingEventRoots  map[string]map[string]string
	pendingAmbiguity   []string
	observerErrors     []string
}

type (
	d11CallEvidence struct {
		root         string
		transactions map[string]int64
	}
	d11ToolIdentity struct{ root, call string }
	d11CallCommit   struct {
		Call        string `json:"call_id"`
		Root        string `json:"root_id"`
		Event       string `json:"event"`
		Transaction int64  `json:"transaction"`
	}
)

type d11Provider struct {
	server     *httptest.Server
	fixture    string
	workload   string
	state      *d11TransitionCounts
	requests   atomic.Int64
	results    sync.Map
	identities sync.Map // request ID -> tool call ID
}

type d11Env struct {
	t           *testing.T
	app         *App
	provider    *d11Provider
	transitions d11TransitionCounts
	mode        string
	dir         string
	health      *d11HealthGate
}

func (d *d11TransitionCounts) snapshot() (int64, int, int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	counts := [3]int{}
	for _, call := range d.calls {
		for i, kind := range []string{"submit", "started", "settled"} {
			if call.transactions[kind] > 0 {
				counts[i]++
			}
		}
	}
	return d.commits, counts[0], counts[1], counts[2]
}

func (d *d11TransitionCounts) registerCall(callID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls == nil {
		d.calls = make(map[string]*d11CallEvidence)
	}
	if d.calls[callID] == nil {
		d.calls[callID] = &d11CallEvidence{transactions: make(map[string]int64)}
	}
}

func (d *d11TransitionCounts) validateCalls(ids []d11ToolIdentity) ([]d11CallCommit, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.observerErrors) > 0 {
		return nil, fmt.Errorf("checkpoint transaction attribution ambiguous: %s", strings.Join(d.observerErrors, "; "))
	}
	var evidence []d11CallCommit
	for _, id := range ids {
		call := d.calls[id.call]
		if call == nil || call.root == "" || call.root == "duplicate" || (id.root != "" && call.root != id.root) {
			return nil, fmt.Errorf("missing transaction identity for call %s root %s", id.call, id.root)
		}
		for _, kind := range []string{"submit", "started", "settled"} {
			tx := call.transactions[kind]
			if tx <= 0 {
				return nil, fmt.Errorf("call %s missing or duplicated committed %s checkpoint", id.call, kind)
			}
			evidence = append(evidence, d11CallCommit{Call: id.call, Root: call.root, Event: kind, Transaction: tx})
		}
		if call.transactions["submit"] == call.transactions["started"] || call.transactions["submit"] == call.transactions["settled"] || call.transactions["started"] == call.transactions["settled"] {
			return nil, fmt.Errorf("call %s merged checkpoint events in one transaction", id.call)
		}
	}
	return evidence, nil
}

func newD11Provider(t *testing.T, fixture, workload string, state *d11TransitionCounts) *d11Provider {
	t.Helper()
	p := &d11Provider{fixture: fixture, workload: workload, state: state}
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
					http.Error(w, "production workload did not return sentinel", http.StatusInternalServerError)
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
		toolName := "view"
		args, _ := json.Marshal(map[string]string{"file_path": p.fixture})
		if p.workload == "legacy-bash" {
			toolName = "bash"
			args, _ = json.Marshal(map[string]string{"command": "printf 'd11 fixture sentinel\\\\n'", "description": "D11 trivial command"})
		}
		calls := 0
		for _, item := range request.Messages[lastUser+1:] {
			for _, call := range item.ToolCalls {
				calls++
				if call.Function.Name != toolName {
					http.Error(w, "unexpected tool requested", http.StatusInternalServerError)
					return
				}
			}
		}
		if calls != 0 {
			http.Error(w, "unexpected historical/current tool call", http.StatusInternalServerError)
			return
		}
		toolID := fmt.Sprintf("d11-%s-%d", toolName, id)
		p.identities.Store(requestID, toolID)
		p.state.registerCall(toolID)
		tool, _ := json.Marshal(map[string]any{"index": 0, "id": toolID, "type": "function", "function": map[string]string{"name": toolName, "arguments": string(args)}})
		chunk, _ := json.Marshal(map[string]any{"id": "d11-tool", "object": "chat.completion.chunk", "created": 1, "model": "probe", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{json.RawMessage(tool)}}, "finish_reason": nil}}})
		write(string(chunk), `{"id":"d11-tool","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	}))
	t.Cleanup(p.server.Close)
	return p
}

// Hook observations stay transaction-local until a later writer boundary proves
// the transaction committed. Checkpoints are written before projections in the
// store transaction, so root attribution is finalized in CommitHook afterward.
func d11InstallHooks(t *testing.T, conn *sql.DB, state *d11TransitionCounts) {
	t.Helper()
	c, err := conn.Conn(context.Background())
	require.NoError(t, err)
	defer c.Close()
	rows, err := c.QueryContext(context.Background(), `SELECT name, cid FROM pragma_table_info('batch_nodes')`)
	require.NoError(t, err)
	if rows == nil {
		return
	}
	defer func() { _ = rows.Close() }()
	indexes := map[string]int{}
	for rows.Next() {
		var name string
		var index int
		require.NoError(t, rows.Scan(&name, &index))
		indexes[name] = index
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	rootIndex, rootOK := indexes["root_id"]
	callIndex, callOK := indexes["tool_call_id"]
	require.True(t, rootOK && callOK, "batch_nodes identity columns missing")
	state.mu.Lock()
	if state.calls == nil {
		state.calls = make(map[string]*d11CallEvidence)
	}
	if state.rootCalls == nil {
		state.rootCalls = make(map[string]string)
	}
	state.mu.Unlock()
	err = c.Raw(func(raw any) error {
		hooks, ok := raw.(modernsqlite.HookRegisterer)
		if !ok {
			return fmt.Errorf("SQLite hook API unavailable on %T", raw)
		}
		hooks.RegisterPreUpdateHook(func(change modernsqlite.SQLitePreUpdateData) {
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.currentTx == 0 {
				d11PromotePending(state)
				state.currentTx = state.nextTx + 1
				state.currentEvents = make(map[string]string)
				state.currentEventCounts = make(map[string]int)
				state.currentRootCalls = make(map[string]string)
				state.pendingAmbiguity = nil
			}
			if change.TableName == "batch_nodes" && (change.Op == 18 || change.Op == 23) {
				values := make([]any, change.Count())
				if change.New(values...) == nil {
					root, _ := values[rootIndex].(string)
					callID, _ := values[callIndex].(string)
					if root == "" && change.Op == 23 {
						oldValues := make([]any, change.Count())
						if change.Old(oldValues...) == nil {
							root, _ = oldValues[rootIndex].(string)
							if callID == "" {
								callID, _ = oldValues[callIndex].(string)
							}
						}
					}
					if root != "" {
						if callID == "" {
							callID = state.rootCalls[root]
						}
						if callID != "" {
							state.currentRootCalls[root] = callID
						}
					}
				}
			}
			if change.TableName == "batch_runner_checkpoints" && (change.Op == 18 || change.Op == 23) {
				var values [7]any
				if change.New(values[:]...) == nil {
					if event, ok := values[3].(string); ok && (event == "submit" || event == "started" || event == "settled") {
						state.currentEventCounts[event]++
						if state.currentEventCounts[event] > 1 {
							state.currentEvents[event] = "duplicate"
						} else {
							state.currentEvents[event] = event
						}
					}
				}
			}
		})
		hooks.RegisterCommitHook(func() int32 {
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.currentTx != 0 {
				state.nextTx++
				state.pendingTx = state.currentTx
				state.pendingEvents = state.currentEvents
				state.pendingEventRoots = make(map[string]map[string]string)
				state.pendingAmbiguity = nil
				for event := range state.currentEvents {
					if len(state.currentRootCalls) != 1 {
						state.pendingAmbiguity = append(state.pendingAmbiguity, fmt.Sprintf("event %s transaction touched %d roots", event, len(state.currentRootCalls)))
						continue
					}
					for root, callID := range state.currentRootCalls {
						state.pendingEventRoots[event] = map[string]string{root: callID}
					}
				}
			}
			state.currentTx = 0
			state.currentEvents, state.currentEventCounts = nil, nil
			state.currentRootCalls = nil
			return 0
		})
		hooks.RegisterRollbackHook(func() {
			state.mu.Lock()
			state.currentTx = 0
			state.currentEvents, state.currentEventCounts = nil, nil
			state.currentRootCalls = nil
			state.mu.Unlock()
		})
		return nil
	})
	require.NoError(t, err)
}

func d11PromotePending(state *d11TransitionCounts) {
	if state.pendingTx == 0 {
		return
	}
	state.commits++
	state.observerErrors = append(state.observerErrors, state.pendingAmbiguity...)
	for _, roots := range state.pendingEventRoots {
		for root, callID := range roots {
			if callID == "" {
				continue
			}
			state.rootCalls[root] = callID
			if call := state.calls[callID]; call != nil {
				if call.root != "" && call.root != root {
					call.root = "duplicate"
				} else {
					call.root = root
				}
			}
		}
	}
	for event, status := range state.pendingEvents {
		for _, callID := range state.pendingEventRoots[event] {
			if call := state.calls[callID]; call != nil {
				if status == "duplicate" || call.transactions[event] != 0 {
					call.transactions[event] = -1
				} else {
					call.transactions[event] = state.pendingTx
				}
			}
		}
	}
	state.pendingTx, state.pendingEvents, state.pendingEventRoots, state.pendingAmbiguity = 0, nil, nil, nil
}

// d11OffWaitPath documents the measured wait path in the report output.
const d11OffWaitPath = "call origin agent.WithCallOrigin(OriginCLI), the rush run origin: the OFF async wrapper forces the command to background and the turn waits on the production wait (inline window on the job settled channel / WaitContext on the shell done channel), not the SDK synchronous 100 ms poll"

// d11HealthPatterns are the log classes that mean the turn machinery is
// measuring an unhealthy path: the fail-closed batch-facts sentinel and the
// drain policy's refused-turn warning, plus any batch-debt facts wording. Any
// error-level record fails too.
var d11HealthPatterns = []string{"node source unavailable", "drain policy", "refusing the turn", "turn facts", "batch facts", "batch debt"}

type d11HealthGate struct {
	mu         sync.Mutex
	violations []string
}

func (g *d11HealthGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.violations)
}

func (g *d11HealthGate) add(violation string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.violations) < 100 {
		g.violations = append(g.violations, violation)
	}
}

func (g *d11HealthGate) snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.violations...)
}

// d11HealthHandler wraps a text slog handler and records health
// violations before passing records through unchanged.
type d11HealthHandler struct {
	inner slog.Handler
	gate  *d11HealthGate
}

func (h *d11HealthHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn || h.inner.Enabled(ctx, level)
}

func (h *d11HealthHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.gate.add(d11HealthText(r, ""))
		return h.passThrough(ctx, r)
	}
	if r.Level >= slog.LevelWarn {
		matched := ""
		lower := strings.ToLower(r.Message)
		for _, pattern := range d11HealthPatterns {
			if strings.Contains(lower, pattern) {
				matched = r.Message
				break
			}
		}
		check := func(text string) bool {
			for _, pattern := range d11HealthPatterns {
				if strings.Contains(strings.ToLower(text), pattern) {
					return true
				}
			}
			return false
		}
		r.Attrs(func(a slog.Attr) bool {
			if check(a.Value.Resolve().String()) {
				matched += " " + a.Value.Resolve().String()
			}
			return true
		})
		if matched != "" {
			h.gate.add(d11HealthText(r, matched))
		}
	}
	return h.passThrough(ctx, r)
}

// passThrough forwards only records the wrapped handler would itself emit, so
// the gate never turns on output the default logger filters out (that I/O
// would distort the measured latency).
func (h *d11HealthHandler) passThrough(ctx context.Context, r slog.Record) error {
	if !h.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func d11HealthText(r slog.Record, extra string) string {
	text := r.Level.String() + " " + r.Message
	if extra != "" && extra != r.Message {
		text += " " + extra
	}
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

func (h *d11HealthHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &d11HealthHandler{inner: h.inner.WithAttrs(attrs), gate: h.gate}
}

func (h *d11HealthHandler) WithGroup(name string) slog.Handler {
	return &d11HealthHandler{inner: h.inner.WithGroup(name), gate: h.gate}
}

func newD11Env(t *testing.T, mode, workload string) *d11Env {
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
	e := &d11Env{t: t, mode: mode, dir: dir}
	e.health = &d11HealthGate{}
	// The wrapped handler must be a fresh one, never slog.Default().Handler():
	// SetDefault routes the log package into the new default, and the stock
	// default handler writes through the log package, so wrapping it recurses
	// into itself and deadlocks on the log mutex.
	previous := slog.Default()
	slog.SetDefault(slog.New(&d11HealthHandler{inner: slog.NewTextHandler(os.Stderr, nil), gate: e.health}))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
	provider := newD11Provider(t, fixture, workload, &e.transitions)
	e.provider = provider
	store, err := config.Init(dir, dir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{ID: "openaicompat", Type: openaicompat.Name, BaseURL: provider.server.URL, APIKey: "probe", Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}}})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()
	conn, err := db.Connect(context.Background(), dir)
	require.NoError(t, err)
	// Runs after a.Shutdown (LIFO).
	t.Cleanup(func() { _ = db.Release(dir) })
	a, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(a.Shutdown)
	e.app = a
	d11InstallHooks(t, a.DB(), &e.transitions)
	return e
}

// newSessions creates fresh sessions outside measured commit windows, so
// per-session history stays bounded and identical across samples.
func (e *d11Env) newSessions(label string, n int) []string {
	sessions := make([]string, 0, n)
	for i := 0; i < n; i++ {
		sess, err := e.app.Sessions.Create(context.Background(), fmt.Sprintf("d11-%s-%s-%d", e.mode, label, i))
		require.NoError(e.t, err)
		e.app.Permissions.AutoApproveSession(sess.ID)
		_, err = e.app.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "seed"}}})
		require.NoError(e.t, err)
		sessions = append(sessions, sess.ID)
	}
	return sessions
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

func (e *d11Env) toolIdentity(requestID string) (d11ToolIdentity, bool) {
	value, ok := e.provider.identities.Load(requestID)
	if !ok {
		return d11ToolIdentity{}, false
	}
	callID, valid := value.(string)
	if !valid || callID == "" {
		return d11ToolIdentity{}, false
	}
	return d11ToolIdentity{call: callID}, true
}

// d11PhaseResult carries one phase's timing and commit-window evidence.
type d11PhaseResult struct {
	Values     []float64
	Identities []d11ToolIdentity
	Errors     int
	Commits    int64
	Submit     int
	Started    int
	Settled    int
	Elapsed    time.Duration
	Attempts   int
}

func (e *d11Env) call(ctx context.Context, sessionID, id string) (float64, error) {
	started := time.Now()
	_, err := e.app.AgentCoordinator.Run(agent.WithCallOrigin(ctx, message.OriginCLI), sessionID, "Read fixture. request="+id)
	countValue, ok := e.provider.results.Load(id)
	var count int64
	if ok {
		count, ok = countValue.(*atomic.Int64).Load(), true
	}
	_, hasTool := e.provider.identities.Load(id)
	if err == nil && (!ok || count != 1 || !hasTool) {
		err = fmt.Errorf("expected one result and actual tool call for %s", id)
	}
	if err != nil {
		e.t.Logf("D11 invocation %s failed: %v", id, err)
	}
	return float64(time.Since(started).Microseconds()) / 1000, err
}

// runPhase executes one phase's calls and captures the commit-window deltas
// around the whole phase. Session creation must happen before this call.
func (e *d11Env) runPhase(label string, sessions []string, callsPerSession int, parallel, measured bool) d11PhaseResult {
	before, sub, started, settled := e.writeSnapshot()
	start := time.Now()
	result := d11PhaseResult{Attempts: len(sessions) * callsPerSession}
	var mu sync.Mutex
	record := func(identity d11ToolIdentity, ms float64, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			result.Errors++
			return
		}
		result.Values = append(result.Values, ms)
		if measured {
			result.Identities = append(result.Identities, identity)
		}
	}
	identity := func(requestID string) d11ToolIdentity {
		if found, ok := e.toolIdentity(requestID); ok {
			return found
		}
		return d11ToolIdentity{call: ""}
	}
	if !parallel {
		callIndex := 0
		for _, sessionID := range sessions {
			for i := 0; i < callsPerSession; i++ {
				requestID := fmt.Sprintf("%s-%d", label, callIndex)
				ms, err := e.call(context.Background(), sessionID, requestID)
				record(identity(requestID), ms, err)
				callIndex++
			}
		}
	} else {
		var wg sync.WaitGroup
		for sessionIndex, sessionID := range sessions {
			wg.Add(1)
			go func(sessionIndex int, sessionID string) {
				defer wg.Done()
				for i := 0; i < callsPerSession; i++ {
					requestID := fmt.Sprintf("%s-%d-%d", label, sessionIndex, i)
					ms, err := e.call(context.Background(), sessionID, requestID)
					record(identity(requestID), ms, err)
				}
			}(sessionIndex, sessionID)
		}
		wg.Wait()
	}
	result.Elapsed = time.Since(start)
	after, asub, astarted, asettled := e.writeSnapshot()
	result.Commits = after - before
	result.Submit = asub - sub
	result.Started = astarted - started
	result.Settled = asettled - settled
	return result
}

func (e *d11Env) writeSnapshot() (int64, int, int, int) {
	d11FlushCommitted(e.t, e.app.DB(), &e.transitions)
	return e.transitions.snapshot()
}

func d11FlushCommitted(t *testing.T, writer *sql.DB, state *d11TransitionCounts) {
	t.Helper()
	c, err := writer.Conn(context.Background())
	require.NoError(t, err)
	defer c.Close()
	var one int
	require.NoError(t, c.QueryRowContext(context.Background(), `SELECT 1`).Scan(&one))
	state.mu.Lock()
	d11PromotePending(state)
	state.mu.Unlock()
}

func d11Durability(t *testing.T, conn *sql.DB) (string, string) {
	t.Helper()
	var journal string
	require.NoError(t, conn.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journal))
	var value int
	require.NoError(t, conn.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&value))
	return journal, map[int]string{0: "OFF", 1: "NORMAL", 2: "FULL", 3: "EXTRA"}[value]
}

// d11LogRoundState logs the cumulative per-round state the report ignores:
// batch-runtime row counts, db/WAL file sizes, goroutines and heap-in-use,
// so the round-over-round decay can be separated from per-call cost.
func d11LogRoundState(t *testing.T, e *d11Env, round int) {
	t.Helper()
	count := func(table string) int64 {
		var n int64
		require.NoError(t, e.app.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n))
		return n
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	dbBytes, walBytes, shmBytes := int64(-1), int64(-1), int64(-1)
	storeNames := []string{"rush_store_v1.db", "rush.db"}
	for _, storeName := range storeNames {
		if fi, err := os.Stat(filepath.Join(e.dir, storeName)); err == nil {
			dbBytes = fi.Size()
		}
		if fi, err := os.Stat(filepath.Join(e.dir, storeName+"-wal")); err == nil {
			walBytes = fi.Size()
		}
		if fi, err := os.Stat(filepath.Join(e.dir, storeName+"-shm")); err == nil {
			shmBytes = fi.Size()
		}
	}
	t.Logf("D11 ROUND %d STATE: batch_nodes=%d checkpoints=%d session_notices=%d orphan_outbox=%d messages=%d sessions=%d db_bytes=%d wal_bytes=%d shm_bytes=%d goroutines=%d heap_inuse=%d",
		round, count("batch_nodes"), count("batch_runner_checkpoints"), count("session_notices"),
		count("orphan_call_outbox"), count("messages"), count("sessions"),
		dbBytes, walBytes, shmBytes, runtime.NumGoroutine(), ms.HeapInuse)
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
