package agent

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// pdcRecordingReceipt records every ToolResultReceipt invocation.
type pdcRecordingReceipt struct {
	owners  []string
	roots   []jobbatch.ID
	callIDs []string
	sinks   []*toolResultReceiptSink
	err     error
}

func (r *pdcRecordingReceipt) register(_ context.Context, owner string, root jobbatch.ID, callID string, sink *toolResultReceiptSink) error {
	r.owners = append(r.owners, owner)
	r.roots = append(r.roots, root)
	r.callIDs = append(r.callIDs, callID)
	r.sinks = append(r.sinks, sink)
	return r.err
}

func (r *pdcRecordingReceipt) calls() int { return len(r.owners) }

// pdcDeliveryWithReceipt builds a delivery impl wired to the recording receipt.
func pdcDeliveryWithReceipt(t *testing.T, done *fakeDoneSource, elector *fakeElector, receipt *pdcRecordingReceipt, timer func(time.Duration) <-chan time.Time) *batchDeliveryImpl {
	t.Helper()
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: done, Elect: elector, Runner: noopDeliveryRunner(),
		ArmSupervision:    func(context.Context, string) {},
		Timer:             timer,
		ToolResultReceipt: receipt.register,
	})
	require.NoError(t, err)
	return impl
}

// pdcSinkContext carries a per-step receipt sink exactly like the production
// PrepareStep wrapper does.
func pdcSinkContext() (context.Context, *toolResultReceiptSink) {
	sink := &toolResultReceiptSink{}
	return context.WithValue(context.Background(), toolReceiptContextKey{}, sink), sink
}

// TestPdcDeliveryReceiptNoSinkNoOp pins the byte-identical fallback.
// Mutation caught: making receipt registration unconditional (or erroring
// without a sink in ctx) would break every non-stream delivery caller whose
// context carries no per-step sink.
func TestPdcDeliveryReceiptNoSinkNoOp(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	done.finish(envelope)
	receipt := &pdcRecordingReceipt{}
	impl := pdcDeliveryWithReceipt(t, done, &fakeElector{}, receipt, nil)
	got, err := impl.deliverSync(context.Background(), "owner-1", "root-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, envelope, got, "envelope byte-identical without a sink")
	require.Zero(t, receipt.calls(), "no receipt without a per-step sink in ctx")
}

// TestPdcDeliverySyncRegistersReceipt pins the sync success receipt.
// Mutation caught: registering under an unvalidated callID (e.g. a guessed id
// instead of inv.CallID), or registering before the envelope exists, would let
// a tool result be consumed under the wrong durable delivery identity.
func TestPdcDeliverySyncRegistersReceipt(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	done.finish(envelope)
	receipt := &pdcRecordingReceipt{}
	impl := pdcDeliveryWithReceipt(t, done, &fakeElector{}, receipt, nil)
	ctx, sink := pdcSinkContext()
	got, err := impl.deliverSync(ctx, "owner-1", "root-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, envelope, got, "envelope unchanged when the receipt registers")
	require.Equal(t, 1, receipt.calls())
	require.Equal(t, "owner-1", receipt.owners[0])
	require.Equal(t, jobbatch.ID("root-1"), receipt.roots[0])
	require.Equal(t, "call-1", receipt.callIDs[0])
	require.Same(t, sink, receipt.sinks[0], "the per-step sink is passed through")
}

// TestPdcDeliveryReceiptErrorPropagates pins fail-closed registration.
// Mutation caught: swallowing the adapter's identity-mismatch error (silent
// fallback to ordinary persistence) would preserve the exact bug this transport
// exists to fix.
func TestPdcDeliveryReceiptErrorPropagates(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	done.finish(envelope)
	receipt := &pdcRecordingReceipt{err: errors.New("batch tool result: root identity mismatch")}
	impl := pdcDeliveryWithReceipt(t, done, &fakeElector{}, receipt, nil)
	ctx, _ := pdcSinkContext()
	_, err := impl.deliverSync(ctx, "owner-1", "root-1", "call-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "root identity mismatch")
}

// TestPdcDeliveryInlineNeverRegisters pins the inline exclusion.
// Mutation caught: registering a receipt on the explicit-inline (or
// finish-before-started LoadDone) path would double-deliver: ElectInline's
// history message plus a fused tool-result consumption.
func TestPdcDeliveryInlineNeverRegisters(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	elector := &fakeElector{}
	receipt := &pdcRecordingReceipt{}
	impl := pdcDeliveryWithReceipt(t, done, elector, receipt, nil)
	done.finish(envelope)
	ctx, _ := pdcSinkContext()
	got, err := impl.DeliverBatch(ctx, "root-1", deliveryTestInvocation(batchDeliveryInline))
	require.NoError(t, err)
	require.Equal(t, envelope, got)
	require.True(t, elector.inlineWin.Load(), "the inline election still ran")
	require.Zero(t, receipt.calls(), "explicit inline never registers a receipt")
}

// TestPdcDeliveryTimerArmNeverRegisters pins the window-elapse exclusion.
//
// Asserted here: the timer arm returns the byte-identical started response,
// elects exactly one started history, registers ZERO receipts, and entered
// AwaitDone exactly once; closing done.release AFTER the assertions joins the
// AwaitDone waiter goroutine (no leak) — the completion arm of that same call
// can never win once the select already committed to the timer.
//
// Deferred to the session/adapter tests (batch_tool_result_test.go and the
// onToolResult test below): that a LATE registration after a started election
// loses the consume CAS — the delivery layer cannot know a root was
// started-elected, only the guarded CAS can refuse it.
//
// Mutation caught: registering a receipt on the timer (started) arm would let
// a later tool result consume a root whose delivery already went to the
// notices outbox.
func TestPdcDeliveryTimerArmNeverRegisters(t *testing.T) {
	t.Parallel()
	done := newFakeDone(fantasy.NewTextResponse("later")) // never finishes on its own
	elector := &fakeElector{}
	receipt := &pdcRecordingReceipt{}
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: done, Elect: elector, Runner: fakeStopOnlyRunner{newFakeCancelRunner()},
		Timer: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			go func() { <-done.entered; ch <- time.Now() }()
			return ch
		},
		ToolResultReceipt: receipt.register,
	})
	require.NoError(t, err)
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "500ms"
	ctx, _ := pdcSinkContext()
	got, err := impl.DeliverBatch(ctx, "root-esc", inv)
	require.NoError(t, err)
	require.Equal(t, batchStartedResponse("root-esc"), got, "the started response is byte-identical")
	require.Equal(t, int64(1), elector.startedCalls.Load())
	require.Zero(t, receipt.calls(), "the timer arm never registers a receipt")
	require.Equal(t, int64(1), done.awaitCalls.Load(), "escape waits for the real Done first")
	// Join the AwaitDone waiter: the release closes its blocking select and
	// the buffered result channel terminates the goroutine.
	done.finish(fantasy.NewTextResponse("late-completion"))
}

