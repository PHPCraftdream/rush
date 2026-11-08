package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedLoc resolves the fixed-offset zone used by every deterministic
// instant in this file. UTC+02:00 has no DST, so wall-clock arithmetic
// and weekday anchors are stable.
func fixedLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := ResolvePeakTimezone("UTC+02:00")
	require.NoError(t, err)
	require.NotNil(t, loc)
	return loc
}

// weekdayGuard asserts the calendar anchor a test relies on, so a
// wrong date fails loudly with context instead of silently testing
// the wrong weekday.
func weekdayGuard(t *testing.T, name string, y int, m time.Month, d int, want time.Weekday, loc *time.Location) time.Time {
	t.Helper()
	day := time.Date(y, m, d, 12, 0, 0, 0, loc)
	require.Equal(t, want, day.Weekday(),
		"calendar anchor drifted: %s (%04d-%02d-%02d) is no longer %s in %s",
		name, y, int(m), d, want, loc)
	return day
}

// TestParsePeakDays covers the days-spec grammar: empty, single days,
// lists, forward and wrapping ranges, case-insensitivity, and errors.
func TestParsePeakDays(t *testing.T) {
	t.Parallel()

	all := [7]bool{true, true, true, true, true, true, true}

	tests := []struct {
		name    string
		spec    string
		want    [7]bool
		wantErr bool
	}{
		{"empty means every day", "", all, false},
		{"whitespace means every day", "  ", all, false},
		{"single abbreviation", "mon", [7]bool{true, false, false, false, false, false, false}, false},
		{"case insensitive", "MON", [7]bool{true, false, false, false, false, false, false}, false},
		{"full name", "friday", [7]bool{false, false, false, false, true, false, false}, false},
		{
			"comma list", "mon,wed,fri",
			[7]bool{true, false, true, false, true, false, false},
			false,
		},
		{
			"forward range", "mon-fri",
			[7]bool{true, true, true, true, true, false, false},
			false,
		},
		{
			"wrapping range", "fri-mon",
			[7]bool{true, false, false, false, true, true, true},
			false,
		},
		{
			"wrapping range sat-tue", "sat-tue",
			[7]bool{true, true, false, false, false, true, true},
			false,
		},
		{
			"degenerate range is one day", "fri-fri",
			[7]bool{false, false, false, false, true, false, false},
			false,
		},
		{"unknown day", "nope", [7]bool{}, true},
		{"empty entry", "mon,,tue", [7]bool{}, true},
		{"trailing comma", "mon,", [7]bool{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePeakDays(tc.spec)
			if tc.wantErr {
				require.Error(t, err, "spec %q must fail", tc.spec)
				return
			}
			require.NoError(t, err, "spec %q must parse", tc.spec)
			require.Equal(t, tc.want, got, "mask for %q", tc.spec)
		})
	}
}

// TestFormatPeakDaysRoundTrip pins FormatPeakDays' canonical output for
// every representative mask shape; each expectation is spelled out.
func TestFormatPeakDaysRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec string
		want string
	}{
		{"all days canonicalises to empty", "", ""},
		{"single day is bare name", "friday", "fri"},
		{"contiguous range stays a range", "mon-fri", "mon-fri"},
		{"wrapping range stays wrapping", "fri-mon", "fri-mon"},
		{"scattered days become a comma list", "mon,wed,fri", "mon,wed,fri"},
		{"two-day wrap stays wrapping", "sun-mon", "sun-mon"},
		{"weekend canonicalises to a range", "sat,sun", "sat-sun"},
		{"six-day run stays a range", "mon,tue,wed,thu,fri,sat", "mon-sat"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mask, err := ParsePeakDays(tc.spec)
			require.NoError(t, err, "spec %q must parse", tc.spec)
			require.Equal(t, tc.want, FormatPeakDays(mask),
				"canonical form of %q", tc.spec)
		})
	}
}

