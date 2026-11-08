package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PeakHoursWindowSpec is one weekly window: on the configured days, from
// Start (inclusive) to End (exclusive), in the policy's timezone.
type PeakHoursWindowSpec struct {
	Days  string `json:"days,omitempty" jsonschema:"description=Days of week: mon..sun; a single day (fri), a comma list (mon,wed,fri), a range (mon-fri), or a wrapping range (fri-mon). Empty means every day."`
	Start string `json:"start" jsonschema:"description=Window start HH:MM inclusive in the policy timezone,example=14:00"`
	End   string `json:"end" jsonschema:"description=Window end HH:MM exclusive; 24:00 allowed here only,example=18:00"`
}

// PeakHoursPolicy is the canonical weekly peak-hours policy stored under
// provider.peak_hours (new format).
type PeakHoursPolicy struct {
	Timezone string `json:"timezone,omitempty" jsonschema:"description=Timezone for the windows: empty or local = machine local time, UTC, an IANA name like Asia/Shanghai, or a fixed offset like UTC+08:00"`
	// Windows always marshals, even when empty: an explicit empty list is
	// the persisted form of explicit local off. Dropping the key would
	// make the stored object re-parse as a message-only legacy overlay,
	// which merges onto the inherited global schedule instead of
	// suppressing it.
	Windows     []PeakHoursWindowSpec `json:"windows" jsonschema:"description=Up to 64 weekly windows; overlapping and touching windows merge into one continuous blocked band"`
	Message     string                `json:"message,omitempty" jsonschema:"description=Optional custom text appended to the peak-hours refusal guidance"`
	PausedUntil string                `json:"paused_until,omitempty" jsonschema:"description=RFC3339 timestamp until which the provider is explicitly paused; empty = not paused"`
}

// PeakHoursMaxWindows is the maximum number of weekly windows a single
// policy may declare.
const PeakHoursMaxWindows = 64

// peakHorizonBackDays/peakHorizonForwardDays bound how far behind and
// ahead Evaluate looks when building concrete window intervals for
// band computation.
const (
	peakHorizonBackDays    = 2
	peakHorizonForwardDays = 8
)

// Validate checks the timezone, every window spec, and the pause stamp.
func (p PeakHoursPolicy) Validate() error {
	if _, err := ResolvePeakTimezone(p.Timezone); err != nil {
		return err
	}
	if len(p.Windows) > PeakHoursMaxWindows {
		return fmt.Errorf("peak_hours: too many windows (%d, max %d)", len(p.Windows), PeakHoursMaxWindows)
	}
	for i, w := range p.Windows {
		if err := w.Validate(); err != nil {
			return fmt.Errorf("window %d: %w", i+1, err)
		}
	}
	if p.PausedUntil != "" {
		if _, err := time.Parse(time.RFC3339, p.PausedUntil); err != nil {
			return fmt.Errorf("peak_hours paused_until %q: %w", p.PausedUntil, err)
		}
	}
	return nil
}

// Validate checks one window spec against the policy clock rules. A
// zero-length window (start == end) is a hard error.
func (w PeakHoursWindowSpec) Validate() error {
	if _, err := ParsePeakDays(w.Days); err != nil {
		return err
	}
	startMin, err := ParsePeakClock(w.Start, false)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	endMin, err := ParsePeakClock(w.End, true)
	if err != nil {
		return fmt.Errorf("end: %w", err)
	}
	if startMin == endMin {
		return errors.New("zero-length window")
	}
	return nil
}

// IsZero reports whether the policy carries no windows, no timezone, no
// message, and no pause stamp — i.e. it means nothing at all.
func (p PeakHoursPolicy) IsZero() bool {
	return len(p.Windows) == 0 && p.Timezone == "" && p.Message == "" && p.PausedUntil == ""
}

// InPeak reports whether now falls inside any weekly window (before
// any explicit pause is considered). It is the cheap per-request check.
func (p PeakHoursPolicy) InPeak(now time.Time) bool {
	loc, tzErr := ResolvePeakTimezone(p.Timezone)
	if tzErr != nil {
		loc = time.Local
	}
	return p.inWindow(now, loc)
}

