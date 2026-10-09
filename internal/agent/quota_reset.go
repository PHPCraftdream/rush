package agent

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

// QuotaLimitResetTime extracts, from a provider error, the wall-clock time
// at which a rate-limit/quota window reopens. The hint usually lives in the
// 429 response headers (never the error string), so it's read off
// fantasy.ProviderError's captured headers; z.ai-style providers only put it
// in the error body text, handled as a fallback. Returns ok=false when the
// error isn't a provider error or carries no usable reset hint. `now` is
// taken as an argument for testability.
//
// Moved from internal/cmd/ping.go's former pingRateLimitReset (task #979)
// so `rush run`'s quota-exceeded turn failure can reuse the exact same
// parsing `rush ping` already relied on, instead of reimplementing it.
func QuotaLimitResetTime(err error, now time.Time) (reset time.Time, ok bool) {
	defer func() {
		if !ok && err != nil {
			reset, ok = parseCLIResetText(err.Error(), now)
		}
	}()
	if err == nil {
		return time.Time{}, false
	}
	// Last-resort string parse: even when fantasy didn't surface a
	// ProviderError, the err.Error() text often still contains the
	// z.ai-style "Your limit will reset at ..." hint (it's wrapped by
	// the retry layer). Try it before we give up entirely.
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) {
		if t, ok := parseZAIResetHint(err.Error()); ok {
			return t, true
		}
		return time.Time{}, false
	}
	if pe.ResponseHeaders == nil {
		if pe.Message != "" {
			if t, ok := parseZAIResetHint(pe.Message); ok {
				return t, true
			}
		}
		if t, ok := parseZAIResetHint(err.Error()); ok {
			return t, true
		}
		if reset, ok := structuredLimitReset(pe, now); ok {
			return reset, true
		}
		return time.Time{}, false
	}
	get := func(name string) string {
		for k, v := range pe.ResponseHeaders {
			if strings.EqualFold(k, name) {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}

	// Retry-After (RFC 7231): delta-seconds or an HTTP-date.
	if ra := get("retry-after"); ra != "" {
		if secs, e := strconv.Atoi(ra); e == nil {
			return now.Add(time.Duration(secs) * time.Second), true
		}
		if t, e := http.ParseTime(ra); e == nil {
			return t, true
		}
	}

	// Anthropic reset headers: RFC 3339 timestamps (unix seconds for the
	// unified one). Take the latest — that's when every bucket has refilled.
	var latest time.Time
	for _, h := range []string{
		"anthropic-ratelimit-unified-reset",
		"anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-reset",
		"anthropic-ratelimit-requests-reset",
	} {
		v := get(h)
		if v == "" {
			continue
		}
		if t, e := time.Parse(time.RFC3339, v); e == nil {
			if t.After(latest) {
				latest = t
			}
			continue
		}
		if secs, e := strconv.ParseInt(v, 10, 64); e == nil {
			if t := time.Unix(secs, 0); t.After(latest) {
				latest = t
			}
		}
	}
	if !latest.IsZero() {
		return latest, true
	}

	// OpenAI-style: durations like "1s" / "6m0s" relative to now.
	var maxDur time.Duration
	for _, h := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if v := get(h); v != "" {
			if d, e := time.ParseDuration(v); e == nil && d > maxDur {
				maxDur = d
			}
		}
	}
	if maxDur > 0 {
		return now.Add(maxDur), true
	}

	// z.ai-style fallback: the reset hint only lives in the error body,
	// e.g. "Your limit will reset at 2026-06-17 14:49:28". No tz marker —
	// z.ai's servers report in China Standard Time (UTC+8), so we parse
	// the wall-clock as CST and let the caller .Local() it.
	if pe.Message != "" {
		if t, ok := parseZAIResetHint(pe.Message); ok {
			return t, true
		}
	}

	return structuredLimitReset(pe, now)
}