// TestParsePeakClock covers HH:MM parsing including the end-only 24:00.
func TestParsePeakClock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		spec     string
		allowEnd bool
		want     int
		wantErr  bool
	}{
		{"plain morning time", "09:00", false, 540, false},
		{"24:00 allowed as end", "24:00", true, 1440, false},
		{"24:00 rejected as start", "24:00", false, 0, true},
		{"leading zero required", "9:00", false, 0, true},
		{"minute out of range", "09:60", false, 0, true},
		{"non-digit minute", "09:0;", false, 0, true},
		{"24:01 rejected even as end", "24:01", true, 0, true},
		{"midnight", "00:00", false, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePeakClock(tc.spec, tc.allowEnd)
			if tc.wantErr {
				require.Error(t, err, "spec %q must fail", tc.spec)
				return
			}
			require.NoError(t, err, "spec %q must parse", tc.spec)
			require.Equal(t, tc.want, got, "minutes for %q", tc.spec)
		})
	}
}

// TestResolvePeakTimezone covers local/UTC/offset/IANA resolution and
// the rejection of malformed or out-of-range zone names.
func TestResolvePeakTimezone(t *testing.T) {
	t.Parallel()

	t.Run("empty is machine local", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("")
		require.NoError(t, err)
		assert.Same(t, time.Local, loc)
	})

	t.Run("local is machine local", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("local")
		require.NoError(t, err)
		assert.Same(t, time.Local, loc)
	})

	t.Run("utc", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("utc")
		require.NoError(t, err)
		assert.Same(t, time.UTC, loc)
	})

	t.Run("UTC+08:00 keeps name and offset", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("UTC+08:00")
		require.NoError(t, err)
		_, off := time.Date(2026, 6, 1, 12, 0, 0, 0, loc).Zone()
		assert.Equal(t, 8*3600, off)
		assert.Equal(t, "UTC+08:00", loc.String())
	})

	t.Run("+05:30 half-hour offset", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("+05:30")
		require.NoError(t, err)
		_, off := time.Date(2026, 6, 1, 12, 0, 0, 0, loc).Zone()
		assert.Equal(t, 5*3600+30*60, off)
	})

	t.Run("-05 negative hour", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("-05")
		require.NoError(t, err)
		_, off := time.Date(2026, 6, 1, 12, 0, 0, 0, loc).Zone()
		assert.Equal(t, -5*3600, off)
	})

	t.Run("UTC-5 without minutes", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("UTC-5")
		require.NoError(t, err)
		_, off := time.Date(2026, 6, 1, 12, 0, 0, 0, loc).Zone()
		assert.Equal(t, -5*3600, off)
	})

	t.Run("hour out of range", func(t *testing.T) {
		t.Parallel()
		_, err := ResolvePeakTimezone("UTC+25:00")
		require.Error(t, err)
	})

	t.Run("minute needs two digits", func(t *testing.T) {
		t.Parallel()
		_, err := ResolvePeakTimezone("+08:5")
		require.Error(t, err)
	})

	t.Run("unknown zone name", func(t *testing.T) {
		t.Parallel()
		_, err := ResolvePeakTimezone("Nope/Zone")
		require.Error(t, err)
	})

	t.Run("IANA name resolves via embedded tzdata", func(t *testing.T) {
		t.Parallel()
		loc, err := ResolvePeakTimezone("Asia/Shanghai")
		require.NoError(t, err, "tzdata is embedded, so IANA names must resolve everywhere")
		// Prove the zone really resolved (not a silent fallback): a
		// known summer instant is 20:00 CST for 12:00 UTC, offset +8h.
		inst := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		got := inst.In(loc)
		assert.Equal(t, 20, got.Hour(), "12:00 UTC must be 20:00 in Asia/Shanghai")
		_, gotOff := inst.In(loc).Zone()
		assert.Equal(t, 8*3600, gotOff)
	})
}

