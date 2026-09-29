// The ack gate decides "this tool result is job X's own started result" by a
// claim tag, not by tool_call_id (R2B-3): providers that number calls per
// response reuse ids, so an ordinary result can collide with a running
// job's key. These tests drive turnStream.onToolResult, the real entry
// point, against a real store and message service.
package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// failingCreateMessages fails the plain Create path (CreateTx is the fused
// path's; see failingCreateTxMessages).
type failingCreateMessages struct {
	message.Service
	fail atomic.Bool
}

func (f *failingCreateMessages) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	if f.fail.Load() {
		return message.Message{}, errors.New("simulated Create failure")
	}
	return f.Service.Create(ctx, sessionID, params)
}

// ackGateFixture starts a real async run_command job through asyncTool.Run
// and exposes onToolResult for it.
type ackGateFixture struct {
	l        *workLedger
	store    *session.AsyncJobStore
	messages *failingCreateMessages
	ts       *turnStream
	started  fantasy.ToolResponse // the job's own "started" response
	innerCtx context.Context      // the ctx the executor runs under
	wrapped  *asyncTool
	ctx      context.Context
	call     fantasy.ToolCall
}

func newAckGateFixture(t *testing.T) *ackGateFixture {
	t.Helper()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	real := message.NewService(db.New(conn))
	f := &ackGateFixture{store: store, messages: &failingCreateMessages{Service: real}}
	f.l = newWorkLedger(nil)
	f.l.store = store

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ctxCh := make(chan context.Context, 1)
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		ctxCh <- ctx
		<-release
		return fantasy.NewTextResponse("out"), nil
	})
	f.wrapped = &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: f.l}, name: "run_command"}
	f.ctx = WithCallOrigin(context.WithValue(t.Context(), tools.SessionIDContextKey, "owner-1"), message.OriginWeb)
	f.call = fantasy.ToolCall{ID: "call_0", Name: "run_command", Input: `{}`}
	resp, err := f.wrapped.Run(f.ctx, f.call)
	require.NoError(t, err)
	f.started = resp
	f.innerCtx = <-ctxCh

	sa := &sessionAgent{asyncJobs: f.l, messages: f.messages}
	f.ts = &turnStream{
		a: sa, ctx: t.Context(), currentAssistant: &message.Message{SessionID: "owner-1"},
		bumpActivity: func() {}, toolFinished: func() {},
	}
	return f
}

func (f *ackGateFixture) startedResult() fantasy.ToolResultContent {
	return fantasy.ToolResultContent{
		ToolCallID: "call_0", ToolName: "run_command", ClientMetadata: f.started.Metadata,
		Result: fantasy.ToolResultOutputContentText{Text: f.started.Content},
	}
}

// ordinaryResult is a colliding ordinary tool result: same id, other tool,
// no claim tag.
func ordinaryResult(text string) fantasy.ToolResultContent {
	return fantasy.ToolResultContent{
		ToolCallID: "call_0", ToolName: "view",
		Result: fantasy.ToolResultOutputContentText{Text: text},
	}
}

func (f *ackGateFixture) row(t *testing.T) (announced int64, announceMsg string) {
	t.Helper()
	row, err := f.store.Get(context.Background(), "owner-1", "call_0")
	require.NoError(t, err)
	return row.Announced, row.AnnounceMessageID.String
}

// TestAckGate_CollidingOrdinaryResultDoesNotTouchRunningJob: an ordinary
// `view call_0` result arriving while async `call_0` runs must be persisted
// as an ordinary message and touch neither announce_message_id (Rerun voids
// by it) nor the executor -- also when its own write fails (the abort path).
//
// Revert-check: with claimAck resolving the job by tool_call_id alone
// (no claim tag), the colliding result was fused by AnnounceStarted and
// overwrote announce_message_id; the failing variant aborted the running job
// (row deleted, executor context cancelled).
func TestAckGate_CollidingOrdinaryResultDoesNotTouchRunningJob(t *testing.T) {
	t.Parallel()
	f := newAckGateFixture(t)

	require.NoError(t, f.ts.onToolResult(f.startedResult()))
	announced, ownMsg := f.row(t)
	require.EqualValues(t, 1, announced)
	require.NotEmpty(t, ownMsg)

	require.NoError(t, f.ts.onToolResult(ordinaryResult("file contents")))
	_, afterMsg := f.row(t)
	require.Equal(t, ownMsg, afterMsg, "an ordinary result must not overwrite announce_message_id")
	msgs, err := f.messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Len(t, msgs, 2, "both results are persisted")

	f.messages.fail.Store(true)
	require.Error(t, f.ts.onToolResult(ordinaryResult("second")))
	f.messages.fail.Store(false)
	_, afterFail := f.row(t)
	require.Equal(t, ownMsg, afterFail, "a failed ordinary write must not delete the running job's row")
	require.True(t, f.l.running("owner-1"), "and must not drop the job")
	require.NoError(t, f.innerCtx.Err(), "and must not cancel its executor")
}

