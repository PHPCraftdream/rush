// R11F round-2 P1-7 item 1: REAL per-family acceptance inventory. Every work
// family is driven through the REAL ingress (batchRuntime.Invoke, the
// installed wiring — for the CLI bridge through coordinatorBridgeFallback on
// a stamped invocation context) over a REAL Runner on a REAL in-memory-shape
// SQLite database (db.Connect, FKs ON, a real sessions row for the invoking
// session, the commit hook wired), with a FAKE executor leaf (worker A's P1
// convention) wrapped in COUNTING hook and permission fakes so the once-only
// guarantees are observable. Compile-only in this worktree (CI runs it).
//
// Per family it asserts: exactly ONE batch root for the call (root row +
// single task child), the OLD async ledger recorded NOTHING for the owner
// session, the hook ran EXACTLY ONCE and the permission gate was asked
// EXACTLY ONCE, and exactly ONE completion is delivered carrying the
// executor's FULL envelope. A fake ingress would prove none of this.
package agent

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// inventoryCounters counts hook firings and permission-gate asks across one
// inventory environment.
type inventoryCounters struct {
	hooks atomic.Int64
	perms atomic.Int64
}

// inventoryHookLeaf mirrors the hookedTool LAYER of decoratePolicyTools
// (hooks outermost, restriction inside) with a counting hook body. The
// production hooks.Runner is a concrete type with no seam, so the counting
// fake reproduces the layering contract instead of wrapping it.
type inventoryHookLeaf struct {
	fantasy.AgentTool
	c *inventoryCounters
}

func (h inventoryHookLeaf) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	h.c.hooks.Add(1)
	return h.AgentTool.Run(ctx, call)
}

// inventoryPermAuthorizer is the counting permission fake: it answers the
// restricted-run gate "not restricted" for both the dispatch and tool
// authorizer shapes, and counts every ask.
type inventoryPermAuthorizer struct{ c *inventoryCounters }

func (a inventoryPermAuthorizer) AuthorizeRestrictedRun(permission.CreatePermissionRequest) (bool, bool) {
	a.c.perms.Add(1)
	return false, false
}

func (a inventoryPermAuthorizer) AuthorizeRestrictedTool(permission.CreatePermissionRequest) (bool, bool) {
	a.c.perms.Add(1)
	return false, false
}

// inventoryWrappedLeaf builds one pinned leaf with the production layer
// order: counting hook outermost, the production restrictedTool (with the
// counting authorizer) inside, the counting executor leaf innermost.
func inventoryWrappedLeaf(tool string, c *inventoryCounters) fantasy.AgentTool {
	inner := &countingTool{name: tool}
	restricted := restrictedTool{AgentTool: inner, authorizer: inventoryPermAuthorizer{c: c}}
	return inventoryHookLeaf{AgentTool: restricted, c: c}
}

// batchInventoryEnv is one real end-to-end environment per family case.
type batchInventoryEnv struct {
	t          *testing.T
	conn       *sql.DB
	store      *session.BatchStore
	q          *db.Queries
	sessionID  string
	install    *BatchInstall
	registries *BatchLeafRegistryStore
	registryID string
	leaf       fantasy.AgentTool
	counters   *inventoryCounters
	ctx        context.Context
}

// newBatchInventoryEnv installs the full runtime over a real database with
// the family's counting-wrapped leaf pinned as its only registry entry.
func newBatchInventoryEnv(t *testing.T, tool string, opts ...func(*BatchInstallDeps)) *batchInventoryEnv {
	return newBatchInventoryEnvWithExecutor(t, tool, nil, opts...)
}

