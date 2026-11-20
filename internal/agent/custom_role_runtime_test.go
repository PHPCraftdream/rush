package agent

import (
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
)

func customRoleResolverConfig() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("p", config.ProviderConfig{ID: "p"})
	empty := []string{}
	return &config.Config{
		Providers: providers,
		Roles: map[string]config.CustomRole{
			"analyst": {SelectedModel: config.SelectedModel{Provider: "p", Model: "m", ReasoningEffort: "high", Think: true, MaxTokens: 77}, Tools: &empty},
		},
	}
}