// TestPeakHoursPolicyValidate covers policy-level validation: windows,
// clocks, the window cap, the pause stamp, and the timezone.
func TestPeakHoursPolicyValidate(t *testing.T) {
	t.Parallel()

	valid := PeakHoursPolicy{
		Timezone: "UTC+02:00",
		Windows: []PeakHoursWindowSpec{
			{Days: "mon-fri", Start: "09:00", End: "18:00"},
			{Days: "sat", Start: "10:00", End: "12:00"},
		},
		Message:     "Ping #ops first.",
		PausedUntil: "2026-11-09T10:00:00Z",
	}

	t.Run("valid policy", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, valid.Validate())
	})

	tests := []struct {
		name    string
		mutate  func(*PeakHoursPolicy)
		wantMsg string
	}{
		{
			name: "zero-length window",
			mutate: func(p *PeakHoursPolicy) {
				p.Windows = []PeakHoursWindowSpec{{Days: "mon", Start: "12:00", End: "12:00"}}
			},
			wantMsg: "zero-length",
		},
		{
			name: "24:00 as start",
			mutate: func(p *PeakHoursPolicy) {
				p.Windows = []PeakHoursWindowSpec{{Days: "mon", Start: "24:00", End: "23:00"}}
			},
			wantMsg: "start",
		},
		{
			name: "missing start",
			mutate: func(p *PeakHoursPolicy) {
				p.Windows = []PeakHoursWindowSpec{{Days: "mon", End: "18:00"}}
			},
			wantMsg: "start",
		},
		{
			name: "missing end",
			mutate: func(p *PeakHoursPolicy) {
				p.Windows = []PeakHoursWindowSpec{{Days: "mon", Start: "09:00"}}
			},
			wantMsg: "end",
		},
		{
			name: "too many windows",
			mutate: func(p *PeakHoursPolicy) {
				w := make([]PeakHoursWindowSpec, PeakHoursMaxWindows+1)
				for i := range w {
					w[i] = PeakHoursWindowSpec{Days: "mon", Start: "09:00", End: "09:30"}
				}
				p.Windows = w
			},
			wantMsg: "max 64",
		},
		{
			name: "bad days",
			mutate: func(p *PeakHoursPolicy) {
				p.Windows = []PeakHoursWindowSpec{{Days: "nope", Start: "09:00", End: "10:00"}}
			},
			wantMsg: "nope",
		},
		{
			name:    "bad paused_until",
			mutate:  func(p *PeakHoursPolicy) { p.PausedUntil = "2026-11-09 10:00:00" },
			wantMsg: "paused_until",
		},
		{
			name:    "invalid timezone",
			mutate:  func(p *PeakHoursPolicy) { p.Timezone = "Nope/Zone"; p.PausedUntil = "" },
			wantMsg: "Nope/Zone",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := valid
			tc.mutate(&p)
			err := p.Validate()
			require.Error(t, err, "%s must fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.wantMsg, "error must name the cause")
		})
	}
}

// policyInstant builds an instant in the fixed UTC+02:00 zone for the
// deterministic Evaluate cases.
func policyInstant(t *testing.T, y int, m time.Month, d, hh, mm int) time.Time {
	t.Helper()
	return time.Date(y, m, d, hh, mm, 0, 0, fixedLoc(t))
}

