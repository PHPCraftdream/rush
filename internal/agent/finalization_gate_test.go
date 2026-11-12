package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func quietFinalizationTestFacts(context.Context) (finalizationFacts, error) {
	return finalizationFacts{complete: true}, nil
}

func TestFinalizationQuietCandidateAcceptsExactlyOnce(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(1)
	stamp, err := gate.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	var published int
	publish := func(context.Context, finalizationCandidate) error { published++; return nil }
	candidate := finalizationCandidate{messageID: "message", hash: "hash"}
	verdict, err := gate.trySeal(t.Context(), stamp, candidate, quietFinalizationTestFacts, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationAccepted, verdict, "a never-busy owner needs no extra composition")
	require.Equal(t, 1, published)
	_, err = gate.trySeal(t.Context(), stamp, candidate, quietFinalizationTestFacts, publish)
	require.ErrorIs(t, err, ErrFinalizationSealed)
	require.Equal(t, 1, published)
}

func TestFinalizationMutationInvalidatesTheEarlierCandidate(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(2)
	stamp, err := gate.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	release, err := gate.admitMutation(true)
	require.NoError(t, err)
	var published int
	publish := func(context.Context, finalizationCandidate) error { published++; return nil }
	candidate := finalizationCandidate{messageID: "before-work", hash: "old"}
	verdict, err := gate.trySeal(t.Context(), stamp, candidate, quietFinalizationTestFacts, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationWaitWork, verdict, "an unfinished commit is live even before its row becomes visible")
	require.Zero(t, published)
	release()
	release() // Physical release callbacks are allowed to repeat.
	verdict, err = gate.trySeal(t.Context(), stamp, candidate, quietFinalizationTestFacts, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationCompose, verdict, "quiet cannot retroactively validate an answer produced before the results")
	require.Zero(t, published)
	fresh, err := gate.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	verdict, err = gate.trySeal(t.Context(), fresh, finalizationCandidate{messageID: "after-work", hash: "new"}, quietFinalizationTestFacts, publish)
	require.NoError(t, err)
	require.Equal(t, finalizationAccepted, verdict)
	require.Equal(t, 1, published)
	require.Equal(t, "after-work", gate.accepted.messageID)
}

func TestFinalizationUnknownFactsNeverMeanQuiet(t *testing.T) {
	t.Parallel()
	for _, read := range []func(context.Context) (finalizationFacts, error){
		nil,
		func(context.Context) (finalizationFacts, error) { return finalizationFacts{}, nil },
		func(context.Context) (finalizationFacts, error) {
			return finalizationFacts{complete: true, debt: -1}, nil
		},
		func(context.Context) (finalizationFacts, error) {
			return finalizationFacts{}, errors.New("fixture read failure")
		},
	} {
		gate := newFinalizationGate(3)
		_, err := gate.beginCandidate(t.Context(), read)
		require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
	}
}

func TestFinalizationHeldQuestionIsPauseNotSuccess(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(4)
	read := func(context.Context) (finalizationFacts, error) {
		return finalizationFacts{complete: true, own: 1, debt: 1, heldQuestion: true}, nil
	}
	stamp, err := gate.beginCandidate(t.Context(), read)
	require.NoError(t, err)
	verdict, err := gate.trySeal(t.Context(), stamp, finalizationCandidate{messageID: "question", hash: "question"}, read, func(context.Context, finalizationCandidate) error {
		t.Error("a held question was published as final success")
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, finalizationPauseQuestion, verdict)
	require.False(t, gate.sealed)
}

func TestFinalizationRejectsForeignAndUnstartedCandidates(t *testing.T) {
	t.Parallel()
	first, second := newFinalizationGate(7), newFinalizationGate(7)
	foreign, err := first.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	_, err = second.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	for _, stamp := range []finalizationStamp{{}, foreign} {
		verdict, err := second.trySeal(t.Context(), stamp, finalizationCandidate{messageID: "wrong-owner", hash: "hash"}, quietFinalizationTestFacts, func(context.Context, finalizationCandidate) error {
			t.Error("a candidate from another owner was published")
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, finalizationCompose, verdict)
	}
}

func TestFinalizationPublicationFailureIsSticky(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(8)
	stamp, err := gate.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	failure := errors.New("fixture publication uncertainty")
	var calls int
	publish := func(context.Context, finalizationCandidate) error { calls++; return failure }
	candidate := finalizationCandidate{messageID: "message", hash: "hash"}
	_, err = gate.trySeal(t.Context(), stamp, candidate, quietFinalizationTestFacts, publish)
	require.ErrorIs(t, err, failure)
	_, err = gate.admitMutation(true)
	require.ErrorIs(t, err, failure, "an uncertain publish cannot reopen the old owner's admission")
	_, err = gate.trySeal(t.Context(), stamp, candidate, quietFinalizationTestFacts, publish)
	require.ErrorIs(t, err, failure)
	require.Equal(t, 1, calls, "uncertain publication is not retried blindly")
	require.False(t, gate.sealed)
}

func TestFinalizationCancellationCannotBecomeSuccess(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(9)
	stamp, err := gate.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	gate.cancel()
	_, err = gate.trySeal(t.Context(), stamp, finalizationCandidate{messageID: "message", hash: "hash"}, quietFinalizationTestFacts, func(context.Context, finalizationCandidate) error {
		t.Error("cancelled owner published a successful final")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	_, err = gate.admitMutation(true)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFinalizationSealLinearizesAgainstNewAdmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	gate := newFinalizationGate(10)
	stamp, err := gate.beginCandidate(ctx, quietFinalizationTestFacts)
	require.NoError(t, err)
	publishing, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releasePublish := func() { releaseOnce.Do(func() { close(release) }) }
	defer releasePublish()
	sealed := make(chan error, 1)
	go func() {
		_, err := gate.trySeal(ctx, stamp, finalizationCandidate{messageID: "message", hash: "hash"}, quietFinalizationTestFacts, func(context.Context, finalizationCandidate) error {
			close(publishing)
			<-release
			return nil
		})
		sealed <- err
	}()
	select {
	case <-publishing:
	case <-ctx.Done():
		t.Fatal("publisher did not reach the deterministic seal barrier")
	}
	if gate.mu.TryLock() {
		gate.mu.Unlock()
		t.Error("final publication released the admission gate before its linearization point")
	}
	admitted := make(chan error, 1)
	var sideEffects atomic.Int32
	go func() {
		release, err := gate.admitMutation(true)
		if err == nil {
			sideEffects.Add(1)
			release()
		}
		admitted <- err
	}()
	releasePublish()
	select {
	case err := <-sealed:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("seal did not complete after releasing publication")
	}
	select {
	case err := <-admitted:
		require.ErrorIs(t, err, ErrFinalizationSealed)
	case <-ctx.Done():
		t.Fatal("admission did not complete after the seal")
	}
	require.Zero(t, sideEffects.Load(), "post-seal work must be refused before side effects")
}
