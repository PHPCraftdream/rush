package gate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalizationCompletionBeforeWaitDoesNotLoseItsWake(t *testing.T) {
	t.Parallel()
	gate := New(11)
	release, err := gate.AdmitMutation(true)
	require.NoError(t, err)
	release()
	verdict, changed, err := gate.WaitState(t.Context(), quietFacts)
	require.NoError(t, err)
	require.Equal(t, Compose, verdict)
	require.Nil(t, changed, "already-quiet work must not arm a channel waiting for an event that already happened")
}

func TestFinalizationCompletionAfterWaitClosesTheCapturedSignal(t *testing.T) {
	t.Parallel()
	gate := New(12)
	release, err := gate.AdmitMutation(true)
	require.NoError(t, err)
	verdict, changed, err := gate.WaitState(t.Context(), quietFacts)
	require.NoError(t, err)
	require.Equal(t, WaitWork, verdict)
	require.NotNil(t, changed)
	release()
	select {
	case <-changed:
	default:
		t.Fatal("the exact signal captured while work was pending did not close on commit/release")
	}
	verdict, changed, err = gate.WaitState(t.Context(), quietFacts)
	require.NoError(t, err)
	require.Equal(t, Compose, verdict)
	require.Nil(t, changed)
}

func TestFinalizationObservedWorkCannotRetroactivelyAcceptAnOldCandidate(t *testing.T) {
	t.Parallel()
	gate := New(14)
	old, err := gate.BeginCandidate(t.Context(), quietFacts)
	require.NoError(t, err)
	verdict, _, err := gate.WaitState(t.Context(), func(context.Context) (Facts, error) {
		return Facts{Complete: true, Own: 1}, nil
	})
	require.NoError(t, err)
	require.Equal(t, WaitWork, verdict)
	verdict, err = gate.TrySeal(t.Context(), old, Candidate{MessageID: "before-work", Hash: "old"}, quietFacts, func(context.Context, Candidate) error {
		t.Error("an answer observed before live work was reused as the settled final")
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, Compose, verdict)
}

func TestFinalizationCancellationWakesAPendingOwner(t *testing.T) {
	t.Parallel()
	gate := New(13)
	release, err := gate.AdmitMutation(true)
	require.NoError(t, err)
	defer release()
	_, changed, err := gate.WaitState(t.Context(), quietFacts)
	require.NoError(t, err)
	gate.Cancel()
	select {
	case <-changed:
	default:
		t.Fatal("cancellation did not wake the owner waiting for scope changes")
	}
}
