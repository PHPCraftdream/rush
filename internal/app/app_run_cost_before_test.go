package app

// C9-9/C9-11: the run's cost (the JSON envelope's delta and the --on-finish
// hook's RUSH_COST_USD) is the session's cost delta over the run, and that
// window only exists when BOTH the pre-run and the post-run SubtreeSpent
// reads succeed. When the before-read fails, the delta reports ZERO instead
// of silently carrying the session's whole historical spend.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// failingSubtreeSessions fails the FIRST SubtreeSpent read after arming,
// then delegates: it stands in for a DB read failing while the run's
// before-cost is taken (C9-9/C9-11).
type failingSubtreeSessions struct {
	session.Service
	mu    sync.Mutex
	armed bool
	fails int // number of SubtreeSpent calls that must still fail
}

func (s *failingSubtreeSessions) arm(n int) {
	s.mu.Lock()
	s.armed = true
	s.fails = n
	s.mu.Unlock()
}

func (s *failingSubtreeSessions) SubtreeSpent(ctx context.Context, sessionID string) (float64, error) {
	s.mu.Lock()
	if s.armed && s.fails > 0 {
		s.fails--
		s.mu.Unlock()
		return 0, errors.New("boom: subtree read failed")
	}
	s.mu.Unlock()
	return s.Service.SubtreeSpent(ctx, sessionID)
}

// REVERT CHECK: restoring the old behaviour (deltaCost = spentNow - costBefore
// whenever only the after-read succeeds; hookCost = spentNow - costBefore
// unconditionally) makes the envelope and the hook report the session's WHOLE
// historical spend ($5 seeded below) as this run's cost; this test FAILED
// with delta_cost_usd == 5.00xxxx and RUSH_COST_USD 5.00xxxx.
func TestRunLoop_CostBeforeReadFailureReportsZeroDelta(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "COST-PROBE", 11, 3)
	})
	r4Models(t, h.app, 10)
	_, err := h.app.Sessions.IncrementCost(context.Background(), h.sessionID, 5.0)
	require.NoError(t, err)

	spy := &failingSubtreeSessions{Service: h.app.Sessions}
	spy.arm(1)
	h.app.Sessions = spy

	marker := filepath.Join(t.TempDir(), "cost.txt")
	hook := "echo $RUSH_COST_USD > " + marker
	if runtime.GOOS == "windows" {
		hook = "echo %RUSH_COST_USD%> " + marker
	}

	res, err := h.app.ExecuteRun(loopCtx(t), RunRequest{
		Prompt:            "cost probe",
		Overrides:         RunOverrides{OnFinishHook: hook},
		Mode:              RunModeJSON,
		ContinueSessionID: h.sessionID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.InDelta(t, 0.0, res.Usage.DeltaCostUSD, 1e-9, "the unknown before-cost reports a zero delta, not the history")
	require.GreaterOrEqual(t, h.sessionCost(t), 5.0, "the seeded history is really there")
	data, readErr := os.ReadFile(marker)
	require.NoError(t, readErr, "--on-finish ran")
	require.Equal(t, "0.000000", strings.TrimSpace(string(data)), "the hook cost is zero, never the history")
}

// Control: with both reads healthy the window is exactly the run's own spend
// (the seeded $5 history stays outside it).
func TestRunLoop_CostBeforeControl_WindowOverTheRun(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "COST-PROBE", 11, 3)
	})
	r4Models(t, h.app, 10)
	_, err := h.app.Sessions.IncrementCost(context.Background(), h.sessionID, 5.0)
	require.NoError(t, err)

	marker := filepath.Join(t.TempDir(), "cost.txt")
	hook := "echo $RUSH_COST_USD > " + marker
	if runtime.GOOS == "windows" {
		hook = "echo %RUSH_COST_USD%> " + marker
	}

	res, err := h.app.ExecuteRun(loopCtx(t), RunRequest{
		Prompt:            "cost probe",
		Overrides:         RunOverrides{OnFinishHook: hook},
		Mode:              RunModeJSON,
		ContinueSessionID: h.sessionID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	total := h.sessionCost(t)
	require.GreaterOrEqual(t, total, 5.0)
	expected := total - 5.0
	require.Greater(t, expected, 0.0, "the run's own spend is priced")
	require.InDelta(t, expected, res.Usage.DeltaCostUSD, 1e-6, "the delta windows over the run, not the history")
	data, readErr := os.ReadFile(marker)
	require.NoError(t, readErr, "--on-finish ran")
	parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	require.NoError(t, parseErr)
	require.InDelta(t, expected, parsed, 1e-4, "the hook sees the same window")
}

// inflatedBeforeSessions adds extra to the FIRST SubtreeSpent read only: a
// before-cost above the after-cost (the subtree shrank mid-run).
type inflatedBeforeSessions struct {
	session.Service
	mu    sync.Mutex
	extra float64
	used  bool
}

func (s *inflatedBeforeSessions) SubtreeSpent(ctx context.Context, sessionID string) (float64, error) {
	v, err := s.Service.SubtreeSpent(ctx, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && !s.used {
		s.used = true
		v += s.extra
	}
	return v, err
}

// REVERT CHECK: without the max(..., 0) clamp on the hook's window a cost that
// moved backwards reaches the hook as a negative number (the envelope already
// clamps its own delta).
func TestRunLoop_HookCostNeverNegativeWhenTheSubtreeShrinks(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "COST-PROBE", 11, 3)
	})
	r4Models(t, h.app, 10)
	_, err := h.app.Sessions.IncrementCost(context.Background(), h.sessionID, 5.0)
	require.NoError(t, err)
	h.app.Sessions = &inflatedBeforeSessions{Service: h.app.Sessions, extra: 100}

	marker := filepath.Join(t.TempDir(), "cost.txt")
	hook := "echo $RUSH_COST_USD > " + marker
	if runtime.GOOS == "windows" {
		hook = "echo %RUSH_COST_USD%> " + marker
	}
	res, err := h.app.ExecuteRun(loopCtx(t), RunRequest{
		Prompt:            "cost probe",
		Overrides:         RunOverrides{OnFinishHook: hook},
		Mode:              RunModeJSON,
		ContinueSessionID: h.sessionID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.InDelta(t, 0.0, res.Usage.DeltaCostUSD, 1e-9, "the envelope clamps a backwards window to zero")
	data, readErr := os.ReadFile(marker)
	require.NoError(t, readErr, "--on-finish ran")
	require.Equal(t, "0.000000", strings.TrimSpace(string(data)), "the hook cost is clamped to zero, never negative")
}

// The todos tool accepts only pending/in_progress/completed and drops omitted
// items; the prompt must not invent a "cancelled" status (C9-11).
func TestTodoNudgePrompt_NeverNamesCancelled(t *testing.T) {
	require.NotContains(t, todoNudgePrompt, "cancelled")
	require.Contains(t, todoNudgePrompt, "remove each dropped item from the list")
}
