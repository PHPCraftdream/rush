package agent

// R11F round-2 P1 tests. COMPILE-ONLY IN THIS WORKTREE (regime: no executed
// mutation runs; CI is the runner). Every test here must (a) compile under
// `go vet`, and (b) FAIL against the recorded P1 mutations:
//
//	P1-1 (owner=host)          → TestBatchInvokeEndToEndOwnerIsInvokingSession
//	P1-2 (root-only read)      → TestBatchInvokeReturnsFullEnvelope /
//	                             TestBatchFailedExecutorEnvelopeIsError /
//	                             TestBatchLoadDoneWithoutLiveHandle
//	P1-3 (agent-name dispatch) → TestBatchAgentRouterDispatchesByRecordedTool
//	P1-4 (latest-registry)     → TestCoordinatorBridgeBindsToStampedInvocation
//	P1-5c (ctx rebuild)        → TestBatchExecutorRunContextUsesPinnedPolicy
//	P1-6 (eager Runner)        → internal/app: TestBatchRuntimeRunnerFactoryAtStart

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// gatingLeaf is a counting leaf whose Run blocks until released, so the test
// can observe the reserved root row WHILE the batch is live and capture the
// root identity.
type gatingLeaf struct {
	countingTool
	release chan struct{}
	once    sync.Once
}

func newGatingLeaf(name string) *gatingLeaf {
	return &gatingLeaf{countingTool: countingTool{name: name}, release: make(chan struct{})}
}

func (l *gatingLeaf) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	l.countingTool.Run(ctx, call)
	select {
	case <-l.release:
	case <-ctx.Done():
		return fantasy.ToolResponse{}, ctx.Err()
	}
	return fantasy.NewTextResponse("ok"), nil
}

func (l *gatingLeaf) letGo() { l.once.Do(func() { close(l.release) }) }

// errLeaf is a leaf that always fails (P1-2 (2)).
type errLeaf struct{ countingTool }

func (l *errLeaf) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	l.countingTool.Run(ctx, call)
	resp := fantasy.NewTextResponse("boom")
	resp.IsError = true
	return resp, nil
}

// batchP1Env is one real-SQLite end-to-end environment: FKs ON (db.Connect),
// a REAL sessions row for the invoking session, a real BatchStore, a real
// message service over the same DB and one fully installed batch runtime.
type batchP1Env struct {
	t          *testing.T
	conn       *sql.DB
	store      *session.BatchStore
	q          *db.Queries
	sessionID  string
	install    *BatchInstall
	registries *BatchLeafRegistryStore
	registryID string
	leaf       fantasy.AgentTool
	ctx        context.Context
}

// newBatchP1Env installs the runtime over a real database. The pinned leaf
// set contains exactly one leaf named after leaf.Info().Name (plus extras).
func newBatchP1Env(t *testing.T, leaf fantasy.AgentTool, extraLeaves ...fantasy.AgentTool) *batchP1Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		cancel()
		t.Fatalf("db.Connect: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = db.ReleaseConn(conn) })
	q := db.New(conn)
	// P1-1: batch_nodes.owner_session_id REFERENCES sessions(id) (FKs ON) —
	// a REAL sessions row must exist for the invoking session before any
	// Submit.
	sessionID := "sess-" + uuid.NewString()
	if _, err := q.CreateSession(ctx, db.CreateSessionParams{ID: sessionID, Title: sessionID}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	built := newPolicyToolBuild(append([]fantasy.AgentTool{leaf}, extraLeaves...))
	registries := NewBatchLeafRegistryStore()
	registryID := uuid.NewString()
	if err := registries.Register(registryID, built.leaves); err != nil {
		t.Fatalf("register registry: %v", err)
	}
	profile, err := NewBatchProfileDefault()
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	install, err := InstallBatchRuntime(ctx, BatchInstallDeps{
		Host: "host-" + uuid.NewString(), RunnerID: "runner-" + uuid.NewString(),
		Profile: profile, Registries: registries,
		Store:    session.NewBatchStore(conn),
		Messages: message.NewService(q),
		// R11F P1-1: NO Owner — the commit hook resolves it per invocation
		// from the spec payload; Owner=host would be an FK violation.
		// Round-2 P1-7 item 6: NewRunner also fails closed on zero limits.
		Limits:        jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64},
		CommitTimeout: time.Second, ControlTimeout: time.Second, RecordRetention: 64,
	})
	if err != nil {
		t.Fatalf("InstallBatchRuntime: %v", err)
	}
	t.Cleanup(func() { _ = install.Runner.Close(context.Background()) })
	return &batchP1Env{
		t: t, conn: conn, store: session.NewBatchStore(conn), q: q,
		sessionID: sessionID, install: install, registries: registries,
		registryID: registryID, leaf: leaf, ctx: ctx,
	}
}

