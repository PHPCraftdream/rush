// The A14 inline window (docs/plans/2026-10-01-inline-window.md): a
// bash/run_command job whose natural terminal transition commits inside the
// window is answered by its own tool call; the fused Tx2
// (session.AsyncJobStore.AnnounceInlineResult) marks the row announced AND
// done in the same transaction as the inline tool-result message. These
// tests drive the real asyncTool -> workLedger -> turnStream.onToolResult
// path against a real SQLite store, like work_ledger_ack_tag_test.go.
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// inlineFixture starts a real windowed async job through asyncTool.Run.
type inlineFixture struct {
	l        *workLedger
	store    *session.AsyncJobStore
	conn     *sql.DB
	messages *failingCreateMessages
	ts       *turnStream
	wrapped  *asyncTool
	ctx      context.Context
	call     fantasy.ToolCall
	innerCtx context.Context
	// release unblocks a slow inner tool.
	release chan struct{}
	webDone atomic.Int32
	once    sync.Once
	resp    fantasy.ToolResponse
}

type inlineInnerMode int

const (
	innerFast inlineInnerMode = iota // returns immediately
	innerSlow                        // blocks until f.release is closed
)

func newInlineFixture(t *testing.T, mode inlineInnerMode) *inlineFixture {
	t.Helper()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	f := &inlineFixture{
		store:   store,
		conn:    conn,
		release: make(chan struct{}),
	}
	f.messages = &failingCreateMessages{Service: message.NewService(db.New(conn))}
	f.l = newWorkLedger(func(AsyncCompletion) { f.webDone.Add(1) })
	f.l.store = store
	f.l.timeouts = newTimeoutService(f.l)
	f.l.inlineWindow = 80 * time.Millisecond

	inner := fantasy.NewAgentTool("run_command", "Run a program", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		f.innerCtx = ctx
		if mode == innerSlow {
			<-f.release
		}
		return fantasy.NewTextResponse("out"), nil
	})
	f.wrapped = &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: f.l}, name: "run_command"}
	f.ctx = WithCallOrigin(context.WithValue(t.Context(), tools.SessionIDContextKey, "owner-1"), message.OriginWeb)
	f.call = fantasy.ToolCall{ID: "call_0", Name: "run_command", Input: `{}`}

	t.Cleanup(func() {
		f.once.Do(func() { close(f.release) })
	})

	sa := &sessionAgent{asyncJobs: f.l, messages: f.messages}
	f.ts = &turnStream{
		a: sa, ctx: t.Context(), currentAssistant: &message.Message{SessionID: "owner-1"},
		bumpActivity: func() {}, toolFinished: func() {},
	}
	return f
}

func (f *inlineFixture) unblock() { f.once.Do(func() { close(f.release) }) }

func (f *inlineFixture) run(t *testing.T) fantasy.ToolResponse {
	t.Helper()
	resp, err := f.wrapped.Run(f.ctx, f.call)
	require.NoError(t, err)
	f.resp = resp
	return resp
}

// resultContent turns the fixture's response into the tool result
// onToolResult persists, exactly as the agent's stream does.
func (f *inlineFixture) resultContent() fantasy.ToolResultContent {
	return fantasy.ToolResultContent{
		ToolCallID: "call_0", ToolName: "run_command", ClientMetadata: f.resp.Metadata,
		Result: fantasy.ToolResultOutputContentText{Text: f.resp.Content},
	}
}

func (f *inlineFixture) row(t *testing.T) db.AsyncJob {
	t.Helper()
	row, err := f.store.Get(context.Background(), "owner-1", "call_0")
	require.NoError(t, err)
	return row
}

func (f *inlineFixture) pullNotices(t *testing.T) []session.PulledNotice {
	t.Helper()
	notices, err := f.store.PullJobNotices(context.Background(), f.messages, "owner-1",
		func(session.JobNoticeRow) message.CreateMessageParams {
			return message.CreateMessageParams{Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "notice"}}}
		})
	require.NoError(t, err)
	return notices
}

func (f *inlineFixture) debt(t *testing.T) session.DebtSnapshot {
	t.Helper()
	snap, err := f.store.CaptureDebtSnapshot(context.Background(), "owner-1")
	require.NoError(t, err)
	return snap
}

