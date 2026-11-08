package agent

// Execution-side question-resolution regressions (Blocker 3): the hold's
// durable half. The published notice must never outlive the hold it
// advertises: an accepted inject resolves the incarnation's notices up to
// the answered sequence, a cancelled ask resolves them too (WithoutCancel
// — the caller's ctx may already be dying), and a NotHeld rejection never
// triggers a blind delete.
//
// Header mutation note: removing the execution-side resolution
// reintroduces the notice-outlives-hold window (direct Runner.Inject and
// caller-cancel paths), which the real-store regressions (AWa5,
// StopWhileHeld, incident chain) pin end to end. A rig without
// BindChildSession cannot exercise the resolver at all — the resolution
// guard skips when the execution's ChildSessionID is empty — so the bind
// is part of the fixture, not the test body.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// recordingQuestionResolver records every call as
// "owner|node|child|token|sequence"; ResolveChildQuestions returns r.err.
type recordingQuestionResolver struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingQuestionResolver) ResolveChildQuestions(ctx context.Context, owner, node, child string, token, sequence int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, owner+"|"+node+"|"+child+"|"+strconv.FormatInt(token, 10)+"|"+strconv.FormatInt(sequence, 10))
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.err
}

func (r *recordingQuestionResolver) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// resolutionRig builds the parking-ask rig used by all three tests: one ask
// at depth 1 through askAndShape, a recording resolver passed through deps
// (batchAgentTestRun preserves unknown deps fields). The returned reader is
// the rig's descendants poller: tests settle it (set(false, true)) at the
// end so the scope watcher can close after the answer/cancel.
func resolutionRig(t *testing.T, tl *questionTestTimeline) (*batchAgentExecution, *batchAgentTestFactory, *recordingQuestionResolver, *batchAgentTestReader) {
	t.Helper()
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		// Production binds the child session inside the agent run before
		// any ask (batch_exec_agent.go launches with BindChildSession:
		// e.bindChildSession); the fixture mirrors that. The resolver guard
		// refuses an empty ChildSessionID (batch_exec_agent.go
		// resolvePublishedQuestions), so without the bind the resolution is
		// NEVER exercised. session_notices has no FK on a child id, so no
		// child session row is needed.
		host.BindChildSession(context.Background(), "sess-child-res")
		askAndShape(t2, host, 1, "why?")
	}
	rec := &recordingQuestionResolver{}
	e, factory, reader, _ := batchAgentTestRun(t, batchAgentDeps{QuestionResolver: rec}, factory)
	tl.addf("rig started; asked once at depth 1")
	return e, factory, rec, reader
}

func resolutionWaitHeldPublished(t *testing.T, e *batchAgentExecution, factory *batchAgentTestFactory, tl *questionTestTimeline) {
	t.Helper()
	require.Eventually(t, func() bool {
		depth, held := e.heldDepth()
		return held && depth == 1
	}, 2*time.Second, time.Millisecond, "the question is held at depth 1")
	// The stamp happens after the hold, so wait for the publish before
	// injecting (the sequence fence needs the stamped hold).
	require.Eventually(t, func() bool { return len(factory.questionRig().publishedNotices()) >= 1 },
		2*time.Second, time.Millisecond, "the question is published")
	n := factory.questionRig().publishedNotices()[0]
	tl.addf("published: node=%s token=%d seq=%d child=%s", n.NodeID, n.Token, n.Sequence, n.ChildSessionID)
}

func TestBatchAgentInjectResolvesNoticeWithSequence(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	e, factory, rec, reader := resolutionRig(t, tl)
	resolutionWaitHeldPublished(t, e, factory, tl)
	req := e.requestSnapshot()
	require.NoError(t, jobbatch.Messenger(e).Inject(context.Background(),
		json.RawMessage(`{"depth":1,"sequence":1,"text":"x"}`)), "the matching answer is accepted")
	tl.addf("inject sequence=1 accepted")
	calls := rec.recorded()
	require.Len(t, calls, 1, "the resolver runs exactly once per accepted answer")
	want := req.SessionID + "|" + string(req.NodeID) + "|" + req.ChildSessionID + "|" + strconv.FormatInt(int64(req.Token), 10) + "|1"
	require.Equal(t, want, calls[0], "the resolution carries the execution's request identity and sequence 1")
	// No blind delete on NotHeld: a second inject at the same payload is
	// rejected and must NOT call the resolver again.
	require.ErrorIs(t, jobbatch.Messenger(e).Inject(context.Background(),
		json.RawMessage(`{"depth":1,"sequence":1,"text":"x"}`)), ErrBatchAgentNotHeld)
	tl.addf("second inject rejected ErrBatchAgentNotHeld")
	require.Len(t, rec.recorded(), 1, "NotHeld never triggers a resolution (no blind delete)")
	// Settle hygiene: confirm descendants terminal so the scope watcher can
	// settle after the answer instead of leaving the poller running.
	reader.set(false, true)
}

