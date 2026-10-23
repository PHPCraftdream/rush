// These tests pin the runOwned turn loop's carried-outcome semantics: a
// trailing refused no-turn Drain leg (agent_turn.go's drain-refusal branch,
// which returns a nil result and a nil error) must never clobber the real
// result or error of an earlier completed turn in the same loop, because
// runOwned's caller maps a bare (nil, nil) to the "queued" contract.
package agent

import (
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestRunOwnedOutcomeCarryWinnerResultSurvivesRefusedNoTurnDrain(t *testing.T) {
	winnerResult := &fantasy.AgentResult{}
	winnerErr := errors.New("winner turn failed")

	t.Run("winner result then refused no-turn drain keeps the result", func(t *testing.T) {
		// Iteration 1: a real completed winner turn, followed by a queued
		// Drain that was observed (hasNext=true).
		carried, carriedErr, replaced := mergeTurnOutcome(nil, nil, winnerResult, nil)
		require.True(t, replaced)
		require.Same(t, winnerResult, carried)
		require.NoError(t, carriedErr)

		// Iteration 2: the refused no-turn Drain leg — nil result, nil
		// error, hasNext=false.
		result, err := finalTurnOutcome(nil, nil, carried, carriedErr)
		require.Same(t, winnerResult, result)
		require.NoError(t, err)
	})

	t.Run("winner error then refused no-turn drain keeps the error", func(t *testing.T) {
		carried, carriedErr, replaced := mergeTurnOutcome(nil, nil, nil, winnerErr)
		require.True(t, replaced)
		require.Nil(t, carried)
		require.Same(t, winnerErr, carriedErr)

		result, err := finalTurnOutcome(nil, nil, carried, carriedErr)
		require.Nil(t, result)
		require.Same(t, winnerErr, err)
	})

	t.Run("only refused no-turn drain legs still return nil, nil", func(t *testing.T) {
		// A loop that never produced a real outcome keeps the mailbox
		// queueing contract's own shape: a bare (nil, nil).
		carried, carriedErr, replaced := mergeTurnOutcome(nil, nil, nil, nil)
		require.False(t, replaced)
		require.Nil(t, carried)
		require.NoError(t, carriedErr)

		result, err := finalTurnOutcome(nil, nil, carried, carriedErr)
		require.Nil(t, result)
		require.NoError(t, err)
	})

	t.Run("mergeTurnOutcome carry and replace rules", func(t *testing.T) {
		// A refused leg does not clear an earlier carry.
		carried, carriedErr, replaced := mergeTurnOutcome(winnerResult, nil, nil, nil)
		require.False(t, replaced)
		require.Same(t, winnerResult, carried)
		require.NoError(t, carriedErr)

		// A later real result replaces it.
		laterResult := &fantasy.AgentResult{}
		carried, carriedErr, replaced = mergeTurnOutcome(carried, carriedErr, laterResult, nil)
		require.True(t, replaced)
		require.Same(t, laterResult, carried)
		require.NoError(t, carriedErr)

		// The carry is what the loop exits with once the next leg refuses.
		result, err := finalTurnOutcome(nil, nil, carried, carriedErr)
		require.Same(t, laterResult, result)
		require.NoError(t, err)
	})
}
