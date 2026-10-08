// R4C-2: the run's cost is the session's cost delta between the driver claim
// and the exit, not the sum of what its own turns spent (docs/reviews/
// 2026-09-30-async-phase4-round4.md).
package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const r4ChildCost = 1.0 // 100000 prompt tokens at $10 per million

// newDelegatingCostHarness scripts a root that delegates to an async `agent`
// child whose provider call spends r4ChildCost. The child's request is held
// until the root's first turn returned, so the child's charge on the parent
// (TransferChildCostToParent) lands BETWEEN turns, as it always does for a
// child that outlives the turn that started it.
func newDelegatingCostHarness(t *testing.T) (h *loopHarness, release func()) {
	t.Helper()
	gate := make(chan struct{})
	h = newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "Async job call-agent (agent)"):
			loopText(w, "root-final", "R4-ROOT-DONE", 11, 3)
		case strings.Contains(lastUser, "Generate a concise title"):
			// The child's title request quotes the worker prompt. It must not
			// wait on the gate or spend: a second dollar landing apart from
			// the work request made the run's exit reading race the total.
			loopText(w, "child-title", "child title", 0, 0)
		case strings.Contains(lastUser, "WORKER-MARKER"):
			select {
			case <-gate:
			case <-h.t.Context().Done():
			}
			loopText(w, "child", "child done", 100000, 0)
		case strings.Contains(lastTool, "Async agent job call-agent started"):
			loopText(w, "root-yield", "root waiting", 11, 3)
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER build it"}`),
				admissionSSEStop("delegate", "tool_calls"),
			})
		}
	})
	r4Models(t, h.app, 10)
	return h, func() { close(gate) }
}

func (h *loopHarness) sessionCost(t *testing.T) float64 {
	t.Helper()
	// #1130: "the session's cost" for a running loop is the subtree budget
	// (own + delegation children), the quantity the envelope windows over.
	spent, err := h.app.Sessions.SubtreeSpent(context.Background(), h.sessionID)
	require.NoError(t, err)
	return spent
}

// The async child's spend is charged to the root between its turns; the run's
// envelope (and RUSH_COST_USD for --on-finish) must include it. Before the fix
// the cost was the sum of the turns' own deltas: the child's dollar was in the
// session's total but not in delta_cost_usd.
//
// Revert-check: summing the turns' cost deltas again (loopTotals.cost) drops
// the child: delta_cost_usd stays at the root's own fraction of a cent.
func TestRunNonInteractive_CostIncludesAsyncChildSpentBetweenTurns(t *testing.T) {
	h, release := newDelegatingCostHarness(t)
	h.afterFirstTurn(release)

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err)
	require.NotNil(t, res)
	total := h.sessionCost(t)
	require.GreaterOrEqual(t, total, r4ChildCost, "the child's spend reached the root")
	require.InDelta(t, total, res.Usage.DeltaCostUSD, 1e-9, "the run's cost is the session's cost delta over the run")
}

// A cap crossed by the child's spend ends the run at the Drain precheck, and
// the envelope that says why reports the same dollars the error names.
//
// Revert-check: as above -- the envelope reports $0.0003 next to an error that
// says the run cost more than $1.
func TestRunNonInteractive_MaxCostExitEnvelopeReportsTheChildSpend(t *testing.T) {
	h, release := newDelegatingCostHarness(t)
	h.afterFirstTurn(release)

	res, _, err := h.run(loopCtx(t), RunOverrides{MaxCost: 0.5})

	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds max")
	require.NotNil(t, res)
	require.Equal(t, "error", res.ExitReason)
	require.InDelta(t, h.sessionCost(t), res.Usage.DeltaCostUSD, 1e-9)
	require.GreaterOrEqual(t, res.Usage.DeltaCostUSD, r4ChildCost)
	require.Less(t, res.Usage.DeltaCostUSD, 1.5*r4ChildCost, "the child spends once; a second request would race the exit reading")
}

// Decision (R4C-2): a foreign human turn's spend on the root -- a web tab's
// message on the driven session, another process's queued turn -- is session
// spend inside the run's window, so it counts, exactly like a queued
// iteration's did (queuedMark) and like the non-loop on-finish hook's cost
// (session cost at exit minus session cost at start). Cost is path-independent.
//
// Revert-check: summing the turns' cost deltas drops the foreign dollars
// because the Drain reads its own start mark after them.
func TestRunNonInteractive_CostIncludesForeignSpendOnTheRoot(t *testing.T) {
	const foreign = 2.0
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			loopText(w, "d", "reacting", 30, 10)
			return
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	r4Models(t, h.app, 10)
	h.afterFirstTurn(func() {
		_, err := h.app.Sessions.IncrementCost(context.Background(), h.sessionID, foreign)
		require.NoError(t, err)
		h.seedDebt()
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.GreaterOrEqual(t, res.Usage.DeltaCostUSD, foreign)
	require.InDelta(t, h.sessionCost(t), res.Usage.DeltaCostUSD, 1e-9)
}
