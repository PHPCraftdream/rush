// Phase-4 step 3 coverage that complements drain_notice_pull_test.go:
// DUR-3d (a compaction step never pulls) and the Drain merge rule as wired
// through the real mailbox.submit busy path, not only mergeQueuedCall.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestCompaction_DoesNotPullNotices pins DUR-3d: a compaction stream
// (runSummarizeBody) runs its own PrepareStep and must leave a pending
// notice row untouched -- only a normal turn's start/step boundary pulls.
//
// Revert-check performed: added `a.pullPendingNotices(callContext,
// sessionID)` to runSummarizeBody's PrepareStep closure -- this test FAILED
// (row delivery "done", one BackgroundJobNotice message). Removed it; re-ran,
// passed.
func TestCompaction_DoesNotPullNotices(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		textFinishResponse(w, "summary text")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = asyncStore.Close(context.Background()) })
	ledger := newWorkLedger(nil)
	ledger.store = asyncStore

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
	})
	agent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "dur3d-compaction")
	require.NoError(t, err)
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "earlier work"}},
	})
	require.NoError(t, err)

	_, err = asyncStore.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, asyncStore.MarkAnnounced(ctx, sess.ID, "call-1"))
	_, err = asyncStore.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "call-1", State: "completed", ResultSummary: "job output", Wake: true,
	})
	require.NoError(t, err)

	require.NoError(t, agent.runSummarizeBody(ctx, sess.ID, nil, model, ""))

	row, err := asyncStore.Get(ctx, sess.ID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery, "compaction must never pull a notice")

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.False(t, m.BackgroundJobNotice, "no notice message may appear during compaction")
	}
}

// TestMailboxSubmit_DrainMergeRule drives the real busy path of
// mailbox.submit (not mergeQueuedCall directly): a Drain behind a busy owner
// queues once, a second Drain is dropped, and a user call then replaces the
// queued Drain -- one turn, not two.
//
// Revert-check performed: replaced `mergeQueuedCall(mb.submitted, call)` in
// submit with a plain append -- this test FAILED (queue length 2 after the
// second Drain). Restored; re-ran, passed.
func TestMailboxSubmit_DrainMergeRule(t *testing.T) {
	mb := &mailbox{state: mbOwned, current: generation{id: 1, cancel: func() {}}}

	owner, _ := mb.submit(newDrainCall(SessionAgentCall{SessionID: "s1"}), nil)
	require.False(t, owner)
	owner, _ = mb.submit(newDrainCall(SessionAgentCall{SessionID: "s1"}), nil)
	require.False(t, owner)
	mb.mu.Lock()
	require.Len(t, mb.submitted, 1, "at most one Drain may sit in the queue")
	require.True(t, mb.submitted[0].IsDrain)
	mb.mu.Unlock()

	owner, _ = mb.submit(SessionAgentCall{SessionID: "s1", Prompt: "hello"}, nil)
	require.False(t, owner)
	mb.mu.Lock()
	defer mb.mu.Unlock()
	require.Len(t, mb.submitted, 1, "the user call subsumes the queued Drain")
	require.False(t, mb.submitted[0].IsDrain)
	require.Equal(t, "hello", mb.submitted[0].Prompt)
}

// TestNewDrainCall_ResetsInheritedFlags pins the "never inherited" rule for
// the single constructor: a base carrying another call's prompt/notice/
// existing-message/queue markers comes out clean.
func TestNewDrainCall_ResetsInheritedFlags(t *testing.T) {
	base := SessionAgentCall{
		SessionID: "s1", Prompt: "old prompt", NoticeKind: "supervision",
		ExistingMessageID: "m1", FromDurableQueue: true, FailIfSessionBusy: true,
		OnUserMessageCreated: func(string) {},
	}
	call := newDrainCall(base)
	require.True(t, call.IsDrain)
	require.Empty(t, call.Prompt)
	require.Empty(t, call.NoticeKind)
	require.Empty(t, call.ExistingMessageID)
	require.False(t, call.FromDurableQueue)
	require.False(t, call.FailIfSessionBusy)
	require.Nil(t, call.OnUserMessageCreated)
	require.True(t, call.AutoResumed)
	require.True(t, call.BackgroundJobNotice)

	// A call built from the driver template must not carry IsDrain forward.
	driver := subAgentDriver{call: SessionAgentCall{SessionID: "child", Prompt: "task"}}
	require.False(t, driver.callFor("next").IsDrain)
}

