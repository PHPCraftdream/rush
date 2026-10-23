package agent

// R12-EF fixes F1 and F4, driven through the REAL paths: a flag-ON Drain
// turn whose scripted model reaction must mark the batch debt reacted in
// batch_nodes (persistStepFinish's flag-ON write twin), and Stop's wake-zero
// pass followed by a restarted coordinator whose turn reaches no provider.
// The fixture is drain_attempt_fixture_test.go's attemptFixture; the batch
// completion is seeded by the REAL engine (InstallBatchRuntime) over the
// fixture's own DB, so the message service, the batch store and the bound
// fact source all share one writer -- the production shape.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedBatchCompletionDebt runs the REAL batch engine over the fixture's own
// DB, submits one root owned by the fixture session, settles it completed,
// announces it and delivers its completion notice (delivery='done',
// wake=1, reacted=0 -- exactly the debt a Drain turn reacts to). Returns the
// store and the root id.
func seedBatchCompletionDebt(t *testing.T, f *attemptFixture, callID string) (*session.BatchStore, jobbatch.ID) {
	t.Helper()
	ctx := context.Background()
	store := session.NewBatchStore(f.env.conn)
	deps := installTestDeps(t, store, f.env.conn, "ok")
	deps.Owner = f.sessID
	install, err := InstallBatchRuntime(ctx, deps)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = install.Runner.Close(closeCtx)
	})
	root := batchRootID()
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "view", ToolCallID: callID, SessionID: f.sessID,
		Input: json.RawMessage(`{"path":"x"}`), RegistryID: "reg-1",
	})
	require.NoError(t, err)
	_, err = install.RunnerHandle.Submit(ctx, jobbatch.SubmitRequest{
		Root: root,
		Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}},
		Ref:  callID,
	})
	require.NoError(t, err)
	won, err := install.Elector.ElectStarted(ctx, f.sessID, root)
	require.NoError(t, err)
	require.True(t, won, "the started ack election wins once")
	_, ok, err := install.Done.AwaitDone(ctx, f.sessID, root)
	require.NoError(t, err)
	require.True(t, ok, "the root's completion envelope is available")
	snap, err := store.Read(ctx, f.sessID, string(root))
	require.NoError(t, err)
	require.Equal(t, "completed", snap.Root.State, "the root has settled before its completion is pulled")
	pulled, err := store.BatchPullNotices(ctx, f.env.messages, f.sessID, buildBatchNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "the settled root's completion notice is delivered")
	return store, root
}

func TestBatchDrainTurnReactionMarksBatchNodesON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	ctx := context.Background()
	f := newAttemptFixture(t, "batch-reaction-persist", attemptFixtureOpts{})
	store, root := seedBatchCompletionDebt(t, f, "call-bf1")
	BindCoordinatorBatchCutoverFacts(f.coord, NewBatchCutoverNodeFacts(store, nil, f.env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(f.coord) })

	_, runErr := f.drainRun(ctx)
	require.NoError(t, runErr, "the reaction turn completes over the batch debt")
	f.waitDrainIdle()
	require.EqualValues(t, 1, f.requests.Load(), "exactly one provider call: the reaction itself -- no extra Drain turns")

	var reacted, reactedFailed, wakeAttempts int64
	require.NoError(t, f.env.conn.QueryRowContext(ctx,
		`SELECT reacted, reacted_failed, wake_attempts FROM batch_nodes WHERE root_id = ? AND node_id = root_id`,
		string(root)).Scan(&reacted, &reactedFailed, &wakeAttempts))
	require.EqualValues(t, 1, reacted, "the model's reaction marks the batch debt reacted (F1)")
	require.EqualValues(t, 0, reactedFailed, "a real reaction is not a settle-by-failure")
	require.Zero(t, wakeAttempts, "the reacted row is not counted as an unreacted attempt")
	require.Equal(t, 0, f.markers(ctx), "no wake_failed marker for an already-answered completion")

	debt, err := store.BatchReactionVisibleDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	require.False(t, debt, "the reacted completion is no longer debt: no relaunch fuel")
}

func TestBatchAskQuestionDrainReactionON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	ctx := context.Background()
	f := newAttemptFixture(t, "batch-ask-question-reaction", attemptFixtureOpts{
		noIdle:  true,
		tools:   []fantasy.AgentTool{tools.NewAskQuestionTool()},
		handler: askQuestionResponse,
	})
	store, root := seedBatchCompletionDebt(t, f, "call-baq")
	BindCoordinatorBatchCutoverFacts(f.coord, NewBatchCutoverNodeFacts(store, nil, f.env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(f.coord) })

	_, err := f.drainRun(ctx)
	var awaiting *AwaitingAnswerError
	require.ErrorAs(t, err, &awaiting)

	var reacted, reactedFailed, wakeAttempts int64
	require.NoError(t, f.env.conn.QueryRowContext(ctx,
		`SELECT reacted, reacted_failed, wake_attempts FROM batch_nodes WHERE root_id = ? AND node_id = root_id`,
		string(root)).Scan(&reacted, &reactedFailed, &wakeAttempts))
	require.EqualValues(t, 1, reacted)
	require.EqualValues(t, 0, reactedFailed)
	require.EqualValues(t, 0, wakeAttempts)

	debt, err := store.BatchReactionVisibleDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	require.False(t, debt)
	require.EqualValues(t, 1, f.requests.Load())
}