func TestBatchAgentInjectResolvesDespiteCancelledCallerCtx(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	e, factory, rec, reader := resolutionRig(t, tl)
	resolutionWaitHeldPublished(t, e, factory, tl)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// deliver ignores the caller ctx: an already-cancelled ctx must not
	// fail the inject, and the durable resolution must still run
	// (context.WithoutCancel proof).
	require.NoError(t, jobbatch.Messenger(e).Inject(ctx,
		json.RawMessage(`{"depth":1,"sequence":1,"text":"x"}`)),
		"the inject succeeds despite the cancelled caller ctx")
	tl.addf("inject accepted with cancelled caller ctx")
	require.Eventually(t, func() bool { return len(rec.recorded()) >= 1 },
		2*time.Second, time.Millisecond, "the resolution still ran (WithoutCancel)")
	// Settle hygiene: the answer landed; confirm descendants terminal so
	// the scope watcher settles instead of the poller outliving the test.
	reader.set(false, true)
}

func TestBatchAgentAskCancelResolvesNotice(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		go func() {
			err := host.BindChildSession(context.Background(), "sess-child-res")
			if err == nil {
				_, err = host.Ask(ctx, 1, "why?")
			}
			if err == nil {
				t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextResponse("unexpected answer")}, nil)
			} else {
				t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextErrorResponse(err.Error())}, nil)
			}
			t2.finish()
			t2.exitNow()
		}()
	}
	rec := &recordingQuestionResolver{}
	e, factory, reader, _ := batchAgentTestRun(t, batchAgentDeps{QuestionResolver: rec}, factory)
	resolutionWaitHeldPublished(t, e, factory, tl)
	cancel()
	tl.addf("ask ctx cancelled after held question was published")
	require.Eventually(t, func() bool {
		calls := rec.recorded()
		return len(calls) == 1
	}, 2*time.Second, time.Millisecond,
		"the cancelled ask resolves its published notice exactly once (the cancelled lifecycle)")
	require.Contains(t, rec.recorded()[0], "|1", "the cancelled resolution names sequence 1")
	// Settle hygiene: the asker already finished (ctx.Err); confirm
	// descendants terminal so the scope watcher settles.
	reader.set(false, true)
}

func TestBatchAgentResolutionFailureDoesNotAcceptAnswer(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	e, factory, rec, reader := resolutionRig(t, tl)
	resolutionWaitHeldPublished(t, e, factory, tl)
	failure := errors.New("question resolution transaction failed")
	rec.mu.Lock()
	rec.err = failure
	rec.mu.Unlock()
	raw := json.RawMessage(`{"depth":1,"sequence":1,"text":"x"}`)
	err := e.Inject(context.Background(), raw)
	depth, held := e.heldDepth()
	tl.addf("failed commit: err=%v held=%v depth=%d", err, held, depth)
	require.ErrorIs(t, err, failure)
	require.True(t, held, "the original question must remain answerable after a failed commit")
	require.Equal(t, 1, depth)
	rec.mu.Lock()
	rec.err = nil
	rec.mu.Unlock()
	err = e.Inject(context.Background(), raw)
	tl.addf("retry after store recovery: err=%v", err)
	require.NoError(t, err)
	require.Len(t, rec.recorded(), 2)
	require.ErrorIs(t, e.Inject(context.Background(), raw), ErrBatchAgentNotHeld)
	require.Len(t, rec.recorded(), 2, "duplicate delivery must not commit again")
	reader.set(false, true)
}
