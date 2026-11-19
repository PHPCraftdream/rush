// R12-E AW-a acceptance tests (docs/plans/2026-10-07-job-batches-design.md
// rev.4 ~1868-1907): the await/settle contracts AW-a, driven through the
// REAL installed batch runtime (InstallBatchRuntime, the real jobbatch
// Runner, the session.NewBatchCommitHook commit path, real migrated SQLite,
// FKs ON) and the production wake/arbiter/await functions. COMPILE-ONLY IN
// THIS WORKTREE (regime: no executed mutation runs; CI runs it).
//
// Held work uses gatedLeaf: per-tool-call INDEPENDENT started/release gate
// pairs, so one leaf serves a multi-child batch root and no gate can
// consume another's. ctx is deliberately NOT honored once running (held
// termination). Tests that leave a leaf unreleased register releaseAllAt
// via t.Cleanup or release every gate before Runner.Close cleanup.
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// cutoverGate is one tool call's started/release pair. Each gate closes its
// own channels exactly once.
type cutoverGate struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

// gatedLeaf is a counting leaf with INDEPENDENT per-tool-call gates: each
// call gets its own started/release channel pair, so one leaf serves a
// multi-child batch root and no gate can consume another's.
type gatedLeaf struct {
	countingTool
	mu    sync.Mutex
	gates map[string]*cutoverGate
}

func newGatedLeaf(name string) *gatedLeaf {
	return &gatedLeaf{countingTool: countingTool{name: name}, gates: make(map[string]*cutoverGate)}
}

// gate returns (creating on demand) the call's gate.
func (l *gatedLeaf) gate(callID string) *cutoverGate {
	l.mu.Lock()
	defer l.mu.Unlock()
	g := l.gates[callID]
	if g == nil {
		g = &cutoverGate{started: make(chan struct{}), release: make(chan struct{})}
		l.gates[callID] = g
	}
	return g
}

func (l *gatedLeaf) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	l.countingTool.Run(ctx, call)
	g := l.gate(call.ID)
	g.startedOnce.Do(func() { close(g.started) })
	// Held termination: ctx is deliberately not honored.
	<-g.release
	return fantasy.NewTextResponse("held-ok"), nil
}

func (l *gatedLeaf) waitStarted(t *testing.T, callID string) {
	t.Helper()
	select {
	case <-l.gate(callID).started:
	case <-time.After(5 * time.Second):
		t.Fatalf("gated leaf %s: call %s never started", l.name, callID)
	}
}

// letGo releases each named call's gate.
func (l *gatedLeaf) letGo(callIDs ...string) {
	for _, id := range callIDs {
		g := l.gate(id)
		g.releaseOnce.Do(func() { close(g.release) })
	}
}

// letGoAll releases every gate: the t.Cleanup backstop that keeps a held
// call from hanging the Runner.Close cleanup.
func (l *gatedLeaf) letGoAll() {
	l.mu.Lock()
	gates := make([]*cutoverGate, 0, len(l.gates))
	for _, g := range l.gates {
		gates = append(gates, g)
	}
	l.mu.Unlock()
	for _, g := range gates {
		g.releaseOnce.Do(func() { close(g.release) })
	}
}

// releaseAllAt registers the cleanup release for the given leaves.
func releaseAllAt(t *testing.T, leaves ...*gatedLeaf) {
	t.Helper()
	t.Cleanup(func() {
		for _, l := range leaves {
			l.letGoAll()
		}
	})
}

// cutoverEnv is one real end-to-end batch environment per AW scenario.
type cutoverEnv struct {
	t          *testing.T
	ctx        context.Context
	conn       *sql.DB
	q          *db.Queries
	store      *session.BatchStore
	messages   message.Service
	live       *session.BatchLiveReader
	install    *BatchInstall
	registries *BatchLeafRegistryStore
	registryID string
	sessionID  string
	dataDir    string
}

// newCutoverEnv installs the full batch runtime over a real database with
// the given leaves pinned as the registry's only entries and the DEFAULT
// executor (leaves execute inside the runner). The executor resolves by
// NAME from this pinned registry, never from the Invoke argument.
func newCutoverEnv(t *testing.T, limits jobbatch.Limits, leaves ...fantasy.AgentTool) *cutoverEnv {
	return newCutoverEnvDeps(t, limits, nil, BatchAgentDeps{}, leaves)
}