func metadataOf(t *testing.T, metadata string) asyncToolMetadata {
	t.Helper()
	var meta asyncToolMetadata
	require.NoError(t, json.Unmarshal([]byte(metadata), &meta))
	return meta
}

// TestInlineWindow_FastJobAnswersWithResult (design test 1): a job finishing
// inside the window is answered by its own call; after the inline tool
// result is persisted the row is completed/announced/done/wake=0/reacted=1
// with notice_message_id == announce_message_id, no notice is pulled, no
// debt remains and onWebDone never fired.
//
// Revert-check (mutant, executed): with awaitInline forced to return false
// (window disabled) the same flow answers "started" and AnnounceStarted
// commits a pullable notice with wake=1 -- TestInlineWindow_WindowZero
// below IS that mutant's observable behavior, asserted.
func TestInlineWindow_FastJobAnswersWithResult(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerFast)
	resp := f.run(t)

	meta := metadataOf(t, resp.Metadata)
	require.False(t, meta.Async)
	require.True(t, meta.Inline)
	require.Equal(t, "out", resp.Content)
	require.NotEmpty(t, meta.ClaimID)

	require.NoError(t, f.ts.onToolResult(f.resultContent()))

	row := f.row(t)
	require.Equal(t, "completed", row.State)
	require.EqualValues(t, 1, row.Announced)
	require.Equal(t, "done", row.Delivery)
	require.EqualValues(t, 0, row.Wake)
	require.EqualValues(t, 1, row.Reacted)
	require.Equal(t, row.AnnounceMessageID.String, row.NoticeMessageID.String,
		"the inline message is both the announce and the result message")
	require.EqualValues(t, 0, row.ResultIsError.Int64)

	require.Empty(t, f.pullNotices(t), "a done row is never pulled")
	snap := f.debt(t)
	require.Empty(t, snap.Jobs)
	require.Empty(t, snap.Notices)
	require.EqualValues(t, 0, f.webDone.Load(), "no wake hint for an inline answer")
	require.False(t, f.l.running("owner-1"), "the job left the ledger")
}

// TestInlineWindow_WindowZero is the revert-check executed for real: the
// same fixture with the seam at 0 keeps the pre-A14 flow byte for byte --
// "started" response, one pullable notice, reaction debt until reacted.
func TestInlineWindow_WindowZero(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerFast)
	f.l.inlineWindow = 0
	resp := f.run(t)

	meta := metadataOf(t, resp.Metadata)
	require.True(t, meta.Async)
	require.False(t, meta.Inline)
	require.Contains(t, resp.Content, "started")

	require.NoError(t, f.ts.onToolResult(f.resultContent()))
	row := f.row(t)
	require.EqualValues(t, 1, row.Announced)
	// The terminal transition may still be in flight when the fast inner's
	// ack lands (delivery is 'none' until Tx1 commits); the invariant is
	// "after the transition commits, the notice waits to be pulled".
	require.Eventually(t, func() bool {
		row = f.row(t)
		return row.Delivery == "pending"
	}, 5*time.Second, 5*time.Millisecond, "a terminal job's notice waits to be pulled")

	// A fast job is already terminal when its ack lands, so the ack's own
	// deliverLocked fires the wake hint today (finishAcknowledgeLocally) --
	// exactly the extra circle the inline window removes. The hint fires
	// wherever the terminal transition and the ack meet, so only its
	// existence is pinned.
	require.Eventually(t, func() bool { return f.webDone.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.False(t, f.l.running("owner-1"))

	notices := f.pullNotices(t)
	require.Len(t, notices, 1)
	require.True(t, notices[0].Wake)
	require.Len(t, f.debt(t).Jobs, 1, "the pulled notice is debt until a model step reacts")
}

// TestAnnounceInlineResult_Atomicity (design test 2): a delivery-half
// failure rolls the WHOLE transaction back -- no message, announced stays 0.
//
// Revert-check (analysis): with the message insert and the delivery CAS in
// two separate transactions, the announce half would commit on its own: the
// message would exist in history while the row stays announced=1/pending --
// an announced row whose result never closes its delivery. The trigger
// below (RAISE ABORT on the delivery UPDATE) fails exactly that seam.
func TestAnnounceInlineResult_Atomicity(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerFast)
	resp := f.run(t)
	meta := metadataOf(t, resp.Metadata)

	_, err := f.conn.ExecContext(context.Background(), `CREATE TRIGGER inline_fail_delivery BEFORE UPDATE ON async_jobs
WHEN NEW.delivery = 'done' AND OLD.delivery = 'pending'
BEGIN
    SELECT RAISE(ABORT, 'simulated delivery failure');
END;`)
	require.NoError(t, err)

	_, err = f.store.AnnounceInlineResult(context.Background(), f.messages, "owner-1", "call_0", meta.ClaimID, startedParams("inline out"))
	require.Error(t, err)

	row := f.row(t)
	require.EqualValues(t, 0, row.Announced, "the announce half rolled back with the delivery half")
	require.Empty(t, row.AnnounceMessageID.String)
	msgs, listErr := f.messages.List(context.Background(), "owner-1")
	require.NoError(t, listErr)
	require.Empty(t, msgs, "no message survived the rollback")
}

