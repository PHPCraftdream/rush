package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// weeklyWindowAroundNow builds a weekly-policy window that provably
// covers the current wall clock, rendered in a FIXED offset zone
// (UTC+05:00, no DST) with an empty days spec so the weekday never
// matters. checkPeakHours is time.Now()-based, so the windows are cut
// around a captured instant on purpose; the caller asserts against the
// SAME captured strings/instants, never a fresh time.Now(), so a
// minute boundary crossing mid-test cannot flake.
func weeklyWindowAroundNow() (*config.ProviderPeakHours, string, string) {
	loc, err := config.ResolvePeakTimezone("UTC+05:00")
	if err != nil {
		panic(err)
	}
	nowLoc := time.Now().In(loc)
	start := nowLoc.Add(-time.Hour).Format("15:04")
	end := nowLoc.Add(time.Hour).Format("15:04")
	ph := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
		Timezone: "UTC+05:00",
		Windows:  []config.PeakHoursWindowSpec{{Days: "", Start: start, End: end}},
	}}
	return ph, start, end
}

// weeklyWindowStartingLater builds a window whose start lies one hour
// in the future, so it does not cover now.
func weeklyWindowStartingLater() *config.ProviderPeakHours {
	loc, err := config.ResolvePeakTimezone("UTC+05:00")
	if err != nil {
		panic(err)
	}
	nowLoc := time.Now().In(loc)
	return &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
		Timezone: "UTC+05:00",
		Windows: []config.PeakHoursWindowSpec{{
			Days:  "",
			Start: nowLoc.Add(time.Hour).Format("15:04"),
			End:   nowLoc.Add(2 * time.Hour).Format("15:04"),
		}},
	}}
}

// TestCheckPeakHoursWeeklyPolicyRefusesInWindow pins the weekly-policy
// refusal path: the band containing now feeds Start/End (formatted in
// the policy timezone), and the policy message rides through.
func TestCheckPeakHoursWeeklyPolicyRefusesInWindow(t *testing.T) {
	loc, err := config.ResolvePeakTimezone("UTC+05:00")
	require.NoError(t, err)
	msg := "Ping #ops-oncall first."
	ph, wantStart, wantEnd := weeklyWindowAroundNow()
	ph.Policy.Message = msg
	cfg := config.ProviderConfig{ID: "weekly-peak", PeakHours: ph}

	err = checkPeakHours(cfg)
	require.Error(t, err, "a window covering now must refuse")
	require.ErrorIs(t, err, errProviderPeakHours, "refusal must wrap errProviderPeakHours")

	var pe *PeakHoursError
	require.ErrorAs(t, err, &pe, "refusal must be a *PeakHoursError")
	assert.Equal(t, "weekly-peak", pe.ProviderID)
	assert.Equal(t, msg, pe.Message, "the policy message rides through")
	assert.Equal(t, "UTC+05:00", pe.Timezone)

	// BandStart path: Start/End are the band boundaries in the policy
	// timezone, formatted HH:MM.
	assert.Equal(t, wantStart, pe.Start,
		"Start is the band start in the policy zone")
	assert.Equal(t, wantEnd, pe.End,
		"End is the band end (ReopensAt) in the policy zone")
	require.False(t, pe.ReopensAt.IsZero())
	require.WithinDuration(t,
		time.Now().In(loc).Add(time.Hour).Truncate(time.Minute), pe.ReopensAt, 5*time.Minute)
}

// TestCheckPeakHoursWeeklyPauseSuppressesRefusal pins the new
// suppression semantics: an ACTIVE pause inside a covering window lifts
// the refusal entirely (Blocked = InPeak && !Paused).
func TestCheckPeakHoursWeeklyPauseSuppressesRefusal(t *testing.T) {
	loc, err := config.ResolvePeakTimezone("UTC+05:00")
	require.NoError(t, err)
	ph, _, _ := weeklyWindowAroundNow() // band covers now
	ph.Policy.PausedUntil = time.Now().In(loc).Add(time.Hour).Format(time.RFC3339)
	cfg := config.ProviderConfig{ID: "paused-peak", PeakHours: ph}

	assert.NoError(t, checkPeakHours(cfg),
		"an active pause must lift the refusal inside the window")
}

// TestCheckPeakHoursWeeklyExpiredPauseRefuses pins that after the pause
// expires (PausedUntil in the past) the covering window refuses again,
// with the band boundaries on the error and ReopensAt near now+1h.
func TestCheckPeakHoursWeeklyExpiredPauseRefuses(t *testing.T) {
	loc, err := config.ResolvePeakTimezone("UTC+05:00")
	require.NoError(t, err)
	nowLoc := time.Now().In(loc)
	ph, start, _ := weeklyWindowAroundNow() // band covers now, ends at now+1h
	ph.Policy.PausedUntil = nowLoc.Add(-time.Hour).Format(time.RFC3339)
	cfg := config.ProviderConfig{ID: "expired-peak", PeakHours: ph}

	err = checkPeakHours(cfg)
	require.Error(t, err, "after the pause expires the window must refuse again")
	require.ErrorIs(t, err, errProviderPeakHours)

	var pe *PeakHoursError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, classTerminal, classifyProviderError(err),
		"an expired-pause window refusal must classify as terminal")
	assert.Equal(t, start, pe.Start, "Start is the band start in the policy zone")
	require.False(t, pe.ReopensAt.IsZero())
	require.WithinDuration(t, nowLoc.Add(time.Hour), pe.ReopensAt, 2*time.Minute,
		"ReopensAt is within [now, now+2h]")
}

