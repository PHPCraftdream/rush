// W5 acceptance oracles for the review sections 2c (timeout-notice
// formatting characterization -- CURRENT behavior, literal expected strings)
// and 2d (three supervision presentation gaps ACCEPTED REMOVED: no silence
// flagging from unknown activity, no repeated question text in supervision,
// no legacy idle-child own-jobs line -- while question delivery, node
// identity, liveness and scope closure are preserved).
//
// All tests bind the batch store directly; none read or set any
// environment variable.
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchNoticeFormatting_CharacterizesCurrentTerminalWording (2c) pins
// the CURRENT formatter wording byte-for-byte, via
// buildBatchNoticeMessageParams -> FormatAsyncCompletion.
func TestBatchNoticeFormatting_CharacterizesCurrentTerminalWording(t *testing.T) {
	t.Parallel()

	one := func(row session.BatchNoticeRow) string {
		p := buildBatchNoticeMessageParams(row)
		require.Len(t, p.Parts, 1)
		text, ok := p.Parts[0].(message.TextContent)
		require.True(t, ok)
		return text.Text
	}

	failed := one(session.BatchNoticeRow{ToolCallID: "w5-f", Kind: "task", State: "failed", OutcomeSummary: "boom"})
	require.Equal(t, "Async job w5-f () failed.\n\nboom", failed)

	// The literal `after 0s` is exactly the assertion W4 flips when
	// TimeoutSeconds becomes accurate (BatchNoticeRow gains the duration
	// from the selected task's TimeoutNs and
	// buildBatchNoticeMessageParams sets TimeoutSeconds); the flip replaces
	// this literal with e.g. "timed out after 42s". Unlike
	// batch_timeout_wake_parity_test.go, this does NOT feed
	// FormatAsyncCompletion the same zero value -- it pins current
	// observable wording.
	timedOut := one(session.BatchNoticeRow{ToolCallID: "w5-t", State: "timed_out", OutcomeSummary: "boom"})
	require.Equal(t, "Async job w5-t () timed out after 0s and was stopped. Partial output:\n\nboom", timedOut)

	interrupted := one(session.BatchNoticeRow{ToolCallID: "w5-i", State: "interrupted", Cause: "host_closing", OutcomeSummary: "boom"})
	require.Equal(t, "Async job w5-i () was interrupted: its host process ended before the job finished.\n\nboom", interrupted)

	// Characterization, not endorsement: cause=stop sets Stopped before the
	// Cancelled branch in buildBatchNoticeMessageParams, so the wording is
	// the stopped one; a future reclassification of stop-cause cancellation
	// flips this literal.
	cancelled := one(session.BatchNoticeRow{ToolCallID: "w5-c", State: "cancelled", Cause: "stop", OutcomeSummary: "boom"})
	require.Equal(t, "Async job w5-c () was stopped (job_kill). Partial output before the stop:\n\nboom", cancelled)

	completed := one(session.BatchNoticeRow{ToolCallID: "w5-x", Kind: "task", State: "completed", OutcomeSummary: "done!"})
	require.Equal(t, "Async job w5-x () finished.\n\ndone!", completed)

	texts := []string{failed, timedOut, interrupted, cancelled, completed}
	for i := 0; i < len(texts); i++ {
		for j := i + 1; j < len(texts); j++ {
			require.NotEqual(t, texts[i], texts[j],
				"failed, interrupted, cancelled-or-stopped, timed-out and completed remain distinguishable")
		}
	}
}

// TestBatchSupervisionSummaryBatch_NoFalseSilenceFlagging (2d-a): the batch
// supervision summary never flags unknown activity as silent -- no silence
// derivation exists on batch facts -- while ordinary inspection guidance and
// the live work's identity survive.
func TestBatchSupervisionSummaryBatch_NoFalseSilenceFlagging(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "w5-silence-owner")
	require.NoError(t, err)
	w5SeedRunningRoot(t, env.conn, env.messages, owner.ID, "call-w5-silence")

	live := batchFixtureLiveReader(env.conn, env.workingDir)
	facts := NewBatchCutoverNodeFacts(session.NewBatchStore(env.conn), live, env.conn)
	coord := newBatchRuntimeCoordinator()
	BindCoordinatorBatchCutoverFacts(coord, facts)
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(coord) })

	text := coord.buildSupervisionSummaryAt(time.Now(), owner.ID, 1, time.Minute, false)
	require.NotContains(t, text, "silent")
	require.NotContains(t, text, "job_kill")
	require.Contains(t, text, "batch_output", "ordinary inspection guidance survives")
	require.Contains(t, text, "batch_stop")
	require.Contains(t, text, "call-w5-silence", "the summary still describes the live work by tool call id")
}