// TestAnnounceInlineResult_VoidedByRerunStillCommitsMessage: a 0-rows
// delivery CAS (the row was voided by a Rerun between the window decision
// and Tx2) is not an error: the message commits, announced=1 records it,
// and there is no delivery left to close.
func TestAnnounceInlineResult_VoidedByRerunStillCommitsMessage(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerFast)
	resp := f.run(t)
	meta := metadataOf(t, resp.Metadata)

	_, err := f.conn.ExecContext(context.Background(), `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call_0'`)
	require.NoError(t, err)

	ann, err := f.store.AnnounceInlineResult(context.Background(), f.messages, "owner-1", "call_0", meta.ClaimID, startedParams("inline out"))
	require.NoError(t, err)
	require.EqualValues(t, 0, ann.DeliveryRows)
	row := f.row(t)
	require.EqualValues(t, 1, row.Announced)
	require.Equal(t, "void", row.Delivery)
	require.Equal(t, ann.Msg.ID, row.AnnounceMessageID.String)
}

// TestInlineWindow_SlowJobAnswersStartedWithExactlyOneNotice (design test 3
// and 4, Stop/timeout members): every late outcome answers "started" exactly
// like today, with exactly one completion notice afterwards.
func TestInlineWindow_SlowJobAnswersStartedWithExactlyOneNotice(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerSlow)
	resp := f.run(t)

	meta := metadataOf(t, resp.Metadata)
	require.True(t, meta.Async)
	require.False(t, meta.Inline)
	require.Contains(t, resp.Content, "started")

	require.NoError(t, f.ts.onToolResult(f.resultContent()))
	row := f.row(t)
	require.EqualValues(t, 1, row.Announced)
	require.Equal(t, "none", row.Delivery, "the job is still running: no delivery state yet")
	f.unblock()
	require.Eventually(t, func() bool { return !f.l.running("owner-1") }, 5*time.Second, 10*time.Millisecond)
	require.GreaterOrEqual(t, f.webDone.Load(), int32(1), "the completion wakes the owner")
	notices := f.pullNotices(t)
	require.Len(t, notices, 1)
	require.Empty(t, f.pullNotices(t), "pulling twice yields nothing more")
}

// TestInlineWindow_StopInWindow (design test 4 + the Stop-in-window orphan
// risk): Stop inside the window (cancelSession, the real Stop path) answers
// "started"; the row is cancelled with wake=0 (never debt, never a notice);
// and the job must NOT stay orphaned in memory even when the model's
// OnToolResult for the started response never arrives.
func TestInlineWindow_StopInWindow(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerSlow)
	// Stop must land INSIDE the window: the 80 ms fixture window elapsed
	// before cancelSession under load, taking the timer path instead.
	f.l.inlineWindow = 30 * time.Second
	respCh := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, err := f.wrapped.Run(f.ctx, f.call)
		require.NoError(t, err)
		respCh <- resp
	}()
	require.Eventually(t, func() bool { return f.innerCtx != nil }, 5*time.Second, 5*time.Millisecond)

	f.l.cancelSession("owner-1")
	resp := <-respCh
	require.Contains(t, resp.Content, "started", "Stop inside the window falls back to started")

	row := f.row(t)
	require.Equal(t, "cancelled", row.State)
	require.EqualValues(t, 0, row.Wake, "a Stop-cancelled row is never debt")
	require.Empty(t, f.debt(t).Jobs)

	// The orphan half: no OnToolResult ever arrives for the started
	// response. The job must still leave the ledger (the limit of 50 and
	// l.running must not be held forever by a dead call).
	require.Eventually(t, func() bool { return !f.l.running("owner-1") }, 5*time.Second, 10*time.Millisecond)
	require.Empty(t, f.pullNotices(t), "a cancelled row never produces a notice")
}

