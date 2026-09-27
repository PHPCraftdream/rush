package agent

// Regression coverage for the parked sub-agent delegation outcome registry
// (subagent_outcome.go). Before that registry existed, the async `agent` /
// `agentic_fetch` tools delivered the child's completion to the PARENT
// session the instant the child's Run() returned — which is only the end of
// ONE MODEL TURN. A child that started its own async tools or background
// shells had merely yielded, so the parent was told "Async job ... finished."
// over content that said the child was still waiting, and the child's later
// self-directed work only ever reached the child session.
//
// The tests below pin the five properties the fix must hold:
//
//  1. no notice is emitted while the child still owns async work;
//  2. exactly one notice is emitted, once that work is terminal;
//  3. a failed child turn is delivered as a failure, not a success;
//  4. a cancel releases the parked notice instead of losing it, and a
//     resumed child never replays an already-delivered notice;
//  5. that single notice survives CONCURRENT re-checks, and a cancel
//     survives a child that had already finished a turn with text.

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

const (
	// parkedParentSession is the session whose `agent` tool call spawned
	// the delegation. The notice is always delivered here, never to the
	// child.
	parkedParentSession = "parked-parent"
	// parkedParentCall is the parent's tool call id, i.e. the async job id
	// asyncTool.Run registered on the parent session.
	parkedParentCall = "parent-call"
	// parkedChildSession is the sub-session the delegation ran in. Async
	// work the child owns is registered under THIS id.
	parkedChildSession = "parked-child"
	// parkedChildJob is one async job the child itself started.
	parkedChildJob = "child-job"
)

// newParkedOutcomeCoordinator builds the smallest coordinator that can park
// and release a delegated sub-agent outcome: the async job registry and the
// park registry, with no currentAgent (so subAgentWorkTerminal's negative
// busy gate reads false) and no messages service (so a release keeps the
// completion captured at park time). Tests that need a refreshed completion
// or a busy gate set those fields afterwards.
func newParkedOutcomeCoordinator(onWebDone func(AsyncCompletion)) *coordinator {
	coord := &coordinator{}
	coord.asyncJobs = newAsyncJobRegistry(onWebDone)
	coord.subAgentOutcomes = newSubAgentOutcomeRegistry(coord)
	coord.installSubAgentOutcomeHooks()
	return coord
}

// newYieldedInnerTool is the inner tool asyncTool wraps for the `agent`
// delegation: it returns the text the child's yielding turn ended on.
func newYieldedInnerTool(name, text string) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, "delegate", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse(text), nil
	})
}

// parkDelegation drives asyncTool.run for a delegation whose child session
// is childSession, with the parent's async job already registered and
// acknowledged the way agent_turn_stream.go's onToolResult does once the
// "Async agent job ... started" tool result is persisted.
func parkDelegation(t *testing.T, coord *coordinator, childSession, childYields string) {
	t.Helper()
	require.NoError(t, coord.asyncJobs.start(parkedParentSession, parkedParentCall, false, func() {}))
	coord.asyncJobs.acknowledged(parkedParentSession, parkedParentCall)

	wrapped := &asyncTool{
		inner:       newYieldedInnerTool(AgentToolName, childYields),
		coordinator: coord,
		name:        AgentToolName,
	}
	ctx := WithCallOrigin(t.Context(), message.OriginWeb)
	wrapped.run(ctx, func() {}, parkedParentSession, childSession, fantasy.ToolCall{
		ID: parkedParentCall, Name: AgentToolName, Input: `{}`,
	})
}

// startChildOwnedJob registers an async job the CHILD session owns, and
// acknowledges it exactly the way agent_turn_stream.go's onToolResult does
// once the child's "Async ... job started" tool result message is persisted.
// The acknowledgement matters: asyncJobRegistry.releaseLocked refuses to
// release an unacknowledged row even after its result is captured, so a job
// whose tool result was never recorded stays in the jobs map. That is
// correct in production (the turn is not durably done yet) but irrelevant to
// a test that only wants the child's owned job to reach a terminal state.
func startChildOwnedJob(t *testing.T, registry *asyncJobRegistry, sessionID, toolCallID string, cli bool) {
	t.Helper()
	require.NoError(t, registry.start(sessionID, toolCallID, cli, func() {}))
	registry.acknowledged(sessionID, toolCallID)
}

