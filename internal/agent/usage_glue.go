package agent

import (
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
)

// PeakHoursGuidance returns the peak-hours refusal guidance paragraph
// (single source of truth lives in the usage package).
func PeakHoursGuidance(err error) string {
	return usage.PeakHoursGuidance(err)
}

// peakHoursStoppedFinishText builds the (msg, details) finish pair for a
// pre-flight peak-hours refusal (delegates to the usage package).
func peakHoursStoppedFinishText(err error) (msg, details string) {
	return usage.PeakHoursStoppedFinishText(err)
}

// providerLimitClass aliases the usage package's provider-limit class.
type providerLimitClass = usage.ProviderLimitClass

const (
	providerLimitUnknown   = usage.ProviderLimitUnknown
	providerLimitTransient = usage.ProviderLimitTransient
	providerLimitHard      = usage.ProviderLimitHard
)

// classifyHardProviderLimit classifies a provider error, reading the lazy
// display zone.
func classifyHardProviderLimit(err error, hint string, now time.Time) providerLimitClass {
	return usage.ClassifyHardProviderLimit(err, hint, now, resetDisplayZone)
}

// parseCLIResetText parses CLI-shaped quota reset text (delegates to usage).
func parseCLIResetText(text string, now time.Time) (time.Time, bool) {
	return usage.ParseCLIResetText(text, now)
}

// parseZAIResetHint parses z.ai's quota reset text (delegates to usage).
func parseZAIResetHint(text string) (time.Time, bool) {
	return usage.ParseZAIResetHint(text)
}

// usageIsZero reports whether a fantasy usage record is all-zero (delegates
// to the usage package).
func usageIsZero(u fantasy.Usage) bool {
	return usage.UsageIsZero(u)
}

// fallbackStepUsage estimates a step's usage from messages when the provider
// reported nothing, passing the root's approxTokenCount as the tokenizer
// seam (delegates to the usage package).
func fallbackStepUsage(messages []fantasy.Message, step fantasy.StepResult) (fantasy.Usage, bool) {
	return usage.FallbackStepUsage(messages, step, approxTokenCount)
}

// cloneFantasyMessages deep-copies a fantasy message slice (delegates to the
// usage package).
func cloneFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	return usage.CloneFantasyMessages(messages)
}

// resetDisplayZone is the zone reset times are shown in; tests swap it
// instead of time.Local, which every time.Now call reads.
var resetDisplayZone = func() *time.Location { return time.Local }

// LocalizeResetHint rewrites a provider's zone-less reset hint in the
// display zone, read lazily at call time.
func LocalizeResetHint(text string, now time.Time) string {
	return usage.LocalizeResetHintWithZone(text, now, resetDisplayZone)
}

// QuotaLimitGuidance returns a local-time reset line for a quota-classified
// error, using the lazily read display zone.
func QuotaLimitGuidance(err error) (string, bool) {
	return usage.QuotaLimitGuidanceWithZone(err, resetDisplayZone)
}

// QuotaLimitResetTime returns the reset time of a quota-classified error in
// the display zone, read lazily at call time.
func QuotaLimitResetTime(err error, now time.Time) (time.Time, bool) {
	return usage.QuotaLimitResetTimeWithZone(err, now, resetDisplayZone)
}

// LocalizeResetError rewrites err's reset hint in the display zone, read
// lazily at call time.
func LocalizeResetError(err error) error {
	return usage.LocalizeResetErrorWithZone(err, resetDisplayZone)
}

// FormatResetDuration renders a duration until a limit reset, zoned-free.
func FormatResetDuration(d time.Duration) string {
	return usage.FormatResetDuration(d)
}
