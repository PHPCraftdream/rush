// B2/C2 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN) and R2B-5/
// R2B-7: a Drain refused because another process holds the session's OS lock
// is never counted, never settled, and its own release never relaunches it:
// the refusal paces the launch gate and keeps the wake in the re-check set.
// There is no pre-submit probe any more -- the refusal comes from runOwned's
// real lock acquisition.
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestAdmissionRefusedRelease_NoImmediateRelaunch_ThenTickRecovers: with the
// lock held elsewhere the Drain is refused once (no request, no count), the
// release that follows does not relaunch, the session sits in the re-check
// set, and once the lock is free the next tick after the pause reacts.
//
// Revert-check: dropping noteRefusal from runOwned's busy-lock branch makes
// the release hook relaunch endlessly (Run count far above 1) and this test
// goes red.
func TestAdmissionRefusedRelease_NoImmediateRelaunch_ThenTickRecovers(t *testing.T) {
	ctx := context.Background()
	const retry = 300 * time.Millisecond
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "admission-refused", attemptFixtureOpts{})
	launches := f.countRuns()
	f.seedDebt(ctx, "call-1", true)
	foreignLock, err := session.TryAcquireSessionLock(f.env.workingDir, f.sessID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })

	err = f.wake(ctx, true)
	require.Error(t, err)
	require.True(t, IsDrainNotAttempted(err), "a refusal says the Drain never reached the provider: %v", err)
	time.Sleep(retry / 3)

	require.EqualValues(t, 1, launches.runs.Load(), "a refused release must not relaunch a Drain")
	require.Zero(t, f.requests.Load())
	require.True(t, f.inRecheckSet(), "a refused session stays in the re-check set")
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 0, row.WakeAttempts, "a refusal is never counted")
	require.EqualValues(t, 0, row.Reacted)
	require.Zero(t, f.markers(ctx))

	require.NoError(t, foreignLock.Release())
	time.Sleep(retry)
	f.pass(ctx)
	require.EqualValues(t, 1, f.requests.Load(), "the tick after the pause launches the Drain")
	require.EqualValues(t, 1, f.row(ctx, "call-1").Reacted)
}
