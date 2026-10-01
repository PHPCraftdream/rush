// Stage-4c: pin the §5.1 wake_fired notice texts (wake-tools contract,
// docs/plans/2026-09-27-wake-tools-contract.md §5.1). The text is the
// model's whole view of a fired occurrence, so both templates are asserted
// verbatim: the once form with the scheduled time, the loop form with the
// "occurrence N of {max_runs_or_infinity}" bound and the next-occurrence
// line. Revert-check: restoring the pre-4c loop template (without
// "of <max_runs>") fails the loop case below.
package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func wakeNoticeTexts(t *testing.T, f *wakeFx) []string {
	t.Helper()
	rows, err := f.fx.q.ListSessionNoticesForOwner(context.Background(), rerunOwner)
	require.NoError(t, err)
	texts := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Kind == NoticeKindWakeFired {
			texts = append(texts, r.Text)
		}
	}
	return texts
}

func TestWakeFiredNoticeTexts(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	ctx := context.Background()

	once := f.create(WakeKindOnce, f.base.Add(time.Minute), nil)
	_, err := f.wake.ClaimDue(ctx, "w", f.base.Add(time.Minute), 10, WakeLeaseTTL)
	require.NoError(t, err)
	fire, err := f.wake.FireOccurrence(ctx, once.ID, "w", f.base.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, fire.Fired)

	loop := f.create(WakeKindLoop, f.base.Add(300*time.Second), func(p *CreateWakeScheduleParams) {
		p.MaxRuns = 10
	})
	_, err = f.wake.ClaimDue(ctx, "w", f.base.Add(300*time.Second), 10, WakeLeaseTTL)
	require.NoError(t, err)
	fire, err = f.wake.FireOccurrence(ctx, loop.ID, "w", f.base.Add(300*time.Second))
	require.NoError(t, err)
	require.True(t, fire.Fired)

	texts := wakeNoticeTexts(t, f)
	require.Len(t, texts, 2)
	require.Equal(t,
		"Wake "+once.ID+" fired (scheduled for "+f.base.Add(time.Minute).UTC().Format(time.RFC3339)+"): check on it",
		texts[0],
	)
	require.Equal(t,
		"Loop "+loop.ID+" occurrence 1 of 10 fired: check on it\n\nNext occurrence: "+f.base.Add(600*time.Second).UTC().Format(time.RFC3339)+".",
		texts[1],
	)
}

// The unbounded-loop branch of §5.1's "{max_runs_or_infinity}".
func TestWakeFiredNoticeTexts_LoopUnbounded(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	ctx := context.Background()
	loop := f.create(WakeKindLoop, f.base.Add(300*time.Second), nil)
	_, err := f.wake.ClaimDue(ctx, "w", f.base.Add(300*time.Second), 10, WakeLeaseTTL)
	require.NoError(t, err)
	fire, err := f.wake.FireOccurrence(ctx, loop.ID, "w", f.base.Add(300*time.Second))
	require.NoError(t, err)
	require.True(t, fire.Fired)

	texts := wakeNoticeTexts(t, f)
	require.Len(t, texts, 1)
	require.Contains(t, texts[0], "occurrence 1 of infinity fired")
}
