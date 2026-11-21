package batchexec

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

type argvDisposalProbe struct {
	ArgvHandle
	disposals atomic.Int32
	dispose   func(context.Context) error
}

func (p *argvDisposalProbe) Dispose(ctx context.Context) error {
	p.disposals.Add(1)
	return p.dispose(ctx)
}

type shellDisposalProbe struct {
	ShellHandle
	entered, release chan struct{}
	disposals        int
}

func (h *shellDisposalProbe) Stop(context.Context) error {
	close(h.entered)
	<-h.release
	return nil
}

func (h *shellDisposalProbe) Dispose(context.Context) error {
	h.disposals++
	return nil
}

func TestBatchArgvDisposeOneShotLatePhysicalRelease(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{})
	p := &argvDisposalProbe{dispose: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	e := &ArgvExecution{process: p, finished: make(chan struct{})}
	close(e.finished) // Family has reported Run + Done before its one cleanup call.
	answer := make(chan error, 1)
	go func() { answer <- e.Dispose(t.Context()) }()
	<-entered
	close(release)
	require.NoError(t, <-answer)
	require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvDisposed)
	require.EqualValues(t, 1, p.disposals.Load())
}

func TestBatchShellDisposeWaitsPhysicalCall(t *testing.T) {
	h := &shellDisposalProbe{entered: make(chan struct{}), release: make(chan struct{})}
	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &ShellExecution{handle: h, cancel: cancel, finished: make(chan struct{})}
	stopped := make(chan error, 1)
	go func() { stopped <- e.Stop(t.Context()) }()
	<-h.entered
	close(e.finished)
	disposing := make(chan error, 1)
	go func() { disposing <- e.Dispose(t.Context()) }()
	select {
	case <-disposing:
		t.Error("DISPOSE_PASSED_ACTIVE_STOP")
	case <-time.After(50 * time.Millisecond):
	}
	close(h.release)
	require.NoError(t, <-stopped)
	require.NoError(t, <-disposing)
	require.Equal(t, 1, h.disposals)
}