// TestCheckPeakHoursWeeklyPauseAndWindow pins the suppression semantics
// when both the window and the pause are present: an ACTIVE pause lifts
// the refusal inside the window, while an EXPIRED pause lets the window
// refuse again with ReopensAt at the band end.
func TestCheckPeakHoursWeeklyPauseAndWindow(t *testing.T) {
	loc, err := config.ResolvePeakTimezone("UTC+05:00")
	require.NoError(t, err)
	nowLoc := time.Now().In(loc)

	t.Run("active pause suppresses the in-window refusal", func(t *testing.T) {
		ph, _, _ := weeklyWindowAroundNow() // band ends at now+1h
		ph.Policy.PausedUntil = nowLoc.Add(time.Hour).Format(time.RFC3339)
		assert.NoError(t, checkPeakHours(config.ProviderConfig{ID: "both-peak", PeakHours: ph}),
			"an active pause must lift the refusal inside the window")
	})

	t.Run("expired pause lets the window refuse again", func(t *testing.T) {
		ph, _, end := weeklyWindowAroundNow() // band ends at now+1h
		ph.Policy.PausedUntil = nowLoc.Add(-time.Hour).Format(time.RFC3339)
		err := checkPeakHours(config.ProviderConfig{ID: "both-peak", PeakHours: ph})
		require.Error(t, err)
		var pe *PeakHoursError
		require.ErrorAs(t, err, &pe)
		endLoc := time.Now().In(loc).Add(time.Hour)
		require.WithinDuration(t, endLoc, pe.ReopensAt, 2*time.Minute,
			"ReopensAt is the band end: "+end)
	})
}

// TestCheckPeakHoursWeeklyExpiredPauseOrOpenWindow pins the allow paths:
// an expired pause with a non-covering window allows; an IsOff policy
// (zero windows, no pause) allows too.
func TestCheckPeakHoursWeeklyExpiredPauseOrOpenWindow(t *testing.T) {
	loc, err := config.ResolvePeakTimezone("UTC+05:00")
	require.NoError(t, err)
	nowLoc := time.Now().In(loc)

	t.Run("expired pause and window not covering now allows", func(t *testing.T) {
		ph := weeklyWindowStartingLater()
		ph.Policy.PausedUntil = nowLoc.Add(-time.Hour).Format(time.RFC3339)
		assert.NoError(t, checkPeakHours(config.ProviderConfig{ID: "p", PeakHours: ph}),
			"an expired pause must not block")
	})

	t.Run("off policy allows", func(t *testing.T) {
		ph := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Timezone: "UTC+05:00",
			Windows:  []config.PeakHoursWindowSpec{},
		}}
		assert.NoError(t, checkPeakHours(config.ProviderConfig{ID: "p", PeakHours: ph}),
			"zero windows and no pause is off: allowed")
	})

	t.Run("nil peak hours allows", func(t *testing.T) {
		assert.NoError(t, checkPeakHours(config.ProviderConfig{ID: "p"}))
	})
}

// TestPeakHoursGuidanceWeekly pins the guidance text shapes: the
// timezone suffix only when a timezone is configured (byte-identity for
// legacy output), the standing --allow-peak-hours warning, and the
// zero-reopen fallback (no timed resume suggested).
func TestPeakHoursGuidanceWeekly(t *testing.T) {
	t.Parallel()

	t.Run("in-window with timezone appends the timezone", func(t *testing.T) {
		t.Parallel()
		err := &PeakHoursError{
			ProviderID: "prov-x",
			Start:      "09:00",
			End:        "18:00",
			ReopensAt:  time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC),
			Timezone:   "Asia/Shanghai",
		}
		out := PeakHoursGuidance(err)
		assert.Contains(t, out, "(timezone: Asia/Shanghai)")
		assert.Contains(t, out, "--allow-peak-hours",
			"the bypass warning must still be present")
		assert.NotContains(t, out, "rush providers peak-hours resume",
			"a window refusal is not a pause and must not suggest resume")
	})

	t.Run("in-window without timezone has no timezone suffix", func(t *testing.T) {
		t.Parallel()
		err := &PeakHoursError{
			ProviderID: "prov-x",
			Start:      "09:00",
			End:        "18:00",
			ReopensAt:  time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC),
		}
		out := PeakHoursGuidance(err)
		assert.NotContains(t, out, "timezone:",
			"legacy-shape guidance must stay byte-identical: no timezone suffix at all")
		assert.Contains(t, out, "--allow-peak-hours")
	})

	t.Run("guidance extracts ReopensAt from a wrapped error", func(t *testing.T) {
		t.Parallel()
		base := &PeakHoursError{
			ProviderID: "prov-x",
			Start:      "09:00",
			End:        "18:00",
			ReopensAt:  time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC),
		}
		wrapped := fmt.Errorf("outer: %w", base)
		out := PeakHoursGuidance(wrapped)
		assert.Contains(t, out, "2026-11-09 18:00", "errors.As must recover the exact reopen time")
	})

	t.Run("zero ReopensAt omits the timed resume", func(t *testing.T) {
		t.Parallel()
		err := &PeakHoursError{ProviderID: "prov-x", Start: "00:00", End: "24:00"}
		out := PeakHoursGuidance(err)
		assert.Contains(t, out, "do not schedule a timed resume")
		assert.NotContains(t, out, "0001", "the zero time must never be rendered")
	})

	t.Run("nil error still yields generic guidance", func(t *testing.T) {
		t.Parallel()
		out := PeakHoursGuidance(nil)
		assert.Contains(t, out, "--allow-peak-hours")
		assert.NotContains(t, out, "timezone:")
	})

	t.Run("errors stay recognisable through the sentinel", func(t *testing.T) {
		t.Parallel()
		base := &PeakHoursError{ProviderID: "prov-x"}
		assert.True(t, errors.Is(base, errProviderPeakHours))
	})
}
