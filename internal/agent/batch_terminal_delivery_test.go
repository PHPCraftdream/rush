package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchTerminalTestDoneSource builds a done source over the raw store with a
// NIL batch lookup (no live engine batch): every read is the durable path.
func batchTerminalTestDoneSource(store *session.BatchStore) *batchStoreDoneSource {
	return &batchStoreDoneSource{store: store, batches: func(jobbatch.ID) *jobbatch.Batch { return nil }, poll: time.Millisecond}
}

// batchTerminalTestReserve reserves a one-item root directly against the
// store (the test DB has foreign keys off, matching the isolated
// convention) and returns the root id.
func batchTerminalTestReserve(t *testing.T, store *session.BatchStore, ctx context.Context, owner, call, hash string) string {
	t.Helper()
	r, err := store.ReserveIdentity(ctx, session.BatchReservation{
		Owner: owner, Host: "host", RunnerID: "runner-1", ToolCallID: call, InputHash: hash, Sequence: 1,
		Root: session.BatchReservationNode{
			Values:   db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "pending", ControlState: "none"},
			Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("view"), State: "pending", ControlState: "none"}}},
		},
	})
	require.NoError(t, err)
	return r.Snapshot.Root.RootID
}

// batchTerminalTestProject projects terminal start-rejection facts onto the
// root and its single child. The fences mirror the reservation: host,
// runner incarnation and owner. The projection runner must equal the
// reservation's runner (batch_store.go:211,230).
func batchTerminalTestProject(t *testing.T, store *session.BatchStore, ctx context.Context, owner, root string, sequence int64, updates ...db.ProjectBatchNodeParams) {
	t.Helper()
	for i := range updates {
		updates[i].ControlState = "none"
		updates[i].UpdatedAt = 1
		updates[i].RootID = root
		updates[i].HostID = "host"
		updates[i].OwnerSessionID = owner
		updates[i].RunnerID = "runner-1"
	}
	require.NoError(t, store.Project(ctx, session.BatchProjection{Host: "host", RunnerID: "runner-1", Sequence: sequence, Roots: []session.BatchRootProjection{{Owner: owner, Root: root, Updates: updates}}}))
}

func batchTerminalTestStartRejectedTask(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "failed", OutcomeState: "failed", OutcomeCode: "tool_unavailable"}
}

func batchTerminalTestSettledRoot(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "failed"}
}

func batchTerminalTestRecovered(node string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: node, State: "interrupted", OutcomeState: "interrupted", OutcomeCode: "host_lost", OutcomeSummary: "Host lost; execution was not resumed"}
}

// A terminal start-rejected root without any response envelope is delivered
// as an honest error response from the persisted terminal facts: the
// start-rejected CODE rides in the TASK's outcome_code (the only legal
// cause is ”) and the derived summary falls back to that code, so the REAL
// code reaches the model — this is the P1-3 oracle.
func TestBatchDoneSourceTerminalWithoutEnvelopeIsHonestError(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestBatchStore(t)
	root := batchTerminalTestReserve(t, store, ctx, "owner", "call-t2a", "hash-t2a")
	snap, err := store.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Len(t, snap.Nodes, 2, "one root plus one task child")
	batchTerminalTestProject(t, store, ctx, "owner", root, 2, batchTerminalTestSettledRoot(root), batchTerminalTestStartRejectedTask(snap.Nodes[1].NodeID))
	d := batchTerminalTestDoneSource(store)
	resp, ok, err := d.LoadDone(ctx, "owner", jobbatch.ID(root))
	require.NoError(t, err)
	require.True(t, ok, "terminal without envelope delivers persisted facts")
	require.True(t, resp.IsError, "a failed root delivers an error-shaped response")
	require.Equal(t, "tool_unavailable", resp.Content, "the REAL start-rejected code reaches the model, not invented prose")
	awaited, ok, err := d.AwaitDone(ctx, "owner", jobbatch.ID(root))
	require.NoError(t, err)
	require.True(t, ok, "AwaitDone returns the first LoadDone hit without waiting")
	require.True(t, awaited.IsError)
	require.Equal(t, resp, awaited)
}

