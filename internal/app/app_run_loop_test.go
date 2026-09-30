// The `rush run` loop follows the coordinator's launch decision (docs/reviews/
// 2026-09-30-async-phase4-round2-attempts-design.md sec.5, B1-B11): a real App,
// a real coordinator and SQLite store, an httptest provider. Debt is seeded as
// a plain session notice from the first turn's handler (after that turn's own
// pull, so it stays owed) or between turns through cliLoopTurnDoneSeam; faults
// are SQLite triggers; pacing is shrunk through agent.SetDrainPacingForTest.
package app

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const loopNoticeMarker = "LOOP-OWED-NOTICE"

// loopHarness wraps one App whose provider counts first-turn and Drain
// requests (a Drain request is one whose newest user message is the seeded
// notice) and lets each test script the replies.
type loopHarness struct {
	t         *testing.T
	app       *App
	sessionID string
	dataDir   string

	requests atomic.Int32
	drains   atomic.Int32
	mu       sync.Mutex
	drainAt  []time.Time
}

type loopReply func(h *loopHarness, w http.ResponseWriter, body []byte, drain bool, n int)

func newLoopHarness(t *testing.T, reply loopReply) *loopHarness {
	t.Helper()
	h := &loopHarness{t: t}
	h.app, h.sessionID, h.dataDir = newLockBusyCLITestApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		drain := strings.Contains(string(body), loopNoticeMarker)
		n := int(h.requests.Add(1))
		if drain {
			h.drains.Add(1)
			h.mu.Lock()
			h.drainAt = append(h.drainAt, time.Now())
			h.mu.Unlock()
		}
		reply(h, w, body, drain, n)
	})
	return h
}

func (h *loopHarness) seedDebt() {
	h.t.Helper()
	require.NoError(h.t, h.app.asyncJobStore.InsertSessionNotice(context.Background(), h.sessionID,
		"manual_test_notice", loopNoticeMarker+" something happened", true, ""))
}

func (h *loopHarness) debtOpen() bool {
	h.t.Helper()
	open, err := h.app.asyncJobStore.ReactionDebtExists(context.Background(), h.sessionID)
	require.NoError(h.t, err)
	return open
}

func (h *loopHarness) markers() int {
	h.t.Helper()
	notices, err := h.app.asyncJobStore.ListSessionNotices(context.Background(), h.sessionID)
	require.NoError(h.t, err)
	n := 0
	for _, no := range notices {
		if no.Kind == session.NoticeKindWakeFailed {
			n++
		}
	}
	return n
}

// afterFirstTurn installs the between-turns hook: fn runs once, right after
// the first turn's ExecuteRun returned and before the loop decides.
func (h *loopHarness) afterFirstTurn(fn func()) {
	var once sync.Once
	cliLoopTurnDoneSeam = func() { once.Do(fn) }
	h.t.Cleanup(func() { cliLoopTurnDoneSeam = nil })
}

func (h *loopHarness) run(ctx context.Context, o RunOverrides) (*RunResult, string, error) {
	var out syncBuffer
	o.Origin = message.OriginCLI
	res, err := h.app.RunNonInteractiveWithResult(ctx, &out, "do it", o, true, RunModeJSON, h.sessionID, false)
	return res, out.String(), err
}

func loopCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// loopText answers with one text step and the given usage.
func loopText(w http.ResponseWriter, id, text string, prompt, completion int) {
	admissionWriteSSE(w, []string{
		admissionSSEText(id, text),
		`{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":` +
			strconv.Itoa(prompt) + `,"completion_tokens":` + strconv.Itoa(completion) + `,"total_tokens":` + strconv.Itoa(prompt+completion) + `}}`,
	})
}

// B1: --max-tokens: the Drain's step crosses the cap and is aborted, but the
// step is a recorded reaction, so the loop ends -- with an error -- instead of
// paying for the same reaction again and again.
//
// Revert-check: restoring the old onStepFinish order (caps before the
// reaction write) leaves the notice unreacted and this test goes red.
func TestRunNonInteractive_TokenCapDrainEndsLoop(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			loopText(w, "d", "reacting", 30, 10) // 40 tokens > the cap of 20
			return
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.afterFirstTurn(h.seedDebt)

	res, _, err := h.run(loopCtx(t), RunOverrides{MaxTokens: 20})

	require.Error(t, err, "a cap abort is an error exit")
	require.NotNil(t, res)
	require.EqualValues(t, 2, h.requests.Load(), "the first turn and exactly one Drain")
	require.False(t, h.debtOpen(), "the step that crossed the cap is a recorded reaction")
	require.NotEqual(t, "stop", res.ExitReason)
	require.Equal(t, "first answer", res.FinalText, "a failed Drain never replaces the last completed answer")
}

