package agent

// Delegation half of workLedger: parking a delegated sub-agent's (`agent`/
// `agentic_fetch`) completion until the child's own async work is terminal.
// Ports every scenario from the former subagent_outcome_test.go onto the
// merged workLedger API (park+claim+emit -> armDelegation+recheckChild;
// tryRelease -> recheckChild; two registries -> one), and adds the new
// cancel/recheck race coverage from docs/plans/2026-09-27-async-phase1-spec.md
// §4.2b.
//
// The five properties the former registry's fix held, still pinned here:
//
//  1. no notice is emitted while the child still owns async work;
//  2. exactly one notice is emitted, once that work is terminal;
//  3. a failed child turn is delivered as a failure, not a success;
//  4. a cancel releases the armed notice instead of losing it, and a
//     resumed child never replays an already-delivered notice;
//  5. that single notice survives CONCURRENT re-checks, and a cancel
//     survives a child that had already finished a turn with text, and a
//     concurrent cancel-vs-recheck race for the SAME armed delegation
//     delivers exactly one COHERENT outcome (new: §4.2b).

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
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

// newParkedOutcomeCoordinator builds the smallest coordinator that can arm
// and release a delegated sub-agent outcome: a bare workLedger wired to
// coord, with no currentAgent (so childScopeDrained's negative busy gate
// reads false) and no messages service (so a release keeps the completion
// captured at arm time). Tests that need a refreshed completion or a busy
// gate set those fields afterwards.
func newParkedOutcomeCoordinator(t *testing.T, onWebDone func(AsyncCompletion)) *coordinator {
	coord := &coordinator{}
	coord.asyncJobs = newWorkLedger(onWebDone)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord
	return coord
}

// newParkedOutcomeCoordinatorWithMessages is newParkedOutcomeCoordinator plus
// a real message.Service on the SAME connection, needed by
// simulateChildReaction (B3/C6 fix follow-up, docs/reviews/2026-09-29-async-
// phase4-round1.md): childScopeDrained now also checks cross-process
// reaction debt (childScopeOpenAcrossProcesses -> ScopeOpen), which requires
// an actual pull + a real reacted=1 write to ever clear -- tests that need
// the delegation to release past a child's OWN owned-job completion must
// simulate that reaction explicitly, not just the completion.
func newParkedOutcomeCoordinatorWithMessages(t *testing.T, onWebDone func(AsyncCompletion)) (*coordinator, message.Service) {
	t.Helper()
	coord := &coordinator{}
	coord.asyncJobs = newWorkLedger(onWebDone)
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	coord.asyncJobs.store = store
	coord.asyncJobs.coord = coord
	messages := message.NewService(db.New(conn))
	coord.messages = messages
	return coord, messages
}

// simulateChildReactionWithoutTrigger plays out, directly against the
// store, the DB-write half of what a real child turn does once it wakes
// over a pulled notice: pull the job's notice into history (delivery
// pending -> done) and mark it reacted=1 together with a synthesized final
// assistant message of the given text/reason (persistStepFinish's own
// atomic write, doc sec.3.4). It deliberately does NOT re-drive
// recheckChild -- callers that want to test a release trigger OTHER than
// the in-process "child run ended" one (e.g. the 60s RecheckPass fallback,
// coordinator_recheck_child_test.go) call this instead of
// simulateChildReaction. Callers pass the SAME text their own assertions
// expect refreshSubAgentCompletion to read back, since this message becomes
// the child's new latest finished assistant message.
func simulateChildReactionWithoutTrigger(t *testing.T, coord *coordinator, messages message.Service, childSessionID, text string, reason message.FinishReason) {
	t.Helper()
	ctx := context.Background()
	_, err := coord.asyncJobs.store.PullJobNotices(ctx, messages, childSessionID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	reactionMsg, err := messages.Create(ctx, childSessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: text}, message.Finish{Reason: reason}},
	})
	require.NoError(t, err)
	require.NoError(t, coord.asyncJobs.store.MarkReactedWithMessageUpdate(ctx, messages, childSessionID, reactionMsg))
}

