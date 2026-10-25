package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type round2ScopeProbeLeaf struct {
	t                *testing.T
	caps             *DiskProviderRegistry
	mu               sync.Mutex
	observed, shared *buildResourceScope
	released         atomic.Bool
}

func (*round2ScopeProbeLeaf) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "view", Description: "stub delegation leaf: own-scope oracle"}
}
func (*round2ScopeProbeLeaf) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (*round2ScopeProbeLeaf) SetProviderOptions(fantasy.ProviderOptions) {}
func (l *round2ScopeProbeLeaf) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if callOptionsFrom(ctx) == nil {
		return fantasy.ToolResponse{}, errRound2NoCallOptions
	}
	scope := callBuildScopeFrom(ctx)
	l.mu.Lock()
	l.observed = scope
	l.mu.Unlock()
	if scope == nil {
		return fantasy.ToolResponse{}, errRound2NoCallOptions
	}
	instance := "probe-" + uuid.NewString()
	registry := probeConstructionRegistryID + "-probe"
	if err := l.caps.acquireConstruction("", registry, instance); err != nil {
		return fantasy.ToolResponse{}, err
	}
	scope.register(func() {
		l.released.Store(true)
		l.caps.release(diskOwnerKey{kind: diskOwnerConstruction, instance: instance, registryID: registry})
	})
	return fantasy.NewTextResponse("probe ok"), nil
}

var (
	errRound2NoCallOptions      = errors.New("round2 probe: trusted call state missing")
	probeConstructionRegistryID string
)

type round2DriverScopeInstall struct {
	t                                             *testing.T
	conn                                          *sql.DB
	store                                         *session.BatchStore
	install                                       *BatchInstall
	executor                                      *batchExecutor
	bindingID, registryID, fingerprint, sessionID string
	stub                                          *round2ScopeProbeLeaf
	ctx                                           context.Context
}

func newRound2DriverScopeInstall(t *testing.T) *round2DriverScopeInstall {
	t.Helper()
	store, conn, ctx := newTestBatchStore(t)
	q := db.New(conn)
	sessionID := "sess-" + uuid.NewString()
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: sessionID, Title: sessionID})
	require.NoError(t, err)
	bindings := NewDiskProviderRegistry()
	bindingID, err := bindings.RegisterProvider(round2FakeDisk{DiskProvider: tools.OSDisk()})
	require.NoError(t, err)
	stub := &round2ScopeProbeLeaf{t: t}
	fingerprint := batchToolsetFingerprint([]fantasy.AgentTool{stub})
	registryID := "r2reg-" + uuid.NewString()[:8]
	registries := NewBatchLeafRegistryStore()
	require.NoError(t, registries.Register(registryID, newPolicyToolBuild([]fantasy.AgentTool{stub}).leaves))
	stub.caps = bindings
	probeConstructionRegistryID = registryID
	pinTestConstruction(t, bindings, registryID)
	agentDeps := batchAgentDeps{Runs: batchCoordinatorAgentRuns{}, Descendants: batchCompleteDescendantsReader{}, QuestionPublisher: batchCoordinatorQuestionPublisher{store: store}}
	executor := newBatchExecutor(batchLeafEnv{Leaves: registries.Resolve, DiskBindings: bindings}, newBatchExecutorFamilies(agentDeps))
	deps := installTestDeps(t, store, conn, "unused")
	deps.Executor = executor
	deps.AgentDeps = agentDeps
	deps.DiskBindings = bindings
	deps.Registries = registries
	deps.DonePoll = time.Millisecond
	install, err := InstallBatchRuntime(ctx, deps)
	require.NoError(t, err)
	t.Cleanup(func() {
		if install.Runner != nil {
			require.NoError(t, install.Runner.Close(context.Background()))
		}
	})
	ingress := &batchWorkIngress{rt: install.Runtime, registries: registries, diskBindings: bindings, caps: bindings}
	executor.env.Ingress = func() BatchWorkIngress { return ingress }
	BindRuntimeIngress(install.Runtime, ingress)
	return &round2DriverScopeInstall{t: t, conn: conn, store: store, install: install, executor: executor, bindingID: bindingID, registryID: registryID, fingerprint: fingerprint, sessionID: sessionID, stub: stub, ctx: ctx}
}