// B2: a question the agent asks in its reaction turn is the reaction: the loop
// ends with awaiting_answer after exactly one Drain (it does not re-prompt the
// model with its own unanswered question).
//
// Revert-check: writing the awaiting finish without recording the reaction
// leaves the notice owed, so the loop relaunches until K=3 (a fourth request)
// and this test goes red.
func TestRunNonInteractive_DrainAsksQuestion_AwaitingAnswer(t *testing.T) {
	defer agent.SetDrainPacingForTest(50*time.Millisecond, 0, 0)()
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("q", "call_q", "ask_question", `{"question":"which environment?"}`),
				admissionSSEStop("q", "tool_calls"),
			})
			return
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.afterFirstTurn(h.seedDebt)

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	var awaiting *agent.AwaitingAnswerError
	require.ErrorAs(t, err, &awaiting)
	require.NotNil(t, res)
	require.Equal(t, "awaiting_answer", res.ExitReason)
	require.EqualValues(t, 2, h.requests.Load(), "the first turn and exactly one Drain")
	require.False(t, h.debtOpen(), "the question is the reaction")
}

// B3: a Drain that keeps failing (a transient 403) is retried at the gate's
// pace -- never back to back -- and the debt is closed by failure with one
// marker on the third attempt; the run exits with an error.
//
// Revert-check: launching without the gate (drainPermitted returning the
// policy verdict only) retries back to back and the gap assertion goes red.
func TestRunNonInteractive_FailedDrainPacedThenSettled(t *testing.T) {
	const retry = 300 * time.Millisecond
	defer agent.SetDrainPacingForTest(retry, 0, 0)()
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"forbidden by the fronting balancer","type":"x"}}`))
			return
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.afterFirstTurn(h.seedDebt)

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.Error(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 3, h.drains.Load(), "three counted attempts, then the debt is closed")
	h.mu.Lock()
	times := append([]time.Time(nil), h.drainAt...)
	h.mu.Unlock()
	for i := 1; i < len(times); i++ {
		require.GreaterOrEqual(t, times[i].Sub(times[i-1]), retry*8/10, "attempts %d and %d are paced", i, i+1)
	}
	require.False(t, h.debtOpen())
	require.Equal(t, 1, h.markers(), "exactly one wake_failed marker")
	require.Equal(t, "first answer", res.FinalText)
	require.NotEqual(t, "stop", res.ExitReason)
}

// B4: a pull that never succeeds: the loop makes a bounded number of free
// no-turn attempts, then exits with an error and one stderr line; the Drain
// iterations write nothing to the session (budget columns stay as they were).
//
// Revert-check: without the dormant streak the loop keeps launching until the
// run's deadline and this test goes red.
func TestRunNonInteractive_PermanentPullFailure_ExitsStuck(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 0, 0)()
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "first answer", 11, 3)
	})
	_, err := h.app.DB().ExecContext(context.Background(), `CREATE TRIGGER fx_block_pull BEFORE UPDATE OF delivery ON session_notices
		WHEN NEW.delivery = 'done' BEGIN SELECT RAISE(ABORT, 'fx: pull blocked'); END`)
	require.NoError(t, err)
	h.afterFirstTurn(func() {
		h.seedDebt()
		require.NoError(t, h.app.Sessions.SetBudget(context.Background(), h.sessionID, 123, 456, 789))
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	res, _, runErr := h.run(ctx, RunOverrides{})

	require.Error(t, runErr)
	require.NoError(t, ctx.Err(), "the loop must give up on its own, long before the deadline")
	require.NotNil(t, res)
	require.Equal(t, "error", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load(), "no provider turn for a debt that can never be pulled")
	var maxCost, maxTokens int64
	require.NoError(t, h.app.DB().QueryRowContext(context.Background(),
		`SELECT budget_max_cost, budget_max_tokens FROM sessions WHERE id = ?`, h.sessionID).Scan(&maxCost, &maxTokens))
	require.EqualValues(t, 123, maxCost, "Drain iterations do not rewrite the session budget columns")
	require.EqualValues(t, 456, maxTokens)
}

// B6: a refusal before the turn (here: the provider entered its peak-hours
// window between turns) is never counted or settled: the debt stays, the loop
// retries at the refusal pace and gives up with that refusal after the budget.
//
// Revert-check: settling on a refusal (the old peak-hours path) marks the
// notice reacted_failed and writes a marker; leaving the refusal untyped and
// unpaced (drainRefused returning the bare error) turns the loop into a hot
// spin that only ends at the deadline. Either turns this test red.
func TestRunNonInteractive_PeakRefusalNeverSettles(t *testing.T) {
	defer agent.SetDrainPacingForTest(0, 100*time.Millisecond, 0)()
	orig := cliLockBusyRetryOverallLimit
	cliLockBusyRetryOverallLimit = time.Second
	t.Cleanup(func() { cliLockBusyRetryOverallLimit = orig })
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			t.Error("a refused Drain must never reach the provider")
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.afterFirstTurn(func() {
		h.seedDebt()
		cfg, ok := h.app.config.Config().Providers.Get("openaicompat")
		require.True(t, ok)
		cfg.PeakHours = &config.PeakHoursWindow{Start: "00:00", End: "23:59"}
		h.app.config.Config().Providers.Set("openaicompat", cfg)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	res, _, err := h.run(ctx, RunOverrides{})

	require.NoError(t, ctx.Err(), "the loop gives up on its own after the refusal budget")
	var peak *agent.PeakHoursError
	require.ErrorAs(t, err, &peak, "the run ends with the refusal's own error")
	require.NotNil(t, res)
	require.EqualValues(t, 1, h.requests.Load(), "only the first turn reached the provider")
	require.True(t, h.debtOpen(), "a refusal never closes the debt")
	require.Zero(t, h.markers(), "a refusal never writes a wake_failed marker")
}

// B8: Ctrl-C during a Drain keeps the previous completed answer and sums the
// usage of every turn of the invocation, including the interrupted one.
//
// Revert-check: replacing final with the cancelled Drain's result (or not
// adding its usage) turns the answer or the token assertion red.
func TestRunNonInteractive_CtrlCDuringDrain_KeepsAnswerSumsUsage(t *testing.T) {
	inDrain := make(chan struct{})
	var once sync.Once
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, drain bool, _ int) {
		_, _, lastTool := lastTurnParts(body)
		switch {
		case drain && lastTool != "":
			// Step 2 of the Drain: blocks until the run is interrupted.
			once.Do(func() { close(inDrain) })
			<-h.t.Context().Done()
		case drain:
			// Step 1: a cheap tool call (real usage recorded).
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("t", "call_ls", "ls", `{"path":"."}`),
				`{"id":"t","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}}`,
			})
		default:
			loopText(w, "a", "first answer", 11, 3)
		}
	})
	h.afterFirstTurn(h.seedDebt)

	ctx, cancel := context.WithCancel(loopCtx(t))
	go func() {
		<-inDrain
		cancel()
	}()
	res, _, err := h.run(ctx, RunOverrides{})

	require.Error(t, err)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Equal(t, "first answer", res.FinalText, "the interrupted Drain never replaces the last completed answer")
	require.EqualValues(t, 40, res.Usage.DeltaTokens, "usage of the first turn (14) plus the interrupted Drain's step (+26)")
}