// simulateChildReaction is simulateChildReactionWithoutTrigger plus the
// SECOND onSessionIdleHook release a real reacting turn's own end would
// trigger (noteSubAgentChildRunEnded) -- the common case.
func simulateChildReaction(t *testing.T, coord *coordinator, messages message.Service, childSessionID, text string, reason message.FinishReason) {
	t.Helper()
	simulateChildReactionWithoutTrigger(t, coord, messages, childSessionID, text, reason)
	coord.noteSubAgentChildRunEnded(childSessionID)
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
	_, _, err := coord.asyncJobs.Start(parkedParentSession, parkedParentCall, "", AgentToolName, childSession, false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged(parkedParentSession, parkedParentCall)

	wrapped := &asyncTool{
		inner:       newYieldedInnerTool(AgentToolName, childYields),
		coordinator: coord,
		name:        AgentToolName,
	}
	ctx := WithCallOrigin(t.Context(), message.OriginWeb)
	wrapped.run(ctx, func() {}, parkedParentSession, childSession, fantasy.ToolCall{
		ID: parkedParentCall, Name: AgentToolName, Input: `{}`,
	}, false)
}

// startChildOwnedJob registers an async job the CHILD session owns, and
// acknowledges it exactly the way agent_turn_stream.go's onToolResult does
// once the child's "Async ... job started" tool result message is persisted.
// The acknowledgement matters: deliverLocked refuses to release an
// unacknowledged row even after its result is set -- correct in production
// (the turn is not durably done yet) but irrelevant to a test that only
// wants the child's owned job to reach a terminal state.
func startChildOwnedJob(t *testing.T, l *workLedger, sessionID, toolCallID string, cli bool) {
	t.Helper()
	_, _, err := l.Start(sessionID, toolCallID, "", "bash", "", cli, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(sessionID, toolCallID)
}

// finishChildJob completes a job the child owns and plays out the turn that
// completion wakes: the child itself must receive its own result, and only
// that woken run's end (noteSubAgentChildRunEnded's re-check) may release
// the armed delegation.
func finishChildJob(t *testing.T, coord *coordinator, delivered chan AsyncCompletion, completion AsyncCompletion) {
	t.Helper()
	coord.asyncJobs.finish(completion.SessionID, completion.ToolCallID, jobResult{content: completion.Content, isError: completion.IsError})
	select {
	case got := <-delivered:
		require.Equal(t, completion.SessionID, got.SessionID, "the child's own job result must wake the child")
		require.Equal(t, completion.ToolCallID, got.ToolCallID)
	default:
		t.Fatal("the child's own job result must wake the child")
	}
	coord.noteSubAgentChildRunEnded(completion.SessionID)
}

// TestWorkLedger_NoFinishedNoticeWhileChildOwnedJobsPending pins property 1:
// the parent is NOT told the delegation finished while the child still owns
// an async job.
func TestWorkLedger_NoFinishedNoticeWhileChildOwnedJobsPending(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(t, func(completion AsyncCompletion) { delivered <- completion })

	// The child's own async tool work is still in flight.
	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, false)

	parkDelegation(t, coord, parkedChildSession, "child yielded: async checks still running")

	require.True(t, coord.asyncJobs.hasParked(),
		"the delegation must be armed while the child still owns async work")
	require.True(t, coord.asyncJobs.running(parkedChildSession),
		"precondition: the child's own async job must still be running")
	require.Empty(t, drainCompletions(delivered),
		"no completion notice may reach the parent while the child's own async work is outstanding")
}

// TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain pins property 2:
// exactly one notice, delivered only once the child's owned async work is
// terminal, carrying the child's own final text.
func TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 4)
	coord, messages := newParkedOutcomeCoordinatorWithMessages(t, func(completion AsyncCompletion) { delivered <- completion })

	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
	parkDelegation(t, coord, parkedChildSession, "child yielded: async checks still running")
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered))

	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID:  parkedChildSession,
		ToolCallID: parkedChildJob,
		ToolName:   "bash",
		Content:    "gate ok",
	})
	// B3/C6 fix follow-up: childScopeDrained now also waits for the child's
	// OWN cross-process reaction debt to clear -- simulate the child's own
	// subsequent turn reacting to its just-finished job before the
	// delegation can actually release.
	simulateChildReaction(t, coord, messages, parkedChildSession, "child reacted", message.FinishReasonEndTurn)

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "exactly one final notice must reach the parent")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.Equal(t, parkedParentSession, got[0].SessionID)
	require.Equal(t, AgentToolName, got[0].ToolName)
	require.False(t, got[0].IsError)
	require.Contains(t, got[0].Content, "child reacted",
		"the notice must carry the child's own LATEST (post-reaction) text, not its pre-reaction yield -- "+
			"a live messages service is wired, so refreshSubAgentCompletion re-reads it instead of using the arm-time capture")
	require.False(t, coord.asyncJobs.hasParked(),
		"the armed entry must be consumed by the release")
}

