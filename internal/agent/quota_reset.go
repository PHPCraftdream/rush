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
func QuotaLimitResetTime(err error, now time.Time) (time.Time, bool) {
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

	return time.Time{}, false
}

// zaiResetRe matches z.ai's "Your limit will reset at YYYY-MM-DD HH:MM:SS"
// fragment as emitted by their rate-limit error bodies. Time is captured
// without a zone — by convention CST (UTC+8).
var zaiResetRe = regexp.MustCompile(`limit will reset at\s+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)

func parseZAIResetHint(text string) (time.Time, bool) {
	m := zaiResetRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return time.Time{}, false
	}
	stamp := strings.ReplaceAll(m[1], "T", " ")
	// Fixed CST zone — z.ai's API surface is anchored there. Using a fixed
	// offset (no historical DST table) is exactly right for a wall-clock
	// stamp like this one.
	cst := time.FixedZone("CST", 8*3600)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, cst)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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
	local := resetAt.Local()
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
