package gate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func quietFacts(context.Context) (Facts, error) {
	return Facts{Complete: true}, nil
}

func TestFinalizationQuietCandidateAcceptsExactlyOnce(t *testing.T) {
	t.Parallel()
	gate := New(1)
	stamp, err := gate.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	require.True(t, stamp.ready)
	require.Zero(t, stamp.revision, "a new gate has its own local counter")
	var published int
	publish := func(context.Context, Candidate) error { published++; return nil }
	candidate := Candidate{MessageID: "message", Hash: "hash"}
	verdict, err := gate.TrySeal(t.Context(), stamp, candidate, quietFacts, publish)
	require.NoError(t, err)
	require.Equal(t, Accepted, verdict, "a never-busy owner needs no extra composition")
	require.Equal(t, 1, published)
	_, err = gate.TrySeal(t.Context(), stamp, candidate, quietFacts, publish)
	require.ErrorIs(t, err, ErrFinalizationSealed)
	require.Equal(t, 1, published)
}

func TestFinalizationMutationInvalidatesTheEarlierCandidate(t *testing.T) {
	t.Parallel()
	gate := New(2)
	stamp, err := gate.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	release, err := gate.AdmitMutation(true)
	require.NoError(t, err)
	var published int
	publish := func(context.Context, Candidate) error { published++; return nil }
	candidate := Candidate{MessageID: "before-work", Hash: "old"}
	verdict, err := gate.TrySeal(t.Context(), stamp, candidate, quietFacts, publish)
	require.NoError(t, err)
	require.Equal(t, WaitWork, verdict, "an unfinished commit is live even before its row becomes visible")
	require.Zero(t, published)
	release()
	release() // Physical release callbacks are allowed to repeat.
	verdict, err = gate.TrySeal(t.Context(), stamp, candidate, quietFacts, publish)
	require.NoError(t, err)
	require.Equal(t, Compose, verdict, "quiet cannot retroactively validate an answer produced before the results")
	require.Zero(t, published)
	fresh, err := gate.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	require.Greater(t, fresh.revision, stamp.revision)
	verdict, err = gate.TrySeal(t.Context(), fresh, Candidate{MessageID: "after-work", Hash: "new"}, quietFacts, publish)
	require.NoError(t, err)
	require.Equal(t, Accepted, verdict)
	require.Equal(t, 1, published)
	require.Equal(t, "after-work", gate.accepted.MessageID)
}

func TestFinalizationUnknownFactsNeverMeanQuiet(t *testing.T) {
	t.Parallel()
	for _, read := range []func(context.Context) (Facts, error){
		nil,
		func(context.Context) (Facts, error) { return Facts{}, nil },
		func(context.Context) (Facts, error) {
			return Facts{Complete: true, Debt: -1}, nil
		},
		func(context.Context) (Facts, error) {
			return Facts{}, errors.New("fixture read failure")
		},
	} {
		gate := New(3)
		_, err := gate.BeginCandidate(t.Context(), read)
		require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
	}
}

func TestFinalizationHeldQuestionIsPauseNotSuccess(t *testing.T) {
	t.Parallel()
	gate := New(4)
	read := func(context.Context) (Facts, error) {
		return Facts{Complete: true, Own: 1, Debt: 1, HeldQuestion: true}, nil
	}
	stamp, err := gate.BeginCandidate(t.Context(), read)
	require.NoError(t, err)
	require.False(t, stamp.ready, "a held question is not quiet")
	verdict, err := gate.TrySeal(t.Context(), stamp, Candidate{MessageID: "question", Hash: "question"}, read, func(context.Context, Candidate) error {
		t.Error("a held question was published as final success")
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, PauseQuestion, verdict)
	require.False(t, gate.sealed)
}

func TestFinalizationRejectsForeignAndUnstartedCandidates(t *testing.T) {
	t.Parallel()
	first, second := New(7), New(7)
	foreign, err := first.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	_, err = second.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	for _, stamp := range []Stamp{{}, foreign} {
		verdict, err := second.TrySeal(t.Context(), stamp, Candidate{MessageID: "wrong-owner", Hash: "hash"}, quietFacts, func(context.Context, Candidate) error {
			t.Error("a candidate from another owner was published")
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, Compose, verdict)
	}
}

func TestFinalizationPublicationFailureIsSticky(t *testing.T) {
	t.Parallel()
	gate := New(8)
	stamp, err := gate.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	failure := errors.New("fixture publication uncertainty")
	var calls int
	publish := func(context.Context, Candidate) error { calls++; return failure }
	candidate := Candidate{MessageID: "message", Hash: "hash"}
	_, err = gate.TrySeal(t.Context(), stamp, candidate, quietFacts, publish)
	require.ErrorIs(t, err, failure)
	_, err = gate.AdmitMutation(true)
	require.ErrorIs(t, err, failure, "an uncertain publish cannot reopen the old owner's admission")
	_, err = gate.TrySeal(t.Context(), stamp, candidate, quietFacts, publish)
	require.ErrorIs(t, err, failure)
	require.Equal(t, 1, calls, "uncertain publication is not retried blindly")
	require.False(t, gate.sealed)
}

func TestFinalizationCancellationCannotBecomeSuccess(t *testing.T) {
	t.Parallel()
	gate := New(9)
	stamp, err := gate.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	gate.Cancel()
	_, err = gate.TrySeal(t.Context(), stamp, Candidate{MessageID: "message", Hash: "hash"}, quietFacts, func(context.Context, Candidate) error {
		t.Error("cancelled owner published a successful final")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	_, err = gate.AdmitMutation(true)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFinalizationSealLinearizesAgainstNewAdmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	gate := New(10)
	stamp, err := gate.BeginCandidate(ctx, quietFacts)
	require.NoError(t, err)
	publishing, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releasePublish := func() { releaseOnce.Do(func() { close(release) }) }
	defer releasePublish()
	sealed := make(chan error, 1)
	go func() {
		_, err := gate.TrySeal(ctx, stamp, Candidate{MessageID: "message", Hash: "hash"}, quietFacts, func(context.Context, Candidate) error {
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
		release, err := gate.AdmitMutation(true)
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

func TestFinalizationRefusesUnrepresentableDurableRevision(t *testing.T) {
	t.Parallel()
	gate := New(1)
	_, err := gate.BeginCandidate(t.Context(), func(context.Context) (Facts, error) {
		return Facts{Complete: true, DurableRevision: uint64(1) << 63}, nil
	})
	require.ErrorIs(t, err, ErrFinalizationFactsUnavailable)
}

func TestFinalizationStampKeepsLocalAndDurableRevisionsSeparate(t *testing.T) {
	t.Parallel()
	g := New(7)
	durable := uint64(2)
	read := func(context.Context) (Facts, error) { return Facts{Complete: true, DurableRevision: durable}, nil }
	first, err := g.BeginCandidate(t.Context(), read)
	require.NoError(t, err)
	require.Zero(t, first.revision)
	require.EqualValues(t, 2, first.DurableRevision())
	durable = 4
	verdict, err := g.TrySeal(t.Context(), first, Candidate{MessageID: "message", Hash: "hash"}, read, func(context.Context, Candidate) error {
		t.Error("published after the durable revision changed")
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, Compose, verdict)
	fresh, err := g.BeginCandidate(t.Context(), read)
	require.NoError(t, err)
	require.Zero(t, fresh.revision)
	require.EqualValues(t, 4, fresh.DurableRevision())
}