func (e *round2DriverScopeInstall) submitViewAgentRoot() jobbatch.ID {
	e.t.Helper()
	payload, err := json.Marshal(batchLeafPayload{Tool: "view", ToolCallID: "call-r2-view", SessionID: e.sessionID, Input: json.RawMessage(`{}`), RegistryID: e.registryID, Policy: &batchLeafPolicyContext{RegistryID: e.registryID, RestrictedToolset: e.fingerprint, DiskProvider: true, DiskBindingID: e.bindingID}})
	require.NoError(e.t, err)
	root := jobbatch.ID("root-r2-" + uuid.NewString()[:8])
	_, err = e.install.RunnerHandle.Submit(context.Background(), jobbatch.SubmitRequest{Root: root, Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}}, Ref: "call-r2-view"})
	require.NoError(e.t, err)
	return root
}

func round2ConstructionRegistryIDs(caps *DiskProviderRegistry) map[string]struct{} {
	caps.mu.Lock()
	defer caps.mu.Unlock()
	out := make(map[string]struct{}, len(caps.constructions))
	for id := range caps.constructions {
		out[id] = struct{}{}
	}
	return out
}

// round2StampRunner forwards the counting diskRefusalRunner's observations
// AND the batchReleaseHandle seam: batch_ingress asserts Release on the
// installed runner after a bound submit, so a counting wrapper that drops
// the release seam would fail the invocation with ErrDiskProviderUnavailable
// after the root was already submitted.
type round2StampRunner struct {
	*diskRefusalRunner
	// releaseFrom is the ORIGINAL runner handle captured before wrapping;
	// the Release seam forwards to it (the embedded batchRunnerHandle
	// interface only types Submit/Stop, so Release must be asserted on the
	// original value).
	releaseFrom any
	// installRunner is the install's *jobbatch.Runner fallback for the
	// Release seam when the runtime adapter does not expose one.
	installRunner interface {
		Release(context.Context, jobbatch.ID) error
	}
}

func (r *round2StampRunner) Release(ctx context.Context, id jobbatch.ID) error {
	if rel, ok := r.releaseFrom.(batchReleaseHandle); ok {
		return rel.Release(ctx, id)
	}
	if r.installRunner != nil {
		return r.installRunner.Release(ctx, id)
	}
	return errors.New("round2StampRunner: inner runner has no release handle")
}