// TestWorkLedger_FailedChildJobDeliveredOnceAsFailure pins property 3: when
// the child's last finished turn errored, the single notice the parent
// receives is a FAILURE, not a success.
func TestWorkLedger_FailedChildJobDeliveredOnceAsFailure(t *testing.T) {
	t.Parallel()
	const failedChildSession = "failed-child"
	delivered := make(chan AsyncCompletion, 4)
	coord, messages := newParkedOutcomeCoordinatorWithMessages(t, func(completion AsyncCompletion) { delivered <- completion })

	startChildOwnedJob(t, coord.asyncJobs, failedChildSession, parkedChildJob, true)
	parkDelegation(t, coord, failedChildSession, "child yielded: gate still running")
	require.True(t, coord.asyncJobs.hasParked())

	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID:  failedChildSession,
		ToolCallID: parkedChildJob,
		ToolName:   "bash",
		Content:    "exit 2",
		IsError:    true,
	})
	// B3/C6 fix follow-up: same cross-process reaction-debt wait as
	// TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain -- the child's own
	// reacting turn here is the one whose final message IS the failure text
	// (a real auto-resume turn surfacing its own job's error ends exactly
	// this way, FinishReasonError).
	simulateChildReaction(t, coord, messages, failedChildSession, "the gate failed: exit 2", message.FinishReasonError)

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "exactly one notice, even for a failed child turn")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.True(t, got[0].IsError, "a failed child turn must reach the parent as a failure")
	require.Contains(t, got[0].Content, "the gate failed")
	require.Contains(t, FormatAsyncCompletion(got[0]), "failed",
		"the delivered notice must render the failed status, not a success")
}

// concurrentRecheckers is how many re-check triggers fire at once in
// TestWorkLedger_ConcurrentRecheckDeliversOnce -- the shape of the real
// collision between the safety-net ticker and a job-completion trigger.
const concurrentRecheckers = 8

// refreshBarrierWindow is how long barrieredMessages holds a refresh read
// open before its barrier opens on its own. It only has to outlast the
// launch of concurrentRecheckers goroutines, which takes microseconds, and
// keeps the fixed path's single claimed releaser from waiting longer than
// that.
const refreshBarrierWindow = 250 * time.Millisecond