// inWindow reports wall-clock membership of now in any weekly window,
// ignoring any pause. This is the cheap per-request check: no interval
// building, no sorting, no allocation beyond tiny per-window parses.
func (p PeakHoursPolicy) inWindow(now time.Time, loc *time.Location) bool {
	t := now.In(loc)
	wd := (int(t.Weekday()) + 6) % 7
	cur := t.Hour()*60 + t.Minute()
	for _, w := range p.Windows {
		days, err := ParsePeakDays(w.Days)
		if err != nil {
			continue
		}
		startMin, err := ParsePeakClock(w.Start, false)
		if err != nil {
			continue
		}
		endMin, err := ParsePeakClock(w.End, true)
		if err != nil {
			continue
		}
		if startMin == endMin {
			continue
		}
		// Wall-clock semantics: start inclusive, end exclusive; 24:00 is
		// handled as endMin 1440. An overnight window (startMin > endMin)
		// belongs to its START day: the first leg covers the start day's
		// evening, the second leg covers the next morning under the start
		// day's day mask.
		if days[wd] && startMin < endMin && cur >= startMin && cur < endMin {
			return true
		}
		if days[wd] && startMin > endMin && cur >= startMin {
			// Overnight first leg on the start day.
			return true
		}
		if startMin > endMin && days[(wd+6)%7] && cur < endMin {
			// Overnight second leg under the start day's mask.
			return true
		}
	}
	return false
}

// Evaluate computes the full peak-hours status for now in the policy
// timezone. Wall-clock semantics are used on purpose so DST is handled
// by real clock behaviour. When the configured timezone fails to
// resolve it falls back to machine local time; load-time validation
// makes that unreachable through normal paths.
func (p PeakHoursPolicy) Evaluate(now time.Time) PeakHoursStatus {
	status := PeakHoursStatus{
		CheckedAt: now,
		Timezone:  p.Timezone,
	}
	loc, tzErr := ResolvePeakTimezone(p.Timezone)
	if tzErr != nil {
		loc = time.Local
	}
	status.InPeak = p.inWindow(now, loc)
	status.PausedUntil = pausedUntilTime(p.PausedUntil)
	status.Paused = status.PausedUntil.After(now)
	// A pause SUPPRESSES refusals until paused_until; it never adds
	// prohibition. Blocked is window membership alone, minus any active
	// pause: a paused-only policy refuses nothing — ever.
	status.Blocked = status.InPeak && !status.Paused

	bands := p.mergedBands(now, loc)
	horizon := now.Add(time.Duration(peakHorizonForwardDays) * 24 * time.Hour)
	status.BandStart = bandStartAt(bands, now)
	// ReopensAt is set ONLY when Blocked: a paused-inside-window provider
	// is not blocked and has no reopen moment.
	if status.Blocked {
		status.ReopensAt = bandEndAt(bands, now, horizon)
	}

	// NextTransition is the next future instant where Blocked actually
	// flips. While paused nothing blocks, so the only candidate flips
	// are the pause lift itself — and only when the lift lands inside
	// a band — and, after the lift, the first band starting later.
	// Unpaused, the next band start flips Blocked on. Zero when none
	// exists within the horizon.
	if status.Blocked {
		status.NextTransition = status.ReopensAt
	} else if status.Paused {
		pu := status.PausedUntil
		if !bandStartAt(bands, pu).IsZero() {
			status.NextTransition = pu
		} else if s := nextBandStartAfter(bands, pu); !s.IsZero() {
			status.NextTransition = s
		}
	} else if ns := nextBandStart(bands, now); !ns.IsZero() {
		status.NextTransition = ns
	}
	if status.NextTransition.After(horizon) {
		status.NextTransition = time.Time{}
	}
	return status
}

// pausedUntilTime parses the stored RFC3339 pause stamp; an unparseable
// value yields the zero time (not paused — Validate rejects it at load).
func pausedUntilTime(stored string) time.Time {
	if stored == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, stored)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// PeakHoursStatus is the evaluated peak-hours state at one instant.
type PeakHoursStatus struct {
	CheckedAt      time.Time
	InPeak         bool      // raw window membership, before pause
	Paused         bool      // explicit paused_until in the future
	PausedUntil    time.Time // zero when not paused
	Blocked        bool      // InPeak && !Paused — a pause suppresses, never adds, refusal
	ReopensAt      time.Time // zero = no known reopen moment; set only when Blocked
	BandStart      time.Time // start instant of the continuous blocked band containing CheckedAt; zero when not inside a band
	NextTransition time.Time // next instant where Blocked flips; zero = none within the 8-day bound
	Timezone       string    // configured Timezone verbatim; empty = machine local
}

// interval is one concrete half-open window occurrence [start, end).
type peakInterval struct {
	start time.Time
	end   time.Time
}

