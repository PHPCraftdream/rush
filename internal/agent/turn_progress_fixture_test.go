// The in-turn progress guard (docs/plans/2026-10-01-in-turn-progress-guard.md
// §2), driven end to end through the Drain-attempt harness: a real
// coordinator, a real workLedger on a real SQLite store, a real *sessionAgent
// and the REAL bash/view tools talking to an httptest provider. T5 is the A23
// shape (echo waits while the session's own job runs), T6 the A22 shape (a
// sliding re-read loop in an orchestrator tool set). Both observe the guard
// through the production callbacks a unit test cannot reach: StopWhen ending
// the turn, the assistant finish message, and the ledger's own rows.
package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// guardCLICtx tags ctx with the entry channel every `rush run` turn carries.
// A Drain call's own origin is Unspecified (newDrainCall builds its call from a
// bare SessionAgentCall), so without this tag the guard's async leg is off and
// no wait would ever be refused -- and, worse, the bash tool would take its
// synchronous branch and BLOCK on the held job. This is the same distinction
// §3 draws: a CLI/web launch returns at once, an SDK sleep really waits.
func guardCLICtx(ctx context.Context) context.Context {
	return WithCallOrigin(ctx, message.OriginCLI)
}

// guardSeedNotice gives the session one delivered job notice (visible batch
// debt), so the Drain leg has debt to react to (a Drain whose debt snapshot
// is empty never reaches the provider -- decideDrainTurn).
func guardSeedNotice(t *testing.T, f *attemptFixture, ctx context.Context, toolCallID string) {
	t.Helper()
	f.seedDebt(ctx, toolCallID, false)
}

// guardHeldTools and killHeldJob were DELETEd with the T5 echo-wait test
// (obsolete contract: asyncTool/ledger held-job machinery).

func guardViewTool(f *attemptFixture) fantasy.AgentTool {
	return tools.NewViewTool(f.env.permissions, *f.env.filetracker, nil, f.env.workingDir)
}

// guardDelegateTool is a name-only stand-in for the delegation tool: this test
// never calls it, it only has to be PRESENT in the step's set so the guard
// reads the session as an orchestrator (no edit tool, but `agent`).
func guardDelegateTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(AgentToolName, "delegate a change to a worker",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("delegated"), nil
		})
}

// --- SSE provider helpers (same wire shape drain_notice_pull_test.go uses) ---

