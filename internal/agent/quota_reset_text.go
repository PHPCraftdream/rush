package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	cliEpochResetRe = regexp.MustCompile(`(?i)(?:usage limit reached|hit your (?:usage )?limit)\|([^\s]+)`)
	cliDateResetRe  = regexp.MustCompile(`(?i)try again at\s+([^\r\n]+)`)
	cliOrdinalRe    = regexp.MustCompile(`(?i)(\d)(st|nd|rd|th)\b`)
	cliClockResetRe = regexp.MustCompile(`(?i)(?:reset at|resets)\s+([^\r\n]+)`)
	cliDateClockRe  = regexp.MustCompile(`^(?:[1-9]|1[0-2]):[0-5][0-9]$`)
	cliClockRe      = regexp.MustCompile(`(?i)^(\d{1,2})(?::(\d{2}))?\s*(am|pm)(?:\s+\(([A-Za-z0-9_+./-]+)\))?\.?$`)
)

// parseCLIResetText parses failure-channel hints; callers supply a fixed now.
func parseCLIResetText(text string, now time.Time) (time.Time, bool) {
	if m := cliEpochResetRe.FindStringSubmatch(text); m != nil {
		seconds, err := strconv.ParseInt(m[1], 10, 64)
		// Epoch hints are seconds, not milliseconds or unbounded time.Time years.
		if err == nil && seconds > 0 && seconds <= 253402300799 {
			return time.Unix(seconds, 0), true
		}
		return time.Time{}, false
	}
	if m := cliDateResetRe.FindStringSubmatch(text); m != nil {
		stamp := strings.ToUpper(cliOrdinalRe.ReplaceAllString(strings.TrimSuffix(strings.TrimSpace(m[1]), "."), "$1"))
		// Go's month parser accepts title case, not uppercase month names.
		fields := strings.Fields(stamp)
		if len(fields) == 5 {
			if !cliDateClockRe.MatchString(fields[3]) {
				return time.Time{}, false
			}
			fields[0] = strings.ToUpper(fields[0][:1]) + strings.ToLower(fields[0][1:])
			stamp = strings.Join(fields, " ")
			if t, err := time.ParseInLocation("Jan 2, 2006 3:04 PM", stamp, time.Local); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	if m := cliClockResetRe.FindStringSubmatch(text); m != nil {
		clock := cliClockRe.FindStringSubmatch(strings.TrimSpace(m[1]))
		if clock == nil {
			return time.Time{}, false
		}
		hour, _ := strconv.Atoi(clock[1])
		minute := 0
		if clock[2] != "" {
			minute, _ = strconv.Atoi(clock[2])
		}
		if hour < 1 || hour > 12 || minute > 59 {
			return time.Time{}, false
		}
		hour %= 12
		if strings.EqualFold(clock[3], "pm") {
			hour += 12
		}
		loc := time.Local
		if clock[4] != "" {
			if zone, err := time.LoadLocation(clock[4]); err == nil {
				loc = zone
			}
		}
		localNow := now.In(loc)
		y, month, day := localNow.Date()
		candidate := time.Date(y, month, day, hour, minute, 0, 0, loc)
		if !candidate.After(now) {
			next := localNow.AddDate(0, 0, 1)
			y, month, day = next.Date()
			candidate = time.Date(y, month, day, hour, minute, 0, 0, loc)
		}
		// Reject nonexistent wall clocks (e.g. a DST spring-forward gap).
		if candidate.Hour() != hour || candidate.Minute() != minute {
			return time.Time{}, false
		}
		return candidate, true
	}
	return time.Time{}, false
}