// zaiResetRe matches z.ai's "Your limit will reset at YYYY-MM-DD HH:MM:SS"
// fragment as emitted by their rate-limit error bodies. Time is captured
// without a zone — by convention CST (UTC+8).
var zaiResetRe = regexp.MustCompile(`limit will reset at\s+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)

// resetOffsetRe matches an explicit zone marker right after a stamp, i.e. a
// stamp that is already localized (or otherwise zone-qualified).
var resetOffsetRe = regexp.MustCompile(`^ ?(?:[+-]\d{2}:?\d{2}|Z)`)

const (
	zaiResetLayout   = "2006-01-02 15:04:05"
	localResetLayout = zaiResetLayout + " -07:00"
)

// zaiResetStamp finds the hint in text: the stamp's byte span (including an
// offset marker when present), the parsed time, and whether the stamp
// already carried an offset. The single owner of the no-marker zone
// convention (CST, UTC+8).
func zaiResetStamp(text string) (start, end int, t time.Time, qualified, ok bool) {
	m := zaiResetRe.FindStringSubmatchIndex(text)
	if m == nil {
		return 0, 0, time.Time{}, false, false
	}
	start, end = m[2], m[3]
	stamp := strings.ReplaceAll(text[start:end], "T", " ")
	if raw := resetOffsetRe.FindString(text[end:]); raw != "" {
		off := strings.TrimSpace(raw)
		switch {
		case off == "Z":
			off = "+00:00"
		case !strings.Contains(off, ":"):
			off = off[:3] + ":" + off[3:]
		}
		pt, err := time.Parse(localResetLayout, stamp+" "+off)
		if err != nil {
			return 0, 0, time.Time{}, false, false
		}
		return start, end + len(raw), pt, true, true
	}
	// Fixed CST zone — z.ai's API surface is anchored there. Using a fixed
	// offset (no historical DST table) is exactly right for a wall-clock
	// stamp like this one.
	cst := time.FixedZone("CST", 8*3600)
	pt, err := time.ParseInLocation(zaiResetLayout, stamp, cst)
	if err != nil {
		return 0, 0, time.Time{}, false, false
	}
	return start, end, pt, false, true
}

func parseZAIResetHint(text string) (time.Time, bool) {
	_, _, t, _, ok := zaiResetStamp(text)
	return t, ok
}

// LocalizeResetHint rewrites a provider's zone-less "limit will reset at
// <stamp>" hint (z.ai: CST) in text to machine-local time with an explicit
// offset, plus a countdown while the reset is still ahead of now. Text
// without a hint, or whose stamp already carries an offset, is returned
// unchanged, so applying it twice is a no-op.
func LocalizeResetHint(text string, now time.Time) string {
	start, end, t, qualified, ok := zaiResetStamp(text)
	if !ok || qualified {
		return text
	}
	local := t.In(resetDisplayZone())
	out := local.Format(localResetLayout)
	if d := local.Sub(now); d > 0 {
		out += " (in " + FormatResetDuration(d) + ")"
	}
	return text[:start] + out + text[end:]
}

// localizedResetError carries err with its reset hint already rewritten to
// local time; errors.Is/As still reach err.
type localizedResetError struct {
	err  error
	text string
}

func (e *localizedResetError) Error() string { return e.text }
func (e *localizedResetError) Unwrap() error { return e.err }

// LocalizeResetError returns err whose text shows the reset hint in local
// time (see LocalizeResetHint); err itself when there is nothing to rewrite.
func LocalizeResetError(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	if loc := LocalizeResetHint(text, time.Now()); loc != text {
		return &localizedResetError{err: err, text: loc}
	}
	return err
}

// FormatResetDuration humanises a positive countdown — "4h12m07s" reads
// cleaner than Go's default "4h12m6.832s" because we round to seconds and
// omit zero leading components.
func FormatResetDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// QuotaLimitGuidance returns a local-time reset line for a quota-classified
// 429 (isQuotaLimit — a multi-hour usage wall, not a momentary overload),
// or ok=false when err isn't such a wall or carries no parseable reset
// hint. Callers keep the provider's raw message unchanged in that case
// (graceful degradation — never panics, never returns an empty non-ok
// guidance string).
//
// This is the single source of truth for the local-time conversion, in
// the same spirit as PeakHoursGuidance above it: any call site that needs
// to tell the operator when a quota wall reopens (turn finish details,
// stderr) reuses this one function so the two never drift.
func QuotaLimitGuidance(err error) (guidance string, ok bool) {
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) || !isQuotaLimit(pe) {
		return "", false
	}
	resetAt, found := QuotaLimitResetTime(err, time.Now())
	if !found {
		return "", false
	}
	local := resetAt.In(resetDisplayZone())
	line := fmt.Sprintf(
		"Limit resets: %s (local time, RFC3339: %s)",
		local.Format("2006-01-02 15:04:05 -07:00"),
		local.Format(time.RFC3339),
	)
	if in := time.Until(local).Round(time.Second); in > 0 {
		line += fmt.Sprintf(" — in %s", FormatResetDuration(in))
	}
	return line, true
}

// resetDisplayZone is the zone reset times are shown in; tests swap it
// instead of time.Local, which every time.Now call reads.
var resetDisplayZone = func() *time.Location { return time.Local }