// newCutoverEnvDeps is newCutoverEnv with the two test-only install seams:
// wrapExecutor (nil = the default executor unwrapped, e.g. the AW-b Stop
// gate) and agentDeps (the R7c agent family, e.g. the AW-a5 answer rig).
// Every other default matches newCutoverEnv byte-for-byte.
func newCutoverEnvDeps(t *testing.T, limits jobbatch.Limits, wrapExecutor func(jobbatch.Executor) jobbatch.Executor, agentDeps BatchAgentDeps, leaves []fantasy.AgentTool) *cutoverEnv {
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
	built := newPolicyToolBuild(leaves)
	registries := NewBatchLeafRegistryStore()
	registryID := uuid.NewString()
	if err := registries.Register(registryID, built.Leaves); err != nil {
		t.Fatalf("register registry: %v", err)
	}
	profile, err := NewBatchProfileDefault()
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	// The production publisher seam the install wires when AgentDeps.Runs
	// is set (batch_install_adapters.go): mirrored here because the
	// executor is built OUTSIDE InstallBatchRuntime.
	if agentDeps.Runs != nil {
		agentDeps.QuestionPublisher = batchCoordinatorQuestionPublisher{store: session.NewBatchStore(conn)}
		// Same lifecycle seam production wires (batch_install_adapters.go):
		// accepted answers and cancelled asks close their notices.
		agentDeps.QuestionResolver = batchStoreQuestionResolver{store: session.NewBatchStore(conn)}
	}
	// R12-H2 §1.2: the executor's Execution owner names the pinned
	// registry's construction record; hold its Construction owner for the
	// env lifetime exactly as the real call/build scope would.
	caps := NewDiskProviderRegistry()
	pinTestConstruction(t, caps, registryID)
	install, err := InstallBatchRuntime(ctx, BatchInstallDeps{
		Host: "host-" + uuid.NewString(), RunnerID: "runner-" + uuid.NewString(),
		Profile: profile, Registries: registries,
		DiskBindings: caps,
		Store:        session.NewBatchStore(conn), Messages: message.NewService(q),
		Limits: limits, CommitTimeout: time.Second, ControlTimeout: time.Second,
		RecordRetention: 64, Executor: gatedExecutor(wrapExecutor, registries, agentDeps, caps),
		AgentDeps: agentDeps,
	})
	if err != nil {
		t.Fatalf("InstallBatchRuntime: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = install.Runner.Close(closeCtx)
	})
	live := session.NewBatchLiveReader(db.New(conn), t.TempDir())
	live.IsOwnHost = func(string) bool { return false }
	live.ProbeHost = func(string, string) (session.HostLockStatus, error) {
		return session.HostStatusAlive, nil
	}
	return &cutoverEnv{
		t: t, ctx: ctx, conn: conn, q: q, store: session.NewBatchStore(conn),
		messages: message.NewService(q), live: live, install: install,
		registries: registries, registryID: registryID, sessionID: sessionID,
		dataDir: dataDir,
	}
}

// newCutoverCoordinator builds the fixture coordinator over env's conn: the
// rehomed runtime state plus a REAL sessions service for parent resolution
// (fix 2) and the notice store the delivery paths write.
func newCutoverCoordinator(t *testing.T, env *cutoverEnv) *coordinator {
	t.Helper()
	coord := newBatchRuntimeCoordinator()
	coord.sessions = coresession.NewService(db.New(env.conn), env.conn)
	coord.noticeStore = session.NewSessionNoticeStore(env.conn)
	return coord
}

// cutoverBindFacts binds the node-backed fact source to the coordinator and
// unbinds it at cleanup (the binding map is process-global state keyed by
// the coordinator pointer). writer is the same conn the store wraps.
func cutoverBindFacts(t *testing.T, c *coordinator, env *cutoverEnv) {
	t.Helper()
	BindCoordinatorBatchCutoverFacts(c, NewBatchCutoverNodeFacts(env.store, env.live, env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(c) })
}

// readFactsON calls the REAL ported reader with the flag ON plus binding.
func readFactsON(t *testing.T, coord *coordinator, env *cutoverEnv) TurnFacts {
	t.Helper()
	facts, err := coord.readTurnFacts(env.ctx, env.sessionID, siteCLI)
	require.NoError(t, err)
	return facts
}

// armAwaitAny runs the PRODUCTION top-level ANY arm: bind the coordinator
// to the ingress and call AwaitTasks(mode "any"), which reports the live
// work and NEVER arms the sleep (only mode "all" does).
func armAwaitAny(t *testing.T, coord *coordinator, env *cutoverEnv) tools.AwaitTasksResult {
	t.Helper()
	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	require.NoError(t, err)
	require.NoError(t, BindCoordinatorBatchControls(coord, ingress, nil, env.live))
	res, err := coord.AwaitTasks(env.ctx, env.sessionID, "any", 0)
	require.NoError(t, err)
	return res
}