// finishChildJob completes a job the child owns and plays out the turn that
// completion wakes: the child itself must receive its own result, and only
// that woken run's end (notifyAsyncCompletion's deferred re-check) may
// release the parked delegation.
func finishChildJob(t *testing.T, coord *coordinator, delivered chan AsyncCompletion, completion AsyncCompletion) {
	t.Helper()
	coord.asyncJobs.finish(completion)
	select {
	case got := <-delivered:
		require.Equal(t, completion.SessionID, got.SessionID, "the child's own job result must wake the child")
		require.Equal(t, completion.ToolCallID, got.ToolCallID)
	default:
		t.Fatal("the child's own job result must wake the child")
	}
	coord.noteSubAgentChildRunEnded(completion.SessionID)
}

// drainDelegationNotices collects every completion delivered to the parent
// session, non-blockingly.
func drainDelegationNotices(delivered chan AsyncCompletion) []AsyncCompletion {
	got := make([]AsyncCompletion, 0, 4)
	for {
		select {
		case completion := <-delivered:
			got = append(got, completion)
		default:
			return got
		}
	}
}

// TestAsyncAgentTool_NoFinishedNoticeWhileChildOwnedJobsPending pins
// property 1: the parent is NOT told the delegation finished while the child
// still owns an async job. This is the direct regression for the misleading
// "agent finished" notice that sat on top of content saying the child was
// still waiting on its own async commands.
func TestAsyncAgentTool_NoFinishedNoticeWhileChildOwnedJobsPending(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })

	// The child's own async tool work is still in flight.
	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, false)

	parkDelegation(t, coord, parkedChildSession, "child yielded: async checks still running")

	require.True(t, coord.subAgentOutcomes.hasParked(),
		"the delegation must be parked while the child still owns async work")
	require.True(t, coord.asyncJobs.pending(parkedChildSession),
		"precondition: the child's own async job must still be running")
	require.Empty(t, drainDelegationNotices(delivered),
		"no completion notice may reach the parent while the child's own async work is outstanding")
}

// TestAsyncAgentTool_SingleFinalNoticeAfterChildJobsDrain pins property 2:
// exactly one notice, delivered only once the child's owned async work is
// terminal, carrying the child's own final text.
//
// The child's job is registered CLI-origin on purpose: nothing drains a
// child session's ready queue, so its CLI completion must wake the child
// (the notice then waits for that woken turn) instead of being queued where
// the child never sees its own result.
func TestAsyncAgentTool_SingleFinalNoticeAfterChildJobsDrain(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })

	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
	parkDelegation(t, coord, parkedChildSession, "child yielded: async checks still running")
	require.True(t, coord.subAgentOutcomes.hasParked())
	require.Empty(t, drainDelegationNotices(delivered))

	// The child's own async job reaches a terminal state, wakes the child,
	// and the woken turn's end releases the delegation.
	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID:  parkedChildSession,
		ToolCallID: parkedChildJob,
		ToolName:   "bash",
		Content:    "gate ok",
	})

	got := drainDelegationNotices(delivered)
	require.Len(t, got, 1, "exactly one final notice must reach the parent")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.Equal(t, parkedParentSession, got[0].SessionID)
	require.Equal(t, AgentToolName, got[0].ToolName)
	require.False(t, got[0].IsError)
	require.Contains(t, got[0].Content, "child yielded",
		"the notice must carry the child's own text, not the job's")
	require.False(t, coord.subAgentOutcomes.hasParked(),
		"the parked entry must be consumed by the release")
}

// TestSubAgentOutcome_FailedChildJobDeliveredOnceAsFailure pins property 3:
// when the child's last finished turn errored (which is what the child's own
// failed background job surfaces as once the auto-resume turn records it),
// the single notice the parent receives is a FAILURE, not a success. Status
// is taken from the child session's newest finished assistant message, so
// the "failed" wording in FormatAsyncCompletion is not a guess.
func TestSubAgentOutcome_FailedChildJobDeliveredOnceAsFailure(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "failed-child")
	require.NoError(t, err)

	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = env.messages

	// The child's last finished turn errored.
	row, err := env.messages.Create(t.Context(), child.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "the gate failed: exit 2"}},
	})
	require.NoError(t, err)
	row.AddFinish(message.FinishReasonError, "", "")
	require.NoError(t, env.messages.Update(t.Context(), row))

	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
	parkDelegation(t, coord, child.ID, "child yielded: gate still running")
	require.True(t, coord.subAgentOutcomes.hasParked())

	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID:  child.ID,
		ToolCallID: parkedChildJob,
		ToolName:   "bash",
		Content:    "exit 2",
		IsError:    true,
	})

	got := drainDelegationNotices(delivered)
	require.Len(t, got, 1, "exactly one notice, even for a failed child turn")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.True(t, got[0].IsError, "a failed child turn must reach the parent as a failure")
	require.Contains(t, got[0].Content, "the gate failed")
	require.Contains(t, FormatAsyncCompletion(got[0]), "failed",
		"the delivered notice must render the failed status, not a success")
}