// windowIntervals returns the concrete half-open intervals of every
// window occurrence whose local start date lies in [now-2d, now+8d).
// Occurrences are derived from the zone's REAL offset segments, so the
// result is exactly the instant set inWindow describes — across DST
// transitions too: a spring-forward gap contributes only the instants
// that exist (the window effectively starts at the transition), and a
// fall-back repeat yields two disjoint pieces instead of one stretched
// interval.
func (p PeakHoursPolicy) windowIntervals(now time.Time, loc *time.Location) []peakInterval {
	base := now.In(loc)
	lo := base.AddDate(0, 0, -peakHorizonBackDays)
	hi := base.AddDate(0, 0, peakHorizonForwardDays)
	var out []peakInterval
	for _, w := range p.Windows {
		days, err := ParsePeakDays(w.Days)
		if err != nil {
			continue
		}
		startMin, err := ParsePeakClock(w.Start, false)
		if err != nil {
			continue
		}
		endMin, err := ParsePeakClock(w.End, true)
		if err != nil {
			continue
		}
		if startMin == endMin {
			continue
		}
		for d := lo; !d.After(hi); d = d.AddDate(0, 0, 1) {
			y, m, day := d.Date()
			wd := (int(d.Weekday()) + 6) % 7
			if !days[wd] {
				continue
			}
			if startMin < endMin {
				out = append(out, wallRangeIntervals(y, m, day, startMin, endMin, loc)...)
				continue
			}
			// Overnight: both legs belong to the START day's mask —
			// the evening leg on the start day, the morning leg on
			// the next calendar day.
			out = append(out, wallRangeIntervals(y, m, day, startMin, 1440, loc)...)
			nd := d.AddDate(0, 0, 1)
			ny, nm, nday := nd.Date()
			out = append(out, wallRangeIntervals(ny, nm, nday, 0, endMin, loc)...)
		}
	}
	return out
}

// wallRangeIntervals returns the real half-open intervals of calendar
// day (y, m, day) in loc whose wall clock lies in [fromMin, toMin)
// (toMin may be 1440 = end of day). The day is split at the zone's
// offset changes, the wall range is materialised per segment with that
// segment's own offset, and each piece is clamped to its segment — so
// skipped wall clocks (spring-forward gaps) contribute nothing, and
// repeated wall clocks (fall-back) produce one piece per pass.
func wallRangeIntervals(y int, m time.Month, day, fromMin, toMin int, loc *time.Location) []peakInterval {
	if fromMin >= toMin {
		return nil
	}
	offAt := func(t time.Time) int {
		_, off := t.In(loc).Zone()
		return off
	}
	start := time.Date(y, m, day, 0, 0, 0, 0, loc)
	end := time.Date(y, m, day+1, 0, 0, 0, 0, loc)
	// Locate the offset changes inside [start, end) by binary search on
	// the offset step function (at most two per day in practice).
	var cuts []time.Time
	if offAt(start) != offAt(end) {
		lo, hi := start, end
		for hi.Sub(lo) > 0 {
			mid := lo.Add(hi.Sub(lo) / 2)
			if !mid.After(lo) {
				break
			}
			if offAt(mid) == offAt(lo) {
				lo = mid
			} else {
				hi = mid
			}
		}
		cuts = append(cuts, hi)
		if offAt(hi) != offAt(end) {
			lo2, hi2 := hi, end
			for hi2.Sub(lo2) > 0 {
				mid := lo2.Add(hi2.Sub(lo2) / 2)
				if !mid.After(lo2) {
					break
				}
				if offAt(mid) == offAt(lo2) {
					lo2 = mid
				} else {
					hi2 = mid
				}
			}
			cuts = append(cuts, hi2)
		}
	}
	points := append([]time.Time{start}, cuts...)
	var out []peakInterval
	for i, segStart := range points {
		segEnd := end
		if i+1 < len(points) {
			segEnd = points[i+1]
		}
		zone := time.FixedZone("", offAt(segStart))
		ws := time.Date(y, m, day, fromMin/60, fromMin%60, 0, 0, zone)
		we := time.Date(y, m, day, toMin/60, toMin%60, 0, 0, zone)
		if ws.Before(segStart) {
			ws = segStart
		}
		if we.After(segEnd) {
			we = segEnd
		}
		if !ws.Before(we) {
			continue
		}
		out = append(out, peakInterval{start: ws, end: we})
	}
	return mergeTouching(out)
}