// awaitAnyThroughTool drives the PRODUCTION await_tasks TOOL layer (not the
// coordinator method directly): tools.NewAwaitTasksTool over the
// coordinator, a context carrying the session id (tools.GetSessionFromContext),
// params Until "any". Returns the text response and its StopTurn flag.
func awaitAnyThroughTool(t *testing.T, coord *coordinator, env *cutoverEnv) (string, bool) {
	t.Helper()
	tool := tools.NewAwaitTasksTool(coord)
	ctx := context.WithValue(env.ctx, tools.SessionIDContextKey, env.sessionID)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID: "await-tool-a3", Name: tools.AwaitTasksToolName, Input: `{"until":"any"}`,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "the tool call succeeds")
	return resp.Content, resp.StopTurn
}

// armSleepAll arms await_tasks' sleep through the PRODUCTION top-level
// path: bind the coordinator to the ingress (AwaitTasks' capture reads the
// bound bind.live) and call AwaitTasks(mode "all"), which sets the
// arbiter's sleepAll flag.
func armSleepAll(t *testing.T, coord *coordinator, env *cutoverEnv) tools.AwaitTasksResult {
	t.Helper()
	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	require.NoError(t, err)
	require.NoError(t, BindCoordinatorBatchControls(coord, ingress, nil, env.live))
	res, err := coord.AwaitTasks(env.ctx, env.sessionID, "all", 0)
	require.NoError(t, err)
	t.Logf("armSleepAll: waiting=%d", len(res.WaitingFor))
	return res
}

// invocation is the detached-mode invocation every AW scenario uses: the
// caller returns immediately with the started response and the root stays
// live in the background.
func (e *cutoverEnv) invocation(callID string) BatchInvocation {
	return BatchInvocation{
		SessionID: e.sessionID, MessageID: "msg-1", RegistryID: e.registryID,
		Origin: "coordinator", WaiterRank: batchGroupRanks[batchGroupIO],
		Delivery: batchDeliveryChoice{Mode: batchDeliveryDetached},
		Policy:   batchLeafPolicyContext{RegistryID: e.registryID, RestrictedToolset: e.registryID},
	}
}

// invokeDetached submits one detached call through the REAL runtime. The
// leaf argument supplies the tool NAME; the executor resolves the pinned
// registry entry of that name.
func (e *cutoverEnv) invokeDetached(callID, toolName string, leaf fantasy.AgentTool) {
	e.t.Helper()
	if _, err := e.install.Runtime.Invoke(e.ctx, e.invocation(callID), leaf,
		fantasy.ToolCall{ID: callID, Name: toolName, Input: `{"p":1}`}); err != nil {
		e.t.Fatalf("%s: detached Invoke: %v", callID, err)
	}
}

// leafItemCalls names the per-item tool call ids of a submitted batch root.
func leafItemCalls(callID string, items int) []string {
	ids := make([]string, 0, items)
	for i := 0; i < items; i++ {
		ids = append(ids, fmt.Sprintf("%s-item-%d", callID, i))
	}
	return ids
}

// submitBatchRoot submits a NotifyAll parallel batch root through the REAL
// runner handle: items one-task Specs whose payloads carry batchLeafPayload
// JSON (Tool matching the registered gated leaf, unique ToolCallID,
// SessionID=this owner -- the commit hook's OwnerFor walks batch items).
// After Submit succeeds the root is ANNOUNCED through the production
// election API (the same batchStoreElector.ElectStarted deliverDetached
// runs): without it the root's selected completion debt rows are invisible
// to every announced-gated read (BatchOutboxPending/DebtNodes require
// announced=1). Returns the requested root id.
func (e *cutoverEnv) submitBatchRoot(callID string, items int, leaf *gatedLeaf) string {
	e.t.Helper()
	calls := leafItemCalls(callID, items)
	specs := make([]jobbatch.Spec, 0, items)
	for _, tc := range calls {
		payload, err := json.Marshal(batchLeafPayload{
			Tool: leaf.name, ToolCallID: tc, SessionID: e.sessionID,
			Input: json.RawMessage(`{"p":1}`), RegistryID: e.registryID,
		})
		require.NoError(e.t, err)
		specs = append(specs, jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}})
	}
	root := uuid.NewString()
	_, err := e.install.RunnerHandle.Submit(e.ctx, jobbatch.SubmitRequest{
		Root: jobbatch.ID(root),
		Spec: jobbatch.Spec{Batch: &jobbatch.BatchSpec{
			Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll, Items: specs,
		}},
		Ref: callID,
	})
	require.NoError(e.t, err, "%s: runner submit", callID)
	won, err := batchStoreElector{store: e.store, messages: e.messages}.
		ElectStarted(e.ctx, e.sessionID, jobbatch.ID(root))
	require.NoError(e.t, err, "%s: started election", callID)
	require.True(e.t, won, "%s: the started election must win on first announce", callID)
	return root
}