// TestWorkLedger_ConcurrentRecheckDeliversOnce pins the exactly-once
// guarantee against CONCURRENT re-checks: the safety-net ticker and a
// job-completion trigger can both walk the same armed delegation at the same
// time, and only one may deliver the completion.
//
// The window is widened deterministically instead of by luck:
// barrieredMessages holds refreshSubAgentCompletion's DB read until every
// concurrent releaser has arrived inside it.
func TestWorkLedger_ConcurrentRecheckDeliversOnce(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "race-child")
	require.NoError(t, err)
	writeParkedChildTurn(t, env, child.ID, "child final answer", message.FinishReasonEndTurn)

	delivered := make(chan AsyncCompletion, concurrentRecheckers)
	coord := newParkedOutcomeCoordinator(t, func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = &barrieredMessages{
		Service: env.messages,
		barrier: newRefreshBarrier(concurrentRecheckers, refreshBarrierWindow),
	}

	// Hold the child non-terminal while the delegation arms, so the arm-site
	// re-check cannot release the entry before the race is armed.
	stub := newBusyStubAgent()
	stub.setBusy(true)
	coord.currentAgent = stub
	parkDelegation(t, coord, child.ID, "child yielded: gate still running")
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered))

	// Open the terminal gate and fire the concurrent re-checks, exactly the
	// way the fallback ticker and a completion trigger would collide.
	stub.setBusy(false)
	var wg sync.WaitGroup
	for i := 0; i < concurrentRecheckers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			coord.asyncJobs.recheckChild(child.ID)
		}()
	}
	wg.Wait()

	got := drainCompletions(delivered)
	require.Len(t, got, 1,
		"concurrent re-checks must deliver the armed completion exactly once")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.Equal(t, parkedParentSession, got[0].SessionID)
	require.Equal(t, AgentToolName, got[0].ToolName)
	require.Contains(t, got[0].Content, "child final answer",
		"the single notice must carry the child's refreshed final text")
	require.False(t, coord.asyncJobs.hasParked(),
		"the armed entry must be consumed by the release")
}

// TestWorkLedger_CancelReleasesArmedDelegation pins property 4a: a cancel
// must release the armed notice as a cancellation rather than losing it, and
// must do so as part of the SAME cancelSession call that drops the owning
// job row -- otherwise the notice would have nowhere to land.
func TestWorkLedger_CancelReleasesArmedDelegation(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(t, func(completion AsyncCompletion) { delivered <- completion })
	coord.currentAgent = &mockSessionAgent{}

	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
	parkDelegation(t, coord, parkedChildSession, "child yielded: long gate running")
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered))

	// Cancel on the PARENT session id: the delegations it spawned must be
	// released before their job rows are dropped.
	coord.Cancel(parkedParentSession)

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "a cancel must still deliver exactly one notice")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.True(t, got[0].IsError)
	require.Equal(t, "sub-agent canceled", got[0].Content)
	require.False(t, coord.asyncJobs.hasParked(),
		"the armed entry must be consumed by the cancel release")
}

// TestWorkLedger_CancelSurvivesFinishedChildTurn pins the cancel path against
// a child that had already finished a turn with text -- the common cancel,
// since a canceled child usually produced an end_turn turn first. The cancel
// path must NOT refresh the completion from the child's newest finished
// assistant message: that read would overwrite the cancellation with the
// child's last text and its non-error finish reason, delivering a canceled
// delegation to the parent as a SUCCESS.
//
// This also exercises cancelSession's other required property: the child's
// OWN unrelated async job (parkedChildJob, still running here) is silently
// dropped by the same cancelSession(child.ID) call, exactly like today's
// asyncJobRegistry.cancelSession for a plain job -- it must NOT also surface
// as a second, unexpected notice.
func TestWorkLedger_CancelSurvivesFinishedChildTurn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "canceled-child")
	require.NoError(t, err)

	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(t, func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = env.messages
	coord.currentAgent = &mockSessionAgent{}

	// The child's last turn finished cleanly with text, which is what
	// refreshSubAgentCompletion would fold into the completion.
	writeParkedChildTurn(t, env, child.ID, "child final answer", message.FinishReasonEndTurn)

	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
	parkDelegation(t, coord, child.ID, "child yielded: long gate running")
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered))

	// Cancel on the CHILD session id directly: every delegation that ran in
	// it is released as a cancellation, not as the child's last turn, and
	// the child's own unrelated job is dropped without a notice.
	coord.asyncJobs.cancelSession(child.ID)

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "a cancel must still deliver exactly one notice")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.True(t, got[0].IsError,
		"a canceled delegation must reach the parent as an error, not a success")
	require.Equal(t, subAgentOutcomeCancelledText, got[0].Content,
		"the cancel body must survive the child's last finished turn")
	require.NotContains(t, got[0].Content, "child final answer",
		"the child's own text must never masquerade as a canceled outcome")
	require.False(t, coord.asyncJobs.hasParked(),
		"the armed entry must be consumed by the cancel release")
	require.False(t, coord.asyncJobs.running(child.ID),
		"the child's own unrelated job must be dropped too, not left dangling")
}