// TestPeakHoursPolicyEvaluate is the core weekly-window evaluation
// table: each case names the policy, the exact instant, and the full
// expected status fields.
func TestPeakHoursPolicyEvaluate(t *testing.T) {
	t.Parallel()
	loc := fixedLoc(t)
	mon := weekdayGuard(t, "test Monday", 2026, 11, 9, time.Monday, loc)
	fri := weekdayGuard(t, "test Friday", 2026, 11, 13, time.Friday, loc)
	sat := weekdayGuard(t, "test Saturday", 2026, 11, 14, time.Saturday, loc)

	// Convenience constructors.
	at := func(day time.Time, hh, mm int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc)
	}
	wd := func(days, start, end string) PeakHoursWindowSpec {
		return PeakHoursWindowSpec{Days: days, Start: start, End: end}
	}

	t.Run("weekday window boundaries", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{wd("mon-fri", "09:00", "18:00")}}

		out0859 := p.Evaluate(at(mon, 8, 59))
		assert.False(t, out0859.InPeak, "Mon 08:59 is before the inclusive start")
		assert.True(t, out0859.NextTransition.Equal(at(mon, 9, 0)), "next band starts at 09:00")

		in0900 := p.Evaluate(at(mon, 9, 0))
		assert.True(t, in0900.InPeak, "start is inclusive")
		assert.True(t, in0900.ReopensAt.Equal(at(mon, 18, 0)), "band ends at 18:00")

		in1759 := p.Evaluate(at(mon, 17, 59))
		assert.True(t, in1759.InPeak, "17:59 is still inside")

		out1800 := p.Evaluate(at(mon, 18, 0))
		assert.False(t, out1800.InPeak, "end is exclusive")

		satNoon := p.Evaluate(at(sat, 12, 0))
		assert.False(t, satNoon.InPeak, "saturday is outside the mon-fri mask")
	})

	t.Run("whole-day window", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{wd("mon", "00:00", "24:00")}}
		assert.True(t, p.Evaluate(at(mon, 0, 0)).InPeak, "midnight start is inclusive")
		assert.True(t, p.Evaluate(at(mon, 23, 59)).InPeak, "23:59 is inside")
		assert.False(t, p.Evaluate(at(mon.AddDate(0, 0, 1), 0, 0)).InPeak, "24:00 end is exclusive")
	})

	t.Run("overnight fri 22:00-06:00", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{wd("fri", "22:00", "06:00")}}
		assert.False(t, p.Evaluate(at(fri, 21, 59)).InPeak, "21:59 is before the start")
		assert.True(t, p.Evaluate(at(fri, 22, 0)).InPeak, "start of the first leg")
		assert.True(t, p.Evaluate(at(fri, 23, 30)).InPeak, "first leg on the start day")
		assert.True(t, p.Evaluate(at(sat, 3, 0)).InPeak,
			"second leg on saturday follows the START day's fri mask")
		assert.True(t, p.Evaluate(at(sat, 5, 59)).InPeak, "05:59 is inside the second leg")
		assert.False(t, p.Evaluate(at(sat, 6, 0)).InPeak, "06:00 ends the band")
		assert.False(t, p.Evaluate(at(sat, 23, 0)).InPeak,
			"the second friday window only covers sat morning, not sat evening")
	})

	t.Run("touching windows merge into one band", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{
			wd("mon", "09:00", "12:00"), wd("mon", "12:00", "15:00"),
		}}
		st := p.Evaluate(at(mon, 11, 30))
		assert.True(t, st.InPeak)
		assert.True(t, st.ReopensAt.Equal(at(mon, 15, 0)),
			"touching windows form one continuous band, so the reopen time is 15:00 not 12:00")
	})

	t.Run("overlapping windows merge into one band", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{
			wd("mon", "09:00", "13:00"), wd("mon", "12:00", "15:00"),
		}}
		st := p.Evaluate(at(mon, 11, 30))
		assert.True(t, st.InPeak)
		assert.True(t, st.ReopensAt.Equal(at(mon, 15, 0)),
			"overlapping windows form one continuous band")
	})

	t.Run("two separate windows same day", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{
			wd("mon", "09:00", "10:00"), wd("mon", "15:00", "16:00"),
		}}
		st := p.Evaluate(at(mon, 9, 30))
		assert.True(t, st.InPeak)
		assert.True(t, st.ReopensAt.Equal(at(mon, 10, 0)), "first band ends at 10:00")
		assert.True(t, st.Blocked, "blocked, so the next transition is the reopen itself")

		gap := p.Evaluate(at(mon, 12, 0))
		assert.False(t, gap.InPeak)
		assert.True(t, gap.NextTransition.Equal(at(mon, 15, 0)),
			"outside a band the next transition is the 15:00 band start")
	})

	t.Run("wrap band continues through a touch into the next day", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{
			wd("fri", "22:00", "06:00"), wd("sat", "06:00", "10:00"),
		}}
		st := p.Evaluate(at(sat, 4, 0))
		assert.True(t, st.InPeak)
		assert.True(t, st.ReopensAt.Equal(at(sat, 10, 0)),
			"the overnight band touches the saturday band, so the reopen is 10:00")
	})

	t.Run("fully blocked week invents no reopen", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{
			wd("", "00:00", "24:00"),
		}}
		st := p.Evaluate(at(mon, 12, 0))
		assert.True(t, st.InPeak, "every day, all day: always in peak")
		assert.True(t, st.ReopensAt.IsZero(),
			"the band extends beyond the 8-day horizon, so no reopen is invented")
		assert.True(t, st.NextTransition.IsZero(),
			"blocked with unknown reopen, so no next transition either")
	})

	t.Run("days-less window behaves as every day", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{wd("", "09:00", "10:00")}}
		assert.True(t, p.Evaluate(at(mon, 9, 30)).InPeak, "monday")
		assert.True(t, p.Evaluate(at(sat, 9, 30)).InPeak, "saturday — empty days means every day")
		assert.False(t, p.Evaluate(at(mon, 10, 30)).InPeak)
	})

	t.Run("timezone effect", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{wd("", "09:00", "10:00")}}
		// 09:30 local == 07:30 UTC: inside.
		inUTC := p.Evaluate(time.Date(2026, 11, 9, 7, 30, 0, 0, time.UTC))
		assert.True(t, inUTC.InPeak, "07:30 UTC is 09:30 in UTC+02:00, inside the window")
		assert.True(t, inUTC.BandStart.Equal(time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)),
			"band start is 09:00 local == 07:00 UTC")
		// 08:29 local == 06:29 UTC: outside.
		outUTC := p.Evaluate(time.Date(2026, 11, 9, 6, 29, 0, 0, time.UTC))
		assert.False(t, outUTC.InPeak, "06:29 UTC is 08:29 local, before the window")
	})

	t.Run("BandStart set inside, zero outside", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "UTC+02:00", Windows: []PeakHoursWindowSpec{wd("mon", "09:00", "18:00")}}
		in := p.Evaluate(at(mon, 10, 0))
		assert.True(t, in.BandStart.Equal(at(mon, 9, 0)), "band start is the window start")
		out := p.Evaluate(at(mon, 20, 0))
		assert.True(t, out.BandStart.IsZero(), "no band contains an instant outside every window")
	})

	t.Run("paused only, no windows", func(t *testing.T) {
		t.Parallel()
		now := at(mon, 12, 0)
		p := PeakHoursPolicy{
			Timezone:    "UTC+02:00",
			PausedUntil: now.Add(2 * time.Hour).Format(time.RFC3339),
		}
		st := p.Evaluate(now)
		assert.True(t, st.Paused)
		assert.False(t, st.InPeak, "no windows configured")
		assert.False(t, st.Blocked, "a pause suppresses refusal, it never adds one")
		assert.True(t, st.ReopensAt.IsZero(), "nothing is blocked while paused")
		assert.True(t, st.NextTransition.IsZero())
		assert.True(t, st.PausedUntil.Equal(now.Add(2*time.Hour)))
	})

	t.Run("expired pause is not paused", func(t *testing.T) {
		t.Parallel()
		now := at(mon, 12, 0)
		p := PeakHoursPolicy{
			Timezone:    "UTC+02:00",
			PausedUntil: now.Add(-1 * time.Hour).Format(time.RFC3339),
		}
		st := p.Evaluate(now)
		assert.False(t, st.Paused)
		assert.False(t, st.Blocked)
		assert.True(t, st.ReopensAt.IsZero())
	})

	t.Run("pause outlasting the window", func(t *testing.T) {
		t.Parallel()
		now := at(mon, 12, 0)
		p := PeakHoursPolicy{
			Timezone:    "UTC+02:00",
			Windows:     []PeakHoursWindowSpec{wd("mon", "11:30", "13:00")},
			PausedUntil: now.Add(3 * time.Hour).Format(time.RFC3339),
		}
		st := p.Evaluate(now)
		assert.True(t, st.InPeak)
		assert.True(t, st.Paused)
		assert.False(t, st.Blocked, "nothing blocks while paused")
		assert.True(t, st.ReopensAt.IsZero(), "nothing blocks while paused")
		// The pause lift at 15:00 is outside every band, so Blocked does
		// not flip there; the real flip is the next Monday at 11:30.
		assert.True(t, st.NextTransition.Equal(at(mon.AddDate(0, 0, 7), 11, 30)))
	})

	t.Run("window outlasting the pause", func(t *testing.T) {
		t.Parallel()
		now := at(mon, 12, 0)
		p := PeakHoursPolicy{
			Timezone:    "UTC+02:00",
			Windows:     []PeakHoursWindowSpec{wd("mon", "11:30", "13:00")},
			PausedUntil: now.Add(30 * time.Minute).Format(time.RFC3339),
		}
		st := p.Evaluate(now)
		assert.True(t, st.InPeak)
		assert.True(t, st.Paused)
		assert.False(t, st.Blocked, "nothing blocks while paused")
		assert.True(t, st.ReopensAt.IsZero(), "nothing blocks while paused")
		assert.True(t, st.NextTransition.Equal(now.Add(30*time.Minute)))
	})

	t.Run("pause lifting inside a later band flips at the lift", func(t *testing.T) {
		t.Parallel()
		now := at(mon, 12, 0)
		p := PeakHoursPolicy{
			Timezone:    "UTC+02:00",
			Windows:     []PeakHoursWindowSpec{wd("mon", "14:00", "16:00")},
			PausedUntil: now.Add(3 * time.Hour).Format(time.RFC3339),
		}
		st := p.Evaluate(now)
		assert.False(t, st.InPeak, "12:00 is outside the 14:00-16:00 band")
		assert.True(t, st.Paused)
		assert.False(t, st.Blocked)
		assert.True(t, st.ReopensAt.IsZero(), "nothing blocks while paused")
		// The lift at 15:00 lands INSIDE the band, so Blocked flips true
		// exactly there.
		assert.True(t, st.NextTransition.Equal(at(mon, 15, 0)))
	})

	t.Run("pause lifting before a later band flips at the band start", func(t *testing.T) {
		t.Parallel()
		now := at(mon, 12, 0)
		p := PeakHoursPolicy{
			Timezone:    "UTC+02:00",
			Windows:     []PeakHoursWindowSpec{wd("mon", "16:00", "17:00")},
			PausedUntil: now.Add(3 * time.Hour).Format(time.RFC3339),
		}
		st := p.Evaluate(now)
		assert.True(t, st.Paused)
		assert.False(t, st.Blocked)
		// The lift at 15:00 is outside the band, so Blocked flips at the
		// band start instead.
		assert.True(t, st.NextTransition.Equal(at(mon, 16, 0)))
	})

	t.Run("pause expired inside the band blocks again", func(t *testing.T) {
		t.Parallel()
		now := at(mon, 12, 0)
		p := PeakHoursPolicy{
			Timezone:    "UTC+02:00",
			Windows:     []PeakHoursWindowSpec{wd("mon", "11:30", "13:00")},
			PausedUntil: now.Add(-30 * time.Minute).Format(time.RFC3339),
		}
		st := p.Evaluate(now)
		assert.False(t, st.Paused, "the pause has expired")
		assert.True(t, st.InPeak)
		assert.True(t, st.Blocked, "after expiry the window blocks again")
		assert.True(t, st.ReopensAt.Equal(at(mon, 13, 0)), "reopen is the band end")
	})

	t.Run("DST spring forward skips the wall-clock window", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "America/New_York", Windows: []PeakHoursWindowSpec{
			wd("", "02:00", "03:00"),
		}}
		// 2026-03-08: 02:00 EST jumps to 03:00 EDT, so the 02:00-03:00
		// wall-clock window never exists that day.
		before := time.Date(2026, 3, 8, 6, 59, 59, 0, time.UTC) // 01:59:59 EST
		after := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)    // 03:00:00 EDT
		stBefore := p.Evaluate(before)
		stAfter := p.Evaluate(after)
		assert.False(t, stBefore.InPeak, "01:59:59 EST wall clock is before 02:00")
		assert.False(t, stAfter.InPeak,
			"the wall clock jumped 01:59:59 EST -> 03:00 EDT, skipping 02:00-03:00 entirely")
		// Control: on the previous (non-DST) day the window does apply.
		ctrl := p.Evaluate(time.Date(2026, 3, 7, 7, 30, 0, 0, time.UTC)) // 02:30 EST
		assert.True(t, ctrl.InPeak, "02:30 EST on a non-transition day is inside")
	})

	t.Run("DST fall back repeats the wall-clock hour", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "America/New_York", Windows: []PeakHoursWindowSpec{
			wd("", "01:00", "02:30"),
		}}
		// 2026-11-01: 02:00 EDT falls back to 01:00 EST; both passes of
		// the repeated wall-clock hour are plain instants inside 01:00-02:30.
		firstPass := p.Evaluate(time.Date(2026, 11, 1, 5, 59, 59, 0, time.UTC))  // 01:59:59 EDT
		secondPass := p.Evaluate(time.Date(2026, 11, 1, 6, 59, 59, 0, time.UTC)) // 01:59:59 EST
		assert.True(t, firstPass.InPeak, "01:59:59 EDT is wall clock 01:59, inside")
		assert.True(t, secondPass.InPeak, "the REPEATED 01:59:59 EST pass is also inside")
	})

	t.Run("DST fall back: both passes of 01:15-01:45", func(t *testing.T) {
		t.Parallel()
		p := PeakHoursPolicy{Timezone: "America/New_York", Windows: []PeakHoursWindowSpec{
			wd("", "01:15", "01:45"),
		}}
		passOne := p.Evaluate(time.Date(2026, 11, 1, 5, 15, 0, 0, time.UTC)) // 01:15 EDT
		passTwo := p.Evaluate(time.Date(2026, 11, 1, 6, 15, 0, 0, time.UTC)) // 01:15 EST (repeated hour)
		assert.True(t, passOne.InPeak, "first pass of 01:15 (EDT) is in-window")
		assert.True(t, passTwo.InPeak, "the REPEATED 01:15 EST pass is also in-window")
	})

	nyLoc, err := ResolvePeakTimezone("America/New_York")
	require.NoError(t, err)
	t.Run("DST spring gap start keeps band consistent with membership", func(t *testing.T) {
		p := PeakHoursPolicy{Timezone: "America/New_York", Windows: []PeakHoursWindowSpec{
			wd("", "02:15", "04:00"),
		}}
		// 2026-03-08: 02:15 EST does not exist; the wall clock jumps
		// 01:59:59 -> 03:00, so the window effectively runs 03:00-04:00.
		after := p.Evaluate(time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC)) // 03:30 EDT
		assert.True(t, after.InPeak, "03:30 EDT wall clock is inside 02:15-04:00")
		assert.True(t, after.Blocked)
		assert.True(t, after.BandStart.Equal(time.Date(2026, 3, 8, 3, 0, 0, 0, nyLoc)),
			"the band starts at the transition, not at the nonexistent 02:15")
		assert.True(t, after.ReopensAt.Equal(time.Date(2026, 3, 8, 4, 0, 0, 0, nyLoc)),
			"membership and band agree on the 04:00 end")
		before := p.Evaluate(time.Date(2026, 3, 8, 6, 59, 59, 0, time.UTC)) // 01:59:59 EST
		assert.False(t, before.InPeak)
		// Control: the next day the window keeps its full wall-clock span.
		ctrl := p.Evaluate(time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC)) // 02:30 EDT
		assert.True(t, ctrl.InPeak)
		assert.True(t, ctrl.BandStart.Equal(time.Date(2026, 3, 9, 2, 15, 0, 0, nyLoc)))
	})

	t.Run("DST spring gap end clamps the band to the transition", func(t *testing.T) {
		p := PeakHoursPolicy{Timezone: "America/New_York", Windows: []PeakHoursWindowSpec{
			wd("", "01:00", "02:30"),
		}}
		// 2026-03-08: 02:30 does not exist either; membership ends when
		// the clock jumps, so the band must end at 03:00 EDT (07:00 UTC),
		// not at the normalized 03:30.
		in := p.Evaluate(time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC)) // 01:30 EST
		assert.True(t, in.InPeak)
		assert.True(t, in.ReopensAt.Equal(time.Date(2026, 3, 8, 3, 0, 0, 0, nyLoc)))
		out := p.Evaluate(time.Date(2026, 3, 8, 7, 1, 0, 0, time.UTC)) // 03:01 EDT
		assert.False(t, out.InPeak, "membership ends at the gap; band must agree")
	})

	t.Run("DST fall back yields two disjoint passes with their own reopens", func(t *testing.T) {
		p := PeakHoursPolicy{Timezone: "America/New_York", Windows: []PeakHoursWindowSpec{
			wd("", "01:15", "01:45"),
		}}
		// 2026-11-01: the repeated wall-clock hour splits the window into
		// [01:15 EDT, 01:45 EDT) and [01:15 EST, 01:45 EST).
		first := p.Evaluate(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) // 01:30 EDT
		assert.True(t, first.InPeak)
		assert.True(t, first.ReopensAt.Equal(time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC)),
			"the first pass ends at 01:45 EDT, before the fall-back transition")
		between := p.Evaluate(time.Date(2026, 11, 1, 6, 7, 0, 0, time.UTC)) // 01:07 EST
		assert.False(t, between.InPeak, "01:07 EST is below the 01:15 start")
		assert.True(t, between.NextTransition.Equal(time.Date(2026, 11, 1, 6, 15, 0, 0, time.UTC)),
			"the second pass starts at 01:15 EST")
		second := p.Evaluate(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)) // 01:30 EST
		assert.True(t, second.InPeak)
		assert.True(t, second.ReopensAt.Equal(time.Date(2026, 11, 1, 6, 45, 0, 0, time.UTC)))
	})

	t.Run("DST fall back window spanning the repeat stays one band", func(t *testing.T) {
		p := PeakHoursPolicy{Timezone: "America/New_York", Windows: []PeakHoursWindowSpec{
			wd("", "00:30", "02:30"),
		}}
		// The two segment pieces touch at the transition, so the merged
		// band is continuous and reopens at 02:30 EST.
		st := p.Evaluate(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) // 01:30 EDT
		assert.True(t, st.InPeak)
		assert.True(t, st.ReopensAt.Equal(time.Date(2026, 11, 1, 2, 30, 0, 0, nyLoc)))
	})
}

