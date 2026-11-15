//go:build windows

package shexec

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/interp"
)

// TestWindowsCommandResultCancellationPrecedence pins the linearization of
// windowsCommandResult: a wait that won before the callback started keeps a
// natural result; a callback that won the wait maps the result to the
// caller's cancellation classification.
func TestWindowsCommandResultCancellationPrecedence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Wait winning before the callback starts preserves a natural result even
	// when the context is canceled immediately afterward.
	require.NoError(t, windowsCommandResult(ctx, io.Discard, nil, false))
	naturalExit := interp.ExitStatus(7)
	require.Equal(t, naturalExit, windowsCommandResult(ctx, io.Discard, naturalExit, false))

	// A callback that won the wait linearization maps the result to the
	// caller's cancellation classification.
	require.ErrorIs(t, windowsCommandResult(ctx, io.Discard, naturalExit, true), context.Canceled)
}