// submitBatchRootUnannounced is submitBatchRoot WITHOUT the started
// announcement: the root stays announced=0, so its synchronous inline
// election can win (BatchOutboxInlineElect requires announced=0, one task
// child). Used by the AW-b fixtures whose script calls
// BatchAnnounceInlineResult explicitly.
func (e *cutoverEnv) submitBatchRootUnannounced(callID string, items int, leaf *gatedLeaf) string {
	e.t.Helper()
	calls := leafItemCalls(callID, items)
	specs := make([]jobbatch.Spec, 0, items)
	for _, tc := range calls {
		payload, err := json.Marshal(batchLeafPayload{
			Tool: leaf.name, ToolCallID: tc, SessionID: e.sessionID,
			Input: json.RawMessage(`{"p":1}`), RegistryID: e.registryID,
		})
		require.NoError(e.t, err)
		specs = append(specs, jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}})
	}
	root := uuid.NewString()
	_, err := e.install.RunnerHandle.Submit(e.ctx, jobbatch.SubmitRequest{
		Root: jobbatch.ID(root),
		Spec: jobbatch.Spec{Batch: &jobbatch.BatchSpec{
			Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll, Items: specs,
		}},
		Ref: callID,
	})
	require.NoError(e.t, err, "%s: runner submit", callID)
	return root
}

// rootID reads the single root row reserved for callID (the commit hook's
// reservation is visible to reads).
func (e *cutoverEnv) rootID(callID string) string {
	e.t.Helper()
	var rootID string
	err := e.conn.QueryRowContext(e.ctx,
		`SELECT root_id FROM batch_nodes WHERE owner_session_id = ?1 AND tool_call_id = ?2 AND node_id = root_id`,
		e.sessionID, callID).Scan(&rootID)
	require.NoError(e.t, err, "no root row reserved for %s", callID)
	return rootID
}

// nodeState reads one node's state.
func (e *cutoverEnv) nodeState(nodeID string) string {
	e.t.Helper()
	var state string
	err := e.conn.QueryRowContext(e.ctx,
		`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		nodeID, e.sessionID).Scan(&state)
	require.NoError(e.t, err, "no batch node %s", nodeID)
	return state
}

// nodeControl reads one node's control_state (the executor Stop result:
// 'acked' means the execution's Stop returned without error).
func (e *cutoverEnv) nodeControl(nodeID string) string {
	e.t.Helper()
	var control string
	err := e.conn.QueryRowContext(e.ctx,
		`SELECT control_state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		nodeID, e.sessionID).Scan(&control)
	require.NoError(e.t, err, "no batch node %s", nodeID)
	return control
}

// childNodeID reads the root's task child at the given ordinal (a one-item
// root's executor child is ordinal 1).
func (e *cutoverEnv) childNodeID(root string, ordinal int) string {
	e.t.Helper()
	var nodeID string
	err := e.conn.QueryRowContext(e.ctx,
		`SELECT node_id FROM batch_nodes WHERE root_id = ?1 AND ordinal = ?2 AND node_id != root_id`,
		root, ordinal).Scan(&nodeID)
	require.NoError(e.t, err, "no child ordinal %d under root %s", ordinal, root)
	return nodeID
}

// childState reads the FIRST non-root child's state under root.
func (e *cutoverEnv) childState(root string) string {
	e.t.Helper()
	var state string
	err := e.conn.QueryRowContext(e.ctx,
		`SELECT state FROM batch_nodes WHERE root_id = ?1 AND node_id != root_id ORDER BY ordinal LIMIT 1`,
		root).Scan(&state)
	require.NoError(e.t, err, "no child under root %s", root)
	return state
}