// concurrentTryReleasers is how many re-check triggers fire at once in
// TestSubAgentOutcome_ConcurrentTryReleaseDeliversOnce — the shape of the
// real collision between the 2s fallback ticker and the job-completed hook.
const concurrentTryReleasers = 8

// refreshBarrierWindow is how long barrieredMessages holds a refresh read
// open before its barrier opens on its own. It only has to outlast the
// launch of concurrentTryReleasers goroutines, which takes microseconds, and
// keeps the fixed path's single claimed releaser from waiting longer than
// that.
const refreshBarrierWindow = 250 * time.Millisecond

// TestSubAgentOutcome_ConcurrentTryReleaseDeliversOnce pins the one-shot
// latch against CONCURRENT re-checks: the 2s fallback ticker and the
// job-completed hook can both walk the same parked entry at the same time,
// and only one of them may deliver the completion.
//
// The window is widened deterministically instead of by luck:
// barrieredMessages holds refreshSubAgentCompletion's DB read until every
// concurrent releaser has arrived inside it. A release that flipped its
// latch only AFTER that read handed the same entry to every caller that had
// already walked nextReleasable, and finishParked's missing-row re-insert
// branch then delivered the parent's completion again — the old order fails
// this test with concurrentTryReleasers deliveries, the claim-first latch
// yields exactly one.
func TestSubAgentOutcome_ConcurrentTryReleaseDeliversOnce(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "race-child")
	require.NoError(t, err)
	writeParkedChildTurn(t, env, child.ID, "child final answer", message.FinishReasonEndTurn)

	delivered := make(chan AsyncCompletion, concurrentTryReleasers)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = &barrieredMessages{
		Service: env.messages,
		barrier: newRefreshBarrier(concurrentTryReleasers, refreshBarrierWindow),
	}

	// Hold the child non-terminal while the delegation parks, so the
	// park-site re-check cannot release the entry before the race is armed.
	stub := newBusyStubAgent()
	stub.setBusy(true)
	coord.currentAgent = stub
	parkDelegation(t, coord, child.ID, "child yielded: gate still running")
	require.True(t, coord.subAgentOutcomes.hasParked())
	require.Empty(t, drainDelegationNotices(delivered))

	// Open the terminal gate and fire the concurrent re-checks, exactly the
	// way the fallback ticker and the job-completed hook would collide.
	stub.setBusy(false)
	var wg sync.WaitGroup
	for i := 0; i < concurrentTryReleasers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			coord.subAgentOutcomes.tryRelease(child.ID)
		}()
	}
	wg.Wait()

	got := drainDelegationNotices(delivered)
	require.Len(t, got, 1,
		"concurrent re-checks must deliver the parked completion exactly once")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.Equal(t, parkedParentSession, got[0].SessionID)
	require.Equal(t, AgentToolName, got[0].ToolName)
	require.Contains(t, got[0].Content, "child final answer",
		"the single notice must carry the child's refreshed final text")
	require.False(t, coord.subAgentOutcomes.hasParked(),
		"the parked entry must be consumed by the release")
}

// TestSubAgentOutcome_CancelReleasesParkedOutcome pins property 4a: a cancel
// must release the parked notice as a cancellation rather than losing it,
// and must do so BEFORE asyncJobRegistry.cancelSession drops the job row —
// otherwise the notice would have nowhere to land and the parent would wait
// forever.
func TestSubAgentOutcome_CancelReleasesParkedOutcome(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })
	coord.currentAgent = &mockSessionAgent{}

	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
	parkDelegation(t, coord, parkedChildSession, "child yielded: long gate running")
	require.True(t, coord.subAgentOutcomes.hasParked())
	require.Empty(t, drainDelegationNotices(delivered))

	// Cancel on the PARENT session id: the delegations it spawned must be
	// released before cancelSession drops their job rows.
	coord.Cancel(parkedParentSession)

	got := drainDelegationNotices(delivered)
	require.Len(t, got, 1, "a cancel must still deliver exactly one notice")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.True(t, got[0].IsError)
	require.Equal(t, "sub-agent canceled", got[0].Content)
	require.False(t, coord.subAgentOutcomes.hasParked(),
		"the parked entry must be consumed by the cancel release")
}