// mergeTouching sorts intervals by start and unions overlapping or
// touching neighbours, mirroring mergedBands' rule at the per-day level
// (a window boundary landing exactly on a transition splits one logical
// interval across two segments).
func mergeTouching(ivs []peakInterval) []peakInterval {
	if len(ivs) < 2 {
		return ivs
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].start.Before(ivs[j].start) })
	out := ivs[:1]
	for _, iv := range ivs[1:] {
		if n := len(out); !iv.start.After(out[n-1].end) {
			if iv.end.After(out[n-1].end) {
				out[n-1].end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// mergedBands unions all window occurrences (overlapping AND touching)
// into sorted disjoint bands.
func (p PeakHoursPolicy) mergedBands(now time.Time, loc *time.Location) []peakInterval {
	intervals := p.windowIntervals(now, loc)
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start.Before(intervals[j].start) })
	var bands []peakInterval
	for _, iv := range intervals {
		if n := len(bands); n > 0 && !iv.start.After(bands[n-1].end) {
			if iv.end.After(bands[n-1].end) {
				bands[n-1].end = iv.end
			}
			continue
		}
		bands = append(bands, iv)
	}
	return bands
}

// bandEndAt returns the end of the merged band containing now, or the
// zero time when now is not inside any band or the band extends
// beyond the horizon (a fully blocked week invents no reopen time).
func bandEndAt(bands []peakInterval, now, horizon time.Time) time.Time {
	for _, b := range bands {
		if !b.start.After(now) && b.end.After(now) {
			if b.end.After(horizon) {
				return time.Time{}
			}
			return b.end
		}
	}
	return time.Time{}
}

// bandStartAt returns the start of the merged band containing now, or
// the zero time when now is not inside any band. Unlike bandEndAt the
// result is not horizon-limited: the band's start may lie in the past.
func bandStartAt(bands []peakInterval, now time.Time) time.Time {
	for _, b := range bands {
		if !b.start.After(now) && b.end.After(now) {
			return b.start
		}
	}
	return time.Time{}
}

// nextBandStart returns the smallest band start strictly after now,
// or the zero time when none exists.
func nextBandStart(bands []peakInterval, now time.Time) time.Time {
	for _, b := range bands {
		if b.start.After(now) {
			return b.start
		}
	}
	return time.Time{}
}

// nextBandStartAfter returns the smallest band start strictly after t,
// or the zero time when none exists.
func nextBandStartAfter(bands []peakInterval, t time.Time) time.Time {
	for _, b := range bands {
		if b.start.After(t) {
			return b.start
		}
	}
	return time.Time{}
}

// ParsePeakPauseDuration turns a pause argument into an absolute
// paused-until timestamp relative to now. Accepted forms: Go durations
// ("2h", "90m", "1h30m"), whole days with a d suffix ("7d", also
// "6d12h" — each d-suffixed number contributes 24h), or an RFC3339
// absolute timestamp. Errors on empty input, non-positive durations,
// timestamps not strictly in the future, and anything extending beyond
// PeakPauseMaxWindow (7 days) from now.
const PeakPauseMaxWindow = 7 * 24 * time.Hour

func ParsePeakPauseDuration(spec string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return time.Time{}, errors.New("peak pause: empty duration")
	}
	if until, err := time.Parse(time.RFC3339, s); err == nil {
		if !until.After(now) {
			return time.Time{}, fmt.Errorf("peak pause %q: timestamp must be in the future", spec)
		}
		if until.After(now.Add(PeakPauseMaxWindow)) {
			return time.Time{}, fmt.Errorf("peak pause %q: extends beyond the %s maximum window", spec, PeakPauseMaxWindow)
		}
		return until, nil
	}
	d, err := parsePeakPauseRelative(s, spec)
	if err != nil {
		return time.Time{}, err
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("peak pause %q: duration must be positive", spec)
	}
	if d > PeakPauseMaxWindow {
		return time.Time{}, fmt.Errorf("peak pause %q: extends beyond the %s maximum window", spec, PeakPauseMaxWindow)
	}
	return now.Add(d), nil
}

// parsePeakPauseRelative converts a relative pause spec into a
// duration: each d-suffixed whole-day token contributes 24h and the
// remainder must parse as a Go duration. Anything unrecognised is a
// wrapped error naming the original input.
func parsePeakPauseRelative(s, spec string) (time.Duration, error) {
	var total time.Duration
	parts := strings.Split(s, "d")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		last := i == len(parts)-1
		if !last {
			days, err := strconv.ParseUint(part, 10, 32)
			if err != nil {
				return 0, fmt.Errorf("peak pause %q: invalid day count %q", spec, part)
			}
			total += time.Duration(days) * 24 * time.Hour
			continue
		}
		if part == "" {
			break
		}
		rest, err := time.ParseDuration(part)
		if err != nil {
			return 0, fmt.Errorf("peak pause %q: %w", spec, err)
		}
		total += rest
	}
	return total, nil
}