// nodeNotice reads one node's notice/delivery/debt columns.
func (e *cutoverEnv) nodeNotice(nodeID string) (delivery string, wake, reacted, selected int64) {
	e.t.Helper()
	err := e.conn.QueryRowContext(e.ctx,
		`SELECT delivery, wake, reacted, notice_selected FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		nodeID, e.sessionID).Scan(&delivery, &wake, &reacted, &selected)
	require.NoError(e.t, err, "no batch node %s", nodeID)
	return delivery, wake, reacted, selected
}

// rootTerminal polls until the root row reaches a terminal state.
func (e *cutoverEnv) rootTerminal(rootID string) string {
	e.t.Helper()
	var state string
	require.Eventually(e.t, func() bool {
		return dbQueryTerminal(e.t, e.conn, e.ctx, e.sessionID, rootID, &state)
	}, 5*time.Second, 5*time.Millisecond, "root %s never settled", rootID)
	return state
}

func dbQueryTerminal(t *testing.T, conn *sql.DB, ctx context.Context, owner, root string, state *string) bool {
	t.Helper()
	err := conn.QueryRowContext(ctx,
		`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		root, owner).Scan(state)
	if err != nil {
		return false
	}
	switch *state {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

// asyncJobsCount pins the removed legacy authority: the async_jobs table no
// longer exists in the batch-only schema, so ANY read of it fails.
func (e *cutoverEnv) asyncJobsCount() int64 {
	e.t.Helper()
	var n int64
	err := e.conn.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM async_jobs`).Scan(&n)
	require.Error(e.t, err, "the legacy async_jobs authority is gone from the schema")
	return 0
}

// backgroundNoticeCount counts delivered background notices in history.
func (e *cutoverEnv) backgroundNoticeCount() int64 {
	e.t.Helper()
	var n int64
	require.NoError(e.t, e.conn.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?1 AND background_job_notice = 1`,
		e.sessionID).Scan(&n))
	return n
}

// pullBatchNotices runs ONE production batch pull and returns the delivered
// count.
func (e *cutoverEnv) pullBatchNotices() int {
	e.t.Helper()
	pulled, err := e.store.BatchPullNotices(e.ctx, e.messages, e.sessionID, buildBatchNoticeMessageParams)
	require.NoError(e.t, err)
	return len(pulled)
}

// requireBatchPull polls until at least min notices deliver.
func (e *cutoverEnv) requireBatchPull(min int) {
	e.t.Helper()
	require.Eventually(e.t, func() bool { return e.pullBatchNotices() >= min },
		5*time.Second, 5*time.Millisecond, "batch pull never delivered %d notices", min)
}

// capture reads the session scope's live roots ONCE through the real reader.
func (e *cutoverEnv) capture() session.BatchCapturedRoots {
	return e.live.BatchCaptured(e.ctx, []string{e.sessionID})
}

// awaitAsync runs the production BatchAwaitWorker over captured real reads
// in a goroutine and returns its result channel.
func (e *cutoverEnv) awaitAsync(mode string, captured session.BatchCapturedRoots, notifyAll bool, waitCap time.Duration) <-chan BatchAwaitResult {
	out := make(chan BatchAwaitResult, 1)
	go func() {
		res, err := BatchAwaitWorker(e.ctx, BatchAwaitParams{
			Mode: mode,
			Capture: func(context.Context) session.BatchCapturedRoots {
				return captured
			},
			Levels: func(ctx context.Context) session.BatchCapturedLevels {
				return e.live.BatchCapturedLevel(ctx, captured)
			},
			NotifyModeOf: func(session.BatchLiveRef) string {
				if notifyAll {
					return batchNotifyAll
				}
				return batchNotifyEach
			},
			PollInterval: 5 * time.Millisecond,
			WaitCap:      waitCap,
		})
		require.NoError(e.t, err)
		out <- res
	}()
	return out
}

// logNodeRows dumps the asserted node rows for the CI log.
func (e *cutoverEnv) logNodeRows(t *testing.T, roots ...string) {
	t.Helper()
	for _, root := range roots {
		rows, err := e.conn.QueryContext(e.ctx,
			`SELECT node_id, parent_id, kind, state, tool_call_id, notice_selected, wake, reacted, delivery, state
			 FROM batch_nodes WHERE root_id = ?1 ORDER BY ordinal`, root)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id, kind, state2, delivery string
			var parent, call sql.NullString
			var selected, wake, reacted int64
			require.NoError(t, rows.Scan(&id, &parent, &kind, &state2, &call, &selected, &wake, &reacted, &delivery, &state2))
			t.Logf("AW node root=%s id=%s parent=%s kind=%s state=%s call=%v sel=%d wake=%d reacted=%d delivery=%s",
				root, id, parent.String, kind, state2, call, selected, wake, reacted, delivery)
		}
		require.NoError(t, rows.Err())
	}
}