// TestAckGate_OrdinaryResultBeforeOwnStartedDoesNotAnnounce: the collision in
// the other order -- the ordinary result lands first -- leaves the job
// unannounced until its own started result arrives.
func TestAckGate_OrdinaryResultBeforeOwnStartedDoesNotAnnounce(t *testing.T) {
	t.Parallel()
	f := newAckGateFixture(t)

	require.NoError(t, f.ts.onToolResult(ordinaryResult("file contents")))
	announced, _ := f.row(t)
	require.EqualValues(t, 0, announced, "an ordinary result must not announce the job")

	require.NoError(t, f.ts.onToolResult(f.startedResult()))
	announced, msgID := f.row(t)
	require.EqualValues(t, 1, announced)
	msgs, err := f.messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, msgs[1].ID, msgID, "announce_message_id names the job's own started message")
}

// TestAckGate_SecondStartedResultForSameJobDoesNotReannounce: an idempotent
// repeat of the same call returns another tagged started response for the
// same job; once announced, it must not overwrite announce_message_id.
func TestAckGate_SecondStartedResultForSameJobDoesNotReannounce(t *testing.T) {
	t.Parallel()
	f := newAckGateFixture(t)

	require.NoError(t, f.ts.onToolResult(f.startedResult()))
	_, first := f.row(t)

	repeat, err := f.wrapped.Run(f.ctx, f.call) // existing job: same claim tag
	require.NoError(t, err)
	require.Equal(t, f.started.Metadata, repeat.Metadata)
	require.NoError(t, f.ts.onToolResult(fantasy.ToolResultContent{
		ToolCallID: "call_0", ToolName: "run_command", ClientMetadata: repeat.Metadata,
		Result: fantasy.ToolResultOutputContentText{Text: repeat.Content},
	}))
	_, second := f.row(t)
	require.Equal(t, first, second)
}

// TestAckGate_AbortLeavesAnnouncedJobAlone pins "abort only when still
// announced=0": an abort that arrives after the row was announced must
// neither delete it nor cancel the executor.
//
// Revert-check: without the announced guard in abort the row was deleted and
// the executor cancelled.
func TestAckGate_AbortLeavesAnnouncedJobAlone(t *testing.T) {
	t.Parallel()
	f := newAckGateFixture(t)
	require.NoError(t, f.ts.onToolResult(f.startedResult()))
	job := jobOf(f.l, "owner-1", "call_0")

	f.l.abort(job)

	announced, _ := f.row(t)
	require.EqualValues(t, 1, announced, "the announced row must survive")
	require.True(t, f.l.running("owner-1"))
	require.NoError(t, f.innerCtx.Err())
}

// TestAckGate_RowAnnouncedInDBIsNotFusedAgain: the durable row, not just
// memory, must be unannounced: a row already announced (memory lagging) is
// treated as an ordinary result.
func TestAckGate_RowAnnouncedInDBIsNotFusedAgain(t *testing.T) {
	t.Parallel()
	f := newAckGateFixture(t)
	require.NoError(t, f.store.MarkAnnounced(context.Background(), "owner-1", "call_0"))

	require.NoError(t, f.ts.onToolResult(f.startedResult()))
	_, msgID := f.row(t)
	require.Empty(t, msgID, "no fused write: announce_message_id stays unset")
	msgs, err := f.messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "the result is still persisted, once")
}