// TestSubAgentOutcome_CancelSurvivesFinishedChildTurn pins the cancel path
// against a child that had already finished a turn with text — the common
// cancel, since a canceled child usually produced an end_turn turn first.
// The cancel path must NOT refresh the completion from the child's newest
// finished assistant message: that read would overwrite the cancellation
// with the child's last text and its non-error finish reason, delivering a
// canceled delegation to the parent as a SUCCESS.
func TestSubAgentOutcome_CancelSurvivesFinishedChildTurn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "canceled-child")
	require.NoError(t, err)

	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = env.messages
	coord.currentAgent = &mockSessionAgent{}

	// The child's last turn finished cleanly with text, which is what
	// refreshSubAgentCompletion would fold into the completion.
	writeParkedChildTurn(t, env, child.ID, "child final answer", message.FinishReasonEndTurn)

	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
	parkDelegation(t, coord, child.ID, "child yielded: long gate running")
	require.True(t, coord.subAgentOutcomes.hasParked())
	require.Empty(t, drainDelegationNotices(delivered))

	// Cancel on the CHILD session id: every delegation that ran in it is
	// released as a cancellation, not as the child's last turn.
	coord.releaseSubAgentOutcomesForChildCancel(child.ID)

	got := drainDelegationNotices(delivered)
	require.Len(t, got, 1, "a cancel must still deliver exactly one notice")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.True(t, got[0].IsError,
		"a canceled delegation must reach the parent as an error, not a success")
	require.Equal(t, subAgentOutcomeCancelledText, got[0].Content,
		"the cancel body must survive the child's last finished turn")
	require.NotContains(t, got[0].Content, "child final answer",
		"the child's own text must never masquerade as a canceled outcome")
	require.False(t, coord.subAgentOutcomes.hasParked(),
		"the parked entry must be consumed by the cancel release")
}

// TestSubAgentOutcome_ResumeAfterNoticeDoesNotReemit pins property 4b: once a
// delegation's notice has been delivered, resuming the same child session
// (the resume_session_id path, or `rush run --session <id>`) parks a SECOND
// entry under its own tool call id and releases that one — it must never
// replay the already-delivered parent call.
func TestSubAgentOutcome_ResumeAfterNoticeDoesNotReemit(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "resumed-child")
	require.NoError(t, err)

	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = env.messages

	writeParkedChildTurn(t, env, child.ID, "first delegation done", message.FinishReasonEndTurn)

	// Delegation A: the child yields with owned async work outstanding.
	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
	parkDelegation(t, coord, child.ID, "yield A")
	require.True(t, coord.subAgentOutcomes.hasParked())
	require.Empty(t, drainDelegationNotices(delivered))

	// The child's owned job drains, which releases delegation A exactly
	// once.
	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID: child.ID, ToolCallID: parkedChildJob, ToolName: "bash", Content: "gate ok",
	})
	first := drainDelegationNotices(delivered)
	require.Len(t, first, 1, "delegation A must be delivered exactly once")
	require.Equal(t, parkedParentCall, first[0].ToolCallID)
	require.Contains(t, first[0].Content, "first delegation done")

	// A later turn on the same child (a resume) produces a second, final
	// answer.
	writeParkedChildTurn(t, env, child.ID, "second delegation done", message.FinishReasonEndTurn)

	// Delegation B: the same child, resumed, yields again. The child is now
	// idle, so this parks and releases at once — exactly one NEW notice for
	// the new tool call, and nothing replayed for the old one.
	const secondParentCall = "parent-call-2"
	require.NoError(t, coord.asyncJobs.start(parkedParentSession, secondParentCall, false, func() {}))
	coord.asyncJobs.acknowledged(parkedParentSession, secondParentCall)
	wrapped := &asyncTool{
		inner:       newYieldedInnerTool(AgentToolName, "yield B"),
		coordinator: coord,
		name:        AgentToolName,
	}
	ctx := WithCallOrigin(t.Context(), message.OriginWeb)
	wrapped.run(ctx, func() {}, parkedParentSession, child.ID, fantasy.ToolCall{
		ID: secondParentCall, Name: AgentToolName, Input: `{}`,
	})

	second := drainDelegationNotices(delivered)
	require.Len(t, second, 1, "a resumed child must produce exactly one NEW notice")
	require.Equal(t, secondParentCall, second[0].ToolCallID)
	require.Contains(t, second[0].Content, "second delegation done",
		"the resumed delegation's notice must carry the child's latest final text")
	require.False(t, coord.subAgentOutcomes.hasParked())
}

