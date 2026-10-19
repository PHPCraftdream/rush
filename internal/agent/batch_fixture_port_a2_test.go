// Package A2's batch-fixture helpers (pull/compaction/splice lifecycle).
// Everything here is the dual-mode counterpart of a legacy-only seed or
// readback in the A2-owned test files: under RUSH_BATCH_RUNTIME=1 the
// flag-selected authority is the batch node store (reached through the
// coordinator's bound BatchCutoverFacts), so a raw async_jobs seed or read
// is invisible to the turn under test. Names carry the a2 prefix so the
// sibling packages' helpers cannot collide. No helper here mutates
// RUSH_BATCH_RUNTIME.
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// a2AttachBatchFacts gives a ledger-only fixture's coordinator the real
// node-facts binding, so the drain turn's flag-ON reads (visible debt,
// pending-inclusive debt, notice pull) hit the batch store over env's
// database instead of the unknown fail-safe. The ledger keeps its own
// back-link (a.asyncJobs.coord), exactly like the shared attempt fixture.
func a2AttachBatchFacts(t *testing.T, env fakeEnv, ledger *workLedger) *coordinator {
	t.Helper()
	coord := &coordinator{
		subAgentDrivers: newSubAgentDriverRegistry(),
		sessions:        env.sessions, messages: env.messages, permissions: env.permissions,
	}
	ledger.coord = coord
	coord.asyncJobs = ledger
	bindBatchNodeFacts(t, coord, env.conn, env.workingDir)
	return coord
}

// a2ClaimJob seeds the RUNNING half of a legacy job fixture: flag OFF claims
// the async_jobs row (announced on request); flag ON reserves and announces a
// live batch root, so live-scope reads (the supervision check-in's openScope)
// see open work the same way the OFF claim's running async_jobs row does.
func a2ClaimJob(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, owner, callID string, announce bool) {
	t.Helper()
	if batchFlagEnabled() {
		seedBatchDelegation(t, conn, owner, callID, "", "command", "running", false)
		return
	}
	_, err := legacy.Claim(context.Background(), session.ClaimParams{
		Owner: owner, ToolCallID: callID, Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	if announce {
		require.NoError(t, legacy.MarkAnnounced(context.Background(), owner, callID))
	}
}

// a2CompleteJobErr terminal-transitions the seeded job with wake=1. Flag ON
// the completion is projected into the batch store (root and child to
// "completed", the child's completion notice selected, delivery left
// 'pending' until a pull) with summary as the outcome text; OFF it is the
// plain async_jobs Transition. Projection shape mirrors the foundation's
// seedBatchJobDebt (batch_fixture_port_test.go).
func a2CompleteJobErr(ctx context.Context, conn *sql.DB, legacy *session.AsyncJobStore, owner, callID, summary string) error {
	if !batchFlagEnabled() {
		_, err := legacy.Transition(ctx, session.TransitionParams{
			Owner: owner, ToolCallID: callID, State: "completed", ResultSummary: summary, Wake: true,
		})
		return err
	}
	var rootID string
	err := conn.QueryRowContext(ctx,
		`SELECT root_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id ORDER BY rowid DESC LIMIT 1`,
		owner, callID).Scan(&rootID)
	if err != nil {
		return err
	}
	store := session.NewBatchStore(conn)
	snap, err := store.Read(ctx, owner, rootID)
	if err != nil {
		return err
	}
	var child string
	for _, node := range snap.Nodes {
		if node.NodeID != rootID {
			child = node.NodeID
			break
		}
	}
	if child == "" {
		return fmt.Errorf("fixture batch root %s has no child node", rootID)
	}
	return store.Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: owner, Root: rootID,
			Updates: []db.ProjectBatchNodeParams{
				{NodeID: rootID, State: "completed", OutcomeState: "completed", OutcomeSummary: summary, ControlState: "none"},
				{NodeID: child, State: "completed", OutcomeState: "completed", OutcomeSummary: summary, ControlState: "none"},
			},
			Notices: []db.SelectBatchNodeNoticeParams{{
				NodeID:     child,
				NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: "completed", Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true},
				NoticeOrdinal: sql.NullInt64{Int64: 1, Valid: true}, NoticeCompleted: 1, Wake: 1,
			}},
		}},
	})
}

// a2CompleteJob is a2CompleteJobErr with require on the error.
func a2CompleteJob(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, owner, callID, summary string) {
	t.Helper()
	require.NoError(t, a2CompleteJobErr(context.Background(), conn, legacy, owner, callID, summary))
}