// invoke runs the REAL ingress (batchRuntime.Invoke — not a fake) for one
// call and returns the response.
func (e *batchP1Env) invoke(callID, toolName string, leaf fantasy.AgentTool) (fantasy.ToolResponse, error) {
	inv := BatchInvocation{
		SessionID: e.sessionID, MessageID: "msg-1", RegistryID: e.registryID,
		Origin: "coordinator", WaiterRank: batchGroupRanks[batchGroupIO],
		Policy: batchLeafPolicyContext{RegistryID: e.registryID, RestrictedToolset: e.registryID},
	}
	return e.install.Runtime.Invoke(e.ctx, inv, leaf, fantasy.ToolCall{ID: callID, Name: toolName, Input: `{"p":1}`})
}

// liveRootID polls the store for the live root of the invoking session (the
// reservation is committed by the commit hook at Submit).
func (e *batchP1Env) liveRootID() string {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		roots, err := e.q.ListLiveBatchRootsForOwner(e.ctx, e.sessionID)
		if err == nil && len(roots) > 0 {
			return roots[0].RootID
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatal("no live batch root reserved for the invoking session")
	return ""
}

// setProcessBatchBindingForTest installs the process batch binding and
// restores the previous one at cleanup (the binding is process-global; the
// test must not leave other tests flag-on).
func setProcessBatchBindingForTest(t *testing.T, bind *batchCoordinatorBinding) {
	t.Helper()
	prev := processBatchBinding.Load()
	processBatchBinding.Store(bind)
	t.Cleanup(func() { processBatchBinding.Store(prev) })
}

// TestBatchInvokeEndToEndOwnerIsInvokingSession (P1-1): the REAL ingress
// Invoke for one work tool with a fake executor leaf (a) writes the root +
// task rows with owner = the INVOKING session (never the host UUID), (b)
// settles the envelope, and (c) returns the executor's response.
func TestBatchInvokeEndToEndOwnerIsInvokingSession(t *testing.T) {
	leaf := newGatingLeaf("view")
	env := newBatchP1Env(t, leaf)

	type outcome struct {
		resp fantasy.ToolResponse
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := env.invoke("call-e2e-1", "view", leaf)
		done <- outcome{resp, err}
	}()

	root := env.liveRootID()
	// (a) root + task rows exist, owned by the invoking session.
	nodes, err := env.q.ListBatchNodes(env.ctx, db.ListBatchNodesParams{RootID: root, OwnerSessionID: env.sessionID})
	if err != nil {
		t.Fatalf("list batch nodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("root+task rows = %d, want 2", len(nodes))
	}
	rootRow, taskRow := nodes[0], nodes[1]
	if rootRow.Kind != "batch" || rootRow.ParentID.Valid {
		t.Fatalf("root row kind %q parent %v", rootRow.Kind, rootRow.ParentID)
	}
	if taskRow.Kind != "task" || !taskRow.ParentID.Valid || taskRow.ParentID.String != root {
		t.Fatalf("task row kind %q parent %v", taskRow.Kind, taskRow.ParentID)
	}
	for _, n := range nodes {
		if n.OwnerSessionID != env.sessionID {
			t.Fatalf("node %s owner %q, want the invoking session %q (host UUID as owner = FK violation)", n.NodeID, n.OwnerSessionID, env.sessionID)
		}
	}

	leaf.letGo()
	out := <-done
	if out.err != nil {
		t.Fatalf("Invoke: %v", out.err)
	}
	// (b)+(c): the envelope settles on the settled task child and Invoke
	// returns the executor's response.
	if out.resp.Content != "ok" || out.resp.IsError {
		t.Fatalf("Invoke response %+v, want the executor's own ok envelope", out.resp)
	}
	e2, ok, err := env.store.BatchDeliveryOneItemCompletion(env.ctx, env.sessionID, root)
	if err != nil || !ok {
		t.Fatalf("one-item completion read ok=%v err=%v", ok, err)
	}
	if e2.State != "completed" {
		t.Fatalf("envelope state %q, want completed", e2.State)
	}
}

// TestBatchInvokeReturnsFullEnvelope (P1-2 (1)): the e2e Invoke returns the
// executor's FULL envelope (text content, IsError=false) after real
// settlement — this mutation-fails when LoadDone reads only the root row
// (the envelope lives on the settled task child root.1).
func TestBatchInvokeReturnsFullEnvelope(t *testing.T) {
	leaf := newGatingLeaf("view")
	env := newBatchP1Env(t, leaf)
	done := make(chan error, 1)
	var resp fantasy.ToolResponse
	go func() {
		var err error
		resp, err = env.invoke("call-e2e-2", "view", leaf)
		done <- err
	}()
	env.liveRootID()
	leaf.letGo()
	if err := <-done; err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Content != "ok" || resp.IsError {
		t.Fatalf("full envelope not delivered after settlement: %+v", resp)
	}
}

// TestBatchFailedExecutorEnvelopeIsError (P1-2 (2)): a failed-executor
// settlement returns IsError=true, never a collapsed success.
func TestBatchFailedExecutorEnvelopeIsError(t *testing.T) {
	leaf := &errLeaf{countingTool: countingTool{name: "view"}}
	env := newBatchP1Env(t, leaf)
	resp, err := env.invoke("call-e2e-3", "view", leaf)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.IsError || resp.Content != "boom" {
		t.Fatalf("failed settlement envelope %+v, want IsError=true content boom", resp)
	}
}

// TestBatchLoadDoneWithoutLiveHandle (P1-2 (3)): simulated restart — a fresh
// InstallBatchRuntime over the same DB with a NEW Runner returns the SAME
// envelope from LoadDone/AwaitDone without any live in-process Batch.
func TestBatchLoadDoneWithoutLiveHandle(t *testing.T) {
	leaf := newGatingLeaf("view")
	env := newBatchP1Env(t, leaf)
	done := make(chan error, 1)
	var resp fantasy.ToolResponse
	go func() {
		var err error
		resp, err = env.invoke("call-e2e-4", "view", leaf)
		done <- err
	}()
	root := env.liveRootID()
	leaf.letGo()
	if err := <-done; err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Content != "ok" || resp.IsError {
		t.Fatalf("first settlement %+v", resp)
	}

	// Fresh install over the SAME DB, NEW Runner incarnation: nothing is
	// live, the settled root is foreign to the new runnerAdapter.
	profile, err := NewBatchProfileDefault()
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	fresh, err := InstallBatchRuntime(env.ctx, BatchInstallDeps{
		Host: "host-2", RunnerID: "runner-2",
		Profile: profile, Registries: env.registries,
		Store: env.store, Messages: message.NewService(env.q),
		Limits:        jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64},
		CommitTimeout: time.Second, ControlTimeout: time.Second, RecordRetention: 64,
	})
	if err != nil {
		t.Fatalf("fresh install: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Runner.Close(context.Background()) })
	got, ok, err := fresh.Done.LoadDone(env.ctx, env.sessionID, jobbatch.ID(root))
	if err != nil || !ok {
		t.Fatalf("restart LoadDone ok=%v err=%v", ok, err)
	}
	if got.Content != "ok" || got.IsError {
		t.Fatalf("restart envelope %+v, want the settled executor envelope", got)
	}
	again, ok, err := fresh.Done.AwaitDone(env.ctx, env.sessionID, jobbatch.ID(root))
	if err != nil || !ok {
		t.Fatalf("restart AwaitDone ok=%v err=%v", ok, err)
	}
	if again.Content != "ok" || again.IsError {
		t.Fatalf("restart AwaitDone envelope %+v", again)
	}
}

// TestBatchAgentRouterDispatchesByRecordedTool (P1-3): driving the REAL
// executor router, an "agent" payload and an "agentic_fetch" payload each
// reach their OWN pinned leaf (two distinct counting leaves in the registry);
// the pre-fix driver named every call "agent", so the agentic_fetch leaf
// never ran.
func TestBatchAgentRouterDispatchesByRecordedTool(t *testing.T) {
	agentLeaf := &countingTool{name: AgentToolName}
	fetchLeaf := &countingTool{name: "agentic_fetch"}
	env := newBatchP1Env(t, agentLeaf, fetchLeaf)

	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	if err != nil {
		t.Fatalf("ingress: %v", err)
	}
	// The driver resolves through the PROCESS binding; install it for the
	// router and restore it afterwards so other tests stay flag-off.
	setProcessBatchBindingForTest(t, &batchCoordinatorBinding{ingress: ingress})

	// Router deps: the driver runs the delegation to completion inside the
	// turn, so the scope reader here is a confirmed-complete read; the
	// nil-reader fail-closed shape is asserted separately below.
	deps := CoordinatorBatchAgentDeps()
	deps.Descendants = batchCompleteDescendantsReader{}
	executor := newBatchExecutor(batchLeafEnv{Leaves: env.registries.Resolve}, newBatchExecutorFamilies(deps))
	for _, name := range []string{AgentToolName, "agentic_fetch"} {
		payload, err := json.Marshal(batchLeafPayload{
			Tool: name, ToolCallID: "call-" + name, SessionID: env.sessionID,
			Input: json.RawMessage(`{"prompt":"hi"}`), RegistryID: env.registryID,
		})
		if err != nil {
			t.Fatalf("payload: %v", err)
		}
		results := make(chan jobbatch.Result, 1)
		if _, err := executor.Start(env.ctx, jobbatch.Launch{
			ID: jobbatch.ID("node-" + name), Token: 1,
			Task: jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload},
		}, func(r jobbatch.Result) { results <- r }); err != nil {
			t.Fatalf("%s: executor start: %v", name, err)
		}
		select {
		case r := <-results:
			if r.State != jobbatch.StateCompleted {
				t.Fatalf("%s: result %+v, want completed", name, r)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: executor did not report", name)
		}
	}
	if agentLeaf.runCount() != 1 {
		t.Fatalf("agent leaf ran %d times, want exactly 1", agentLeaf.runCount())
	}
	if fetchLeaf.runCount() != 1 {
		t.Fatalf("agentic_fetch leaf ran %d times, want exactly 1 (P1-3: dispatch by the recorded tool name)", fetchLeaf.runCount())
	}
}

// TestCoordinatorBridgeBindsToStampedInvocation (P1-4): two interleaved
// invocations with different pinned registries — each stamped context
// resolves ITS OWN registry; an unstamped (coordinator non-batch) context
// keeps the legacy no-bridge behaviour. With the removed "latest registry"
// the two stamped contexts can never cross-contaminate.
func TestCoordinatorBridgeBindsToStampedInvocation(t *testing.T) {
	leafA := &countingTool{name: "view"}
	leafB := &countingTool{name: "view"}
	regA := newPolicyToolBuild([]fantasy.AgentTool{leafA}).leaves
	regB := newPolicyToolBuild([]fantasy.AgentTool{leafB}).leaves
	registries := NewBatchLeafRegistryStore()
	if err := registries.Register("reg-A", regA); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if err := registries.Register("reg-B", regB); err != nil {
		t.Fatalf("register B: %v", err)
	}
	fake := &bridgeTestIngress{registries: registries}
	setProcessBatchBindingForTest(t, &batchCoordinatorBinding{ingress: fake})

	bridgeA := coordinatorBridgeFallback(withBatchBridgeInvocation(context.Background(), batchBridgeInvocation{RegistryID: "reg-A", MessageID: "m1"}))
	bridgeB := coordinatorBridgeFallback(withBatchBridgeInvocation(context.Background(), batchBridgeInvocation{RegistryID: "reg-B", MessageID: "m2"}))
	if bridgeA == nil || bridgeB == nil {
		t.Fatalf("stamped invocations must each get a bridge (A=%v B=%v)", bridgeA, bridgeB)
	}
	callA := fantasy.ToolCall{ID: "c-a1", Name: "view", Input: `{}`}
	callB := fantasy.ToolCall{ID: "c-b1", Name: "view", Input: `{}`}
	// Interleaved: A, B, A — each call must cross the authority with the
	// bridge's OWN registry identity and run ITS OWN leaf; P1-4: a bridge
	// bound to the latest registry would route A's second call to reg-B.
	resp, err := bridgeA.Call(context.Background(), callA)
	if err != nil || resp.Content != "ok" {
		t.Fatalf("A call 1: resp %q err %v", resp.Content, err)
	}
	if got := fake.lastInvocation().RegistryID; got != "reg-A" {
		t.Fatalf("A call 1 crossed the authority as registry %q, want reg-A", got)
	}
	resp, err = bridgeB.Call(context.Background(), callB)
	if err != nil || resp.Content != "ok" {
		t.Fatalf("B call 1: resp %q err %v", resp.Content, err)
	}
	if got := fake.lastInvocation().RegistryID; got != "reg-B" {
		t.Fatalf("B call 1 crossed the authority as registry %q, want reg-B (cross-contamination)", got)
	}
	resp, err = bridgeA.Call(context.Background(), callA)
	if err != nil || resp.Content != "ok" {
		t.Fatalf("A call 2: resp %q err %v", resp.Content, err)
	}
	if got := fake.lastInvocation().RegistryID; got != "reg-A" {
		t.Fatalf("A call 2 crossed the authority as registry %q, want reg-A (latest-registry contamination)", got)
	}
	if got := fake.callsFor("reg-A"); got != 2 {
		t.Fatalf("reg-A crossed %d times, want 2", got)
	}
	if got := fake.callsFor("reg-B"); got != 1 {
		t.Fatalf("reg-B crossed %d times, want 1", got)
	}
	if got := leafA.runCount(); got != 2 {
		t.Fatalf("reg-A leaf ran %d times, want 2", got)
	}
	if got := leafB.runCount(); got != 1 {
		t.Fatalf("reg-B leaf ran %d times, want 1", got)
	}
	if got := coordinatorBridgeFallback(context.Background()); got != nil {
		t.Fatalf("unstamped context must keep the legacy no-bridge behaviour, got %v", got)
	}
	if got := coordinatorBridgeFallback(withBatchBridgeInvocation(context.Background(), batchBridgeInvocation{})); got != nil {
		t.Fatalf("stamp without a registry id must be refused, got %v", got)
	}
}

// bridgeTestIngress is a minimal BatchWorkIngress over a real registry store
// for the bridge test. It records the LAST invocation (with its RegistryID)
// and a per-registry call count under a mutex, and RUNS the leaf so bridge
// calls produce real responses.
type bridgeTestIngress struct {
	registries *BatchLeafRegistryStore

	mu      sync.Mutex
	invokes int
	last    BatchInvocation
	perReg  map[string]int
}

func (f *bridgeTestIngress) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	f.mu.Lock()
	f.invokes++
	f.last = inv
	if f.perReg == nil {
		f.perReg = make(map[string]int)
	}
	f.perReg[inv.RegistryID]++
	f.mu.Unlock()
	return leaf.Run(ctx, call)
}

func (f *bridgeTestIngress) lastInvocation() BatchInvocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *bridgeTestIngress) callsFor(registryID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.perReg[registryID]
}

func (f *bridgeTestIngress) PinLeafRegistry(r *BatchLeafRegistry) (string, error) {
	id := uuid.NewString()
	return id, f.registries.Register(id, r)
}

func (f *bridgeTestIngress) ResolveLeaf(registryID, name string) (fantasy.AgentTool, error) {
	reg, err := f.registries.Resolve(registryID)
	if err != nil {
		return nil, err
	}
	return reg.resolve(name)
}

func (f *bridgeTestIngress) Registry(registryID string) (*BatchLeafRegistry, error) {
	return f.registries.Resolve(registryID)
}

// batchPolicyProbeTool records the ctx values the executor rebuilt for it.
type batchPolicyProbeTool struct {
	countingTool
	seen chan context.Context
}

func (l *batchPolicyProbeTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	l.seen <- ctx
	return fantasy.NewTextResponse("ok"), nil
}

// TestBatchExecutorRunContextUsesPinnedPolicy (P1-5c executor side): the
// generic family rebuilds the leaf run context EXCLUSIVELY from the payload's
// PINNED policy transport — the Start context's conflicting values lose.
func TestBatchExecutorRunContextUsesPinnedPolicy(t *testing.T) {
	probe := &batchPolicyProbeTool{
		countingTool: countingTool{name: "recorder"},
		seen:         make(chan context.Context, 1),
	}
	reg := newPolicyToolBuild([]fantasy.AgentTool{probe})
	registries := NewBatchLeafRegistryStore()
	if err := registries.Register("reg-policy", reg.leaves); err != nil {
		t.Fatalf("register: %v", err)
	}
	executor := newBatchExecutor(batchLeafEnv{Leaves: registries.Resolve}, newBatchExecutorFamilies(batchAgentDeps{}))

	pinned := batchLeafPolicyContext{
		RegistryID: "reg-policy", MessageID: "pinned-msg", SupportsImages: true,
		ModelName:    "pinned-model",
		FolderScope:  &permission.FolderScopeSpec{},
		RunAllowlist: &permission.RunAllowlistSpec{},
		CustomDisk:   "pinned-disk",
	}
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "recorder", ToolCallID: "call-policy", SessionID: "pinned-session",
		Input: json.RawMessage(`{}`), RegistryID: "reg-policy", Policy: &pinned,
	})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	// The Start ctx carries CONFLICTING values: pinned must win.
	startCtx := context.WithValue(context.Background(), tools.SessionIDContextKey, "start-session")
	startCtx = context.WithValue(startCtx, tools.MessageIDContextKey, "start-msg")
	results := make(chan jobbatch.Result, 1)
	if _, err := executor.Start(startCtx, jobbatch.Launch{
		ID: "n1", Token: 1, Task: jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload},
	}, func(r jobbatch.Result) { results <- r }); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case r := <-results:
		if r.State != jobbatch.StateCompleted {
			t.Fatalf("result %+v, want completed", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not report")
	}
	var leafCtx context.Context
	select {
	case leafCtx = <-probe.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("probe leaf never ran")
	}
	if got := tools.GetSessionFromContext(leafCtx); got != "pinned-session" {
		t.Fatalf("session id %q, want the pinned payload session", got)
	}
	if got := tools.GetMessageFromContext(leafCtx); got != "pinned-msg" {
		t.Fatalf("message id %q, want the PINNED message id (start-ctx value leaked)", got)
	}
	if got, _ := leafCtx.Value(batchFolderScopeContextKey{}).(*permission.FolderScopeSpec); got == nil {
		t.Fatal("pinned folder scope lost in the executor-built ctx")
	}
	if got, _ := leafCtx.Value(batchRunAllowlistContextKey{}).(*permission.RunAllowlistSpec); got == nil {
		t.Fatal("pinned run allowlist lost in the executor-built ctx")
	}
	if got, _ := leafCtx.Value(batchCustomDiskContextKey{}).(string); got != "pinned-disk" {
		t.Fatalf("custom disk %q, want the pinned disk", got)
	}
}

// batchCompleteDescendantsReader is the confirmed-complete scope read used by
// router tests (the coordinator driver runs its delegation to completion
// inside the turn).
type batchCompleteDescendantsReader struct{}

func (batchCompleteDescendantsReader) DescendantsLive(context.Context, batchAgentRunRequest) (bool, bool) {
	return false, true
}

// TestBatchDescendantsNilStoreStaysIncomplete (P1-5b): without a store-backed
// reader the read is INCOMPLETE — (false, false), never the old optimistic
// (false, true) that closed the scope on a guess.
func TestBatchDescendantsNilStoreStaysIncomplete(t *testing.T) {
	live, complete := CoordinatorBatchAgentDeps().Descendants.DescendantsLive(context.Background(), batchAgentRunRequest{SessionID: "s"})
	if live || complete {
		t.Fatalf("nil-store descendant read = (%v, %v), want (false, false): incomplete keeps the scope open", live, complete)
	}
}
