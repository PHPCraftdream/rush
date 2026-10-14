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
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
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
	registryID string
	sessionID  string
}

// newCutoverEnv installs the full batch runtime over a real database with
// the given leaves pinned as the registry's only entries and the DEFAULT
// executor (leaves execute inside the runner). The executor resolves by
// NAME from this pinned registry, never from the Invoke argument.
func newCutoverEnv(t *testing.T, limits jobbatch.Limits, leaves ...fantasy.AgentTool) *cutoverEnv {
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
		Store: session.NewBatchStore(conn), Messages: message.NewService(q),
		Limits: limits, CommitTimeout: time.Second, ControlTimeout: time.Second,
		RecordRetention: 64,
	})
	if err != nil {
		t.Fatalf("InstallBatchRuntime: %v", err)
	}
	t.Cleanup(func() { _ = install.Runner.Close(context.Background()) })
	live := session.NewBatchLiveReader(db.New(conn), t.TempDir())
	live.IsOwnHost = func(string) bool { return false }
	live.ProbeHost = func(string, string) (session.HostLockStatus, error) {
		return session.HostStatusAlive, nil
	}
	return &cutoverEnv{
		t: t, ctx: ctx, conn: conn, q: q, store: session.NewBatchStore(conn),
		messages: message.NewService(q), live: live, install: install,
		registryID: registryID, sessionID: sessionID,
	}
}

// newCutoverCoordinator builds the fixture coordinator over env's conn: a
// real workLedger (its async_jobs store is a SEPARATE test DB, unused on the
// ON path) and a REAL sessions service for parent resolution (fix 2) and
// flag tests.
func newCutoverCoordinator(t *testing.T, env *cutoverEnv) *coordinator {
	t.Helper()
	coord := &coordinator{
		subAgentDrivers: newSubAgentDriverRegistry(),
		sessions:        session.NewService(db.New(env.conn), env.conn),
	}
	l := newWorkLedger(coord.notifyAsyncCompletion)
	l.store = newTestAsyncJobStore(t)
	l.coord = coord
	coord.asyncJobs = l
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

// armSleepAll arms the production await_tasks sleep on the fixture arbiter.
func armSleepAll(t *testing.T, coord *coordinator, sessionID string) {
	t.Helper()
	coord.arb.setSleepAll(sessionID)
}

// clearSleepAllArb disarms it (the coordinator funnel cancels schedules too;
// the tests only need the arbiter flag gone).
func clearSleepAllArb(t *testing.T, coord *coordinator, sessionID string) {
	t.Helper()
	_ = coord.arb.clearSleepAll(sessionID)
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
// Returns the requested root id.
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
		specs = append(specs, jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: "test", Payload: payload}})
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

