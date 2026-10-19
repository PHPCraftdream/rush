package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Revert-check: dropping keepContextErr from createSessionWithAdmission lets
// the SDK's torn-down-transport error escape unclassified (the macOS flake of
// TestSessionCancellationClassification/caller_deadline_remains_caller_deadline).
func TestKeepContextErr(t *testing.T) {
	t.Parallel()
	transportErr := errors.New(`connection closed: calling "initialize": use of closed network connection`)
	ownedTimeout := errors.New("mcp initialization timeout")

	t.Run("deadline stays classifiable", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		err := keepContextErr(ctx, transportErr, context.Cause(ctx), ownedTimeout)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, err, transportErr)
	})

	t.Run("cancellation stays classifiable", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := keepContextErr(ctx, transportErr, context.Cause(ctx), ownedTimeout)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("already wrapped is untouched", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		wrapped := errors.Join(context.Canceled, transportErr)
		require.Same(t, wrapped, keepContextErr(ctx, wrapped, context.Cause(ctx), ownedTimeout))
	})

	t.Run("owned initialization timeout keeps its own wording", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(ownedTimeout)
		timedOut := errors.New("timed out after 1s")
		require.Same(t, timedOut, keepContextErr(ctx, timedOut, context.Cause(ctx), ownedTimeout))
	})

	t.Run("live context is untouched", func(t *testing.T) {
		t.Parallel()
		require.Same(t, transportErr, keepContextErr(context.Background(), transportErr, nil, ownedTimeout))
	})
}
