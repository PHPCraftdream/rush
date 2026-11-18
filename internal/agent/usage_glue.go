package agent

import (
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
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
	return usage.ClassifyHardProviderLimit(err, hint, now, quotaResetDisplayZone)
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
// reported nothing, passing the provauth.ApproxTokenCount estimator as the
// tokenizer seam (delegates to the usage package).
func fallbackStepUsage(messages []fantasy.Message, step fantasy.StepResult) (fantasy.Usage, bool) {
	return usage.FallbackStepUsage(messages, step, provauth.ApproxTokenCount)
}

// cloneFantasyMessages deep-copies a fantasy message slice (delegates to the
// usage package).
func cloneFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	return usage.CloneFantasyMessages(messages)
}

// quotaResetDisplayZone is the zone callback the quota reset display paths use;
// it defaults to the usage package's lazy read-only accessor. Tests swap
// this callback (see setTestLocal) instead of time.Local, which
// every time.Now call reads.
var quotaResetDisplayZone = usage.DisplayZone

// LocalizeResetHint rewrites a provider's zone-less reset hint in the
// display zone, read lazily at call time.
func LocalizeResetHint(text string, now time.Time) string {
	return usage.LocalizeResetHintWithZone(text, now, quotaResetDisplayZone)
}

// QuotaLimitGuidance returns a local-time reset line for a quota-classified
// error, using the lazily read display zone.
func QuotaLimitGuidance(err error) (string, bool) {
	return usage.QuotaLimitGuidanceWithZone(err, quotaResetDisplayZone)
}

// QuotaLimitResetTime returns the reset time of a quota-classified error in
// the display zone, read lazily at call time.
func QuotaLimitResetTime(err error, now time.Time) (time.Time, bool) {
	return usage.QuotaLimitResetTimeWithZone(err, now, quotaResetDisplayZone)
}

// LocalizeResetError rewrites err's reset hint in the display zone, read
// lazily at call time.
func LocalizeResetError(err error) error {
	return usage.LocalizeResetErrorWithZone(err, quotaResetDisplayZone)
}

// FormatResetDuration renders a duration until a limit reset, zoned-free.
func FormatResetDuration(d time.Duration) string {
	return usage.FormatResetDuration(d)
}