// TestSubAgentOutcome_BusyChildDefersReleaseUntilTurnEnds pins R1's negative
// busy gate: a re-check that lands while the child is mid-turn must defer
// rather than release, and the child's run end must then release it. Without
// the gate, a release could fire in the window where an auto-resume turn had
// claimed the session but had not yet registered its next async job.
func TestSubAgentOutcome_BusyChildDefersReleaseUntilTurnEnds(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "busy-child")
	require.NoError(t, err)

	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = env.messages
	writeParkedChildTurn(t, env, child.ID, "child final answer", message.FinishReasonEndTurn)

	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
	parkDelegation(t, coord, child.ID, "child yielded")
	require.True(t, coord.subAgentOutcomes.hasParked())

	// The child's job drains while the child is MID-TURN on its own
	// auto-resume. The release must be deferred, not emitted.
	stub := newBusyStubAgent()
	stub.setBusy(true)
	coord.currentAgent = stub
	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID: child.ID, ToolCallID: parkedChildJob, ToolName: "bash", Content: "gate ok",
	})
	require.Empty(t, drainDelegationNotices(delivered),
		"a re-check that lands mid-turn must defer the release")
	require.True(t, coord.subAgentOutcomes.hasParked())

	// Trigger (iv): the child's run ends. Now the gate opens and the notice
	// is delivered, exactly once.
	stub.setBusy(false)
	coord.noteSubAgentChildRunEnded(child.ID)

	got := drainDelegationNotices(delivered)
	require.Len(t, got, 1)
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.Contains(t, got[0].Content, "child final answer")
	require.False(t, coord.subAgentOutcomes.hasParked())
}

// writeParkedChildTurn records a finished assistant turn on a child session,
// which is what refreshSubAgentCompletion reads at release time.
func writeParkedChildTurn(t *testing.T, env fakeEnv, sessionID, text string, reason message.FinishReason) {
	t.Helper()
	row, err := env.messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	row.AddFinish(reason, "", "")
	require.NoError(t, env.messages.Update(t.Context(), row))
}

// busyStubAgent is a SessionAgent stub whose only meaningful behavior is a
// settable IsSessionBusy, used to drive subAgentWorkTerminal's negative busy
// gate.
type busyStubAgent struct {
	mockSessionAgent
	mu   sync.Mutex
	busy bool
}

func newBusyStubAgent() *busyStubAgent {
	return &busyStubAgent{}
}

func (s *busyStubAgent) IsSessionBusy(string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

func (s *busyStubAgent) setBusy(busy bool) {
	s.mu.Lock()
	s.busy = busy
	s.mu.Unlock()
}

// barrieredMessages is a message.Service whose List blocks on a barrier
// until every concurrent releaser has arrived inside the refresh read, or
// the barrier's safety window expires. It widens the window between
// nextReleasable handing out an entry and that entry's latch being flipped —
// the DB read refreshSubAgentCompletion performs OUTSIDE the registry mutex
// — so a late latch is caught deterministically rather than by race luck.
type barrieredMessages struct {
	message.Service
	barrier *refreshBarrier
}

func (m *barrieredMessages) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	m.barrier.wait()
	return m.Service.List(ctx, sessionID)
}

// refreshBarrier opens once want arrivals have accumulated, or after window
// has elapsed, whichever comes first. The timeout keeps a releaser that
// legitimately lost the race — and therefore never arrives — from wedging
// the test that armed the barrier.
type refreshBarrier struct {
	want int

	mu     sync.Mutex
	seen   int
	opened bool
	openCh chan struct{}
}

func newRefreshBarrier(want int, window time.Duration) *refreshBarrier {
	b := &refreshBarrier{want: want, openCh: make(chan struct{})}
	time.AfterFunc(window, b.open)
	return b
}

// wait blocks the caller until the barrier opens.
func (b *refreshBarrier) wait() {
	b.mu.Lock()
	if b.opened {
		b.mu.Unlock()
		return
	}
	b.seen++
	complete := b.seen >= b.want
	b.mu.Unlock()
	if complete {
		b.open()
		return
	}
	<-b.openCh
}

func (b *refreshBarrier) open() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.opened {
		return
	}
	b.opened = true
	close(b.openCh)
}
