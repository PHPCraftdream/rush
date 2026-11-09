package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalizationCompletionBeforeWaitDoesNotLoseItsWake(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(11)
	release, err := gate.admitMutation(true)
	require.NoError(t, err)
	release()
	verdict, changed, err := gate.waitState(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	require.Equal(t, finalizationCompose, verdict)
	require.Nil(t, changed, "already-quiet work must not arm a channel waiting for an event that already happened")
}

func TestFinalizationCompletionAfterWaitClosesTheCapturedSignal(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(12)
	release, err := gate.admitMutation(true)
	require.NoError(t, err)
	verdict, changed, err := gate.waitState(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	require.Equal(t, finalizationWaitWork, verdict)
	require.NotNil(t, changed)
	release()
	select {
	case <-changed:
	default:
		t.Fatal("the exact signal captured while work was pending did not close on commit/release")
	}
	verdict, changed, err = gate.waitState(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	require.Equal(t, finalizationCompose, verdict)
	require.Nil(t, changed)
}

func TestFinalizationObservedWorkCannotRetroactivelyAcceptAnOldCandidate(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(14)
	old, err := gate.beginCandidate(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	verdict, _, err := gate.waitState(t.Context(), func(context.Context) (finalizationFacts, error) {
		return finalizationFacts{complete: true, own: 1}, nil
	})
	require.NoError(t, err)
	require.Equal(t, finalizationWaitWork, verdict)
	verdict, err = gate.trySeal(t.Context(), old, finalizationCandidate{messageID: "before-work", hash: "old"}, quietFinalizationTestFacts, func(context.Context, finalizationCandidate) error {
		t.Error("an answer observed before live work was reused as the settled final")
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, finalizationCompose, verdict)
}

func TestFinalizationCancellationWakesAPendingOwner(t *testing.T) {
	t.Parallel()
	gate := newFinalizationGate(13)
	release, err := gate.admitMutation(true)
	require.NoError(t, err)
	defer release()
	_, changed, err := gate.waitState(t.Context(), quietFinalizationTestFacts)
	require.NoError(t, err)
	gate.cancel()
	select {
	case <-changed:
	default:
		t.Fatal("cancellation did not wake the owner waiting for scope changes")
	}
}
