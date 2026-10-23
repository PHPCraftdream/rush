// Package A2's batch-fixture helpers (pull/compaction/splice lifecycle).
// Everything here seeds and reads the batch node store (reached through the
// coordinator's bound BatchCutoverFacts); names carry the a2 prefix so the
// sibling packages' helpers cannot collide. No helper here mutates
// RUSH_BATCH_RUNTIME.
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// a2AttachBatchFacts gives a fixture's coordinator the real node-facts
// binding, so the drain turn's reads (visible debt, pending-inclusive debt,
// notice pull) hit the batch store over env's database instead of the
// unknown fail-safe.
func a2AttachBatchFacts(t *testing.T, env fakeEnv) *coordinator {
	t.Helper()
	coord := newBatchRuntimeCoordinator()
	coord.sessions, coord.messages, coord.permissions = env.sessions, env.messages, env.permissions
	bindBatchNodeFacts(t, coord, env.conn, env.workingDir)
	return coord
}

// a2ClaimJob seeds the RUNNING half of a job fixture: a live announced batch
// root, so live-scope reads (the supervision check-in's openScope) see open
// work.
func a2ClaimJob(t *testing.T, conn *sql.DB, owner, callID string, announce bool) {
	t.Helper()
	_ = announce
	seedBatchDelegation(t, conn, owner, callID, "", "command", "running", false)
}

// a2CompleteJobErr completes the seeded job with wake=1: the completion is
// projected into the batch store (root and child to "completed", the child's
// completion notice selected, delivery left 'pending' until a pull) with
// summary as the outcome text. Projection shape mirrors the foundation's
// seedBatchJobDebt (batch_fixture_port_test.go).
func a2CompleteJobErr(ctx context.Context, conn *sql.DB, owner, callID, summary string) error {
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
func a2CompleteJob(t *testing.T, conn *sql.DB, owner, callID, summary string) {
	t.Helper()
	require.NoError(t, a2CompleteJobErr(context.Background(), conn, owner, callID, summary))
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
	f.env = env
	f.coord = newBatchRuntimeCoordinator()
	f.coord.sessions, f.coord.messages, f.coord.permissions = env.sessions, env.messages, env.permissions
	f.coord.noticeStore = session.NewSessionNoticeStore(env.conn)
	bindBatchNodeFacts(t, f.coord, env.conn, env.workingDir)

	f.messages = &failingCreateTxMessages{Service: env.messages}
	opts := SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: f.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, Coord: f.coord,
	}
	if wireOnSessionIdle {
		opts.OnSessionIdle = f.coord.onSessionIdleHook
	}
	sa := NewSessionAgent(opts)
	f.sa = sa.(*sessionAgent)
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: f.sessID}})
	return f
}

// a2ClaimAndFinish mirrors wakeDebtFixture.claimAndFinish: the terminal
// wake-armed debt lands in batch_nodes.
func (f *a2WakeDebtFixture) a2ClaimAndFinish(t *testing.T, ctx context.Context, toolCallID string) {
	t.Helper()
	_ = ctx
	seedBatchJobDebt(t, f.conn, f.sessID, toolCallID, false)
}

// a2DebtVisible is wakeDebtFixture.debtVisible through the flag-selected
// authority (the same CaptureVisibleDebt read the drain turn itself makes).
func (f *a2WakeDebtFixture) a2DebtVisible(t *testing.T, ctx context.Context) bool {
	t.Helper()
	snap, err := coordinatorBatchCutoverFacts(f.coord).CaptureVisibleDebt(ctx, f.sessID)
	require.NoError(t, err)
	return !snap.Empty()
}

// a2DebtExists is the pending-inclusive predicate (the row is debt before
// anything has pulled it).
func (f *a2WakeDebtFixture) a2DebtExists(t *testing.T, ctx context.Context) bool {
	t.Helper()
	exists, err := coordinatorBatchCutoverFacts(f.coord).SelectedDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	return exists
}

// a2PullJobNoticesOnce is one raw pull by "another lock holder": any
// session-lock holder can call the pull primitive. Flag ON the batch
// completion notice is what is pending; OFF the async_jobs job notice.
func (f *a2WakeDebtFixture) a2PullJobNoticesOnce(t *testing.T, ctx context.Context) {
	t.Helper()
	pulled, err := coordinatorBatchCutoverFacts(f.coord).PullBatchNotices(ctx, f.messages, f.sessID)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "the seeded completion notice is pending and must be pulled")
}
