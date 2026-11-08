package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// Embed the IANA tz database so IANA zone names resolve even on
	// deployed Windows machines that have no system zoneinfo.
	_ "time/tzdata"
)

// ResolvePeakTimezone resolves a peak-hours timezone name into a
// *time.Location. Accepted: "" or "local"/"Local"/"LOCAL" (machine local
// time, the default), "utc"/"UTC", fixed offsets written as "UTC+08:00",
// "UTC-5", "+08:00", "-05:30" (hours 00..23, minutes 00..59), and IANA
// zone names ("Asia/Shanghai") resolved via time.LoadLocation.
func ResolvePeakTimezone(name string) (*time.Location, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return time.Local, nil
	}
	switch strings.ToLower(trimmed) {
	case "local":
		return time.Local, nil
	case "utc":
		return time.UTC, nil
	}
	if loc, ok, err := parseFixedOffsetZone(trimmed); ok {
		return loc, err
	}
	loc, err := time.LoadLocation(trimmed)
	if err != nil {
		return nil, fmt.Errorf("unknown peak_hours timezone %q: %w", name, err)
	}
	return loc, nil
}

// parseFixedOffsetZone parses the fixed-offset forms of a timezone name:
// "+HH:MM", "-HH", "UTC+HH:MM", and so on. The boolean result reports
// whether the input has the offset shape at all; when it does the error
// is non-nil only if the offset is out of range.
func parseFixedOffsetZone(name string) (*time.Location, bool, error) {
	rest := name
	lowered := strings.ToLower(rest)
	if strings.HasPrefix(lowered, "utc") {
		rest = name[3:]
	}
	if rest == "" || (rest[0] != '+' && rest[0] != '-') {
		return nil, false, nil
	}
	sign := 1
	body := rest[1:]
	if rest[0] == '-' {
		sign = -1
	}
	hoursPart := body
	minutesPart := 0
	if idx := strings.IndexByte(body, ':'); idx >= 0 {
		hoursPart = body[:idx]
		minutesStr := body[idx+1:]
		mi, err := strconv.Atoi(minutesStr)
		if err != nil || len(minutesStr) != 2 {
			return nil, true, fmt.Errorf("invalid peak_hours timezone %q: bad minute offset", name)
		}
		minutesPart = mi
	}
	hr, err := strconv.Atoi(hoursPart)
	if err != nil || len(hoursPart) < 1 || len(hoursPart) > 2 {
		return nil, true, fmt.Errorf("invalid peak_hours timezone %q: bad hour offset", name)
	}
	if hr < 0 || hr > 23 || minutesPart < 0 || minutesPart > 59 {
		return nil, true, fmt.Errorf("invalid peak_hours timezone %q: offset out of range", name)
	}
	offset := sign * (hr*3600 + minutesPart*60)
	return time.FixedZone(name, offset), true, nil
}