// TestInlineWindow_CtxCancelInWindow: the caller's ctx ending inside the
// window (a cancelled turn) also falls back to "started"; the detached
// executor keeps running and delivers exactly one completion notice after
// the late started ack.
func TestInlineWindow_CtxCancelInWindow(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerSlow)
	ctx, cancel := context.WithCancel(f.ctx)
	f.ctx = ctx
	runDone := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, err := f.wrapped.Run(f.ctx, f.call)
		require.NoError(t, err)
		runDone <- resp
	}()
	require.Eventually(t, func() bool { return f.innerCtx != nil }, 5*time.Second, 5*time.Millisecond)
	cancel()
	resp := <-runDone
	require.Contains(t, resp.Content, "started")

	// The detached executor survives and delivers normally afterwards.
	f.unblock()
	require.Eventually(t, func() bool {
		row, err := f.store.Get(context.Background(), "owner-1", "call_0")
		require.NoError(t, err)
		return row.State == "completed" && row.Delivery == "pending"
	}, 5*time.Second, 10*time.Millisecond)
	f.resp = resp
	require.NoError(t, f.ts.onToolResult(f.resultContent()))
	notices := f.pullNotices(t)
	require.Len(t, notices, 1, "exactly one completion notice for the late job")
}

// TestInlineWindow_LegacyTimeoutAnswersStarted (design test 4, timeout
// member): a legacy run_command timeout_seconds=1 cannot land inside the
// window (compile-time assert N < timeoutSecondsFloor); its timed-out
// outcome takes the started path and delivers the timeout notice.
func TestInlineWindow_LegacyTimeoutAnswersStarted(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerSlow)
	f.call.Input = `{"timeout_seconds":1}`
	resp := f.run(t)
	require.Contains(t, resp.Content, "started", "a 1s timeout can never resolve inside an 80ms window")

	require.NoError(t, f.ts.onToolResult(f.resultContent()))
	// The 1s timeout must fire while the executor is still blocked; the
	// timed-out outcome transitions the row independently of the executor.
	require.Eventually(t, func() bool {
		row, err := f.store.Get(context.Background(), "owner-1", "call_0")
		require.NoError(t, err)
		return row.State == "timed_out"
	}, 5*time.Second, 10*time.Millisecond)
	notices := f.pullNotices(t)
	require.Len(t, notices, 1)
}

// TestInlineWindow_CloseInWindow (design test 4, shutdown member): a ledger
// close inside the window leaves the row running for the next host (the
// shutdown latch) and answers started.
func TestInlineWindow_CloseInWindow(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerSlow)
	runDone := make(chan string, 1)
	go func() {
		resp, err := f.wrapped.Run(f.ctx, f.call)
		require.NoError(t, err)
		runDone <- resp.Content
	}()
	require.Eventually(t, func() bool { return f.innerCtx != nil }, 5*time.Second, 5*time.Millisecond)
	f.l.close()
	require.Contains(t, <-runDone, "started")
	row := f.row(t)
	require.Equal(t, "running", row.State, "close leaves the row for dead-host recovery")
}

// TestInlineWindow_IdempotentRetryDoesNotStealTheInlineAck: a repeated tool
// call racing the window gets a "started" response, but its tagged result
// must not claim the job's ack -- the inline result is the one that fuses.
//
// Revert-check (analysis): without job.inlinePending in claimAck, the
// retry's started result would win the ack (job.acking was free) and fuse
// AnnounceStarted -- announced=1 with a "started" message -- so the inline
// result's own fusion would be refused and the row's delivery would stay
// pending, waiting for a pulled notice nobody needs.
func TestInlineWindow_IdempotentRetryDoesNotStealTheInlineAck(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerFast)
	resp := f.run(t)
	meta := metadataOf(t, resp.Metadata)

	retryMeta, err := json.Marshal(asyncToolMetadata{Async: true, JobID: "call_0", Status: "running", ClaimID: meta.ClaimID})
	require.NoError(t, err)
	require.NoError(t, f.ts.onToolResult(fantasy.ToolResultContent{
		ToolCallID: "call_0", ToolName: "run_command",
		ClientMetadata: string(retryMeta),
		Result:         fantasy.ToolResultOutputContentText{Text: "Async run_command job call_0 started."},
	}))
	row := f.row(t)
	require.EqualValues(t, 0, row.Announced, "the retry's started result must not announce the inline job")

	require.NoError(t, f.ts.onToolResult(f.resultContent()))
	row = f.row(t)
	require.EqualValues(t, 1, row.Announced)
	require.Equal(t, "done", row.Delivery)
	require.Equal(t, row.AnnounceMessageID.String, row.NoticeMessageID.String)
	require.EqualValues(t, 0, f.webDone.Load())
}