// TestSpliceCarried_KeepsOriginalPositions pins the carry-forward rule for
// ANY mid-turn splice (user inject or pulled notice -- both flow through the
// same stepSplices list): a splice made at step 2's boundary sits at the
// same position in steps 3 and 4, and a step-3 splice lands after it.
//
// Revert-check performed: made spliceCarried append every carried splice at
// the end of base -- this test FAILED (step 3 order "p r1 r2 inject
// notice"). Restored; re-ran, passed.
func TestSpliceCarried_KeepsOriginalPositions(t *testing.T) {
	u := fantasy.NewUserMessage
	texts := func(msgs []fantasy.Message) []string {
		out := make([]string, 0, len(msgs))
		for _, m := range msgs {
			part, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0])
			require.True(t, ok)
			out = append(out, part.Text)
		}
		return out
	}

	// Step 2: base = prompt + step-1 response; an inject lands at the end.
	base2 := []fantasy.Message{u("p"), u("r1")}
	step2 := spliceCarried(base2, nil, []fantasy.Message{u("inject")})
	require.Equal(t, []string{"p", "r1", "inject"}, texts(step2))
	carried := []carriedSplice{{pos: len(base2), msgs: []fantasy.Message{u("inject")}}}

	// Step 3: fantasy's base no longer contains the inject; a notice arrives.
	base3 := []fantasy.Message{u("p"), u("r1"), u("r2")}
	step3 := spliceCarried(base3, carried, []fantasy.Message{u("notice")})
	require.Equal(t, []string{"p", "r1", "inject", "r2", "notice"}, texts(step3))
	carried = append(carried, carriedSplice{pos: len(base3), msgs: []fantasy.Message{u("notice")}})

	// Step 4: both carried splices keep their positions.
	base4 := []fantasy.Message{u("p"), u("r1"), u("r2"), u("r3")}
	step4 := spliceCarried(base4, carried, nil)
	require.Equal(t, []string{"p", "r1", "inject", "r2", "notice", "r3"}, texts(step4))
	require.Len(t, base4, 4, "base must not be mutated")
}

// TestPullPendingNotices_RecordsProgressExceptSupervision pins doc sec.3.4:
// moving a notice into history resets the supervision countdown, except a
// supervision check-in itself.
//
// Revert-check performed: removed the recordProgress loop from
// pullPendingNotices -- this test FAILED (tickCount stayed 2 after the
// bg-shell notice was pulled). Restored; re-ran, passed.
func TestPullPendingNotices_RecordsProgressExceptSupervision(t *testing.T) {
	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ledger := newWorkLedger(nil)
	ledger.store = store
	ledger.supervision = newSupervisionRegistry()
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions, Messages: env.messages, DataDirectory: env.workingDir, AsyncJobs: ledger,
	})
	agent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "pull-progress")
	require.NoError(t, err)
	// A running job keeps the supervision notice deliverable (not voided).
	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)

	cfg := SupervisionConfig{Enabled: true, Interval: time.Hour, MaxInterval: 4 * time.Hour, MaxNoProgress: 6}
	ledger.supervision.byRoot[sess.ID] = &supervisionState{rootSessionID: sess.ID, cfg: cfg, interval: 2 * time.Hour, tickCount: 2, generation: 1}
	tickCount := func() int {
		ledger.supervision.mu.Lock()
		defer ledger.supervision.mu.Unlock()
		return ledger.supervision.byRoot[sess.ID].tickCount
	}

	require.NoError(t, store.InsertSessionNotice(ctx, sess.ID, session.NoticeKindSupervision, "check-in", true, ""))
	pulled := agent.pullPendingNotices(ctx, sess.ID)
	require.Len(t, pulled, 1)
	require.Equal(t, 2, tickCount(), "a supervision check-in must not reset its own backoff")

	require.NoError(t, store.InsertSessionNotice(ctx, sess.ID, session.NoticeKindBGShellDone, "shell done", true, ""))
	pulled = agent.pullPendingNotices(ctx, sess.ID)
	require.Len(t, pulled, 1)
	require.Zero(t, tickCount(), "any other notice moving into history is progress")
}