// TestParsePeakPauseDuration covers relative durations, absolute
// RFC3339 stamps, and every rejection path.
func TestParsePeakPauseDuration(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		spec    string
		want    time.Time
		wantErr bool
	}{
		{"hours", "2h", now.Add(2 * time.Hour), false},
		{"minutes only", "90m", now.Add(90 * time.Minute), false},
		{"compound duration", "1h30m", now.Add(90 * time.Minute), false},
		{"days suffix", "7d", now.Add(168 * time.Hour), false},
		{"days plus hours", "6d12h", now.Add(156 * time.Hour), false},
		{"absolute RFC3339", now.Add(time.Hour).Format(time.RFC3339), now.Add(time.Hour), false},
		{"empty", "", time.Time{}, true},
		{"zero duration", "0s", time.Time{}, true},
		{"negative duration", "-1h", time.Time{}, true},
		{"over the cap", "8d", time.Time{}, true},
		{"absolute in the past", now.Add(-time.Hour).Format(time.RFC3339), time.Time{}, true},
		{"absolute beyond the cap", now.Add(8 * 24 * time.Hour).Format(time.RFC3339), time.Time{}, true},
		{"garbage", "garbage", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePeakPauseDuration(tc.spec, now)
			if tc.wantErr {
				require.Error(t, err, "spec %q must be rejected", tc.spec)
				return
			}
			require.NoError(t, err, "spec %q must parse", tc.spec)
			require.WithinDuration(t, tc.want, got, time.Second, "result for %q", tc.spec)
		})
	}
}