// TestWorkLedger_ResumeAfterNoticeDoesNotReemit pins property 4b: once a
// delegation's notice has been delivered, resuming the same child session
// parks a SECOND entry under its own tool call id and releases that one --
// it must never replay the already-delivered parent call.
func TestWorkLedger_ResumeAfterNoticeDoesNotReemit(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "resumed-child")
	require.NoError(t, err)

	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(t, func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = env.messages

	// Delegation A: the child yields with owned async work outstanding.
	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
	parkDelegation(t, coord, child.ID, "yield A")
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered))

	// The child's owned job drains, which releases delegation A exactly
	// once, once the child's own reacting turn clears the cross-process
	// debt (B3/C6 fix follow-up) -- that reacting turn's own final message
	// IS "first delegation done", the text refreshSubAgentCompletion reads
	// back below.
	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID: child.ID, ToolCallID: parkedChildJob, ToolName: "bash", Content: "gate ok",
	})
	simulateChildReaction(t, coord, env.messages, child.ID, "first delegation done", message.FinishReasonEndTurn)
	first := drainCompletions(delivered)
	require.Len(t, first, 1, "delegation A must be delivered exactly once")
	require.Equal(t, parkedParentCall, first[0].ToolCallID)
	require.Contains(t, first[0].Content, "first delegation done")

	// A later turn on the same child (a resume) produces a second, final
	// answer.
	writeParkedChildTurn(t, env, child.ID, "second delegation done", message.FinishReasonEndTurn)

	// Delegation B: the same child, resumed, yields again. The child is now
	// idle, so this arms and releases at once -- exactly one NEW notice for
	// the new tool call, and nothing replayed for the old one.
	const secondParentCall = "parent-call-2"
	_, _, err = coord.asyncJobs.Start(parkedParentSession, secondParentCall, "", AgentToolName, child.ID, false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged(parkedParentSession, secondParentCall)
	wrapped := &asyncTool{
		inner:       newYieldedInnerTool(AgentToolName, "yield B"),
		coordinator: coord,
		name:        AgentToolName,
	}
	ctx := WithCallOrigin(t.Context(), message.OriginWeb)
	wrapped.run(ctx, func() {}, parkedParentSession, child.ID, fantasy.ToolCall{
		ID: secondParentCall, Name: AgentToolName, Input: `{}`,
	}, false)

	second := drainCompletions(delivered)
	require.Len(t, second, 1, "a resumed child must produce exactly one NEW notice")
	require.Equal(t, secondParentCall, second[0].ToolCallID)
	require.Contains(t, second[0].Content, "second delegation done",
		"the resumed delegation's notice must carry the child's latest final text")
	require.False(t, coord.asyncJobs.hasParked())
}

// TestWorkLedger_BusyChildDefersReleaseUntilTurnEnds pins the negative busy
// gate: a re-check that lands while the child is mid-turn must defer rather
// than release, and the child's run end must then release it.
func TestWorkLedger_BusyChildDefersReleaseUntilTurnEnds(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	child, err := env.sessions.Create(t.Context(), "busy-child")
	require.NoError(t, err)

	delivered := make(chan AsyncCompletion, 4)
	coord := newParkedOutcomeCoordinator(t, func(completion AsyncCompletion) { delivered <- completion })
	coord.messages = env.messages
	writeParkedChildTurn(t, env, child.ID, "child final answer", message.FinishReasonEndTurn)

	startChildOwnedJob(t, coord.asyncJobs, child.ID, parkedChildJob, true)
	parkDelegation(t, coord, child.ID, "child yielded")
	require.True(t, coord.asyncJobs.hasParked())

	// The child's job drains while the child is MID-TURN on its own
	// auto-resume. The release must be deferred, not emitted.
	stub := newBusyStubAgent()
	stub.setBusy(true)
	coord.currentAgent = stub
	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID: child.ID, ToolCallID: parkedChildJob, ToolName: "bash", Content: "gate ok",
	})
	require.Empty(t, drainCompletions(delivered),
		"a re-check that lands mid-turn must defer the release")
	require.True(t, coord.asyncJobs.hasParked())

	// Trigger (iv): the child's run ends. Now the gate opens and the notice
	// is delivered, exactly once, once the child's own reacting turn also
	// clears the cross-process debt (B3/C6 fix follow-up) -- simulateChildReaction
	// both writes that reacting turn's final message and re-drives the
	// child-run-ended re-check.
	stub.setBusy(false)
	simulateChildReaction(t, coord, env.messages, child.ID, "child final answer", message.FinishReasonEndTurn)

	got := drainCompletions(delivered)
	require.Len(t, got, 1)
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.Contains(t, got[0].Content, "child final answer")
	require.False(t, coord.asyncJobs.hasParked())
}

// TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce is new coverage
// (spec §4.2b): a concurrent recheckChild (readiness true) and cancelSession
// for the SAME armed delegation must deliver exactly once, and that one
// delivery must be COHERENT -- either the recheck's success text or the
// cancel's text, never a mix of both outcomes' fields.
//
// Run repeatedly: which of the two wins the mutex is scheduler-dependent.
//
// Revert-check performed: removed deliverLocked's `!present || current !=
// job` guard (so it would deliver from stale/already-removed job state
// unconditionally once terminal+announced, the ad-hoc-condition shape the
// spec's revert-check describes). Ran this test: FAILED with `require.Len(t,
// got, 1)` seeing 2 deliveries within the first handful of the 30
// iterations (recheckChild's own transitionToTerminal lost the CAS but its
// unconditional deliverLocked call still re-delivered the already-canceled
// job). Restored the presence guard; `go build ./...` and 30/30 re-run
// iterations passed.
func TestWorkLedger_ConcurrentRecheckAndCancelDeliversOnce(t *testing.T) {
	t.Parallel()
	for i := 0; i < 30; i++ {
		delivered := make(chan AsyncCompletion, 8)
		coord := newParkedOutcomeCoordinator(t, func(c AsyncCompletion) { delivered <- c })

		// Keep the child's scope open until the delegation is armed, so
		// arming does not resolve synchronously.
		startChildOwnedJob(t, coord.asyncJobs, "child-race", "child-job", false)

		_, _, err := coord.asyncJobs.Start("parent-race", "call-race", "", AgentToolName, "child-race", false, false, nil, nil)
		require.NoError(t, err)
		coord.asyncJobs.acknowledged("parent-race", "call-race")
		coord.asyncJobs.armDelegation("parent-race", "call-race", jobResult{content: "child final answer"})
		require.True(t, coord.asyncJobs.hasParked())

		// Drain the child's own scope so childScopeDrained becomes true,
		// WITHOUT going through recheckChild yet -- finish() only delivers
		// the child's own job, it does not itself walk byChild.
		coord.asyncJobs.finish("child-race", "child-job", jobResult{content: "child job ok"})
		drainCompletions(delivered) // discard the child's own job notice

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			coord.asyncJobs.recheckChild("child-race")
		}()
		go func() {
			defer wg.Done()
			<-start
			coord.asyncJobs.cancelSession("parent-race")
		}()
		close(start)
		wg.Wait()

		got := drainCompletions(delivered)
		require.Len(t, got, 1, "exactly one delivery for the armed delegation (iteration %d)", i)
		require.Equal(t, "call-race", got[0].ToolCallID)
		if got[0].IsError {
			require.Equal(t, subAgentOutcomeCancelledText, got[0].Content,
				"a canceled outcome must never mix in the recheck's success text (iteration %d)", i)
		} else {
			require.Equal(t, "child final answer", got[0].Content,
				"a successful recheck outcome must never mix in the cancel's text (iteration %d)", i)
		}
		require.False(t, coord.asyncJobs.hasParked(), "iteration %d", i)
	}
}

