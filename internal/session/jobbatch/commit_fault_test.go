package jobbatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommitTimeoutCancelsBeforeFaultPublicationAndFreezesDetails(t *testing.T) {
	t.Parallel()
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHook)
	returned := make(chan struct{}, 1)
	r, executor, clock, ctx := runtimeTestCommitHooks(t, func(ctx context.Context, _ Transition) error {
		MarkCommitPhase(ctx, CommitPhaseAcquire)
		entered <- ctx
		<-release
		MarkCommitPhase(ctx, CommitPhaseCommit)
		return nil
	}, &runnerRequestHooks{commitReturned: func() { returned <- struct{}{} }})
	t.Cleanup(func() {
		releaseHook()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Close(closeCtx)
	})
	reply := make(chan error, 1)
	go func() {
		_, err := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
		reply <- err
	}()
	hookContext := runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, clock.armed)
	timerReturned := make(chan struct{})
	// Hold fault publication so the waiting committer cannot run its deferred cancel.
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		go func() {
			clock.advance(time.Second)
			close(timerReturned)
		}()
		runtimeTestReceive(t, ctx, hookContext.Done())
		require.ErrorIs(t, hookContext.Err(), context.Canceled)
		select {
		case <-r.Faulted():
			t.Fatal("fault became visible before the publication barrier was released")
		default:
		}
	}()
	runtimeTestReceive(t, ctx, timerReturned)
	runtimeTestReceive(t, ctx, r.Faulted())
	var fault *CommitFault
	require.ErrorAs(t, r.FaultErr(), &fault)
	details := fault.Details()
	require.Equal(t, "timeout", details.Kind)
	require.EqualValues(t, 1, details.Sequence)
	require.Equal(t, EventSubmit, details.Event)
	require.Equal(t, time.Second, details.Budget)
	require.Equal(t, CommitPhaseAcquire, details.Phase)
	require.GreaterOrEqual(t, details.Elapsed, details.PhaseElapsed)
	require.GreaterOrEqual(t, details.PhaseElapsed, time.Duration(0))
	releaseHook()
	runtimeTestReceive(t, ctx, returned)
	require.Equal(t, details, fault.Details(), "late progress must not rewrite the first fault")
	require.ErrorIs(t, runtimeTestReceive(t, ctx, reply), ErrCommitFailed)
	page, err := r.Events(ctx, 1)
	require.ErrorIs(t, err, ErrCommitFailed)
	require.Zero(t, page.Head)
	require.Empty(t, executor.starts, "late success cannot dispatch work")
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
}

func TestCommitFaultPreservesCauseWithoutExposingContent(t *testing.T) {
	t.Parallel()
	secret := errors.New("secret-hook-payload")
	for _, kind := range []string{"error", "panic"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r, executor, _, ctx := runtimeTestCommitHooks(t, func(ctx context.Context, _ Transition) error {
				MarkCommitPhase(ctx, CommitPhaseStatements)
				if kind == "panic" {
					panic(secret.Error())
				}
				return secret
			}, nil)
			t.Cleanup(func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = r.Close(closeCtx)
			})
			_, err := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
			require.ErrorIs(t, err, ErrCommitFailed)
			var fault *CommitFault
			require.ErrorAs(t, err, &fault)
			require.Equal(t, kind, fault.Details().Kind)
			require.Equal(t, CommitPhaseStatements, fault.Details().Phase)
			require.Equal(t, "jobbatch: commit failed: "+kind, err.Error())
			require.NotContains(t, err.Error(), secret.Error())
			if kind == "error" {
				require.ErrorIs(t, err, secret)
			} else {
				require.NotErrorIs(t, err, secret)
			}
			require.Empty(t, executor.starts)
			require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
		})
	}
}

func TestCommitPhaseRejectsUnboundedLabelsAndIsOptional(t *testing.T) {
	t.Parallel()
	MarkCommitPhase(context.Background(), CommitPhaseCommit)
	progress := newCommitProgress()
	ctx := context.WithValue(context.Background(), commitProgressKey{}, progress)
	MarkCommitPhase(ctx, CommitPhase("secret-payload-as-phase"))
	fault := progress.fault("timeout", Transition{}, time.Second, nil)
	require.Equal(t, CommitPhaseWaitingHook, fault.Details().Phase)
	MarkCommitPhase(ctx, CommitPhasePrepare)
	require.Equal(t, CommitPhasePrepare, progress.fault("timeout", Transition{}, time.Second, nil).Details().Phase)
	require.Equal(t, CommitPhaseWaitingHook, fault.Details().Phase)
}

func TestCommitTimeoutDoesNotAdoptLateError(t *testing.T) {
	t.Parallel()
	late := errors.New("private-late-error")
	entered := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{}, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	r, _, clock, ctx := runtimeTestCommitHooks(t, func(ctx context.Context, _ Transition) error {
		MarkCommitPhase(ctx, CommitPhasePrepare)
		close(entered)
		<-release
		MarkCommitPhase(ctx, CommitPhaseStatements)
		return late
	}, &runnerRequestHooks{commitReturned: func() { returned <- struct{}{} }})
	t.Cleanup(func() {
		unblock()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Close(closeCtx)
	})
	reply := make(chan error, 1)
	go func() {
		_, err := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
		reply <- err
	}()
	runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, clock.armed)
	clock.advance(time.Second)
	err := runtimeTestReceive(t, ctx, reply)
	var fault *CommitFault
	require.ErrorAs(t, err, &fault)
	details := fault.Details()
	unblock()
	runtimeTestReceive(t, ctx, returned)
	require.Same(t, err, r.FaultErr())
	require.Equal(t, details, fault.Details())
	require.Equal(t, "timeout", details.Kind)
	require.Equal(t, CommitPhasePrepare, details.Phase)
	require.NotErrorIs(t, err, late, "an unobserved late error is not the timeout's cause")
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
}
