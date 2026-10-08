// #1157: the delegation-waits-for-an-answer state -- the child_question
// notice, the answer path into the held delegation, the answerHold release
// race, inspect_agent's awaiting_answer, and the supervision texts.
package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// questionFixture: a parent delegating to a child whose FIRST turn ends on
// ask_question while the child keeps one own running job with no executor
// (the hung `go test` of wskills). The delegation X is armed.
func questionFixture(t *testing.T) (*attemptFixture, string, string, chan AsyncCompletion, *shell.BackgroundShell) {
	t.Helper()
	ctx := context.Background()
	f, parentID, childID, ch := childDelegationBase(t, attemptFixtureOpts{
		tools:   []fantasy.AgentTool{tools.NewAskQuestionTool()},
		handler: textThenAskQuestionResponse,
		noIdle:  true,
	})
	f.coord.background = shell.NewBackgroundShellManager()
	t.Cleanup(func() { f.coord.background.Close(context.Background()) })
	held, err := f.coord.background.StartOwned(ctx, childID, f.env.workingDir, nil, "sleep 30", "held")
	require.NoError(t, err)
	_, err = f.sa.Run(ctx, SessionAgentCall{SessionID: childID, Prompt: "do work", NonInteractive: true})
	require.ErrorAs(t, err, new(*AwaitingAnswerError))
	require.True(t, f.coord.autoResumeSuspended(childID), "a question suspends the child's automatic turns")
	f.ledger.armDelegation(jobOf(f.ledger, parentID, "delegate-1"), jobResult{content: "first turn text"})
	return f, parentID, childID, ch, held
}

// finishHungJob completes the child's own background shell (the hung
// `go test`): no ledger job, hence no reaction debt -- the shell is the open
// scope the delegation waits on, exactly as in wskills.
func finishHungJob(t *testing.T, f *attemptFixture, childID string, sh *shell.BackgroundShell) {
	t.Helper()
	require.NoError(t, f.coord.background.Kill(t.Context(), sh.ID))
	require.True(t, sh.WaitContext(t.Context()))
	f.ledger.recheckChild(childID)
}

// childQuestionRows lists the parent's session_notices rows of kind
// child_question straight from the DB.
func childQuestionRows(t *testing.T, f *attemptFixture, owner string) []session.SessionNoticeRow {
	t.Helper()
	rows, err := f.store.ListSessionNotices(context.Background(), owner)
	require.NoError(t, err)
	var out []session.SessionNoticeRow
	for _, r := range rows {
		if r.Kind == session.NoticeKindChildQuestion {
			out = append(out, r)
		}
	}
	return out
}

func waitChildQuestion(t *testing.T, f *attemptFixture, owner string) session.SessionNoticeRow {
	t.Helper()
	var found session.SessionNoticeRow
	require.Eventually(t, func() bool {
		rows := childQuestionRows(t, f, owner)
		if len(rows) > 0 {
			found = rows[0]
			return true
		}
		return false
	}, 5*time.Second, 25*time.Millisecond)
	return found
}

// collectCompletions drains n completions from the delivery channel.
func collectCompletions(t *testing.T, ch chan AsyncCompletion, n int) []AsyncCompletion {
	t.Helper()
	var out []AsyncCompletion
	for len(out) < n {
		select {
		case c := <-ch:
			out = append(out, c)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d completions arrived", len(out), n)
		}
	}
	return out
}

