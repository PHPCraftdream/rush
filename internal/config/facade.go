// Package config facade over extracted domain packages.
//
// This file is the permanent re-export surface reserved by the package
// design (section 2.5): as sub-packages are extracted under
// internal/config/domains/, their exported API is re-declared here so
// the root package and its existing callers keep compiling unchanged.
// One import group per domain; the file grows one block per lane.
//
// Note on the three peak-hours merge helpers
// (StripPeakHoursOverlays, ResolvePeakHoursOverlays,
// ApplyPeakHoursOverlays): their signatures mention the unexported
// type peak.peakHoursOverlay, so they cannot be aliased or forwarded
// from this package; root callers use them via the peak qualifier.
package config

import (
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/peak"
)

// Types re-exported from the peak domain.
type (
	PeakHoursWindow     = peak.PeakHoursWindow
	PeakHoursWindowSpec = peak.PeakHoursWindowSpec
	PeakHoursPolicy     = peak.PeakHoursPolicy
	PeakHoursStatus     = peak.PeakHoursStatus
	ProviderPeakHours   = peak.ProviderPeakHours
)

// Constants re-exported from the peak domain.
const (
	PeakHoursMaxWindows = peak.PeakHoursMaxWindows
	PeakPauseMaxWindow  = peak.PeakPauseMaxWindow
)

// Functions re-exported from the peak domain.
func ParsePeakPauseDuration(spec string, now time.Time) (time.Time, error) {
	return peak.ParsePeakPauseDuration(spec, now)
}

func ParsePeakDays(spec string) ([7]bool, error) {
	return peak.ParsePeakDays(spec)
}

func FormatPeakDays(days [7]bool) string {
	return peak.FormatPeakDays(days)
}

func ParsePeakClock(s string, allowEnd bool) (int, error) {
	return peak.ParsePeakClock(s, allowEnd)
}

func ResolvePeakTimezone(name string) (*time.Location, error) {
	return peak.ResolvePeakTimezone(name)
}
