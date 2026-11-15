package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/interp"
)

// TestPanicGuardHandler exercises panicGuardHandler through the real
// standardHandlers chain, using the testInnermostHandler seam in run.go to
// install a fake command behind the full middleware stack (panic guard,
// builtins, script dispatch, block list, xargs normaliser, Go coreutils).
//
// Pipeline positions: in `a | b` the LEFT stage runs via wg.Go in its own
// goroutine (interp/runner.go:498-506), while the RIGHT stage runs
// synchronously in the caller's goroutine — so both positions must be
// covered. The pass-through subtests are observable through Run because
// mvdan returns handler errors verbatim (interp/api.go:228-242
// fromHandlerError and the Run return path at interp/api.go:911-944).
//
// Deleting panicGuardHandler() from standardHandlers would make the
// goroutine-stage subtests die with the process (the panic escapes the
// wg.Go goroutine) and the synchronous-stage subtests fail on the
// ExitStatus(2) equality, because the panic instead unwinds into Run's own
// recover and is returned as a plain error.
func TestPanicGuardHandler(t *testing.T) {
	// No t.Parallel anywhere in this file: the seam mutates package state.

	commands := map[string]interp.ExecHandlerFunc{
		// boom panics with a sentinel value, as a broken exec handler would.
		"boom": func(context.Context, []string) error {
			panic("sentinel panic value")
		},
		// drain is the successful consumer of a pipeline: it reads its
		// stdin to EOF and succeeds, so a failing left stage is visible
		// only through pipefail propagation.
		"drain": func(ctx context.Context, _ []string) error {
			_, err := io.Copy(io.Discard, interp.HandlerCtx(ctx).Stdin)
			return err
		},
	}

	t.Run("panic in left pipeline stage (goroutine)", func(t *testing.T) {
		stderr, err := runThroughProductionChain(t, commands, "set -o pipefail; boom | drain")
		require.Equal(t, interp.ExitStatus(2), err)
		require.Contains(t, stderr.String(), "boom: internal error: sentinel panic value")
	})

	t.Run("panic in left stage of 3-stage pipeline", func(t *testing.T) {
		stderr, err := runThroughProductionChain(t, commands, "set -o pipefail; boom | drain | drain")
		require.Equal(t, interp.ExitStatus(2), err)
		require.Contains(t, stderr.String(), "boom: internal error: sentinel panic value")
	})

	t.Run("panic in right pipeline stage", func(t *testing.T) {
		stderr, err := runThroughProductionChain(t, commands, "echo hi | boom")
		require.Equal(t, interp.ExitStatus(2), err)
		require.Contains(t, stderr.String(), "boom: internal error: sentinel panic value")
	})

	t.Run("panic in plain command", func(t *testing.T) {
		stderr, err := runThroughProductionChain(t, commands, "boom")
		require.Equal(t, interp.ExitStatus(2), err)
		require.Contains(t, stderr.String(), "boom: internal error: sentinel panic value")
	})

	t.Run("exit status passes through", func(t *testing.T) {
		handler := func(context.Context, []string) error {
			return interp.ExitStatus(7)
		}
		stderr, err := runThroughProductionChain(t, map[string]interp.ExecHandlerFunc{"boom": handler}, "boom")
		require.Equal(t, interp.ExitStatus(7), err)
		require.NotContains(t, stderr.String(), "internal error")
	})

	t.Run("plain error passes through", func(t *testing.T) {
		sentinel := errors.New("plain handler error")
		handler := func(context.Context, []string) error {
			return sentinel
		}
		stderr, err := runThroughProductionChain(t, map[string]interp.ExecHandlerFunc{"boom": handler}, "boom")
		require.Equal(t, sentinel, err)
		require.NotContains(t, stderr.String(), "internal error")
	})

	t.Run("cancelled context passes through", func(t *testing.T) {
		handler := func(context.Context, []string) error {
			return context.Canceled
		}
		_, err := runThroughProductionChain(t, map[string]interp.ExecHandlerFunc{"boom": handler}, "boom")
		require.ErrorIs(t, err, context.Canceled)
	})
}

// runThroughProductionChain installs a test-only innermost exec handler that
// dispatches by argv[0] to commands, and runs command through the real [Run]
// middleware chain, returning the command's stderr buffer and Run's error.
func runThroughProductionChain(t *testing.T, commands map[string]interp.ExecHandlerFunc, command string) (*bytes.Buffer, error) {
	t.Helper()
	if testInnermostHandler != nil {
		t.Fatal("testInnermostHandler is already set")
	}
	testInnermostHandler = func(ctx context.Context, args []string) error {
		if handler, ok := commands[args[0]]; ok {
			return handler(ctx, args)
		}
		return fmt.Errorf("fake chain: unknown command %q", args[0])
	}
	t.Cleanup(func() { testInnermostHandler = nil })

	var stderr bytes.Buffer
	err := Run(t.Context(), RunOptions{
		Command: command,
		Cwd:     t.TempDir(),
		Stderr:  &stderr,
	})
	return &stderr, err
}