// T1: a child that asked a question while its own work still runs wakes the
// parent with a pending child_question notice bound to the held delegation X
// -- the deadlock (#1157/A30: the parent saw only "1 delegation in progress")
// is broken within one trigger, not one supervision tick.
//
// Revert-check: removing the noteChildQuestion call in recheckChild's
// not-drained branch leaves no row and this test goes red on the wait.
func TestChildQuestion_OpenScopeWakesParent(t *testing.T) {
	ctx := context.Background()
	f, parentID, childID, delivered, _ := questionFixture(t)

	row := waitChildQuestion(t, f, parentID)

	var wake int64
	var jobID, delivery string
	require.NoError(t, f.env.conn.QueryRowContext(ctx,
		"SELECT wake, job_tool_call_id, delivery FROM session_notices WHERE id = ?",
		row.ID).Scan(&wake, &jobID, &delivery))
	require.Equal(t, int64(1), wake, "the notice must wake the parent")
	require.Equal(t, "delegate-1", jobID, "the notice is bound to the held delegation X")
	require.Equal(t, "pending", delivery)

	pulled, err := f.store.PullSessionNotices(ctx, f.env.messages, parentID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.True(t, pulled[0].Wake)
	text := pulled[0].Message.FullText()
	require.Contains(t, text, "SUB-AGENT QUESTION (session "+childID+")")
	require.Contains(t, text, "which one?")
	require.Contains(t, text, `resume_session_id="`+childID+`"`)
	require.Contains(t, text, `stop_agent(child_session_id="`+childID+`")`)
	require.Contains(t, text, "delegate-1")

	select {
	case c := <-delivered:
		t.Fatalf("the held delegation must not release while the child still works: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
	xrow, err := f.store.Get(ctx, parentID, "delegate-1")
	require.NoError(t, err)
	require.Equal(t, "running", xrow.State)

	f.ledger.mu.Lock()
	hint := f.ledger.bySession[parentID].hintSeq
	f.ledger.mu.Unlock()
	require.Greater(t, hint, uint64(0), "the fact must bump the parent's hint counter")
}

// T1b: once the child's own work drained WITHOUT an answer, the delegation
// releases carrying the (still unanswered) question, and the stale
// child_question row voids instead of delivering a second copy.
//
// Revert-check: dropping the child_question void condition in
// sessionNoticeVoidCondition delivers a second SUB-AGENT QUESTION and this
// test goes red on the NotContains.
func TestChildQuestion_ReleaseVoidsStaleNotice(t *testing.T) {
	ctx := context.Background()
	f, parentID, childID, delivered, held := questionFixture(t)
	waitChildQuestion(t, f, parentID)

	finishHungJob(t, f, childID, held)
	f.ledger.recheckChild(childID)

	found := false
	for _, c := range collectCompletions(t, delivered, 1) {
		if c.ToolCallID != "delegate-1" {
			continue
		}
		found = true
		require.False(t, c.IsError, "an unanswered question is not a failure")
		require.Contains(t, c.Content, "SUB-AGENT QUESTION")
	}
	require.True(t, found, "the delegation itself must be among the completions")

	pulled, err := f.store.PullSessionNotices(ctx, f.env.messages, parentID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	for _, p := range pulled {
		require.NotContains(t, p.Message.FullText(), "SUB-AGENT QUESTION",
			"the stale child_question row must void: the release already carries the question")
	}
}

// T1c: repeated re-check triggers announce the held question exactly once.
//
// Revert-check: removing the questionNoticed latch under l.mu inserts one row
// per trigger and the count assertion goes red.
func TestChildQuestion_NoticedOnce(t *testing.T) {
	f, parentID, childID, _, _ := questionFixture(t)
	waitChildQuestion(t, f, parentID)
	f.ledger.recheckChild(childID)
	f.ledger.recheckChild(childID)
	require.Eventually(t, func() bool { return true }, 100*time.Millisecond, 50*time.Millisecond)
	require.Len(t, childQuestionRows(t, f, parentID), 1)
}

// T1d: the wskills layout -- the child's LAST message is the ask_question
// tool result, the question lives on the finished assistant message before
// it -- must inspect as awaiting_answer, not idle.
//
// Revert-check: reverting childLastActivity to the literal last message
// (a tool message, no finish part) reports idle and this test goes red.
func TestInspectAgent_AwaitingAfterQuestionToolResult(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "q-insp")
	ctx := t.Context()

	_, title := awaitingAnswerStoppedFinishText(&AwaitingAnswerError{Question: "queue is full: wait or report?", SessionID: child})
	_, err := env.messages.Create(ctx, child, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "the queue is full"},
			message.Finish{Reason: message.FinishReasonError, Message: awaitingAnswerStoppedTitle, Details: title},
		},
	})
	require.NoError(t, err)
	// The ask_question tool result: the literal last message of the child.
	_, err = env.messages.Create(ctx, child, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call_q", Name: "ask_question", Content: "turn ended"},
		},
	})
	require.NoError(t, err)

	insp, err := coord.InspectAgent(ctx, parent, child)
	require.NoError(t, err)
	require.Equal(t, "awaiting_answer", insp.Status)
	require.Contains(t, insp.LastActivitySummary, "the queue is full")
}