// logDebtRows dumps the selected debt rows and the history notice count.
func (e *cutoverEnv) logDebtRows(t *testing.T) {
	t.Helper()
	rows, err := e.conn.QueryContext(e.ctx,
		`SELECT root_id, node_id, wake, reacted, delivery FROM batch_nodes
		 WHERE owner_session_id = ?1 AND notice_selected = 1`, e.sessionID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var root, node, delivery string
		var wake, reacted int64
		require.NoError(t, rows.Scan(&root, &node, &wake, &reacted, &delivery))
		t.Logf("AW debt root=%s node=%s wake=%d reacted=%d delivery=%s", root, node, wake, reacted, delivery)
	}
	require.NoError(t, rows.Err())
	backgroundNotices := e.backgroundNoticeCount()
	t.Logf("AW history background notices: %d", backgroundNotices)
	for _, entry := range e.historyNoticeSnapshot() {
		t.Logf("AW history notice %s", entry)
	}
}

// historyNoticeSnapshot dumps the ordered background-notice history rows as
// {role, id, notice-kind, text-prefix} entries, so the CI log carries the
// identities and content of the history alongside the bare count. The
// ordering mirrors the production ListMessagesBySession query
// (internal/db/sql/messages.sql): (created_at ASC, rowid ASC) -- rowid is
// the tie-breaker because messages.id is a non-monotonic UUID.
func (e *cutoverEnv) historyNoticeSnapshot() []string {
	e.t.Helper()
	rows, err := e.conn.QueryContext(e.ctx,
		`SELECT id, role, COALESCE(notice_kind, ''), COALESCE(origin, ''), substr(parts, 1, 80)
		 FROM messages WHERE session_id = ?1 AND background_job_notice = 1
		 ORDER BY created_at, rowid`, e.sessionID)
	require.NoError(e.t, err)
	defer rows.Close()
	const maxEntries = 20
	var out []string
	for rows.Next() {
		if len(out) >= maxEntries {
			out = append(out, "... (truncated)")
			break
		}
		var id, role, kind, origin, prefix string
		require.NoError(e.t, rows.Scan(&id, &role, &kind, &origin, &prefix))
		out = append(out, fmt.Sprintf("role=%s id=%s kind=%s origin=%s text=%q",
			role, id, kind, origin, prefix))
	}
	require.NoError(e.t, rows.Err())
	return out
}

// cutoverStateLive mirrors the store's live-state vocabulary.
func cutoverStateLive(state string) bool { return batchNodeStateLive(state) }

// debtExists reads the pending-inclusive selected-notice debt predicate.
func (e *cutoverEnv) debtExists() bool {
	debt, err := e.store.BatchReactionDebtExists(e.ctx, e.sessionID)
	require.NoError(e.t, err)
	return debt
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// ---- AW-a1: a NotifyAll CHILD settlement never wakes the worker and never
// creates selected completion debt while the root is live. ----

func TestAWa1ChildSettlementNoWakeNoDebt(t *testing.T) {
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	rootID := env.submitBatchRoot("call-a1", 2, leaf)
	calls := leafItemCalls("call-a1", 2)
	leaf.waitStarted(t, calls[0])
	leaf.waitStarted(t, calls[1])
	require.Equal(t, "running", env.nodeState(rootID))

	// ALL worker, NotifyAll, captured while the root is live.
	captured := env.capture()
	require.Len(t, captured.Refs(), 1)
	resCh := env.awaitAsync(batchAwaitModeAll, captured, true, 400*time.Millisecond)

	// Complete ONLY the first child: the real engine settles the item.
	leaf.letGo(calls[0])
	require.Eventually(t, func() bool { return env.childState(rootID) == "completed" },
		5*time.Second, 5*time.Millisecond, "child 1 settles through the real engine")
	require.True(t, cutoverStateLive(env.nodeState(rootID)), "the root stays nonterminal on one child")

	// The worker does NOT wake: it times out, the unit stays remaining.
	select {
	case out := <-resCh:
		t.Logf("AW-a1 await mode=%s timedOut=%v finished=%d remaining=%d", out.Mode, out.TimedOut, out.FinishedCount, len(out.Remaining))
		require.True(t, out.TimedOut, "child settlement must not wake a NotifyAll worker")
		require.Empty(t, out.Finished, "no NotifyAll unit finishes on a child alone")
		require.Len(t, out.Remaining, 1)
		require.False(t, out.Remaining[0].ChildFinished, "NotifyAll units never classify as ChildFinished")
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not time out")
	}

	// No selected completion debt exists while the root is live: the commit
	// hook selects the root notice only when the ROOT settles.
	require.False(t, env.debtExists(), "no selected debt for the owner while the root is live")
	_, _, _, rootSelected := env.nodeNotice(rootID)
	require.Zero(t, rootSelected, "the live root has no selected notice")
	require.Zero(t, env.asyncJobsCount(), "the legacy async_jobs authority is gone")

	// Releasing the second child settles the root: the selected debt commits.
	leaf.letGo(calls[1])
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "root settlement creates selected debt")
	t.Logf("AW-a1 root=%s child1=%s child2=%s", rootID, env.childState(rootID), env.nodeState(rootID))
	env.logDebtRows(t)
	env.logNodeRows(t, rootID)
}

