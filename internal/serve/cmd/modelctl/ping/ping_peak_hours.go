package ping

import (
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	provpeak "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/providers/peakhours"
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
		provpeak.PeakDisplayLabel(policy.Timezone), provpeak.PeakStatusText(peak.Status(now), provpeak.PeakDisplayLoc(policy.Timezone)))
}