// TestInlineWindow_RerunVoidsDeliveredInlineRow (design test 6): after the
// inline delivery, a Rerun voiding the row (by the announce message id) is
// final, and neither the job_kill re-pend nor any other writer resurrects
// it.
func TestInlineWindow_RerunVoidsDeliveredInlineRow(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerFast)
	resp := f.run(t)
	require.NoError(t, f.ts.onToolResult(f.resultContent()))
	row := f.row(t)
	require.Equal(t, "done", row.Delivery)

	_, err := f.conn.ExecContext(context.Background(), `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call_0'`)
	require.NoError(t, err)

	// The only live-process writer that re-pends a done row is the job_kill
	// repair, keyed by claim: an inline row never matches it.
	repended, err := f.store.RependJobKillRowWithoutNotice(context.Background(), "owner-1", metadataOf(t, resp.Metadata).ClaimID)
	require.NoError(t, err)
	require.False(t, repended)
	row = f.row(t)
	require.Equal(t, "void", row.Delivery)
	require.Empty(t, f.pullNotices(t), "a void row is never pulled")
}

// TestInlineWindow_SupervisionAndChain (design test 5): an inline-answered
// call must not arm (or resume) the supervision timer -- noteWorkStarted
// runs only on the started fallback -- and an inline wait command is chain
// idle WITHOUT a claim, while a real inline command is progress.
func TestInlineWindow_SupervisionAndChain(t *testing.T) {
	t.Parallel()
	f := newInlineFixture(t, innerFast)
	f.l.supervision = newSupervisionRegistry()
	resp := f.run(t)
	require.True(t, metadataOf(t, resp.Metadata).Inline)
	require.Empty(t, f.l.supervision.byRoot, "an inline answer arms no supervision check-in")

	// The started fallback DOES run noteWorkStarted (still inert without a
	// supervision config, so the observable assertion is that the call path
	// is reached without error and no entry appears unconfigured).
	f2 := newInlineFixture(t, innerSlow)
	f2.l.supervision = newSupervisionRegistry()
	f2.l.coord = &coordinator{asyncJobs: f2.l, subAgentDrivers: newSubAgentDriverRegistry()}
	resp2 := f2.run(t)
	require.False(t, metadataOf(t, resp2.Metadata).Inline)
	require.NotEmpty(t, f2.l.supervision.byRoot,
		"the started fallback runs noteWorkStarted (the inline path above must not)")

	// Chain half: an inline wait command is idle with NO claim.
	call := fantasy.ToolCallContent{ToolName: "bash", Input: `{"command":"sleep 2"}`}
	claim, idle := chainIdleClaim(call, asyncToolMetadata{Async: false, Inline: true, ClaimID: "claim-1"})
	require.True(t, idle)
	require.Empty(t, claim, "an inline wait command registers no chain claim")

	// A real inline command is progress, exactly like any other tool.
	realCall := fantasy.ToolCallContent{ToolName: "bash", Input: `{"command":"grep -rn foo ."}`}
	_, idle = chainIdleClaim(realCall, asyncToolMetadata{Async: false, Inline: true})
	require.False(t, idle)
}

// TestInlineWindow_DelegationNeverInline (design "N and scope"): agent/
// agentic_fetch are never answered inline, window or not.
func TestInlineWindow_DelegationNeverInline(t *testing.T) {
	t.Parallel()
	require.False(t, inlineWindowApplies("agent"))
	require.False(t, inlineWindowApplies("agentic_fetch"))
	require.True(t, inlineWindowApplies("bash"))
	require.True(t, inlineWindowApplies("run_command"))
}