// TestWorkLedger_ChildScopeClosesWithoutTicker is phase 3 step 0 (spec §3.3,
// §9). It was written and passed BEFORE the safety-net ticker was deleted
// (with subAgentOutcomeTickInterval forced to an hour, so a pass could not be
// attributed to it): the exact TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain
// scenario delivers SYNCHRONOUSLY through the ordinary triggers alone. That
// was the empirical evidence the ticker's later removal (this same package,
// same phase) was safe. The ticker is gone now, so there is nothing left to
// disable -- this test keeps the scenario as a permanent regression guard.
func TestWorkLedger_ChildScopeClosesWithoutTicker(t *testing.T) {
	delivered := make(chan AsyncCompletion, 4)
	coord, messages := newParkedOutcomeCoordinatorWithMessages(t, func(completion AsyncCompletion) { delivered <- completion })

	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
	parkDelegation(t, coord, parkedChildSession, "child yielded: async checks still running")
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered))

	finishChildJob(t, coord, delivered, AsyncCompletion{
		SessionID:  parkedChildSession,
		ToolCallID: parkedChildJob,
		ToolName:   "bash",
		Content:    "gate ok",
	})
	// B3/C6 fix follow-up: same cross-process reaction-debt wait as
	// TestWorkLedger_SingleFinalNoticeAfterChildJobsDrain.
	simulateChildReaction(t, coord, messages, parkedChildSession, "child reacted", message.FinishReasonEndTurn)

	got := drainCompletions(delivered)
	require.Len(t, got, 1,
		"delivery must happen synchronously through the ordinary triggers, "+
			"not the (deliberately disabled) safety-net ticker")
	require.Equal(t, parkedParentCall, got[0].ToolCallID)
	require.False(t, coord.asyncJobs.hasParked())
}

// TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation pins phase 3
// step 2's deletion (spec §4.4): arming a delegation must not spawn any
// additional long-lived goroutine (a ticker, if one still existed, would be
// exactly that). settle() re-reads NumGoroutine() until it stabilizes
// (Gosched between reads) rather than tolerating a fixed slack, since a
// tolerant "+1" margin would silently mask exactly the one-goroutine leak
// this test exists to catch.
//
// Revert-check performed: temporarily reintroduced a ticker-shaped leak --
// `go func() { t := time.NewTicker(time.Hour); <-t.C }()` right after
// `l.byChild[job.childSession] = append(...)` in armDelegation (mirroring
// where the real startTickerLocked() call used to sit) -- this test FAILED
// consistently (5/5 repeats, goroutine count growing by 1 each run and never
// settling back down). Removed the reintroduced line; re-ran 5/5, passed.
func TestWorkLedger_NoLingeringTickerGoroutineAfterArmDelegation(t *testing.T) {
	coord := newParkedOutcomeCoordinator(t, func(AsyncCompletion) {})
	coord.currentAgent = &mockSessionAgent{}

	settle := func() int {
		var n int
		for i := 0; i < 5; i++ {
			runtime.Gosched()
			n = runtime.NumGoroutine()
		}
		return n
	}
	before := settle()

	startChildOwnedJob(t, coord.asyncJobs, parkedChildSession, parkedChildJob, true)
	parkDelegation(t, coord, parkedChildSession, "child yielded: still running")
	require.True(t, coord.asyncJobs.hasParked())

	after := settle()
	require.LessOrEqual(t, after, before,
		"armDelegation must not leave behind a long-lived goroutine (before=%d after=%d)", before, after)

	// Release the child's own job so the ledger doesn't leak the armed entry
	// past this test.
	coord.asyncJobs.finish(parkedChildSession, parkedChildJob, jobResult{content: "done"})
	coord.noteSubAgentChildRunEnded(parkedChildSession)
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
// settable IsSessionBusy, used to drive childScopeDrained's negative busy
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
// the barrier's safety window expires. It widens the window between a
// recheck finding an armed job and its latch being flipped -- the DB read
// refreshSubAgentCompletion performs OUTSIDE the ledger mutex -- so a late
// latch is caught deterministically rather than by race luck.
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
// legitimately lost the race -- and therefore never arrives -- from wedging
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