// ---- AW-a2: the ROOT settlement unblocks the worker while the completion
// delivery is still held; the production pull delivers exactly once. ----

func TestAWa2RootSettlementUnblocksWorkerDeliveryStillHeld(t *testing.T) {
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	env.invokeDetached("call-a2", "view", leaf)
	leaf.waitStarted(t, "call-a2")
	rootID := env.rootID("call-a2")

	captured := env.capture()
	require.Len(t, captured.Refs(), 1)
	resCh := env.awaitAsync(batchAwaitModeAny, captured, true, 10*time.Second)

	// Settle the real root through the real Runner.
	leaf.letGo("call-a2")
	select {
	case out := <-resCh:
		t.Logf("AW-a2 await mode=%s timedOut=%v finished=%d ref=%s", out.Mode, out.TimedOut, out.FinishedCount, rootID)
		require.False(t, out.TimedOut, "the root settlement must unblock the worker")
		require.Equal(t, 1, out.FinishedCount)
		require.Len(t, out.Finished, 1)
		require.Equal(t, rootID, out.Finished[0].Ref.RootID)
		require.False(t, out.Finished[0].ChildFinished)
	case <-time.After(5 * time.Second):
		t.Fatal("worker never woke on the root settlement")
	}

	// Deterministic happens-before: AT WAKE the selected notice is still
	// HELD (delivery='pending', wake=1, unreacted) -- nothing else pulls.
	delivery, wake, reacted, _ := env.nodeNotice(rootID)
	require.Equal(t, "pending", delivery, "the delivery was still held when the worker woke")
	require.EqualValues(t, 1, wake)
	require.EqualValues(t, 0, reacted)

	// The production pull delivers exactly once, then the row is consumed.
	before := env.backgroundNoticeCount()
	env.requireBatchPull(1)
	require.EqualValues(t, 1, env.backgroundNoticeCount()-before, "exactly one completion delivered by the pull")
	delivery, _, _, _ = env.nodeNotice(rootID)
	require.Equal(t, "done", delivery)
	require.Equal(t, 0, env.pullBatchNotices(), "the second pull is empty: consumed once")
	require.Zero(t, env.asyncJobsCount())
	env.logNodeRows(t, rootID)
}

// ---- AW-a3: top-level ANY launches only on ROOT debt; a live other root
// keeps rule 4b deferring until it settles too. ----

