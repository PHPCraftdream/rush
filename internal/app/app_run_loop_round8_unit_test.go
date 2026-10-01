package app

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// R8A-1: a loop exit without an envelope (the first turn launched but returned
// none) still records how the run ended: the turn's own write is gone, so the
// exit derives the reason from the error. A refused first turn (nothing ran)
// writes nothing.
//
// Revert-check: persistEndedReason returning early for a nil envelope leaves
// the row empty in the first case.
func TestPersistEndedReason_NoEnvelopeDerivesTheReasonFromTheError(t *testing.T) {
	cases := []struct {
		name      string
		submitted bool
		err       error
		ctxDone   bool
		want      string
	}{
		{name: "launched turn interrupted by Ctrl-C", submitted: true, err: context.Canceled, ctxDone: true, want: "canceled"},
		{name: "launched turn, deadline", submitted: true, err: context.DeadlineExceeded, want: "canceled"},
		{name: "launched turn, plain failure", submitted: true, err: errors.New("boom"), want: "error"},
		{name: "launched turn, incomplete run", submitted: true, err: &runIncompleteError{reason: "max_tokens"}, want: "max_tokens"},
		{name: "refused first turn wrote nothing", submitted: false, err: errors.New("busy"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
				loopText(w, "a", "R8-X", 1, 1)
			})
			ctx, cancel := context.WithCancel(t.Context())
			if tc.ctxDone {
				cancel()
			}
			defer cancel()
			l := &cliLoop{app: h.app, ctx: ctx, sessionID: h.sessionID, firstSubmitted: tc.submitted}

			l.persistEndedReason(nil, tc.err)

			require.Equal(t, tc.want, sessionEndedReason(t, h.app, h.sessionID))
		})
	}
}

// countingSessions counts the reads a cap/cancel check costs.
type countingSessions struct {
	session.Service
	gets, cancelReads, budgetReads atomic.Int32
	sess                           session.Session
}

func (c *countingSessions) Get(context.Context, string) (session.Session, error) {
	c.gets.Add(1)
	return c.sess, nil
}

func (c *countingSessions) IsCancelRequested(context.Context, string) (bool, error) {
	c.cancelReads.Add(1)
	return c.sess.CancelRequested, nil
}

func (c *countingSessions) SubtreeBudget(context.Context, string) (float64, error) {
	c.budgetReads.Add(1)
	return c.sess.OwnCost, nil
}

// R8B-3: the check per wake is ONE session-row read for the cancel flag and
// the token snapshots; the SUBTREE budget (#1130) is read only when a
// max-cost is actually set (the budget, not the row's own ledger, is what
// --max-cost compares). Without a cap only the cheap flag read runs.
//
// Revert-check: reading the flag separately (IsCancelRequested next to Get)
// makes cancelReads 1 in the capped case; reading the budget when no
// max-cost is set makes budgetReads 1 in the "no caps" case.
func TestStopError_OneReadPerWake(t *testing.T) {
	t.Run("caps set", func(t *testing.T) {
		svc := &countingSessions{sess: session.Session{OwnCost: 0.1}}
		l := &cliLoop{app: &App{Sessions: svc}, ctx: t.Context(), sessionID: "s", overrides: RunOverrides{MaxCost: 5}}

		require.NoError(t, l.stopError())

		require.EqualValues(t, 1, svc.gets.Load())
		require.EqualValues(t, 1, svc.budgetReads.Load())
		require.EqualValues(t, 0, svc.cancelReads.Load())
	})
	t.Run("caps set, cancel requested", func(t *testing.T) {
		svc := &countingSessions{sess: session.Session{CancelRequested: true}}
		l := &cliLoop{app: &App{Sessions: svc}, ctx: t.Context(), sessionID: "s", overrides: RunOverrides{MaxTokens: 5}}

		var inc *runIncompleteError
		require.ErrorAs(t, l.stopError(), &inc)
		require.Equal(t, "canceled", inc.reason)
		require.EqualValues(t, 1, svc.gets.Load())
		require.EqualValues(t, 0, svc.budgetReads.Load(), "a token cap never reads the budget")
		require.EqualValues(t, 0, svc.cancelReads.Load())
	})
	t.Run("no caps", func(t *testing.T) {
		svc := &countingSessions{sess: session.Session{}}
		l := &cliLoop{app: &App{Sessions: svc}, ctx: t.Context(), sessionID: "s"}

		require.NoError(t, l.stopError())

		require.EqualValues(t, 0, svc.gets.Load())
		require.EqualValues(t, 0, svc.budgetReads.Load())
		require.EqualValues(t, 1, svc.cancelReads.Load())
	})
}