func newBatchInventoryEnvWithExecutor(t *testing.T, tool string, executor jobbatch.Executor, opts ...func(*BatchInstallDeps)) *batchInventoryEnv {
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
	sessionID := "sess-" + uuid.NewString()
	if _, err := q.CreateSession(ctx, db.CreateSessionParams{ID: sessionID, Title: sessionID}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	counters := &inventoryCounters{}
	leaf := inventoryWrappedLeaf(tool, counters)
	built := newPolicyToolBuild([]fantasy.AgentTool{leaf})
	registries := NewBatchLeafRegistryStore()
	registryID := uuid.NewString()
	if err := registries.Register(registryID, built.leaves); err != nil {
		t.Fatalf("register registry: %v", err)
	}
	profile, err := NewBatchProfileDefault()
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	deps := BatchInstallDeps{
		Host: "host-" + uuid.NewString(), RunnerID: "runner-" + uuid.NewString(),
		Executor: executor,
		Profile:  profile, Registries: registries,
		DiskBindings: NewDiskProviderRegistry(),
		Store:        session.NewBatchStore(conn), Messages: message.NewService(q), Sessions: session.NewService(q, conn),
		// Round-2 P1-7 item 6: NewRunner fails closed on zero limits.
		Limits:        jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64},
		CommitTimeout: time.Second, ControlTimeout: time.Second, RecordRetention: 64,
	}
	for _, opt := range opts {
		opt(&deps)
	}
	install, err := InstallBatchRuntime(ctx, deps)
	if err != nil {
		t.Fatalf("InstallBatchRuntime: %v", err)
	}
	t.Cleanup(func() { _ = install.Runner.Close(context.Background()) })
	// R12-H2 §1.2: the executor's Execution owner (and the root lease) name
	// the pinned registry's construction record; hold its Construction owner
	// for the env lifetime exactly as the real call/build scope would.
	pinTestConstruction(t, install.DiskBindings, registryID)
	return &batchInventoryEnv{
		t: t, conn: conn, store: session.NewBatchStore(conn), q: q,
		sessionID: sessionID, install: install, registries: registries,
		registryID: registryID, leaf: leaf, counters: counters, ctx: ctx,
	}
}

// inventoryBindProcess wires the env's real ingress as the process batch
// binding (the coordinator seams the agent family and the CLI bridge read).
func inventoryBindProcess(t *testing.T, env *batchInventoryEnv) {
	t.Helper()
	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	if err != nil {
		t.Fatalf("ingress: %v", err)
	}
	setProcessBatchBindingForTest(t, &batchCoordinatorBinding{ingress: ingress, diskBindings: env.install.DiskBindings})
}

// inventoryAgentDeps wires the R7c coordinator agent-family seams with a
// confirmed-complete descendant read (the driver runs the delegation to
// completion inside the call).
func inventoryAgentDeps(deps *BatchInstallDeps) {
	d := CoordinatorBatchAgentDeps()
	d.Descendants = batchCompleteDescendantsReader{}
	deps.AgentDeps = d
}

// inventoryMCPResolver admits every (non-reentrant) MCP leaf.
func inventoryMCPResolver(deps *BatchInstallDeps) {
	deps.Resolver = BatchNonReentrantMCPResolver()
}

// invoke runs the REAL ingress for one call.
func (e *batchInventoryEnv) invoke(callID, toolName string) (fantasy.ToolResponse, error) {
	inv := BatchInvocation{
		SessionID: e.sessionID, MessageID: "msg-1", RegistryID: e.registryID,
		Origin: "coordinator", WaiterRank: batchGroupRanks[batchGroupIO],
		Policy: batchLeafPolicyContext{RegistryID: e.registryID, RestrictedToolset: e.registryID},
	}
	return e.install.Runtime.Invoke(e.ctx, inv, e.leaf, fantasy.ToolCall{ID: callID, Name: toolName, Input: `{"p":1}`})
}