func TestAWa3TopLevelAnyWakesOnlyOnRootDebt(t *testing.T) {
	viewLeaf := newGatedLeaf("view")
	argvLeaf := newGatedLeaf("run_command")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, viewLeaf, argvLeaf)
	releaseAllAt(t, viewLeaf, argvLeaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// R: a 2-child NotifyAll gated root. B: a live gated root.
	rootR := env.submitBatchRoot("call-a3-r", 2, viewLeaf)
	rCalls := leafItemCalls("call-a3-r", 2)
	viewLeaf.waitStarted(t, rCalls[0])
	viewLeaf.waitStarted(t, rCalls[1])
	env.invokeDetached("call-a3-b", "run_command", argvLeaf)
	argvLeaf.waitStarted(t, "call-a3-b")
	rootB := env.rootID("call-a3-b")
	// The PRODUCTION top-level ANY arm (both roots live): it reports the
	// waiting task children and NEVER arms the sleep (only mode "all"
	// does); a second arm is idempotent and equally harmless.
	resAny := armAwaitAny(t, coord, env)
	require.GreaterOrEqual(t, len(resAny.WaitingFor), 1, "phase 1: ANY reports the live task children")
	resAny = armAwaitAny(t, coord, env)
	require.GreaterOrEqual(t, len(resAny.WaitingFor), 1, "phase 1: a second ANY arm is harmless")
	require.False(t, readFactsON(t, coord, env).Session.SleepAll,
		"phase 1: production ANY never arms the sleep")
	// Round 5 (AW-a5 4b): the SAME arm observed through the PRODUCTION TOOL
	// layer: the await_tasks tool's non-blocked path sets StopTurn on the
	// text response and names the live task children.
	toolText, stopTurn := awaitAnyThroughTool(t, coord, env)
	require.True(t, stopTurn, "the ANY arm's tool response sets StopTurn (tools/await_tasks.go)")
	// Production truth (batch_commit.go writes task_kind = Spec.Task.Kind;
	// batch_ingress.go maps run_command onto the argv family): the refs'
	// ToolName is the TASK KIND, not the leaf name.
	require.Contains(t, toolText, batchKindGeneric, "the tool response names the generic batch children by task kind")
	require.Contains(t, toolText, batchKindArgv, "the tool response names the detached run_command root by task kind")
	// The renderer emits "- %s (tool_call_id %s, ...)" per ref, so the call
	// ids tie the names to the real children. Production truth: every
	// subtree ref carries the ROOT's tool_call_id (batchLiveFact propagates
	// level.Node.ToolCallID, read at the root -- session/batch_read.go
	// batchLiveFact), so the R root renders as "call-a3-r"; "call-a3-b" is
	// the detached root's own ref (invokeDetached's BatchInvocation carries
	// ToolCallID=callID and rootID() matches tool_call_id = callID AND
	// node_id = root_id).
	require.Contains(t, toolText, "call-a3-r", "the tool response names the R root's call id (the root ref, not a child id)")
	require.Contains(t, toolText, "call-a3-b", "the tool response names the detached root call id")
	// The ANY variant never arms the sleep (only mode "all" does).

	// Phase 1: no settlement, no debt: nothing launches.
	facts := readFactsON(t, coord, env)
	require.False(t, facts.Debt.PendingIncl, "phase 1: no debt before any settlement")
	require.False(t, facts.Session.SleepAll, "phase 1: ANY never arms the sleep")
	debt, dv, err := coord.drainDecision(env.ctx, env.sessionID, false)
	require.NoError(t, err)
	require.False(t, debt, "phase 1: drainDecision reports no debt")
	t.Logf("AW-a3 phase1 debt=%v verdict=%v", debt, dv)

	// Phase 2: R's FIRST child completes: still no selected root notice.
	viewLeaf.letGo(rCalls[0])
	require.Eventually(t, func() bool { return env.childState(rootR) == "completed" },
		5*time.Second, 5*time.Millisecond, "child 1 settles")
	require.True(t, cutoverStateLive(env.nodeState(rootR)))
	facts = readFactsON(t, coord, env)
	require.False(t, facts.Debt.PendingIncl, "phase 2: a child settlement selects no root notice")
	debt, _, err = coord.drainDecision(env.ctx, env.sessionID, false)
	require.NoError(t, err)
	require.False(t, debt, "phase 2: top-level ANY has NO launch on a child alone")

	// Phase 3: R settles -> root debt while B is live. The sleep is NOT
	// armed (ANY), so rule 4b cannot defer: the root-debt launch is
	// ELIGIBLE. The gate is fresh (zero value: no streak, not paced), so
	// gateShut is false and the verdict is an allow.
	viewLeaf.letGo(rCalls[1])
	require.Equal(t, "completed", env.rootTerminal(rootR))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "root debt after R settles")
	facts = readFactsON(t, coord, env)
	require.True(t, facts.Debt.PendingIncl, "phase 3: the settled root is debt")
	require.True(t, facts.Debt.CompletionOnly, "phase 3: root completion debt is completion-class")
	require.False(t, facts.Session.SleepAll, "phase 3: sleep not armed on ANY")
	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, drainAllow, pv.kind,
		"phase 3: top-level ANY: one eligible launch after root debt")
	t.Logf("AW-a3 phase3 runningOwn=%d kind=%v reason=%q", facts.Session.RunningOwnJobs, pv.kind, pv.reason)

	// Phase 4: B settles too: the debt remains and the launch stays
	// eligible (nothing consumed the debt: no launch actually ran).
	argvLeaf.letGo("call-a3-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))
	require.True(t, env.debtExists(), "phase 4: the durable debt is untouched")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, drainAllow, pv.kind, "phase 4: the launch stays eligible")
	t.Logf("AW-a3 phase4 kind=%v reason=%q", pv.kind, pv.reason)
	env.logDebtRows(t)
	env.logNodeRows(t, rootR, rootB)
}