// TestDrain_CommittedRetryReachesProviderWithNothingToPull: the runTurn side
// of the retry rule -- a committed Drain retry reaches the provider even
// though its own turn-start pull finds nothing.
//
// Revert-check performed: removed `&& !call.drainTurnCommitted` from
// runTurn's Drain gate -- this test FAILED (provider never called). Restored;
// re-ran, passed.
func TestDrain_CommittedRetryReachesProviderWithNothingToPull(t *testing.T) {
	t.Parallel()
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		textFinishResponse(w, "reacted")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ledger := newWorkLedger(nil)
	ledger.store = store
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "drain-committed-retry")
	require.NoError(t, err)
	// The notice the first attempt already pulled.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Async job call-1 (bash) finished."}},
		BackgroundJobNotice: true,
	})
	require.NoError(t, err)

	call := newDrainCall(SessionAgentCall{SessionID: sess.ID})
	call.drainTurnCommitted = true
	result, err := sa.Run(ctx, call)
	require.NoError(t, err)
	require.NotNil(t, result)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls, "a committed Drain retry must reach the provider")
}

// TestWebAsyncJob_EndToEnd_OneNoticeOneTurn drives the whole step-3 path for a
// non-CLI (web/child) session: a real ledger job finishes -> transition
// commits delivery='pending' -> onWebDone hint -> wakeSession submits a Drain
// to the session's driver -> the Drain's turn-start pull inserts the notice
// -> exactly one provider turn. The session ends with exactly ONE notice
// message and ONE assistant reply, and the row is 'done'.
//
// Revert-check performed: made notifyAsyncCompletion pass wake=false to
// wakeSession -- this test FAILED (no provider call, no notice message).
// Restored; re-ran, passed.
func TestWebAsyncJob_EndToEnd_OneNoticeOneTurn(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		textFinishResponse(w, "saw the job result")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry(), sessions: env.sessions, messages: env.messages}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = coord
	coord.asyncJobs = ledger
	t.Cleanup(ledger.close)

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
	})
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "web-async-e2e")
	require.NoError(t, err)
	coord.subAgentDrivers.register(sess.ID, subAgentDriver{agent: sa, call: SessionAgentCall{SessionID: sess.ID}})

	_, _, err = ledger.Start(sess.ID, "call-1", "{}", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	ledger.acknowledged(sess.ID, "call-1")
	ledger.finish(sess.ID, "call-1", jobResult{content: "job output"})

	require.Eventually(t, func() bool {
		mu.Lock()
		n := calls
		mu.Unlock()
		return n >= 1 && !sa.IsSessionBusy(sess.ID)
	}, 10*time.Second, 10*time.Millisecond, "the job's completion must wake exactly one Drain turn")

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	var notices, users, assistants int
	for _, m := range msgs {
		switch m.Role {
		case message.User:
			users++
			if m.BackgroundJobNotice {
				notices++
				require.Contains(t, m.FullText(), "Async job call-1 (bash) finished.")
			}
		case message.Assistant:
			assistants++
		}
	}
	require.Equal(t, 1, notices, "exactly one notice message")
	require.Equal(t, 1, users, "no other user-role row (no empty Drain prompt, no duplicate)")
	require.Equal(t, 1, assistants, "exactly one turn")
	mu.Lock()
	require.Equal(t, 1, calls)
	mu.Unlock()

	row, err := store.Get(ctx, sess.ID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "done", row.Delivery)
}