// asyncJobsCount counts the OLD ledger's rows for the owner (must stay 0).
func (e *cutoverEnv) asyncJobsCount() int64 {
	e.t.Helper()
	var n int64
	require.NoError(e.t, e.conn.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM async_jobs WHERE owner_session_id = ?1`, e.sessionID).Scan(&n))
	return n
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
		for rows.Next() {
			var id, kind, state2, delivery string
			var parent, call sql.NullString
			var selected, wake, reacted int64
			require.NoError(t, rows.Scan(&id, &parent, &kind, &state2, &call, &selected, &wake, &reacted, &delivery, &state2))
			t.Logf("AW node root=%s id=%s parent=%s kind=%s state=%s call=%v sel=%d wake=%d reacted=%d delivery=%s",
				root, id, parent.String, kind, state2, call, selected, wake, reacted, delivery)
		}
		require.NoError(t, rows.Close())
	}
}

// logDebtRows dumps the selected debt rows and the history notice count.
func (e *cutoverEnv) logDebtRows(t *testing.T) {
	t.Helper()
	rows, err := e.conn.QueryContext(e.ctx,
		`SELECT root_id, node_id, wake, reacted, delivery FROM batch_nodes
		 WHERE owner_session_id = ?1 AND notice_selected = 1`, e.sessionID)
	require.NoError(t, err)
	for rows.Next() {
		var root, node, delivery string
		var wake, reacted int64
		require.NoError(t, rows.Scan(&root, &node, &wake, &reacted, &delivery))
		t.Logf("AW debt root=%s node=%s wake=%d reacted=%d delivery=%s", root, node, wake, reacted, delivery)
	}
	require.NoError(t, rows.Close())
	t.Logf("AW history background notices: %d", e.backgroundNoticeCount())
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
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
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
	require.Zero(t, env.asyncJobsCount(), "no async_jobs rows for the owner")

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
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
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
	env.requireBatchPull(1)
	require.EqualValues(t, 1, eDelivered(t, env), "exactly one delivered completion")
	delivery, _, _, _ = env.nodeNotice(rootID)
	require.Equal(t, "done", delivery)
	require.Equal(t, 0, env.pullBatchNotices(), "the second pull is empty: consumed once")
	require.Zero(t, env.asyncJobsCount())
	env.logNodeRows(t, rootID)
}

func eDelivered(t *testing.T, env *cutoverEnv) int64 {
	t.Helper()
	return env.backgroundNoticeCount()
}

// ---- AW-a3: top-level ANY launches only on ROOT debt; a live other root
// keeps rule 4b deferring until it settles too. ----

func TestAWa3TopLevelAnyWakesOnlyOnRootDebt(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	viewLeaf := newGatedLeaf("view")
	argvLeaf := newGatedLeaf("run_command")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, viewLeaf, argvLeaf)
	releaseAllAt(t, viewLeaf, argvLeaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	armSleepAll(t, coord, env.sessionID)

	// R: a 2-child NotifyAll gated root. B: a live gated root.
	rootR := env.submitBatchRoot("call-a3-r", 2, viewLeaf)
	rCalls := leafItemCalls("call-a3-r", 2)
	viewLeaf.waitStarted(t, rCalls[0])
	viewLeaf.waitStarted(t, rCalls[1])
	env.invokeDetached("call-a3-b", "run_command", argvLeaf)
	argvLeaf.waitStarted(t, "call-a3-b")
	rootB := env.rootID("call-a3-b")

	// Phase 1: no settlement, no debt: nothing launches.
	facts := readFactsON(t, coord, env)
	require.False(t, facts.Debt.PendingIncl, "phase 1: no debt before any settlement")
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

	// Phase 3: R settles -> root debt, completion-class, B still live:
	// rule 4b defers with EXACTLY the armed-sleep reason.
	viewLeaf.letGo(rCalls[1])
	require.Equal(t, "completed", env.rootTerminal(rootR))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "root debt after R settles")
	facts = readFactsON(t, coord, env)
	require.True(t, facts.Debt.PendingIncl, "phase 3: the settled root is debt")
	require.True(t, facts.Debt.CompletionOnly, "phase 3: root completion debt is completion-class")
	require.Greater(t, facts.Session.RunningOwnJobs, 0, "phase 3: B is live work")
	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, "sleeping until all tasks finish", pv.reason,
		"phase 3: one root-debt-triggered 4b defer, no launch")
	t.Logf("AW-a3 phase3 facts runningOwn=%d reason=%q", facts.Session.RunningOwnJobs, pv.reason)

	// Phase 4: B settles too -> no live work: 4b no longer defers.
	argvLeaf.letGo("call-a3-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))
	require.Eventually(t, func() bool {
		f, err := coord.readTurnFacts(env.ctx, env.sessionID, siteCLI)
		return err == nil && f.Session.RunningOwnJobs == 0
	}, 5*time.Second, 5*time.Millisecond, "phase 4: no live work after B settles")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.NotEqual(t, "sleeping until all tasks finish", pv.reason,
		"phase 4: the sleep hold lifted with the last live root")
	t.Logf("AW-a3 phase4 reason=%q", pv.reason)
	env.logDebtRows(t)
	env.logNodeRows(t, rootR, rootB)
}

// ---- AW-a4: Mode ALL stays asleep while another captured root is live. ----

func TestAWa4AllStaysAsleepWhileOtherRootLive(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	viewLeaf := newGatedLeaf("view")
	argvLeaf := newGatedLeaf("run_command")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, viewLeaf, argvLeaf)
	releaseAllAt(t, viewLeaf, argvLeaf)
	env.invokeDetached("call-a4-r", "view", viewLeaf)
	viewLeaf.waitStarted(t, "call-a4-r")
	rootR := env.rootID("call-a4-r")
	env.invokeDetached("call-a4-b", "run_command", argvLeaf)
	argvLeaf.waitStarted(t, "call-a4-b")
	rootB := env.rootID("call-a4-b")

	captured := env.capture()
	require.Len(t, captured.Refs(), 2)
	resCh := env.awaitAsync(batchAwaitModeAll, captured, true, 600*time.Millisecond)

	// Settle only R; ALL must NOT wake (B still live): it times out.
	viewLeaf.letGo("call-a4-r")
	select {
	case out := <-resCh:
		t.Logf("AW-a4 await mode=%s timedOut=%v finished=%d remaining=%d", out.Mode, out.TimedOut, out.FinishedCount, len(out.Remaining))
		require.True(t, out.TimedOut, "ALL must stay asleep while another root is live")
		require.Equal(t, 1, out.FinishedCount, "R is finished but ALL is not satisfied")
		require.Len(t, out.Remaining, 1)
		require.Equal(t, rootB, out.Remaining[0].Ref.RootID)
		require.True(t, cutoverStateLive(env.nodeState(rootB)), "the other root is untouched")
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not report its timeout")
	}

	// Settle B too: every captured root is terminal, so ALL's wait is over.
	argvLeaf.letGo("call-a4-b")
	require.Equal(t, "completed", env.rootTerminal(rootR))
	require.Equal(t, "completed", env.rootTerminal(rootB))
	t.Logf("AW-a4 both settled r=%s b=%s", env.nodeState(rootR), env.nodeState(rootB))
	env.logDebtRows(t)
	env.logNodeRows(t, rootR, rootB)
}

// ---- AW-a5: a pending child-question debt breaks the armed sleep without
// settling the question. ----

func TestAWa5ChildQuestionBreaksSleepQuestionStillPending(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	// A completed detached root (completion debt, delivery held) plus a live root.
	fast := &countingTool{name: "run_command"}
	env.invokeDetached("call-a5-done", "run_command", fast)
	doneRoot := env.rootID("call-a5-done")
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "completion debt after the detached root settles")
	env.invokeDetached("call-a5-live", "view", leaf)
	leaf.waitStarted(t, "call-a5-live")
	liveRoot := env.rootID("call-a5-live")

	debt := func(context.Context) (session.BatchPendingDebt, error) {
		return env.store.BatchReactionPendingInclusiveDebtRows(env.ctx, env.sessionID)
	}
	live := func(context.Context) session.BatchLiveResult {
		return env.live.BatchLiveForOwner(env.ctx, env.sessionID)
	}
	facts, err := ReadBatchArbiterFacts(env.ctx, BatchArbiterDeps{Live: live, Debt: debt})
	require.NoError(t, err)
	t.Logf("AW-a5 facts before question: liveRoots=%v debt=%v completionOnly=%v",
		facts.LiveRoots, facts.DebtPendingIncl, facts.CompletionOnly)
	require.True(t, facts.LiveRoots, "the live root keeps the hold")
	require.True(t, facts.DebtPendingIncl)
	require.True(t, facts.CompletionOnly, "selected completions are completion-class")
	require.True(t, facts.SleepHolds(true, BatchSleepBreakers{}), "rule 4b holds on completions only")

	// Publish the question through the PRODUCTION publisher path: the live
	// node's token fences the incarnation, the marker text needs \n.
	var token int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT token FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		liveRoot, env.sessionID).Scan(&token))
	require.Greater(t, token, int64(0), "the live root carries an incarnation token")
	publisher := batchCoordinatorQuestionPublisher{store: env.store}
	require.NoError(t, publisher.PublishChildQuestion(env.ctx, batchChildQuestionNotice{
		Owner: env.sessionID, ChildSessionID: "sess-child-a5", NodeID: liveRoot,
		Token: uint64(token), Depth: 1, Sequence: 1, Question: "continue?",
	}))

	facts, err = ReadBatchArbiterFacts(env.ctx, BatchArbiterDeps{Live: live, Debt: debt})
	require.NoError(t, err)
	t.Logf("AW-a5 facts after question: completionOnly=%v", facts.CompletionOnly)
	require.False(t, facts.CompletionOnly, "a question is not completion-class")
	require.False(t, facts.SleepHolds(true, BatchSleepBreakers{}), "the question breaks the armed sleep")

	// The question is NOT settled by the wake: still pending and unreacted.
	var delivery string
	var reacted int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery, reacted FROM session_notices WHERE owner = ?1 AND kind = ?2`,
		env.sessionID, session.NoticeKindChildQuestion).Scan(&delivery, &reacted))
	require.Equal(t, "pending", delivery)
	require.EqualValues(t, 0, reacted)
	require.True(t, cutoverStateLive(env.nodeState(liveRoot)), "the live root is untouched")
	require.Zero(t, env.asyncJobsCount())
	env.logDebtRows(t)
	env.logNodeRows(t, doneRoot, liveRoot)
}
