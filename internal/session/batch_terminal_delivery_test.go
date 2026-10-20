package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// batchTerminalTestReserve reserves a ONE-item live root whose task child
// carries the given group/timeout and returns the root id.
func batchTerminalTestReserve(t *testing.T, s *BatchStore, ctx context.Context, group string, timeout time.Duration) string {
	t.Helper()
	p := batchStoreTestReservation()
	p.RunnerID = "runner"
	p.Root.Children = []BatchReservationNode{{Values: db.ReserveBatchNodeParams{
		Kind: "task", TaskKind: batchStoreTestString("shell"),
		TaskPayload: batchStoreTestString(`{"k":"v"}`),
		TaskGroup:   group, TimeoutNs: int64(timeout),
		State: "pending", ControlState: "none",
	}}}
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot.Root.RootID
}

// batchTerminalTestProject projects terminal facts (and an optional
// response envelope) onto the given rows. The runner MUST equal the
// reservation's runner: batchProjectRoot fences every update and the root
// sequence advance with the projection-level runner (batch_store.go:207-233).
func batchTerminalTestProject(t *testing.T, s *BatchStore, ctx context.Context, root, runner string, sequence int64, updates ...db.ProjectBatchNodeParams) {
	t.Helper()
	for i := range updates {
		updates[i].ControlState = "none"
		updates[i].UpdatedAt = 1
		updates[i].RootID = root
		updates[i].HostID = "host"
		updates[i].OwnerSessionID = "owner"
		updates[i].RunnerID = runner
	}
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", RunnerID: runner, Sequence: sequence, Roots: []BatchRootProjection{{Owner: "owner", Root: root, Updates: updates}}}))
}

// batchTerminalTestStartRejectedTask leaves the TASK row as EventStartRejected
// does (jobbatch engine.go): state failed, outcome state failed, the real
// start-rejected code, NO summary, cause ” (the schema CHECK allows only
// ”, stop, timeout, host_closing, stop_on_fail — batch_nodes migration :30).
func batchTerminalTestStartRejectedTask(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "failed", OutcomeState: "failed", OutcomeCode: "tool_unavailable"}
}

// batchTerminalTestSettledRoot leaves the structurally settled ROOT row as
// the engine does: state failed with NO outcome of its own (settle computes
// the root state from counts/causes and never copies the task's outcome).
func batchTerminalTestSettledRoot(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "failed"}
}

// batchTerminalTestStopped leaves a pending-stop cancelled row: cause 'stop'
// (a legal CHECK value) with the root's own outcome.
func batchTerminalTestStopped(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "cancelled", Cause: "stop", OutcomeState: "cancelled", OutcomeCode: "stop_accepted", OutcomeSummary: "stopped before start"}
}

// batchTerminalTestRecovered leaves a recovery-interrupted row exactly as
// BatchRecoveryInterrupt persists it (batch_recovery.sql:20-26).
func batchTerminalTestRecovered(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "interrupted", OutcomeState: "interrupted", OutcomeCode: "host_lost", OutcomeSummary: "Host lost; execution was not resumed"}
}

// A terminal start-rejected root WITHOUT any response envelope is delivered
// honestly from the persisted terminal facts. The child outcome is the
// engine-faithful shape: EventStartRejected puts the rejected outcome and
// code on the TASK row and structural settlement leaves the root's own
// outcome empty, so the old code fabricated the same outcome on both rows
// and masked the child-facts-wins rule. Read-only: nothing is written back.
func TestBatchDeliveryTerminalStartRejectedWithoutEnvelope(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	root := batchTerminalTestReserve(t, s, ctx, "", 0)
	snap, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Len(t, snap.Nodes, 2, "one root plus one task child")
	child := snap.Nodes[1].NodeID
	require.Equal(t, root+".1", child)
	batchTerminalTestProject(t, s, ctx, root, "runner", 2, batchTerminalTestSettledRoot(root), batchTerminalTestStartRejectedTask(child))
	e, ok, err := s.BatchDeliveryOneItemCompletion(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok, "terminal without envelope must deliver persisted facts, not stay unavailable")
	require.Equal(t, "failed", e.State)
	require.Equal(t, "tool_unavailable", e.Code, "the REAL task outcome code reaches the delivery")
	require.Equal(t, "tool_unavailable", e.Summary, "the fallback chain ends at the code — no invented prose")
	require.Empty(t, e.Details)
	require.Empty(t, e.Ref)
	got, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.False(t, got.Root.ResponseEnvelope.Valid, "the derived delivery never writes an envelope")
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes WHERE response_envelope IS NOT NULL AND response_envelope != ''").Scan(&count))
	require.Zero(t, count)
}