// TestReminderBeforeTail keeps a Drain turn's pulled notices as the final
// user messages: preparePrompt's trailing todo reminder moves in front of
// them. Non-reminder tails and k=0 are left alone.
//
// Revert-check performed: made reminderBeforeTail return history unchanged
// -- this test FAILED, and so did internal/app's
// TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult (the root's Drain
// request ended with the reminder, not the delegation's notice). Restored;
// re-ran, both passed.
func TestReminderBeforeTail(t *testing.T) {
	u := fantasy.NewUserMessage
	reminder := u("<system_reminder>todo list</system_reminder>")
	text := func(m fantasy.Message) string {
		part, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0])
		require.True(t, ok)
		return part.Text
	}

	got := reminderBeforeTail([]fantasy.Message{u("old"), u("notice-1"), u("notice-2"), reminder}, 2)
	require.Len(t, got, 4)
	require.Equal(t, []string{"old", reminder.Content[0].(fantasy.TextPart).Text, "notice-1", "notice-2"},
		[]string{text(got[0]), text(got[1]), text(got[2]), text(got[3])})

	plain := []fantasy.Message{u("old"), u("notice")}
	require.Equal(t, plain, reminderBeforeTail(plain, 1), "no trailing reminder: untouched")
	withReminder := []fantasy.Message{u("old"), reminder}
	require.Equal(t, withReminder, reminderBeforeTail(withReminder, 0), "k=0: untouched")
}

// TestNoTurnDrain_QueuedRealCallRunsNextInSameLoop pins B13: a no-turn Drain
// (accounted before its release) must not swallow a real call already queued
// behind it: drainOrReleaseMerged hands that call to the SAME Run() loop as
// its next turn.
//
// runTurnToolsSnapshotSeam fires synchronously inside runTurn, strictly
// before the notice pull / commit decision, so it deterministically queues
// the second call into the SAME mailbox generation without any timing race.
//
// Revert-check: making the no-turn branch release the mailbox without
// draining (dropping the queued call) leaves result nil and this test red.
func TestNoTurnDrain_QueuedRealCallRunsNextInSameLoop(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		textFinishResponse(w, "real turn reply")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ledger := newWorkLedger(nil)
	ledger.store = store

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
	})
	agent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "no-turn-marker")
	require.NoError(t, err)
	// A non-default title PLUS a pre-existing message keep needsTitle false
	// for the real call below (agent_turn.go's needsTitle is `len(msgs)==0 ||
	// title in {"", default}`, so either alone is not enough) -- otherwise a
	// concurrent title-generation call to the same probe server would inflate
	// the provider-request assertion.
	require.NoError(t, env.sessions.Rename(ctx, sess.ID, "already titled"))
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "prior turn"}},
	})
	require.NoError(t, err)
	// No debt at all: decideDrainTurn will take the no-turn branch.

	orig := runTurnToolsSnapshotSeam
	runTurnToolsSnapshotSeam = func() {
		// Land a real, prompted call in the mailbox's queue BEFORE this
		// turn's own decideDrainTurn/drainOrReleaseMerged run -- synchronous,
		// no race.
		agent.getMailbox(sess.ID).submit(SessionAgentCall{SessionID: sess.ID, Prompt: "are you there"}, nil)
		runTurnToolsSnapshotSeam = orig // fire once
	}
	t.Cleanup(func() { runTurnToolsSnapshotSeam = orig })

	drainCall := newDrainCall(SessionAgentCall{SessionID: sess.ID})
	result, err := sa.Run(ctx, drainCall)
	require.NoError(t, err)
	require.NotNil(t, result, "the queued real call must run as the loop's next turn")
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "the real call reached the provider exactly once")
}