// TestPdcDeliveryWindowCompletionRegistersReceipt pins the in-window arm.
// The Timer seam is deliberately left nil (time.After with a 10s window):
// completion already finished before DeliverBatch, so the done arm provably
// wins the select without racing the timer — mirroring
// TestBatchDeliveryEscapeCompletesWithinWindow.
// Mutation caught: registering on the wrong select arm (e.g. before the done
// channel wins) would consume a root whose window may still elapse, splitting
// the delivery identity between the tool message and the notices outbox.
func TestPdcDeliveryWindowCompletionRegistersReceipt(t *testing.T) {
	t.Parallel()
	envelope := fantasy.ToolResponse{
		Type: "text", Content: "fast", Metadata: `{"cost":1}`,
	}
	done := newFakeDone(envelope)
	elector := &fakeElector{}
	receipt := &pdcRecordingReceipt{}
	impl := pdcDeliveryWithReceipt(t, done, elector, receipt, nil)
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "10s"
	inv.CallID = "call-1"
	done.finish(envelope)
	ctx, sink := pdcSinkContext()
	got, err := impl.DeliverBatch(ctx, "root-esc", inv)
	require.NoError(t, err)
	require.Equal(t, envelope, got, "the full executor envelope is unchanged")
	require.Zero(t, elector.startedCalls.Load(), "an in-window completion elects no started history")
	require.Equal(t, 1, receipt.calls())
	require.Equal(t, "call-1", receipt.callIDs[0])
	require.Same(t, sink, receipt.sinks[0])
}