// dayNames maps the mon=0..sun=6 mask index to both abbreviations and
// full day names.
var peakDayNames = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// peakDayIndex resolves a single day token ("mon" or "monday", any
// case) to its mask index.
func peakDayIndex(token string) (int, error) {
	lowered := strings.ToLower(strings.TrimSpace(token))
	if lowered == "" {
		return 0, errors.New("empty day name")
	}
	for i, name := range peakDayNames {
		if lowered == name {
			return i, nil
		}
	}
	for i, name := range peakDayNames {
		if lowered == name+"day" {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown day %q", token)
}

// ParsePeakDays parses a days spec into a fixed-size mask indexed
// mon=0..sun=6. Empty/whitespace = every day. Accepts: single day
// ("mon"), comma list ("mon,wed,fri"), range ("mon-fri"), wrapping
// range ("fri-mon" = fri,sat,sun,mon), any case, 3-letter or full
// names. Unknown day = error.
func ParsePeakDays(spec string) ([7]bool, error) {
	var mask [7]bool
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		for i := range mask {
			mask[i] = true
		}
		return mask, nil
	}
	for _, part := range strings.Split(trimmed, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return mask, fmt.Errorf("empty day entry in %q", spec)
		}
		if idx := strings.IndexByte(part, '-'); idx >= 0 {
			from, err := peakDayIndex(part[:idx])
			if err != nil {
				return mask, err
			}
			to, err := peakDayIndex(part[idx+1:])
			if err != nil {
				return mask, err
			}
			if from == to {
				// Degenerate range: a single day, not the whole week.
				mask[from] = true
				continue
			}
			// Walk forward through the week, wrapping at sunday.
			mask[from] = true
			for d := (from + 1) % 7; ; d = (d + 1) % 7 {
				mask[d] = true
				if d == to {
					break
				}
			}
			continue
		}
		idx, err := peakDayIndex(part)
		if err != nil {
			return mask, err
		}
		mask[idx] = true
	}
	return mask, nil
}

// FormatPeakDays renders a mask back into the canonical minimal spec:
// all 7 days -> "" (every day), none -> "", single day -> "fri", a
// plain range -> "mon-fri", a wrapping range -> "fri-mon" (a cyclically
// contiguous mask), otherwise a comma list ordered mon..sun.
func FormatPeakDays(days [7]bool) string {
	all := true
	none := true
	for _, on := range days {
		if !on {
			all = false
		} else {
			none = false
		}
	}
	if all {
		return ""
	}
	if none {
		return ""
	}
	// Count the set days.
	total := 0
	for _, on := range days {
		if on {
			total++
		}
	}
	if total == 1 {
		for i := 0; i < 7; i++ {
			if days[i] {
				return peakDayNames[i]
			}
		}
	}
	// Cyclic run detection: rotate the week to start just after the
	// last unset day; when the set days then form one contiguous run,
	// render it as a range — covering both plain ranges (mon-fri) and
	// wrapping ones (fri-mon) with one rule.
	lastUnset := -1
	for i := 0; i < 7; i++ {
		if !days[i] {
			lastUnset = i
		}
	}
	if lastUnset >= 0 {
		runStart := (lastUnset + 1) % 7
		run := 0
		for i := runStart; days[i]; i = (i + 1) % 7 {
			run++
		}
		if run == total {
			return peakDayNames[runStart] + "-" + peakDayNames[(runStart+run-1)%7]
		}
	}
	// Scattered days: a comma list ordered mon..sun.
	parts := make([]string, 0, total)
	for i := 0; i < 7; i++ {
		if days[i] {
			parts = append(parts, peakDayNames[i])
		}
	}
	return strings.Join(parts, ",")
}

// ParsePeakClock parses HH:MM into minutes-since-midnight. When
// allowEnd is true, "24:00" is accepted as end-of-day (1440).
func ParsePeakClock(s string, allowEnd bool) (int, error) {
	if allowEnd && s == "24:00" {
		return 1440, nil
	}
	minutes, err := parseHHMM(s)
	if err != nil {
		return 0, err
	}
	return minutes, nil
}