// Terminality is judged from the root AND every child in the same snapshot:
// a terminal root with a live child, and a live root with a terminal child,
// both stay live (ok=false).
func TestBatchDeliveryLiveTaskWithoutEnvelopeStaysLive(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	root := batchTerminalTestReserve(t, s, ctx, "", 0)
	snap, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	child := snap.Nodes[1].NodeID
	batchTerminalTestProject(t, s, ctx, root, "runner", 2, batchTerminalTestSettledRoot(root), db.ProjectBatchNodeParams{NodeID: child, State: "running"})
	e, ok, err := s.BatchDeliveryOneItemCompletion(ctx, "owner", root)
	require.NoError(t, err)
	require.False(t, ok, "a live child keeps the batch live even when the root is terminal")
	require.Empty(t, e)
	// Twin: terminal child under a still-running root.
	root2 := batchTerminalTestReserve(t, s, ctx, "", 0)
	snap2, err := s.Read(ctx, "owner", root2)
	require.NoError(t, err)
	child2 := snap2.Nodes[1].NodeID
	batchTerminalTestProject(t, s, ctx, root2, "runner", 2, db.ProjectBatchNodeParams{NodeID: root2, State: "running"}, batchTerminalTestStartRejectedTask(child2))
	e, ok, err = s.BatchDeliveryOneItemCompletion(ctx, "owner", root2)
	require.NoError(t, err)
	require.False(t, ok, "a live root keeps the batch live even when the child is terminal")
	require.Empty(t, e)
}

// Accepted-execution envelopes are returned unchanged by the new terminal
// path: a stored envelope (root or single child) always wins over the
// derived root-level facts.
func TestBatchDeliveryAcceptedEnvelopeUnchanged(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	root := batchTerminalTestReserve(t, s, ctx, "", 0)
	snap, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	child := snap.Nodes[1].NodeID
	envelope := `{"state":"failed","code":"exec_failed","summary":"boom","details":{"x":1}}`
	batchTerminalTestProject(t, s, ctx, root, "runner", 2, batchTerminalTestSettledRoot(root), db.ProjectBatchNodeParams{
		NodeID: child, State: "failed", OutcomeState: "failed",
		ResponseEnvelope: batchStoreTestString(envelope), ResponseEnvelopeRef: batchStoreTestString(child + "#2"),
	})
	e, ok, err := s.BatchDeliveryOneItemCompletion(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok, "the stored child envelope is delivered, not the derived facts")
	require.Equal(t, "failed", e.State)
	require.Equal(t, "exec_failed", e.Code)
	require.Equal(t, "boom", e.Summary)
	require.JSONEq(t, `{"x":1}`, string(e.Details))
	require.Equal(t, child+"#2", e.Ref)
	// Root-envelope priority: the root envelope wins over the child's.
	root2 := batchTerminalTestReserve(t, s, ctx, "", 0)
	snap2, err := s.Read(ctx, "owner", root2)
	require.NoError(t, err)
	child2 := snap2.Nodes[1].NodeID
	rootEnvelope := `{"state":"cancelled","code":"stop_accepted","summary":"stopped"}`
	batchTerminalTestProject(t, s, ctx, root2, "runner", 2, db.ProjectBatchNodeParams{
		NodeID: root2, State: "cancelled", Cause: "stop", OutcomeState: "cancelled",
		ResponseEnvelope: batchStoreTestString(rootEnvelope), ResponseEnvelopeRef: batchStoreTestString(root2 + "#3"),
	}, batchTerminalTestSettledRoot(child2))
	e, ok, err = s.BatchDeliveryOneItemCompletion(ctx, "owner", root2)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "cancelled", e.State, "the ROOT envelope is returned")
	require.Equal(t, root2+"#3", e.Ref, "the root envelope's ref identifies it")
}

// A multi-item terminal root WITHOUT envelopes returns the ROOT-level
// outcome — never an arbitrary child's response (Details and Ref stay
// empty because no executor envelope exists). The shared facts no longer
// pretend to distinguish child from root: the pin is that the aggregate is
// the ROOT row's outcome with no executor envelope.
func TestBatchDeliveryMultiItemTerminalWithoutEnvelopeReturnsRootOutcome(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	root := snap.Root.RootID
	batchTerminalTestProject(t, s, ctx, root, "", 2,
		batchTerminalTestStopped(root),
		batchTerminalTestStopped(snap.Nodes[1].NodeID),
		batchTerminalTestStopped(snap.Nodes[2].NodeID))
	e, ok, err := s.BatchDeliveryOneItemCompletion(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok, "a fully terminal multi-item root delivers its root-level outcome")
	require.Equal(t, "cancelled", e.State)
	require.Equal(t, "stop_accepted", e.Code)
	require.Equal(t, "stopped before start", e.Summary)
	require.Empty(t, e.Details, "no child response is collapsed into the aggregate")
	require.Empty(t, e.Ref)
}

// Recovery-interrupted work (batch_recovery.sql:20-26: state and outcome
// state 'interrupted', code 'host_lost', summary 'Host lost; execution was
// not resumed') must reach the model error-shaped: the session side already
// carries the state (the agent-side LoadDone dependency is the
// orchestrator's one-line follow-up, see the agent test file).
func TestBatchDeliveryRecoveryInterruptedWithoutEnvelope(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	root := batchTerminalTestReserve(t, s, ctx, "", 0)
	snap, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	child := snap.Nodes[1].NodeID
	batchTerminalTestProject(t, s, ctx, root, "runner", 2, batchTerminalTestRecovered(root), batchTerminalTestRecovered(child))
	e, ok, err := s.BatchDeliveryOneItemCompletion(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok, "recovery-interrupted terminal facts deliver without an envelope")
	require.Equal(t, "interrupted", e.State)
	require.Equal(t, "host_lost", e.Code)
	require.Equal(t, "Host lost; execution was not resumed", e.Summary)
	require.Empty(t, e.Details)
	require.Empty(t, e.Ref)
}

