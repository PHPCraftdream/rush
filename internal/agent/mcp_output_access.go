package agent

import (
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
)

// Activation requires the finalization-aware session service adapter. The base
// service intentionally has no MCPOutputStore method: nil means legacy output,
// never truncation without durable retention or an unrestricted write fallback.
func (c *coordinator) mcpOutputAccess() *tools.MCPOutputAccess {
	access := &tools.MCPOutputAccess{
		Allowed: make(map[string]bool), Permissions: c.permissions, WorkingDir: c.cfg.WorkingDir(),
	}
	if provider, ok := c.sessions.(interface {
		MCPOutputStore() *session.MCPOutputStore
	}); ok {
		if store := provider.MCPOutputStore(); store != nil {
			access.Store = store
		}
	}
	return access
}

func admitMCPOutputSources(access *tools.MCPOutputAccess, final []fantasy.AgentTool) {
	for _, tool := range final {
		if source, ok := tool.(*tools.Tool); ok {
			access.Allowed[source.Name()] = true
		}
	}
}