// a2WakeDebtFixture is wakeDebtFixture plus the connection handle its
// dual-mode seed/read helpers need. The constructor replicates
// newWakeDebtFixtureOpts (coordinator_wake_reaction_debt_test.go, owned by a
// sibling package) with the store on the shared testEnv database, so the
// batch store and the legacy store see ONE database and the coordinator
// carries the real node-facts binding.
type a2WakeDebtFixture struct {
	*wakeDebtFixture
	conn *sql.DB
}

func newA2WakeDebtFixture(t *testing.T, title string, wireOnSessionIdle bool) *a2WakeDebtFixture {
	t.Helper()
	f := &a2WakeDebtFixture{wakeDebtFixture: &wakeDebtFixture{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		textFinishResponse(w, "reacted")
	}))
	t.Cleanup(f.srv.Close)
	model := newProbeModel(t, f.srv)
	f.model = model

	env := testEnv(t)
	sess, err := env.sessions.Create(context.Background(), title)
	require.NoError(t, err)
	f.sessID = sess.ID

	f.conn = env.conn
	f.store = session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = f.store.Close(context.Background()) })
	f.coord = &coordinator{subAgentDrivers: newSubAgentDriverRegistry(), sessions: env.sessions, messages: env.messages, permissions: env.permissions}
	f.ledger = newWorkLedger(f.coord.notifyAsyncCompletion)
	f.ledger.store = f.store
	f.ledger.coord = f.coord
	f.coord.asyncJobs = f.ledger
	bindBatchNodeFacts(t, f.coord, env.conn, env.workingDir)

	f.messages = &failingCreateTxMessages{Service: env.messages}
	opts := SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: f.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: f.ledger,
	}
	if wireOnSessionIdle {
		opts.OnSessionIdle = f.coord.onSessionIdleHook
	}
	sa := NewSessionAgent(opts)
	f.sa = sa.(*sessionAgent)
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: f.sessID}})
	return f
}

// a2ClaimAndFinish mirrors wakeDebtFixture.claimAndFinish: the in-memory
// ledger job is registered on BOTH paths (its open entry is what keeps this
// bare session's scope open for a later wakeSession), while the terminal
// write goes to the flag-selected authority.
func (f *a2WakeDebtFixture) a2ClaimAndFinish(t *testing.T, ctx context.Context, toolCallID string) {
	t.Helper()
	_, existing, err := f.ledger.Start(f.sessID, toolCallID, toolCallID, "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	if batchFlagEnabled() {
		seedBatchJobDebt(t, f.conn, f.sessID, toolCallID, false)
		return
	}
	require.NoError(t, f.store.MarkAnnounced(ctx, f.sessID, toolCallID))
	a2CompleteJob(t, f.conn, f.store, f.sessID, toolCallID, "output")
}

// a2DebtVisible is wakeDebtFixture.debtVisible through the flag-selected
// authority (the same CaptureVisibleDebt read the drain turn itself makes).
func (f *a2WakeDebtFixture) a2DebtVisible(t *testing.T, ctx context.Context) bool {
	t.Helper()
	if batchFlagEnabled() {
		snap, err := coordinatorBatchCutoverFacts(f.coord).CaptureVisibleDebt(ctx, f.sessID)
		require.NoError(t, err)
		return !snap.Empty()
	}
	visible, err := f.store.VisibleReactionDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	return visible
}

// a2DebtExists is the pending-inclusive predicate (the row is debt before
// anything has pulled it).
func (f *a2WakeDebtFixture) a2DebtExists(t *testing.T, ctx context.Context) bool {
	t.Helper()
	if batchFlagEnabled() {
		exists, err := coordinatorBatchCutoverFacts(f.coord).SelectedDebtExists(ctx, f.sessID)
		require.NoError(t, err)
		return exists
	}
	exists, err := f.store.ReactionDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	return exists
}

// a2PullJobNoticesOnce is one raw pull by "another lock holder": any
// session-lock holder can call the pull primitive. Flag ON the batch
// completion notice is what is pending; OFF the async_jobs job notice.
func (f *a2WakeDebtFixture) a2PullJobNoticesOnce(t *testing.T, ctx context.Context) {
	t.Helper()
	if batchFlagEnabled() {
		pulled, err := coordinatorBatchCutoverFacts(f.coord).PullBatchNotices(ctx, f.messages, f.sessID)
		require.NoError(t, err)
		require.Len(t, pulled, 1, "the seeded completion notice is pending and must be pulled")
		return
	}
	pulled, err := f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "the seeded completion notice is pending and must be pulled")
}
