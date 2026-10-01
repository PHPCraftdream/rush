// Round-4 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round4.md, W4-APP: R4C-1, R4C-2): same harness as app_run_loop_test.go (real
// App and SQLite, httptest provider).
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

const (
	r4First         = "R4-FIRST-ANSWER"
	r4Waiting       = "R4-REVIEWER-WAITING"
	r4Verdict       = "R4-REVIEWER-VERDICT-AFTER-OUTPUT"
	r4NoOutput      = "R4-VERDICT-WITHOUT-OUTPUT"
	r4ReviewerModel = "probe-reviewer"
	r4BashOutput    = "reviewed"
)

// wireModelAndTools reads the model id and the tool names off a chat request.
func wireModelAndTools(body []byte) (model string, tools []string) {
	var req struct {
		Model string `json:"model"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", nil
	}
	for _, tool := range req.Tools {
		tools = append(tools, tool.Function.Name)
	}
	return req.Model, tools
}

// r4Models gives the provider the smart model and a distinct reviewer model,
// both with per-million prices so a turn's usage costs money.
func r4Models(t *testing.T, application *App, costPer1M float64) {
	t.Helper()
	cfg := application.config.Config()
	provider, ok := cfg.Providers.Get("openaicompat")
	require.True(t, ok)
	model := func(id string) catwalk.Model {
		return catwalk.Model{ID: id, Name: id, ContextWindow: 200000, DefaultMaxTokens: 1000, CostPer1MIn: costPer1M, CostPer1MOut: costPer1M}
	}
	provider.Models = []catwalk.Model{model("probe"), model(r4ReviewerModel)}
	cfg.Providers.Set("openaicompat", provider)
}

// reviewerAsyncHarness scripts a smart run whose REVIEWER starts an async bash
// job: the job waits for a gate file that the provider writes only when it
// serves the reviewer's last step, so the job cannot finish inside the turn
// (no step-boundary pull) and its result must come back through a Drain.
type reviewerAsyncHarness struct {
	*loopHarness
	gate string

	mu             sync.Mutex
	reviewerStarts int
	drains         []reviewerDrainRequest
}

type reviewerDrainRequest struct {
	model string
	tools []string
}

// holdJob keeps the gate closed for good: the job never finishes.
func newReviewerAsyncHarness(t *testing.T, holdJob bool) *reviewerAsyncHarness {
	t.Helper()
	rh := &reviewerAsyncHarness{gate: filepath.ToSlash(filepath.Join(t.TempDir(), "gate"))}
	rh.loopHarness = newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, lastUser, lastTool := lastTurnParts(body)
		model, tools := wireModelAndTools(body)
		switch {
		case strings.Contains(lastUser, "Async job call-bash (bash)"):
			rh.mu.Lock()
			rh.drains = append(rh.drains, reviewerDrainRequest{model: model, tools: tools})
			rh.mu.Unlock()
			if strings.Contains(lastUser, r4BashOutput) {
				loopText(w, "drain", r4Verdict, 40, 8)
			} else {
				loopText(w, "drain", r4NoOutput, 40, 8)
			}
		case strings.Contains(lastUser, "independent reviewer") && strings.Contains(lastTool, "Async bash job"):
			// The reviewer's last step: it ends its turn "waiting". Only now may
			// the job finish.
			if !holdJob {
				_ = os.WriteFile(rh.gate, []byte("go"), 0o600)
			}
			loopText(w, "wait", r4Waiting, 30, 6)
		case strings.Contains(lastUser, "independent reviewer"):
			rh.mu.Lock()
			rh.reviewerStarts++
			rh.mu.Unlock()
			command := `until [ -f "` + rh.gate + `" ]; do :; done; echo ` + r4BashOutput
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("rb", "call-bash", "bash", `{"command":`+jsonString(command)+`,"description":"check"}`),
				admissionSSEStop("rb", "tool_calls"),
			})
		default:
			loopText(w, "a", r4First, 11, 3)
		}
	})
	r4Models(t, rh.app, 0)
	rh.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: r4ReviewerModel})
	return rh
}

func (rh *reviewerAsyncHarness) snapshot() (starts int, drains []reviewerDrainRequest) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	return rh.reviewerStarts, slices.Clone(rh.drains)
}

func (rh *reviewerAsyncHarness) requireNothingOpen(t *testing.T) {
	t.Helper()
	running, err := rh.app.asyncJobStore.ListRunningAsyncJobsForOwners(context.Background(), []string{rh.sessionID})
	require.NoError(t, err)
	require.Empty(t, running, "no async job may be left running at exit")
	require.False(t, rh.debtOpen(), "no reaction debt may be left at exit")
}

// R4C-1: the reviewer turn starts an async bash job (every CLI bash is one) and
// ends its turn "waiting". The loop goes back through nextStep after the
// reviewer turn: it waits for the job, Drains its result on the REVIEWER's
// model and call options (sub-agents off), and the run's answer is that last
// completed turn -- a verdict made after the output -- while the reviewer ran
// exactly once. Before the fix the run ended at the reviewer turn: the answer
// was the "waiting" text, the job was cancelled by the shutdown, exit 0.
//
// Revert-check: exitClosed returning right after the reviewer turn (the R3C-1
// shape) makes FinalText the waiting text and leaves the job running / no Drain.
func TestRunNonInteractive_ReviewerAsyncBashIsWaitedOnAndAnswered(t *testing.T) {
	rh := newReviewerAsyncHarness(t, false)

	res, out, err := rh.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.NoError(t, err)
	require.NotNil(t, res)
	starts, drains := rh.snapshot()
	require.Equal(t, 1, starts, "the reviewer runs at most once per invocation")
	require.Len(t, drains, 1, "the job's result is reacted to once")
	require.Equal(t, r4ReviewerModel, drains[0].model, "the reaction runs on the reviewer's model")
	require.NotContains(t, drains[0].tools, "agent", "the reaction keeps the reviewer's call options (sub-agents off)")
	require.Equal(t, r4Verdict, res.FinalText, "the answer is the verdict made after the output, not the stale waiting text")
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, 1, strings.Count(out, `"final_text"`), "one envelope is flushed")
	require.NotContains(t, out, r4Waiting)
	rh.requireNothingOpen(t)
}

// R4C-1: terse mode prints only the answer, once, and stream/JSON flush paths
// see the same last completed turn.
//
// Revert-check: as above -- the waiting text is printed.
func TestRunNonInteractive_ReviewerAsyncBashTerseAnswerIsTheReaction(t *testing.T) {
	rh := newReviewerAsyncHarness(t, false)
	var out syncBuffer

	res, err := rh.app.RunNonInteractiveWithResult(loopCtx(t), &out, "do it",
		RunOverrides{ModelRole: config.SelectedModelTypeSmart, Origin: message.OriginCLI}, true, RunModeTerse, rh.sessionID, false)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, r4Verdict, strings.TrimSpace(out.String()), "terse output is the answer and nothing else")
	require.Equal(t, r4Verdict, res.FinalText)
	rh.requireNothingOpen(t)
}

// cancelOnWrite cancels once a diagnostic line containing substr is written:
// a deterministic "Ctrl-C while the loop waits".
type cancelOnWrite struct {
	syncBuffer
	substr string
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if strings.Contains(string(p), w.substr) {
		w.cancel()
	}
	return n, err
}

// R4C-1: Ctrl-C while the loop waits on the reviewer's job (the job never
// finishes) ends the run canceled with the reviewer's answer -- the last
// completed turn -- in one envelope; the reviewer is not re-run and no Drain
// starts.
//
// Revert-check: exiting right after the reviewer turn (the R3C-1 shape) never
// waits, so the run returns no error and never reaches the cancellation.
func TestRunNonInteractive_CtrlCWhileWaitingOnReviewerJob_KeepsReviewerAnswer(t *testing.T) {
	rh := newReviewerAsyncHarness(t, true)
	ctx, cancel := context.WithCancel(loopCtx(t))
	var out syncBuffer
	stderr := &cancelOnWrite{substr: "still has open work", cancel: cancel}
	l := r4LoopWith(ctx, rh, driverSource(t, rh.app), &out, stderr)

	res, err := l.run()

	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Equal(t, r4First, res.FinalText, "A10: final_text stays the executor's answer")
	require.Equal(t, r4Waiting, res.Review, "the reviewer's completed turn is the review field")
	require.Equal(t, 1, strings.Count(out.String(), `"final_text"`), "one envelope is flushed")
	starts, drains := rh.snapshot()
	require.Equal(t, 1, starts)
	require.Empty(t, drains)
}

// scopeAfterReviewerSource answers the real scope until the reviewer turn has
// run, then answers from script (one state per call, the last repeated).
type scopeAfterReviewerSource struct {
	agent.ReactionDebtSource
	rh     *reviewerAsyncHarness
	script []agent.CLIScopeState
	mu     sync.Mutex
	after  int
}

func (s *scopeAfterReviewerSource) CLIScope(ctx context.Context, sessionID string) (agent.CLIScopeState, error) {
	if starts, _ := s.rh.snapshot(); starts == 0 {
		return s.ReactionDebtSource.CLIScope(ctx, sessionID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.after, len(s.script)-1)
	s.after++
	return s.script[i], nil
}

func r4LoopWith(ctx context.Context, rh *reviewerAsyncHarness, source agent.ReactionDebtSource, out *syncBuffer, stderr io.Writer) *cliLoop {
	overrides := RunOverrides{ModelRole: config.SelectedModelTypeSmart, Origin: message.OriginCLI}
	turnOverrides := overrides
	turnOverrides.OnFinishHook = ""
	return &cliLoop{
		app: rh.app, source: source, ctx: ctx, output: out, mode: RunModeJSON, hideSpinner: true,
		overrides: overrides, turnOverrides: turnOverrides, prompt: "do it",
		continueSessionID: rh.sessionID, started: time.Now(), sessionID: rh.sessionID,
		lastBuffered: &bytes.Buffer{}, stderr: stderr,
	}
}

// R4C-1: debt the reviewer turn left that the launch decision refuses to run
// (Stuck) ends the run with the loop's error exit, keeping the reviewer's
// answer; Deferred debt ends it as a normal exit. Neither runs the reviewer
// again or a Drain.
//
// Revert-check: exiting right after the reviewer turn skips nextStep -- the
// Stuck row returns no error.
func TestCLILoop_ReviewerLeftStuckOrDeferredDebt(t *testing.T) {
	cases := []struct {
		name       string
		state      agent.CLIScopeState
		wantErr    bool
		wantReason string
	}{
		{"stuck", agent.CLIScopeState{Drain: agent.DrainStuck, Reason: "pull keeps failing"}, true, "error"},
		{"deferred", agent.CLIScopeState{Drain: agent.DrainDeferred, Reason: "no automatic turn"}, false, "end_turn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rh := newReviewerAsyncHarness(t, false)
			var out, stderr syncBuffer
			real := driverSource(t, rh.app)
			src := &scopeAfterReviewerSource{ReactionDebtSource: real, rh: rh, script: []agent.CLIScopeState{tc.state}}
			l := r4LoopWith(loopCtx(t), rh, src, &out, &stderr)

			res, err := l.run()

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.NotNil(t, res)
			require.Equal(t, tc.wantReason, res.ExitReason)
			require.Equal(t, r4First, res.FinalText, "A10: final_text stays the executor's answer")
			require.Equal(t, r4Waiting, res.Review, "the reviewer's verdict is the review field")
			starts, drains := rh.snapshot()
			require.Equal(t, 1, starts, "the reviewer never runs twice")
			require.Empty(t, drains)
			require.Equal(t, 1, strings.Count(out.String(), `"final_text"`))
		})
	}
}