// originRecordingAgent records the call origin of every Run it forwards to
// the fixture's real session agent.
type originRecordingAgent struct {
	*sessionAgent
	mu      sync.Mutex
	origins []message.Origin
}

func (a *originRecordingAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	a.mu.Lock()
	a.origins = append(a.origins, CallOriginFrom(ctx))
	a.mu.Unlock()
	return a.sessionAgent.Run(ctx, call)
}

func (a *originRecordingAgent) recordedOrigins() []message.Origin {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]message.Origin(nil), a.origins...)
}

// T2: the parent's answer goes INTO the held delegation -- no new row, no
// second delegation, the child's turn receives the prompt, and the delegation
// releases exactly once -- for EVERY call origin, including the absent one: a
// parent woken for the child_question by a Drain turn (#1212) answers with no
// origin on its ctx at all, and the intercept must not depend on it. The
// replayed answer turn itself runs with X's OWN origin (OriginCLI for a
// CLI-claimed X, OriginWeb otherwise), not the answering call's.
//
// Revert-check: restoring the `!sync` gate on the intercept makes the
// unspecified-origin subtest miss the held path and block in a sync
// delegation's awaitSync -- Run misses the 2s deadline and the inner tool
// runs (innerCalled).
func TestChildQuestion_ResumeAnswersHeldDelegation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		origin     message.Origin
		claimCLI   bool
		wantReplay message.Origin
	}{
		{name: "cli", origin: message.OriginCLI, claimCLI: true, wantReplay: message.OriginCLI},
		{name: "web", origin: message.OriginWeb, claimCLI: false, wantReplay: message.OriginWeb},
		// The Drain turn's ctx: no WithCallOrigin was ever applied to it.
		{name: "unspecified", origin: message.OriginUnspecified, claimCLI: false, wantReplay: message.OriginWeb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, parentID, childID, delivered, held := questionFixture(t)
			waitChildQuestion(t, f, parentID)

			// The driver's agent becomes a recording wrapper so the test can
			// observe the origin the replayed answer turn is run with.
			rec := &originRecordingAgent{sessionAgent: f.sa}
			f.coord.subAgentDrivers.register(childID, subAgentDriver{
				agent: rec, call: SessionAgentCall{SessionID: childID}, parentSessionID: parentID,
			})
			if tc.claimCLI {
				// A CLI parent claims X with origin_cli=1 (asyncTool.Run's Start).
				xjob := jobOf(f.ledger, parentID, "delegate-1")
				f.ledger.mu.Lock()
				xjob.cli = true
				f.ledger.mu.Unlock()
			}

			var mu sync.Mutex
			var bodies []string
			f.setHandler(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(b))
				mu.Unlock()
				textFinishResponse(w, "final answer after the answer")
			})

			innerCalled := &atomic.Bool{}
			inner := fantasy.NewAgentTool(AgentToolName, "inner",
				func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
					innerCalled.Store(true)
					return fantasy.NewTextResponse("inner"), nil
				})
			tl := &asyncTool{inner: inner, coordinator: f.coord, name: AgentToolName}
			call := fantasy.ToolCall{ID: "call-answer", Input: fmt.Sprintf(`{"prompt":"report now","resume_session_id":%q}`, childID)}
			sessCtx := context.WithValue(ctx, tools.SessionIDContextKey, parentID)
			if tc.origin != message.OriginUnspecified {
				sessCtx = WithCallOrigin(sessCtx, tc.origin)
			}

			// Run must return promptly while the child's own job (the held
			// shell) is still running: a missed intercept falls into a sync
			// delegation whose awaitSync only unblocks when the child's scope
			// drains -- it never does here until finishHungJob below.
			type runOutcome struct {
				resp fantasy.ToolResponse
				err  error
			}
			done := make(chan runOutcome, 1)
			go func() {
				resp, err := tl.Run(sessCtx, call)
				done <- runOutcome{resp, err}
			}()
			var res runOutcome
			select {
			case res = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("agent tool did not return within 2s: the answer missed the held delegation and blocked in awaitSync")
			}
			require.NoError(t, res.err)
			require.False(t, res.resp.IsError)
			require.Contains(t, res.resp.Content, "Answer delivered to sub-agent "+childID)
			require.Contains(t, res.resp.Content, "delegate-1")
			require.False(t, innerCalled.Load(), "the inner agent tool must not run for an answered delegation")

			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, b := range bodies {
					if strings.Contains(b, "report now") {
						return true
					}
				}
				return false
			}, 10*time.Second, 25*time.Millisecond, "the child's answer turn must reach the provider")

			origins := rec.recordedOrigins()
			require.Len(t, origins, 1, "exactly one replayed answer turn")
			require.Equal(t, tc.wantReplay, origins[0], "the replayed turn carries X's own origin")

			xrow, err := f.store.Get(ctx, parentID, "delegate-1")
			require.NoError(t, err)
			require.Equal(t, "running", xrow.State, "no new row: the answer lands in the held delegation")
			require.Equal(t, childID, xrow.ChildSessionID.String)
			require.Equal(t, phaseRunning, jobOf(f.ledger, parentID, "delegate-1").state)
			f.ledger.mu.Lock()
			jobs := len(f.ledger.bySession[parentID].jobs)
			f.ledger.mu.Unlock()
			require.Equal(t, 1, jobs, "no second delegation may be registered next to X")

			finishHungJob(t, f, childID, held)
			f.ledger.recheckChild(childID)
			found := 0
			for _, c := range collectCompletions(t, delivered, 1) {
				if c.ToolCallID != "delegate-1" {
					continue
				}
				found++
				require.Contains(t, c.Content, "final answer after the answer")
				require.NotContains(t, c.Content, "SUB-AGENT QUESTION",
					"after the answer the delegation releases with the final text, not the question")
			}
			require.Equal(t, 1, found, "the delegation itself must release, exactly once")
			select {
			case c := <-delivered:
				t.Fatalf("the delegation must release exactly once: got a second completion %+v", c)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

// settleRequests waits until the fixture's request count has been stable for
// 200ms (requests cancelled mid-flight still reach the server late).
func settleRequests(t *testing.T, f *attemptFixture) {
	t.Helper()
	last, since := f.requests.Load(), time.Now()
	require.Eventually(t, func() bool {
		if n := f.requests.Load(); n != last {
			last, since = n, time.Now()
		}
		return time.Since(since) >= 200*time.Millisecond
	}, 10*time.Second, 20*time.Millisecond)
}

// T2c: an X claimed by an SDK-sync caller is never answered into (#1212): it
// has no durable hold of its own, so the resume falls through to the ordinary
// path and the child is not resumed by the answer machinery.
//
// Revert-check: dropping the `job.sync` guard makes answerHeldDelegation
// intercept this resume and return answerAcceptedResponse.
func TestChildQuestion_SyncHeldJobNotAnswered(t *testing.T) {
	ctx := context.Background()
	f, parentID, childID, _, _ := questionFixture(t)
	waitChildQuestion(t, f, parentID)

	// Make X look SDK-sync-claimed (work_ledger.Start sets done for sync jobs).
	xjob := jobOf(f.ledger, parentID, "delegate-1")
	f.ledger.mu.Lock()
	xjob.sync = true
	xjob.done = make(chan struct{})
	f.ledger.mu.Unlock()

	// The child's detached title goroutine keeps retrying against the probe
	// server (~100 requests/s): stop and join it, then let requests already
	// in flight land, or the count below moves on its own.
	f.sa.titleGens.cancelAll()
	f.sa.runWg.Wait()
	settleRequests(t, f)
	requestsBefore := f.requests.Load()
	resp, answered := f.ledger.answerHeldDelegation(ctx, parentID, childID, "report now")
	require.False(t, answered, "a sync X must not take the held-answer path")
	require.Empty(t, resp.Content)
	require.Equal(t, requestsBefore, f.requests.Load(), "no answer turn may reach the provider")

	innerCalled := &atomic.Bool{}
	inner := fantasy.NewAgentTool(AgentToolName, "inner",
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			innerCalled.Store(true)
			return fantasy.NewTextResponse("inner"), nil
		})
	tl := &asyncTool{inner: inner, coordinator: f.coord, name: AgentToolName}
	call := fantasy.ToolCall{ID: "call-answer", Input: fmt.Sprintf(`{"prompt":"report now","resume_session_id":%q}`, childID)}
	sessCtx := WithCallOrigin(context.WithValue(ctx, tools.SessionIDContextKey, parentID), message.OriginCLI)
	toolResp, err := tl.Run(sessCtx, call)
	require.NoError(t, err)
	require.True(t, toolResp.IsError, "the ordinary path's refusal stands for a sync X")
	require.NotContains(t, toolResp.Content, "Answer delivered")
	require.Contains(t, toolResp.Content, "still has work running",
		"the resume hits the child-session-busy refusal, as before the held-answer path")

	xrow, err := f.store.Get(ctx, parentID, "delegate-1")
	require.NoError(t, err)
	require.Equal(t, "running", xrow.State)
	f.ledger.mu.Lock()
	hold := xjob.answerHold
	f.ledger.mu.Unlock()
	require.False(t, hold, "answerHold must stay off a sync X")
}