// TestPdcTurnStreamOnToolResultConsumesRoot drives the REAL onToolResult
// consume branch over a REAL session store: the receipt is registered through
// the production adapter shape (store.Read identity validation + sink.add,
// mirroring batch_install_adapters.go's ToolResultReceipt closure), then
// onToolResult persists the tool message FUSED with the root consumption.
//
// First production-facing assertion: after onToolResult, store.Read shows
// announced=1, delivery='done', wake=0, reacted=1 and both message-id columns
// equal the persisted tool message's id — with exactly ONE message for the
// session (the Tool result; NO background User completion message).
//
// Mutation caught: dropping the receipt lookup from onToolResult (or letting
// the ordinary Create path run first) leaves the root unannounced/pending and
// the tool message unwired — the pre-fix duplicate/notice state.
//
// turnStream construction reuses the thinnest existing harness shape
// (newToolSuggestFixture, agent_turn_stream_tool_suggest_test.go): a
// sessionAgent with a real message service over one migrated SQLite conn, no
// coordinator (plain persistence), nil stall (stallClock methods are nil-safe).
// The batch store rides the SAME conn; foreign keys are disabled by the
// shared helper so no session row is required.
func TestPdcTurnStreamOnToolResultConsumesRoot(t *testing.T) {
	conn := batchFixtureConnNoFK(t)
	ctx := context.Background()
	svc := message.NewService(db.New(conn))
	store := session.NewBatchStore(conn)
	const owner = "owner"

	// Reserve + settle ONE terminal selected one-task root under the full
	// production incarnation (owner/host/runner/call), delivery 'pending',
	// announced=0 — the exact CAS preimage.
	reserved, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: "host", RunnerID: "runner",
		ToolCallID: "call", InputHash: "hash", Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "pending", ControlState: "none"},
			Children: []session.BatchReservationNode{{
				Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"},
			}},
		},
	})
	require.NoError(t, err)
	root := reserved.Snapshot.Root.RootID
	// Settle the root terminal with a selected notice, under the same
	// host/runner incarnation the reservation used (the sequence advance is
	// host+runner fenced).
	require.NoError(t, store.Project(ctx, session.BatchProjection{
		Host: "host", RunnerID: "runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: owner, Root: root,
			Updates: []db.ProjectBatchNodeParams{{NodeID: root, State: "completed", ControlState: "none", OutcomeSummary: root}},
			Notices: []db.SelectBatchNodeNoticeParams{{
				NodeID: root, NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: "completed", Valid: true},
				NoticeCause: sql.NullString{String: "", Valid: true}, NoticeOrdinal: sql.NullInt64{Int64: 0, Valid: true}, NoticeCompleted: 1, Wake: 1,
			}},
		}},
	}))

	// Production adapter shape: validate the persisted incarnation, then add
	// the receipt to THIS step's sink.
	ts := &turnStream{
		a:                &sessionAgent{messages: svc},
		call:             SessionAgentCall{SessionID: owner},
		ctx:              ctx,
		currentAssistant: &message.Message{SessionID: owner},
		bumpActivity:     func() {},
		toolFinished:     func() {},
	}
	sink := &toolResultReceiptSink{}
	ts.toolResultMu.Lock()
	ts.toolReceipts = sink
	ts.toolResultMu.Unlock()

	snapshot, err := store.Read(ctx, owner, root)
	require.NoError(t, err)
	n := snapshot.Root
	require.True(t, n.ToolCallID.Valid && n.ToolCallID.String == "call")
	require.Equal(t, "pending", n.Delivery)
	require.EqualValues(t, 0, n.Announced)
	identity := session.BatchToolResultIdentity{
		Owner: owner, RootID: n.RootID, NodeID: n.NodeID,
		HostID: n.HostID, RunnerID: n.RunnerID, ToolCallID: n.ToolCallID.String,
	}
	require.NoError(t, sink.add(toolResultReceipt{
		identity: identity,
		persist: func(ctx context.Context, params message.CreateMessageParams) (message.Message, bool, error) {
			return store.BatchConsumeToolResult(ctx, svc, owner, identity, params)
		},
	}))

	// The turn's real callback: onToolResult must take the receipt, persist
	// the tool message fused with the consume, and skip ordinary persistence.
	require.NoError(t, ts.onToolResult(fantasy.ToolResultContent{
		ToolCallID: "call", ToolName: "bash",
		Result: fantasy.ToolResultOutputContentText{Text: "done"},
	}))

	got, err := store.Read(ctx, owner, root)
	require.NoError(t, err)
	require.EqualValues(t, 1, got.Root.Announced)
	require.Equal(t, "done", got.Root.Delivery)
	require.EqualValues(t, 0, got.Root.Wake)
	require.EqualValues(t, 1, got.Root.Reacted)

	msgs, err := svc.List(ctx, owner)
	require.NoError(t, err)
	require.Len(t, msgs, 1, "exactly one message: the Tool result; zero background User completion messages")
	require.Equal(t, message.Tool, msgs[0].Role)
	results := msgs[0].ToolResults()
	require.Len(t, results, 1)
	require.Equal(t, "call", results[0].ToolCallID)
	require.Equal(t, "done", results[0].Content)
	require.Equal(t, msgs[0].ID, got.Root.AnnounceMessageID.String)
	require.Equal(t, msgs[0].ID, got.Root.NoticeMessageID.String)
}
