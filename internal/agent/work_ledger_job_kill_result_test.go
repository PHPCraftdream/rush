// job_kill result recording (A3 fusion + R2A-8 tool side): the tool result of
// a job_kill that stopped a tracked job is fused into that row's
// notice_message_id, and whenever that fused write does not happen the row is
// re-pended so the captured output is delivered by the ordinary pull instead
// of being lost (DUR-11).
package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type rependCall struct {
	owner, claimID string
	ctxErr         error
}

// fakeRepender records rependJobKill's store call (the real store method is
// RependJobKillRowWithoutNotice; see jobKillRepender).
type fakeRepender struct {
	mu    sync.Mutex
	calls []rependCall
}

func (f *fakeRepender) RependJobKillRowWithoutNotice(ctx context.Context, owner, claimID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rependCall{owner, claimID, ctx.Err()})
	return true, nil
}

func (f *fakeRepender) snapshot() []rependCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rependCall(nil), f.calls...)
}

type jobKillResultFixture struct {
	l        *workLedger
	messages *failingCreateTxMessages
	rep      *fakeRepender
	claim    string
	result   message.ToolResult
}

func (f *jobKillResultFixture) params() message.CreateMessageParams {
	return message.CreateMessageParams{Role: message.Tool, Parts: []message.ContentPart{f.result}}
}

// newJobKillResultFixture stops a real, announced bash job through
// MarkJobStopped and builds the job_kill tool result the tool would return.
func newJobKillResultFixture(t *testing.T) *jobKillResultFixture {
	t.Helper()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	f := &jobKillResultFixture{messages: &failingCreateTxMessages{Service: message.NewService(db.New(conn))}, rep: &fakeRepender{}}
	f.l = newWorkLedger(nil)
	f.l.store = store
	f.l.jobKillRepend = f.rep

	_, _, err := f.l.Start("owner-1", "call_0", "sleep 9", tools.BashToolName, "", false, false, nil, func() {})
	require.NoError(t, err)
	f.l.acknowledged(jobOf(f.l, "owner-1", "call_0"))
	text, claim, verdict := f.l.MarkJobStopped("owner-1", "call_0")
	require.Equal(t, tools.JobStopStopped, verdict)
	row, err := store.Get(context.Background(), "owner-1", "call_0")
	require.NoError(t, err)
	require.Equal(t, row.ClaimID, claim, "the verdict reports the claim of the row it stopped")
	f.claim = claim

	meta, err := json.Marshal(tools.JobKillResponseMetadata{JobID: "call_0", ShellID: "s1", KilledClaimID: claim})
	require.NoError(t, err)
	f.result = message.ToolResult{ToolCallID: "kill-1", Name: tools.JobKillToolName, Content: text, Metadata: string(meta)}
	return f
}

func (f *jobKillResultFixture) noticeMessageID(t *testing.T) (string, bool) {
	t.Helper()
	row, err := f.l.store.Get(context.Background(), "owner-1", "call_0")
	require.NoError(t, err)
	return row.NoticeMessageID.String, row.NoticeMessageID.Valid
}

func (f *jobKillResultFixture) messageCount(t *testing.T) int {
	t.Helper()
	msgs, err := f.messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	return len(msgs)
}

// TestPersistToolResult_JobKillResultFusesIntoRowAndDoesNotRepend is the
// happy path: nothing to re-pend.
func TestPersistToolResult_JobKillResultFusesIntoRowAndDoesNotRepend(t *testing.T) {
	t.Parallel()
	f := newJobKillResultFixture(t)

	require.NoError(t, f.l.persistToolResult(context.Background(), "owner-1", f.result, f.messages, f.params()))

	id, valid := f.noticeMessageID(t)
	require.True(t, valid, "the row must name the message that carries its result")
	msgs, err := f.messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, msgs[0].ID, id)
	require.Empty(t, f.rep.snapshot())
}

// TestPersistToolResult_JobKillFusedWriteFailureRependsRow: the fused
// transaction fails, so the row would stay done with no notice_message_id --
// it must be re-pended for the exact claim that was stopped.
//
// Revert-check: with rependJobKill a no-op, no re-pend was recorded.
func TestPersistToolResult_JobKillFusedWriteFailureRependsRow(t *testing.T) {
	t.Parallel()
	f := newJobKillResultFixture(t)
	f.messages.fail.Store(true)

	err := f.l.persistToolResult(context.Background(), "owner-1", f.result, f.messages, f.params())

	require.Error(t, err)
	require.Equal(t, []rependCall{{"owner-1", f.claim, nil}}, f.rep.snapshot())
	require.Zero(t, f.messageCount(t))
	_, valid := f.noticeMessageID(t)
	require.False(t, valid)
}

// TestPersistToolResult_JobKillErrorResultStillPersistsAndRependsRow: a
// result that is an error (so the fused write is skipped) but names a row it
// stopped: the message is persisted the ordinary way and the row re-pended.
//
// Revert-check: with the error branch not re-pending (handled=false, plain
// Create only), no re-pend was recorded.
func TestPersistToolResult_JobKillErrorResultStillPersistsAndRependsRow(t *testing.T) {
	t.Parallel()
	f := newJobKillResultFixture(t)
	f.result.IsError = true

	require.NoError(t, f.l.persistToolResult(context.Background(), "owner-1", f.result, f.messages, f.params()))

	require.Equal(t, 1, f.messageCount(t))
	require.Equal(t, []rependCall{{"owner-1", f.claim, nil}}, f.rep.snapshot())
	_, valid := f.noticeMessageID(t)
	require.False(t, valid, "the error path does not fuse")
}

// TestPersistToolResult_JobKillCancelledCtxRependsOnDetachedCtx: the turn's
// ctx is cancelled, so the fused write fails; the re-pend still runs, on a
// ctx that is not cancelled.
func TestPersistToolResult_JobKillCancelledCtxRependsOnDetachedCtx(t *testing.T) {
	t.Parallel()
	f := newJobKillResultFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.Error(t, f.l.persistToolResult(ctx, "owner-1", f.result, f.messages, f.params()))

	calls := f.rep.snapshot()
	require.Len(t, calls, 1)
	require.NoError(t, calls[0].ctxErr, "the re-pend must not inherit the cancelled ctx")
}

// TestPersistToolResult_JobKillResultWithoutKilledClaimIsPlain: a result that
// stopped nothing tracked (raw shell_id kill, refusal) has no row to fuse
// onto or re-pend.
func TestPersistToolResult_JobKillResultWithoutKilledClaimIsPlain(t *testing.T) {
	t.Parallel()
	f := newJobKillResultFixture(t)
	f.result.Metadata = `{"job_id":"call_0","shell_id":"s1"}`

	require.NoError(t, f.l.persistToolResult(context.Background(), "owner-1", f.result, f.messages, f.params()))

	require.Equal(t, 1, f.messageCount(t))
	_, valid := f.noticeMessageID(t)
	require.False(t, valid)
	require.Empty(t, f.rep.snapshot())
}

// TestStopRunCommandJob_ReportsClaimOfStoppedRow: the run_command path
// reports the stopped row's claim too.
func TestStopRunCommandJob_ReportsClaimOfStoppedRow(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	store := newTestAsyncJobStore(t)
	l.store = store
	_, _, err := l.Start("owner", "call", "", tools.RunCommandToolName, "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner", "call"))

	_, claim, stopErr := l.StopRunCommandJob("owner", "call")
	require.NoError(t, stopErr)
	row, err := store.Get(context.Background(), "owner", "call")
	require.NoError(t, err)
	require.Equal(t, row.ClaimID, claim)
	require.NotEmpty(t, claim)
}
