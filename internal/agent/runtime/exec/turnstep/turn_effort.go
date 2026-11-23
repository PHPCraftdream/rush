package turnstep

import "github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

// TurnSmartServiceTier is the configured speed tier of the resolved smart model.
func TurnSmartServiceTier(smartModel agentcall.Model) string {
	return smartModel.ModelCfg.ServiceTier
}