// TestBatchQuestionNotice_ExactDeliveryAndAwaitingCount (2d-b): the child
// question is delivered to the parent EXACTLY ONCE through ordinary notice
// delivery with the existing node-identity marker and verbatim question
// text; the debt persists after delivery (delivery=done, reacted=0 still
// holds); supervision reports the awaiting count WITHOUT repeating the
// question prose; and a terminal incarnation stops counting.
func TestBatchQuestionNotice_ExactDeliveryAndAwaitingCount(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	ctx := context.Background()

	owner, err := env.sessions.Create(ctx, "w5-question-owner")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(ctx, "call-w5-q-task", owner.ID, "w5-question-child")
	require.NoError(t, err)
	root, childNode := seedBatchDelegation(t, env.conn, owner.ID, "call-w5-q", child.ID, "agent", "running", false)

	// The synthetic fixture reserves tokens as 0; BatchPublishQuestion needs
	// the exact nonzero admission-style token of the live node.
	const tok = int64(42)
	_, err = env.conn.ExecContext(ctx, `UPDATE batch_nodes SET token = ?1 WHERE node_id = ?2`, tok, childNode)
	require.NoError(t, err)

	live := batchFixtureLiveReader(env.conn, env.workingDir)
	facts := NewBatchCutoverNodeFacts(session.NewBatchStore(env.conn), live, env.conn)
	coord := newBatchRuntimeCoordinator()
	BindCoordinatorBatchCutoverFacts(coord, facts)
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(coord) })

	publisher := batchCoordinatorQuestionPublisher{store: session.NewBatchStore(env.conn)}
	require.NoError(t, publisher.PublishChildQuestion(ctx, batchChildQuestionNotice{
		Owner: owner.ID, ChildSessionID: child.ID, NodeID: childNode,
		Token: uint64(tok), Depth: 1, Sequence: 1, Question: "continue with plan B?",
	}))

	held, err := facts.LiveHeldQuestions(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, held, 1, "supervision says the parent is awaiting one answer")

	out, err := facts.PullPendingSessionNotices(ctx, env.messages, owner.ID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, out, 1, "the parent sees the exact question ONCE through ordinary notice delivery")
	text := out[0].Message.FullText()
	require.Contains(t, text, "[batch-question node="+childNode+" token=", "the marker carries the EXISTING node identity")
	require.Contains(t, text, "SUB-AGENT QUESTION: continue with plan B?", "the durable question text is delivered verbatim")

	out2, err := facts.PullPendingSessionNotices(ctx, env.messages, owner.ID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, out2, 0, "consumed once")

	held2, err := facts.LiveHeldQuestions(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, held2, 1, "delivered-but-unreacted debt still holds")
	text2 := coord.buildSupervisionSummaryAt(time.Now(), owner.ID, 1, time.Minute, false)
	require.Contains(t, text2, "(1 awaiting your answer)")
	require.NotContains(t, text2, "continue with plan B?",
		"supervision does NOT repeat the question prose (accepted removal) -- the notice already carried it")

	// Stale incarnation: project the delegation terminal; the fenced
	// question no longer counts as awaiting.
	store := session.NewBatchStore(env.conn)
	require.NoError(t, store.Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: owner.ID, Root: root,
			Updates: []db.ProjectBatchNodeParams{
				{NodeID: root, State: "completed", OutcomeState: "completed", OutcomeSummary: "released", ControlState: "none"},
				{NodeID: childNode, State: "completed", OutcomeState: "completed", OutcomeSummary: "released", ControlState: "none"},
			},
		}},
	}))
	held3, err := facts.LiveHeldQuestions(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, held3, 0, "a terminal incarnation does not count as awaiting")
}

// TestBatchIdleChildSeparateRootKeepsScopeLiveThenCloses (2d-c): an idle
// child owning a live separate-root task keeps its owner scope live with NO
// legacy own-jobs prose line; once the task settles the scope closes.
func TestBatchIdleChildSeparateRootKeepsScopeLiveThenCloses(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	ctx := context.Background()

	childOwner, err := env.sessions.Create(ctx, "w5-idle-child")
	require.NoError(t, err)
	root, child := w5SeedRunningRoot(t, env.conn, env.messages, childOwner.ID, "call-w5-idle")

	live := batchFixtureLiveReader(env.conn, env.workingDir)
	facts := NewBatchCutoverNodeFacts(session.NewBatchStore(env.conn), live, env.conn)
	coord := newBatchRuntimeCoordinator()
	BindCoordinatorBatchCutoverFacts(coord, facts)
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(coord) })

	text := coord.buildSupervisionSummaryAt(time.Now(), childOwner.ID, 1, time.Minute, false)
	require.NotContains(t, text, "(sub-agent, child session",
		"no legacy idle-child own-jobs line is rebuilt (accepted removal)")
	require.Contains(t, text, "call-w5-idle", "the live node line describes the real batch work")

	entries, err := facts.LiveScopeForOwners(ctx, []string{childOwner.ID})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Live,
		"an idle child owning a live separate-root task keeps the owner scope live -- without any legacy job-map line")

	require.NoError(t, session.NewBatchStore(env.conn).Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: childOwner.ID, Root: root,
			Updates: []db.ProjectBatchNodeParams{
				{NodeID: root, State: "completed", OutcomeState: "completed", OutcomeSummary: "settled", ControlState: "none"},
				{NodeID: child, State: "completed", OutcomeState: "completed", OutcomeSummary: "settled", ControlState: "none"},
			},
		}},
	}))

	entries2, err := facts.LiveScopeForOwners(ctx, []string{childOwner.ID})
	require.NoError(t, err)
	require.Len(t, entries2, 1)
	require.False(t, entries2[0].Live, "after the task settles the scope closes")
}