// The probe's reconstructed fallback hash must carry the child's stored
// group and timeout: a caller hashing the FULL task spec attaches; dropping
// either one diverges and is refused.
func TestBatchProbeLiveFallbackCarriesGroupAndTimeout(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	root := batchTerminalTestReserve(t, s, ctx, "audit", 5*time.Second)
	snap, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, "audit", snap.Nodes[1].TaskGroup)
	require.Equal(t, int64(5*time.Second), snap.Nodes[1].TimeoutNs)
	task := jobbatch.TaskSpec{Kind: "shell", Group: "audit", Payload: json.RawMessage(`{"k":"v"}`), Timeout: 5 * time.Second}
	probe := BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner", ToolCallID: "call", InputHash: batchCommitSpecHash(jobbatch.Spec{Task: &task}), SubmittedMessageID: "msg-1", SubmittedRegistryID: "reg-1"}
	got, attached, err := s.BatchProbeLive(ctx, probe)
	require.NoError(t, err)
	require.True(t, attached, "the full-task-spec hash matches the reconstruction")
	require.Equal(t, root, got)
	// Divergence twins: any group/timeout difference is a mismatch error.
	noTimeout := task
	noTimeout.Timeout = 6 * time.Second
	_, _, err = s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner", ToolCallID: "call", InputHash: batchCommitSpecHash(jobbatch.Spec{Task: &noTimeout})})
	require.ErrorIs(t, err, ErrBatchInputMismatch)
	otherGroup := task
	otherGroup.Group = "other"
	_, _, err = s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner", ToolCallID: "call", InputHash: batchCommitSpecHash(jobbatch.Spec{Task: &otherGroup})})
	require.ErrorIs(t, err, ErrBatchInputMismatch)
}

// An identical retry attaches to the SAME live root exactly once — via the
// read-only probe and via the commit hook's Reserve attach (Reserve with
// the same identity is the commit hook a second submission racing the probe
// hits: ingress.go attaches at the probe and never re-submits a live call)
// — and a changed identity never attaches: the refusal changes the REAL
// registry identity the canonical hash covers (registry_id rides inside the
// task payload), not an arbitrary string.
func TestBatchProbeLiveIdenticalRetryAttachesOnce(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	oneItem := jobbatch.OneItem(jobbatch.TaskSpec{Kind: "shell", Payload: json.RawMessage(`{"k":"v"}`)})
	hash := batchCommitSpecHash(oneItem)
	root := batchProbeTestReserve(t, s, ctx, "host", "runner", hash, batchProbeTestTask(`{"k":"v"}`))
	snap, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, "shell", snap.Nodes[1].TaskKind.String)
	for i := 0; i < 2; i++ {
		got, attached, err := s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner", ToolCallID: "call", InputHash: hash})
		require.NoError(t, err)
		require.True(t, attached, "identical retry probes attach")
		require.Equal(t, root, got)
	}
	p := batchStoreTestReservation()
	p.RunnerID = "runner"
	p.InputHash = hash
	p.Root.Children = []BatchReservationNode{{Values: batchProbeTestTask(`{"k":"v"}`)}}
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	require.True(t, r.Attached, "the commit-hook attach path accepts the identical retry")
	require.Equal(t, root, r.Snapshot.Root.RootID)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes WHERE owner_session_id='owner' AND tool_call_id='call' AND node_id=root_id").Scan(&count))
	require.Equal(t, 1, count, "exactly ONE launch reservation exists for the (owner, call) identity")
	// Changed identity is refused, never resubmitted: a different registry
	// id carried in the payload is a changed real policy identity.
	changedIdentity := jobbatch.OneItem(jobbatch.TaskSpec{Kind: "shell", Payload: json.RawMessage(`{"registry_id":"reg-2"}`)})
	changed := batchStoreTestReservation()
	changed.RunnerID = "runner"
	changed.InputHash = batchCommitSpecHash(changedIdentity)
	changed.Root.Children = []BatchReservationNode{{Values: batchProbeTestTask(`{"registry_id":"reg-2"}`)}}
	_, err = s.Reserve(ctx, changed)
	require.ErrorIs(t, err, ErrBatchInputMismatch)
	_, _, err = s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "elsewhere", RunnerID: "runner", ToolCallID: "call", InputHash: hash})
	require.ErrorIs(t, err, ErrBatchForeignLiveHost)
	_, _, err = s.BatchProbeLive(ctx, BatchLiveProbe{Owner: "owner", Host: "host", RunnerID: "runner-2", ToolCallID: "call", InputHash: hash})
	require.ErrorIs(t, err, ErrBatchRunnerMismatch)
}
