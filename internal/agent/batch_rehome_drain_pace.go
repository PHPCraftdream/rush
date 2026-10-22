// Batch rehome (H4 lane P5, phase A): drainPace moved verbatim out of
// work_ledger_reaction.go before its deletion. The pacing taxonomy is
// ledger-free: drain_attempt.go's paceUnreacted and the arbiter's gate
// (turn_arbiter_state.go pace) consume it in the batch world.
package agent

// drainPace says what a pacing counts toward the dormancy streaks (the
// arbiter's gate keeps the same accounting the old ledger gate had).
type drainPace uint8

const (
	// paceUncounted: a refusal or a failed ordinary turn -- pause only.
	paceUncounted drainPace = iota
	// paceFreeNoTurn: a no-turn Drain left pending debt behind (pull failing).
	paceFreeNoTurn
	// pacePaidUnreacted: a paid attempt left the debt unreacted.
	pacePaidUnreacted
)
