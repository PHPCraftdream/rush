package batchexec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/stretchr/testify/require"
)

type bindingProbe struct {
	stopErr, disposeErr error
}

func (p *bindingProbe) Stop(context.Context) error    { return p.stopErr }
func (p *bindingProbe) Dispose(context.Context) error { return p.disposeErr }

type bindingOutput struct{ jobbatch.Execution }

func (bindingOutput) ReadOutput(_ context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return jobbatch.OutputChunk{Next: cursor}, nil
}

type bindingMessenger struct{ jobbatch.Execution }

func (bindingMessenger) Inject(context.Context, json.RawMessage) error { return nil }

type bindingBoth struct {
	*bindingProbe
	jobbatch.OutputReader
	jobbatch.Messenger
}

func TestBatchBindingExecutionCapabilities(t *testing.T) {
	ctx := t.Context()
	stopErr := errors.New("stop failed")
	base := &bindingProbe{stopErr: stopErr}
	output, messenger := bindingOutput{base}, bindingMessenger{base}
	for _, tc := range []struct {
		name              string
		exec              jobbatch.Execution
		output, messenger bool
	}{
		{"neither", base, false, false},
		{"output", output, true, false},
		{"messenger", messenger, false, true},
		{"both", bindingBoth{base, output, messenger}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := 0
			exec := WithBindingLease(tc.exec, func() { releases++ })
			require.ErrorIs(t, exec.Stop(ctx), stopErr)
			require.Zero(t, releases)
			reader, ok := exec.(jobbatch.OutputReader)
			require.Equal(t, tc.output, ok)
			if ok {
				chunk, err := reader.ReadOutput(ctx, 42)
				require.NoError(t, err)
				require.EqualValues(t, 42, chunk.Next)
			}
			injector, ok := exec.(jobbatch.Messenger)
			require.Equal(t, tc.messenger, ok)
			if ok {
				require.NoError(t, injector.Inject(ctx, nil))
			}
			disposer := exec.(jobbatch.Disposer)
			require.NoError(t, disposer.Dispose(ctx))
			require.NoError(t, disposer.Dispose(ctx))
			require.Equal(t, 1, releases)
		})
	}
}

func TestBatchBindingExecutionRetriesDisposalBeforeRelease(t *testing.T) {
	physicalErr := errors.New("physical cleanup failed")
	base := &bindingProbe{disposeErr: physicalErr}
	releases := 0
	exec := WithBindingLease(base, func() { releases++ }).(jobbatch.Disposer)
	require.ErrorIs(t, exec.Dispose(t.Context()), physicalErr)
	require.Zero(t, releases)
	base.disposeErr = nil
	require.NoError(t, exec.Dispose(t.Context()))
	require.Equal(t, 1, releases)
}