// T2b: the race -- the child's own work drains between the answer being
// accepted and the answer turn registering its own work. answerHold keeps X
// open for that whole window; a release there would deliver the question and
// strand the answer outside the delegation.
//
// Revert-check: removing the answerHold guard in recheckChild releases X in
// the window and this test goes red on the Fatalf.
func TestChildQuestion_AnswerHoldBlocksRelease(t *testing.T) {
	f, parentID, childID, delivered, held := questionFixture(t)
	waitChildQuestion(t, f, parentID)

	job := jobOf(f.ledger, parentID, "delegate-1")
	f.ledger.mu.Lock()
	job.answerHold = true
	f.ledger.mu.Unlock()
	finishHungJob(t, f, childID, held)
	f.ledger.recheckChild(childID)

	select {
	case c := <-delivered:
		t.Fatalf("released X while the answer turn was being admitted: %+v", c)
	case <-time.After(300 * time.Millisecond):
	}
	xrow, err := f.store.Get(context.Background(), parentID, "delegate-1")
	require.NoError(t, err)
	require.Equal(t, "running", xrow.State)

	f.ledger.mu.Lock()
	job.answerHold = false
	f.ledger.mu.Unlock()
	f.ledger.recheckChild(childID)
	found := false
	for _, c := range collectCompletions(t, delivered, 1) {
		if c.ToolCallID == "delegate-1" {
			found = true
		}
	}
	require.True(t, found, "the delegation releases once the latch drops")
}

