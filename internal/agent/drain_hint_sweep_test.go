// Regression for the H4 hint-sweep defect (flag-ON shadow round 2): the
// ledger sweep must not drop a session's hint counter while its arbiter
// launch gate records HintSeen. The batch path arms no legacy workLedger
// job, so a batch-only session's entry held nothing and was swept; the
// counter restarted at zero and gateShut's comparison read the reset counter
// as a newer fact, reopening a dormant gate with no fact at all.
package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHintSweep_KeepsCounterBehindDormantGate(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "hint-sweep-dormant", attemptFixtureOpts{noIdle: true, noDriver: true})

	// A dormant free gate whose HintSeen names the entry's current counter.
	f.ledger.bumpHint(f.sessID)
	f.coord.seedArbiterState(f.sessID, func(s *arbiterState) {
		s.gate = GateFacts{HintSeen: 1, HintOpens: true, FreeStreak: drainDormantStreak}
	})
	require.Equal(t, uint64(1), f.ledger.hintSeqOf(f.sessID))

	f.ledger.sweepIdleSessions()

	require.Equal(t, uint64(1), f.ledger.hintSeqOf(f.sessID),
		"the sweep must keep the hint counter while the gate records HintSeen")
	require.Equal(t, drainStuck, f.coord.drainPermitted(ctx, f.sessID, false).kind,
		"no newer fact arrived: the dormant gate must stay shut")

	// A genuinely newer fact (a real counter increment past HintSeen) still
	// reopens the gate: keeping the counter never freezes it.
	f.ledger.bumpHint(f.sessID)
	require.Equal(t, drainAllow, f.coord.drainPermitted(ctx, f.sessID, false).kind,
		"a genuinely newer fact reopens a free-dormant gate")
}