// Production stores the wrapper {state, code, summary, details}
// (batch_commit.go:304-317 shape) and the store decodes
// session.BatchDeliveryEnvelope first, then Details as the executor
// ToolResponse — the old fixture stored a bare ToolResponse and proved
// nothing. An accepted-execution envelope is returned unchanged: content,
// metadata, StopTurn and IsError ride through untouched.
func TestBatchDoneSourceAcceptedEnvelopeUnchanged(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestBatchStore(t)
	root := batchTerminalTestReserve(t, store, ctx, "owner", "call-t2b", "hash-t2b")
	snap, err := store.Read(ctx, "owner", root)
	require.NoError(t, err)
	child := snap.Nodes[1].NodeID
	want := fantasy.NewTextResponse("executed ok")
	want.Metadata = `{"job_id":"j1"}`
	want.StopTurn = true
	raw0, err := json.Marshal(want)
	require.NoError(t, err)
	raw, err := json.Marshal(session.BatchDeliveryEnvelope{
		State: "completed", Code: "ok", Summary: "done", Details: raw0,
	})
	require.NoError(t, err)
	batchTerminalTestProject(t, store, ctx, "owner", root, 2,
		db.ProjectBatchNodeParams{NodeID: root, State: "completed", OutcomeState: "completed"},
		db.ProjectBatchNodeParams{NodeID: child, State: "completed", OutcomeState: "completed", ResponseEnvelope: nullString(string(raw))})
	resp, ok, err := batchTerminalTestDoneSource(store).LoadDone(ctx, "owner", jobbatch.ID(root))
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, resp.IsError)
	require.Equal(t, "executed ok", resp.Content, "the executor response rides through Details untouched")
	require.Equal(t, `{"job_id":"j1"}`, resp.Metadata, "the executor metadata rides through untouched")
	require.True(t, resp.StopTurn, "the executor StopTurn rides through untouched")
}

// TestBatchDoneSourceRecoveryInterruptedIsError pins the session contract
// for recovery-interrupted roots (state 'interrupted', code 'host_lost' —
// batch_recovery.sql:20-26). DEPENDENCY, one line in a file owned by the
// SIBLING jb-pa (package C must NOT edit it): LoadDone marks only
// failed/timed_out/cancelled as errors
// (internal/agent/batch_install_adapters.go:321); the orchestrator must add
// "interrupted" to that condition. Until that line lands this test fails
// red BY DESIGN at its final assertion; with it, it goes green.
func TestBatchDoneSourceRecoveryInterruptedIsError(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestBatchStore(t)
	root := batchTerminalTestReserve(t, store, ctx, "owner", "call-t2r", "hash-t2r")
	snap, err := store.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Len(t, snap.Nodes, 2, "one root plus one task child")
	batchTerminalTestProject(t, store, ctx, "owner", root, 2, batchTerminalTestRecovered(root), batchTerminalTestRecovered(snap.Nodes[1].NodeID))
	resp, ok, err := batchTerminalTestDoneSource(store).LoadDone(ctx, "owner", jobbatch.ID(root))
	require.NoError(t, err)
	require.True(t, ok, "recovery-interrupted terminal facts deliver")
	require.True(t, resp.IsError, "recovery-interrupted work must reach the model error-shaped")
	require.Equal(t, "Host lost; execution was not resumed", resp.Content)
}

// batchTerminalTestOneItemSpec builds the generic one-item spec with the
// given pinned registry identity: the registry id rides INSIDE the payload
// (batchLeafPayload.RegistryID), so two specs differing only in it carry
// different canonical hashes — a changed real policy identity, not an
// arbitrary string.
func batchTerminalTestOneItemSpec(t *testing.T, registryID string) jobbatch.Spec {
	t.Helper()
	payload, err := json.Marshal(batchLeafPayload{Tool: "view", ToolCallID: "call-1", SessionID: "owner", Input: json.RawMessage(`{"path":"p"}`), RegistryID: registryID})
	require.NoError(t, err)
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}}
}