// T3: the supervision tick names the delegation awaiting an answer, quotes
// the question, and gives the exact answer and give-up calls.
//
// Revert-check: reverting buildSupervisionSummary to the plain "running ...,
// last activity" line loses every marker and this test goes red on the first
// Contains.
func TestSupervisionSummary_NamesDelegationAwaitingAnswer(t *testing.T) {
	f, parentID, childID, _, _ := questionFixture(t)
	waitChildQuestion(t, f, parentID)

	text := f.ledger.buildSupervisionSummary(parentID, 2, 10*time.Minute, false)
	for _, want := range []string{
		"1 sub-agent delegation(s) in progress (1 awaiting your answer)",
		"AWAITING YOUR ANSWER",
		"which one?",
		`resume_session_id="` + childID,
		`stop_agent(child_session_id="` + childID + `")`,
		"A sub-agent is blocked on your answer; waiting will not progress it.",
	} {
		require.Contains(t, text, want)
	}
}

// T3b: the OTHER variant -- a child that finished its turn with text and is
// idle while its own jobs still run -- is named as such instead of a stale
// "last activity" reading.
//
// Revert-check: without idleChildSummary the tick falls back to "running ...,
// last activity" and the Contains goes red.
func TestSupervisionSummary_IdleChildWithOwnJobs(t *testing.T) {
	ctx := context.Background()
	f, parentID, childID, _ := childDelegationBase(t, attemptFixtureOpts{noIdle: true})
	_, _, err := f.ledger.Start(childID, "hung-job", "hung-job", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	_, err = f.sa.Run(ctx, SessionAgentCall{SessionID: childID, Prompt: "work", NonInteractive: true})
	require.NoError(t, err)
	f.ledger.armDelegation(jobOf(f.ledger, parentID, "delegate-1"), jobResult{content: "did things"})

	text := f.ledger.buildSupervisionSummary(parentID, 1, 5*time.Minute, false)
	require.Contains(t, text, "idle, waiting on its own 1 job(s)")
	require.Contains(t, text, "child session "+childID)
	require.NotContains(t, text, "last activity")
}