func TestAgenticFetchToolProductionStampArmsIngress(t *testing.T) {
	dataDir := t.TempDir()
	var turnReqs atomic.Int64
	fastSrv := round2FetchFastServer(t, dataDir, &turnReqs)
	t.Cleanup(fastSrv.Close)
	smartSrv := singleTurnSSEServer(nil)
	t.Cleanup(smartSrv.Close)
	env := newBatchInventoryEnv(t, "view", inventoryAgentDeps)
	env.install.DiskBindings.setRegistryDrop(env.registries.Delete)
	h := newRound2LiveHarness(t, smartSrv, fastSrv)
	h.coord.sessions = session.NewService(db.New(env.conn), env.conn)
	h.coord.messages = message.NewService(db.New(env.conn))
	// The real view tool records reads through the coordinator's
	// filetracker; bind it to the SAME shared store so the write does not
	// hit a foreign key against another database.
	h.coord.filetracker = filetracker.NewService(db.New(env.conn))
	h.coord.cfg.Config().Options.DataDirectory = dataDir
	runnerOrig := env.install.Runtime.cfg.Runner
	runner := &round2StampRunner{diskRefusalRunner: &diskRefusalRunner{batchRunnerHandle: runnerOrig}, releaseFrom: runnerOrig, installRunner: env.install.Runner}
	env.install.Runtime.cfg.Runner = runner
	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	require.NoError(t, err)
	bind := &batchCoordinatorBinding{ingress: ingress, controls: workingModelControls{}, diskBindings: env.install.DiskBindings}
	coordinatorBatchBindings.Store(h.coord, bind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(h.coord) })
	opts, release, err := BindCallDiskProvider(env.install.DiskBindings, round2FakeDisk{DiskProvider: tools.OSDisk()}, "round2-fetch", &CallOptions{})
	require.NoError(t, err)
	defer release()
	// WithCallOptions auto-provisions the trusted call scope, so the
	// private build pins into the CALL's own scope; this test is the call
	// lifecycle owner and closes it before the disappearance oracle below.
	ctx := withDiskProviderRegistry(WithCallOptions(context.Background(), opts), env.install.DiskBindings)
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, env.sessionID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-r2-fetch")
	pre := round2ConstructionRegistryIDs(env.install.DiskBindings)
	tool, err := h.coord.agenticFetchTool(context.Background(), nil)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-r2-fetch", Name: tools.AgenticFetchToolName, Input: `{"prompt":"read the note"}`})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "round2-fetch-final")
	require.Positive(t, turnReqs.Load())
	require.Positive(t, runner.submitsBound, "the production stamp armed private ingress through SubmitBound")
	require.NotEmpty(t, runner.recordedRoots())
	// The call's own build scope holds the private pin; closing it (the
	// call lifecycle ending) must release the pin, and with the root's
	// execution owner dropped by natural settlement disposal the private
	// construction record disappears.
	require.NotNil(t, opts.buildScope, "the trusted call state carries its own build scope")
	opts.buildScope.close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		leftover := map[string]struct{}{}
		for id := range round2ConstructionRegistryIDs(env.install.DiskBindings) {
			if _, existed := pre[id]; !existed {
				leftover[id] = struct{}{}
			}
		}
		if len(leftover) == 0 {
			break
		}
		if time.Now().After(deadline) {
			env.install.DiskBindings.mu.Lock()
			keys := make([]diskOwnerKey, 0, len(env.install.DiskBindings.owners))
			for k := range env.install.DiskBindings.owners {
				keys = append(keys, k)
			}
			env.install.DiskBindings.mu.Unlock()
			t.Logf("debug owner keys: %+v", keys)
			t.Fatal("private construction did not disappear at handler return")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAgentDriverIncarnationOwnsBuildScope(t *testing.T) {
	env := newRound2DriverScopeInstall(t)
	shared := (&coordinator{}).sharedBuildScopeFor()
	env.stub.mu.Lock()
	env.stub.shared = shared
	env.stub.mu.Unlock()
	root := env.submitViewAgentRoot()
	require.Eventually(t, func() bool { env.stub.mu.Lock(); defer env.stub.mu.Unlock(); return env.stub.observed != nil }, 10*time.Second, 5*time.Millisecond)
	env.stub.mu.Lock()
	observed := env.stub.observed
	env.stub.mu.Unlock()
	require.NotSame(t, shared, observed, "driver must stamp an incarnation-owned scope")
	require.Eventually(t, func() bool {
		var state string
		_ = env.conn.QueryRowContext(context.Background(), `SELECT state FROM batch_nodes WHERE node_id=? AND owner_session_id=?`, root, env.sessionID).Scan(&state)
		return state == "completed" || state == "failed"
	}, 10*time.Second, 5*time.Millisecond)
	require.True(t, env.stub.released.Load(), "driver turn-end close released the construction")
}

func round2SSEChunk(w http.ResponseWriter, chunk string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func round2SSEText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(text)
	round2SSEChunk(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(b)+`},"finish_reason":null}]}`)
	round2SSEChunk(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func round2SSEToolCall(w http.ResponseWriter, id, name, input string) {
	w.Header().Set("Content-Type", "text/event-stream")
	tc, _ := json.Marshal(map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": input}})
	round2SSEChunk(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[`+string(tc)+`]},"finish_reason":null}]}`)
	round2SSEChunk(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func round2FetchFastServer(t *testing.T, dataDir string, turnReqs *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"tools"`)) {
			round2SSEText(w, "title")
			return
		}
		if turnReqs.Add(1) != 1 {
			round2SSEText(w, "round2-fetch-final")
			return
		}
		var fetchDir string
		for i := 0; i < 1000; i++ {
			if dirs, _ := filepath.Glob(filepath.Join(dataDir, "rush-fetch-*")); len(dirs) > 0 {
				fetchDir = dirs[0]
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if fetchDir == "" {
			t.Error("fetch directory did not appear")
			round2SSEText(w, "missing fetch dir")
			return
		}
		if err := os.WriteFile(filepath.Join(fetchDir, "note.txt"), []byte("round2 seeded view target"), 0o644); err != nil {
			t.Errorf("seed view file: %v", err)
		}
		round2SSEToolCall(w, "call-view", "view", `{"file_path":"note.txt"}`)
	}))
}