// The attach adapter accepts an identical retry exactly once (one launch
// reservation per live call): a real identical retry of a LIVE call
// attaches at the probe and never re-submits (batch_ingress.go:116-135) —
// this pins the probe-attach plus exactly one launch reservation for the
// (owner, call) identity. A changed identity is refused as an error: the
// refusal changes the REAL registry identity the canonical hash covers,
// not an arbitrary string.
func TestBatchAttachAdapterIdenticalRetryAttachesOnce(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	deps := installTestDeps(t, store, conn, "ok")
	// A GATED executor keeps the root genuinely live while the probe runs:
	// the engine settles the root asynchronously with the instant fake, so
	// an ungated submit would race the settlement and the probe would read
	// no live root at all (see the fence-probe test above).
	gate := newGatingInstallExecutor("ok")
	deps.Executor = gate
	install := installRunnerForTest(t, ctx, deps)
	// Registered AFTER the Close cleanup, so LIFO runs letGo first.
	t.Cleanup(gate.letGo)
	spec := batchTerminalTestOneItemSpec(t, "reg-1")
	hash := batchInvocationHash(spec)
	root := installSubmit(t, install, spec)
	adapter := batchAttachAdapter{store: store}
	got, attached, err := adapter.ProbeLive(ctx, batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: hash})
	require.NoError(t, err)
	require.True(t, attached, "an identical retry attaches to the live root")
	require.Equal(t, root, got)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes WHERE owner_session_id='owner' AND tool_call_id='call-1' AND node_id=root_id").Scan(&count))
	require.Equal(t, 1, count, "exactly ONE launch reservation exists for the call")
	// Changed identity (a different registry id => different canonical hash)
	// is refused.
	_, _, err = adapter.ProbeLive(ctx, batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: batchInvocationHash(batchTerminalTestOneItemSpec(t, "reg-2"))})
	require.ErrorIs(t, err, session.ErrBatchInputMismatch)
	// Release the gate so the root settles and the Runner closes cleanly.
	gate.letGo()
}

// The attach adapter's reconstructed fallback hash must carry the child's
// stored group and timeout: the bare-task hash attaches, and any group or
// timeout divergence is refused.
func TestBatchAttachAdapterFallbackCarriesGroupAndTimeout(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestBatchStore(t)
	adapter := batchAttachAdapter{store: store}
	task := jobbatch.TaskSpec{Kind: "shell", Group: "audit", Payload: json.RawMessage(`{"k":"v"}`), Timeout: 5 * time.Second}
	r, err := store.ReserveIdentity(ctx, session.BatchReservation{
		Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-fb", InputHash: batchInvocationHash(jobbatch.OneItem(task)), Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "pending", ControlState: "none"},
			Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: nullString("shell"), TaskGroup: "audit", TimeoutNs: int64(5 * time.Second),
				TaskPayload: nullString(`{"k":"v"}`), State: "pending", ControlState: "none",
			}}},
		},
	})
	require.NoError(t, err)
	root := r.Snapshot.Root.RootID
	got, attached, err := adapter.ProbeLive(ctx, batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-fb", InputHash: batchInvocationHash(jobbatch.Spec{Task: &task})})
	require.NoError(t, err)
	require.True(t, attached, "the bare-task hash attaches through the RECONSTRUCTED fallback (group and timeout preserved)")
	require.Equal(t, jobbatch.ID(root), got)
	diverged := task
	diverged.Timeout = 6 * time.Second
	_, _, err = adapter.ProbeLive(ctx, batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-fb", InputHash: batchInvocationHash(jobbatch.Spec{Task: &diverged})})
	require.ErrorIs(t, err, session.ErrBatchInputMismatch)
	diverged = task
	diverged.Group = "other"
	_, _, err = adapter.ProbeLive(ctx, batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-fb", InputHash: batchInvocationHash(jobbatch.Spec{Task: &diverged})})
	require.ErrorIs(t, err, session.ErrBatchInputMismatch)
}