// B9: a session driven by this very loop that is also the finished target of a
// delegation (a durable delegation child: `child_session_id` on a parent's
// row, `parent_session_id` set) still reacts to its own notice: the loop's own
// session skips the delegation-child refusal (its turns always run on the root
// agent) and its Drain iterations continue the session they already claimed.
//
// Revert-check: applying the released-child refusal to the loop's own session
// again leaves the notice unreacted (one request); resolving Drain iterations
// through the operator's child-session refusal fails them at session setup.
func TestRunNonInteractive_FormerDelegationChildReacts(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			loopText(w, "d", "the child reacts to its own notice", 11, 3)
			return
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	ctx := loopCtx(t)
	parent, err := h.app.Sessions.Create(ctx, "former-parent")
	require.NoError(t, err)
	h.afterFirstTurn(func() {
		// Between the turns a parent's delegation names the loop's session as
		// its (finished) child.
		_, err := h.app.DB().ExecContext(ctx, `UPDATE sessions SET parent_session_id = ? WHERE id = ?`, parent.ID, h.sessionID)
		require.NoError(t, err)
		_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
			Owner: parent.ID, ToolCallID: "deleg-1", Kind: session.JobKindAgent, Input: "x",
			ChildSessionID: h.sessionID, ToolName: "agent",
		})
		require.NoError(t, err)
		require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, parent.ID, "deleg-1"))
		_, err = h.app.asyncJobStore.Transition(ctx, session.TransitionParams{
			Owner: parent.ID, ToolCallID: "deleg-1", State: "completed", ResultSummary: "done", Wake: false,
		})
		require.NoError(t, err)
		h.seedDebt()
	})

	res, _, runErr := h.run(ctx, RunOverrides{})

	require.NoError(t, runErr)
	require.NotNil(t, res)
	require.EqualValues(t, 2, h.requests.Load(), "the first turn and the reaction")
	require.Equal(t, "the child reacts to its own notice", res.FinalText)
	require.False(t, h.debtOpen())
}

// B10: an operator's `sessions cancel` between turns ends the run (canceled)
// before any paid reaction turn, and a Drain iteration never erases the flag.
//
// Revert-check: dropping the pre-launch cancel check launches the Drain (a
// second request) and this test goes red.
func TestRunNonInteractive_DrainTurnsMutationFree(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			t.Error("no Drain may be launched after `sessions cancel`")
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.afterFirstTurn(func() {
		h.seedDebt()
		require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.Error(t, err)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load())
	canceled, cerr := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
	require.NoError(t, cerr)
	require.True(t, canceled, "the loop must not clear the operator's cancel request")
}