func guardSSEToolCall(callID, name, args string) string {
	return fmt.Sprintf(`{"id":"g","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, callID, name, args)
}

func guardSSEText(text string) string {
	return fmt.Sprintf(`{"id":"g","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, text)
}

func guardSSEStop(finish string) string {
	return fmt.Sprintf(`{"id":"g","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":%q}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`, finish)
}

func guardWriteSSE(w http.ResponseWriter, chunks []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// guardToolCallsStep is one assistant step that ends on tool_calls.
func guardToolCallsStep(w http.ResponseWriter, callID, name, args string) {
	guardWriteSSE(w, []string{guardSSEToolCall(callID, name, args), guardSSEStop("tool_calls")})
}

// guardTextStep is one assistant step that ends the turn with plain text.
func guardTextStep(w http.ResponseWriter, text string) {
	guardWriteSSE(w, []string{guardSSEText(text), guardSSEStop("stop")})
}

// guardToolResult returns the persisted tool result for toolCallID.
func guardToolResult(t *testing.T, f *attemptFixture, ctx context.Context, toolCallID string) message.ToolResult {
	t.Helper()
	msgs, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	for _, m := range msgs {
		for _, result := range m.ToolResults() {
			if result.ToolCallID == toolCallID {
				return result
			}
		}
	}
	t.Fatalf("no persisted tool result for %s", toolCallID)
	return message.ToolResult{}
}

// guardFinishOf returns the assistant finish carrying title (the guard's own,
// or an ordinary one) together with its details.
func guardFinishOf(t *testing.T, f *attemptFixture, ctx context.Context, title string) (string, string) {
	t.Helper()
	msgs, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		part := m.FinishPart()
		if part != nil && strings.HasPrefix(part.Message, title) {
			return part.Message, part.Details
		}
	}
	t.Fatalf("no assistant finish starting with %q", title)
	return "", ""
}

// TestInTurnGuard_StopsEchoWaitSpamWhileOwnJobRuns is T5 (A23): the first step
// of the leg launches a HELD background job W, and every later step is a pure
// wait command. W's own launch is an async wait (a wait is no progress, which is
// exactly why the plan tells a model to end its turn right after starting a
// timer), so the streak starts at 1; because W is then the session's own
// undelivered work, refuseWait refuses every following wait at once -- no
// workLedger.Start, no async_jobs row, no claim -- and the fifth no-progress
// step reaches inTurnStopAfter, so StopWhen ends the turn with the guard's
// finish. Releasing W afterwards produces exactly one more Drain that answers
// normally.
//
// Revert-check: dropping ownWork from refuseWait's condition (turn_progress_
// guard.go) lets the first echo wait through -- it starts a ledger job and an
// async_jobs row, so this test goes red on the row it expects to be absent (and
// on the wait count, which then holds two launches).
// TestInTurnGuard_StopsEchoWaitSpamWhileOwnJobRuns was DELETEd as an
// obsolete contract (see the guardHeldTools note above): its held-job signal,
// job_kill path and running() assertions were all workLedger/asyncTool
// machinery.

// TestInTurnGuard_StopsRereadLoopOrchestratorMode is T6 (A22): one full read of
// a 200-line file, then windows of three lines sliding forward by one -- fresh
// by arguments, stale by content, so loop_detection stays mute and only the
// in-turn guard sees what is happening. The tool set has no edit tool and does
// have `agent`, so each refusal also names the orchestrator escape hatch. The
// sixth step is the third refusal and reaches inTurnStopAfter.
//
// Revert-check: not recording the read windows (applyStep's span loop) leaves
// every view call uncovered -- no refusal at all, nine provider requests
// instead of six, and this test red on the count.
func TestInTurnGuard_StopsRereadLoopOrchestratorMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var served atomic.Int32
	f := newAttemptFixture(t, "guard-reread-a22", attemptFixtureOpts{
		noIdle: true,
		handler: func(w http.ResponseWriter, _ *http.Request) {
			switch n := int(served.Add(1)); n {
			case 1:
				guardToolCallsStep(w, "guard-view-1", tools.ViewToolName,
					`{"file_path":"guard_a22.go","offset":0,"limit":163}`)
			default:
				offset := 68 + (n - 1)
				guardToolCallsStep(w, "guard-view-"+strconv.Itoa(n), tools.ViewToolName,
					`{"file_path":"guard_a22.go","offset":`+strconv.Itoa(offset)+`,"limit":3}`)
			}
		},
		tools: nil,
	})
	f.sa.SetTools([]fantasy.AgentTool{guardViewTool(f), guardDelegateTool()})
	f.seedDebt(ctx, "guard-notice", false)

	var body strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&body, "// re-read fixture line %d\n", i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(f.env.workingDir, "guard_a22.go"), []byte(body.String()), 0o644))

	_, err := f.drainRun(guardCLICtx(ctx))
	require.NoError(t, err)

	require.EqualValues(t, 6, f.requests.Load(),
		"one full read, two free re-reads, three refusals: the guard stops on the sixth")

	for _, n := range []int{2, 3} {
		result := guardToolResult(t, f, ctx, "guard-view-"+strconv.Itoa(n))
		require.False(t, result.IsError, "step %d is still inside the sanctioned series", n)
	}
	for _, n := range []int{4, 5, 6} {
		result := guardToolResult(t, f, ctx, "guard-view-"+strconv.Itoa(n))
		require.True(t, result.IsError, "step %d must be refused", n)
		require.Contains(t, result.Content, "already returned in this turn")
		require.Contains(t, result.Content, "delegate the change with the `agent` tool",
			"an orchestrator set gets the delegation escape hatch in the refusal")
	}

	msg, details := guardFinishOf(t, f, ctx, InTurnGuardStopTitle)
	require.Equal(t, InTurnGuardStopTitle, msg)
	require.NotEmpty(t, details)
	require.Contains(t, details, "5 consecutive steps with no progress")
	require.Contains(t, details, "0 wait call(s)")
	// Only the two SANCTIONED re-reads are counted as such; a refusal is its
	// own class, so the three refused windows do not inflate it.
	require.Contains(t, details, "2 already-read window(s)")
	require.Contains(t, details, "delegate the remaining change with the `agent` tool")
}