// assertDelivered is the per-family acceptance contract: one completion, the
// executor's full envelope, exactly one root (root row + one task child of
// the wanted kind), an EMPTY old async ledger for the owner session, and
// exactly one hook firing and one permission ask.
func (e *batchInventoryEnv) assertDelivered(callID, toolName, wantKind string, resp fantasy.ToolResponse, err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("%s: Invoke: %v", toolName, err)
	}
	// (4) exactly ONE completion, the executor's FULL envelope.
	if resp.Content != "ok" || resp.IsError {
		e.t.Fatalf("%s: completion envelope content %q IsError %v, want the executor's own \"ok\"", toolName, resp.Content, resp.IsError)
	}
	// (1) EXACTLY ONE root for the call: one root row plus its task child.
	var rootID string
	if err := e.conn.QueryRowContext(e.ctx,
		`SELECT root_id FROM batch_nodes WHERE owner_session_id = ?1 AND tool_call_id = ?2 AND node_id = root_id`,
		e.sessionID, callID).Scan(&rootID); err != nil {
		e.t.Fatalf("%s: no root row reserved for the call: %v", toolName, err)
	}
	var roots int
	if err := e.conn.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM batch_nodes WHERE owner_session_id = ?1 AND tool_call_id = ?2 AND node_id = root_id`,
		e.sessionID, callID).Scan(&roots); err != nil {
		e.t.Fatalf("%s: count roots: %v", toolName, err)
	}
	if roots != 1 {
		e.t.Fatalf("%s: %d root rows for one call, want exactly 1", toolName, roots)
	}
	var nodes int
	if err := e.conn.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM batch_nodes WHERE root_id = ?1`, rootID).Scan(&nodes); err != nil {
		e.t.Fatalf("%s: count nodes: %v", toolName, err)
	}
	if nodes != 2 {
		e.t.Fatalf("%s: %d node rows under the root, want root + one task child", toolName, nodes)
	}
	// The task child carries the family's executor kind (the ROOT row's
	// task_kind is NULL by schema CHECK — see batch_adapters.go).
	var kind sql.NullString
	if err := e.conn.QueryRowContext(e.ctx,
		`SELECT task_kind FROM batch_nodes WHERE root_id = ?1 AND parent_id = root_id`, rootID).Scan(&kind); err != nil {
		e.t.Fatalf("%s: read task child: %v", toolName, err)
	}
	if !kind.Valid || kind.String != wantKind {
		e.t.Fatalf("%s: task child kind %q, want %q", toolName, kind.String, wantKind)
	}
	// (2) the removed legacy authority is gone from the schema: ANY read of
	// its table fails.
	var jobs int
	if err := e.conn.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM async_jobs`).Scan(&jobs); err == nil {
		e.t.Fatalf("%s: the legacy async_jobs table is still readable", toolName)
	}
	// (3) hook EXACTLY once, permission gate asked EXACTLY once.
	if got := e.counters.hooks.Load(); got != 1 {
		e.t.Fatalf("%s: hook ran %d times, want exactly 1", toolName, got)
	}
	if got := e.counters.perms.Load(); got != 1 {
		e.t.Fatalf("%s: permission gate asked %d times, want exactly 1", toolName, got)
	}
	// ... and the delivered envelope is the persisted completion itself.
	e2, ok, err := e.store.BatchDeliveryOneItemCompletion(e.ctx, e.sessionID, rootID)
	if err != nil || !ok {
		e.t.Fatalf("%s: one-item completion read ok=%v err=%v", toolName, ok, err)
	}
	if e2.State != "completed" {
		e.t.Fatalf("%s: persisted envelope state %q, want completed", toolName, e2.State)
	}
}

// TestBatchWorkInventoryRealFamilies drives every work family through the
// REAL installed system and pins the per-family contract above.
func TestBatchWorkInventoryRealFamilies(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		kind   string
		opts   []func(*BatchInstallDeps)
		run    func(t *testing.T, env *batchInventoryEnv) (fantasy.ToolResponse, error)
		callID string
	}{
		{name: "bash (shell family)", tool: "bash", kind: batchKindShell, callID: "call-inv-bash"},
		{name: "run_command (argv family)", tool: "run_command", kind: batchKindArgv, callID: "call-inv-argv"},
		{name: "view (generic family)", tool: "view", kind: batchKindGeneric, callID: "call-inv-view"},
		{name: "edit (generic family)", tool: "edit", kind: batchKindGeneric, callID: "call-inv-edit"},
		{name: "fs_read (generic family)", tool: "fs_read", kind: batchKindGeneric, callID: "call-inv-fsread"},
		{
			name: "MCP (mcp_ name routes through the MCP kind)", tool: "mcp_files__read",
			kind: batchKindMCP, opts: []func(*BatchInstallDeps){inventoryMCPResolver}, callID: "call-inv-mcp",
			run: func(t *testing.T, env *batchInventoryEnv) (fantasy.ToolResponse, error) {
				// The flag-on binding sets exactly this override
				// (batch_install_ingress.go); reproduce it so the kind
				// routing is observable without a full coordinator.
				setBatchLeafKindOverride(func(tool string) (string, bool) {
					if len(tool) >= 4 && tool[:4] == "mcp_" {
						return batchKindMCP, true
					}
					return "", false
				})
				t.Cleanup(func() { setBatchLeafKindOverride(nil) })
				return env.invoke("call-inv-mcp", "mcp_files__read")
			},
		},
		{
			name: "agent (delegation through the pinned registry)", tool: "agent",
			kind: batchKindAgent, opts: []func(*BatchInstallDeps){inventoryAgentDeps}, callID: "call-inv-agent",
			run: func(t *testing.T, env *batchInventoryEnv) (fantasy.ToolResponse, error) {
				inventoryBindProcess(t, env)
				return env.invoke("call-inv-agent", "agent")
			},
		},
		{
			name: "agentic_fetch (delegation dispatches by the recorded tool)", tool: "agentic_fetch",
			kind: batchKindAgent, opts: []func(*BatchInstallDeps){inventoryAgentDeps}, callID: "call-inv-fetch",
			run: func(t *testing.T, env *batchInventoryEnv) (fantasy.ToolResponse, error) {
				inventoryBindProcess(t, env)
				return env.invoke("call-inv-fetch", "agentic_fetch")
			},
		},
		{
			name: "CLI bridge call (stamped invocation ctx)", tool: "view",
			kind: batchKindGeneric, callID: "call-inv-bridge",
			run: func(t *testing.T, env *batchInventoryEnv) (fantasy.ToolResponse, error) {
				inventoryBindProcess(t, env)
				ctx := batchCallCtx(env.sessionID)
				stamped := withBatchBridgeInvocation(ctx, batchBridgeInvocation{RegistryID: env.registryID, MessageID: "msg-1"})
				bridge := coordinatorBridgeFallback(stamped)
				if bridge == nil {
					t.Fatal("a stamped invocation must resolve its bridge through ITS pinned registry")
				}
				return bridge.Call(ctx, fantasy.ToolCall{ID: "call-inv-bridge", Name: "view", Input: `{"path":"x"}`})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newBatchInventoryEnv(t, tc.tool, tc.opts...)
			run := tc.run
			if run == nil {
				run = func(*testing.T, *batchInventoryEnv) (fantasy.ToolResponse, error) {
					return env.invoke(tc.callID, tc.tool)
				}
			}
			resp, err := run(t, env)
			env.assertDelivered(tc.callID, tc.tool, tc.kind, resp, err)
		})
	}
}

// TestBatchWorkTargetD1ComplementNotBatchWrapped (P1-7 item 1 (5),
// structural): every D1 control name is outside batchWorkTarget, so a build
// containing them (TestBatchWorkInventoryPerFamilyFlagOn) leaves them on
// their direct pinned leaves — never batchIngressTool-armed.
func TestBatchWorkTargetD1ComplementNotBatchWrapped(t *testing.T) {
	for _, name := range []string{
		"ask_question", "await_tasks", "batch_run", "batch_add",
		"batch_status", "batch_output", "batch_stop", "wakein", "wakeon", "loop",
		"wake_list", "wake_cancel", "inspect_agent", "inject_agent", "stop_agent",
	} {
		if batchWorkTarget(name) {
			t.Fatalf("D1 control %s must NOT be a batch work target", name)
		}
	}
}
