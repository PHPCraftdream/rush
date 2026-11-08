package cmd

import (
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func formatProviderPeakHoursStatus(peak *config.ProviderPeakHours, now time.Time) string {
	if peak == nil {
		return ""
	}
	if peak.Legacy != nil {
		return formatPeakHoursStatus(peak.Legacy, now)
	}
	if peak.Policy == nil {
		return ""
	}
	policy := peak.Policy
	return fmt.Sprintf("%d weekly windows (%s; %s)", len(policy.Windows),
		peakDisplayLabel(policy.Timezone), peakStatusText(peak.Status(now), peakDisplayLoc(policy.Timezone)))
}