func TestBatchGenericProviderErrorSettlesDebtByFailure(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	ctx := context.Background()
	f := newAttemptFixture(t, "batch-generic-provider-error", attemptFixtureOpts{
		noIdle: true,
		handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"batch-error-probe","type":"invalid_request_error"}}`))
		},
	})
	store, root := seedBatchCompletionDebt(t, f, "call-bge")
	BindCoordinatorBatchCutoverFacts(f.coord, NewBatchCutoverNodeFacts(store, nil, f.env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(f.coord) })

	_, err := f.drainRun(ctx)
	require.Error(t, err)

	var reacted, reactedFailed, wakeAttempts int64
	require.NoError(t, f.env.conn.QueryRowContext(ctx,
		`SELECT reacted, reacted_failed, wake_attempts FROM batch_nodes WHERE root_id = ? AND node_id = root_id`,
		string(root)).Scan(&reacted, &reactedFailed, &wakeAttempts))
	require.EqualValues(t, 1, reacted, "a terminal provider failure settles the batch debt reacted")
	require.EqualValues(t, 1, reactedFailed, "the settle is the settle-by-failure form")
	require.EqualValues(t, 1, wakeAttempts, "the failed turn was one counted attempt")
	debt, err := store.BatchReactionVisibleDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	require.False(t, debt, "the settle-by-failure consumed the batch completion debt")

	messages, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	var lastAssistant message.Message
	for _, msg := range messages {
		if msg.Role == message.Assistant {
			lastAssistant = msg
		}
	}
	require.NotEmpty(t, lastAssistant.ID, "the error finish is persisted")
	finish := lastAssistant.FinishPart()
	require.NotNil(t, finish, "the last assistant message has a finish")
	require.Equal(t, message.FinishReasonError, finish.Reason)
}

// TestBatchStopWakeZeroAndRestartNoReactionTurn: Stop's wake-zero pass must
// zero the batch debt's wake bit under the flag (F4), and a RESTARTED
// coordinator + agent over the same DB (a fresh arbiter: the stopped session
// is not suspended there) must reach no provider for it -- the doc sec.3.8
// invariant this site exists for.
//
// REVERT CHECK (F4): remove the batchCutoverReactionStore call from stopTree
// (coordinator_interrupt.go). The first failing assertion is the wake==0
// check: the batch node keeps wake=1 (only the legacy async_jobs rows were
// zeroed). The restarted drain then finds visible batch debt, commits a real
// turn and hits the probe provider -- the requests==0 check fails with at
// least one provider call.
func TestBatchStopWakeZeroAndRestartNoReactionTurn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	ctx := context.Background()
	f := newAttemptFixture(t, "batch-stop-wake-zero", attemptFixtureOpts{noIdle: true})
	store, root := seedBatchCompletionDebt(t, f, "call-bsz")
	BindCoordinatorBatchCutoverFacts(f.coord, NewBatchCutoverNodeFacts(store, nil, f.env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(f.coord) })

	f.coord.stopTree(f.sessID)

	var wake int64
	require.NoError(t, f.env.conn.QueryRowContext(ctx,
		`SELECT wake FROM batch_nodes WHERE root_id = ? AND node_id = root_id`,
		string(root)).Scan(&wake))
	require.Zero(t, wake, "Stop's wake-zero pass zeroes the batch debt's wake bit (F4)")

	// The restart: a fresh coordinator (fresh arbiter state) and a fresh
	// agent over the same DB -- exactly what a restarted process re-derives.
	coord2 := newBatchRuntimeCoordinator()
	BindCoordinatorBatchCutoverFacts(coord2, NewBatchCutoverNodeFacts(store, nil, f.env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(coord2) })
	sa2 := NewSessionAgent(SessionAgentOptions{
		SmartModel: f.model, FastModel: f.model, SystemPrompt: "you are a probe",
		DataDirectory: f.env.workingDir, Sessions: f.env.sessions, Messages: f.env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, Coord: coord2,
		TitleJoinGrace: 100 * time.Millisecond,
	}).(*sessionAgent)

	_, runErr := sa2.Run(ctx, newDrainCall(SessionAgentCall{SessionID: f.sessID}))
	require.NoError(t, runErr)
	require.Zero(t, f.requests.Load(), "the restarted drain reaches no provider: the stopped debt has no wake left")
}
