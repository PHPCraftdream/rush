package wsconfig

import (
	"context"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

// Dispatch carries the WS config-family commands (design §3 S5).
func Dispatch(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage) bool {
	switch msg.Type {
	case wire.CmdGetConfig:
		c.Dispatch("handleGetConfig", msg.ID, func() { handleGetConfig(a, c, msg) })
		return true
	case wire.CmdGetLogs:
		c.Dispatch("handleGetLogs", msg.ID, func() { handleGetLogs(a, c, msg) })
		return true
	case wire.CmdSetTheme:
		c.Dispatch("handleSetTheme", msg.ID, func() { handleSetTheme(a, c, msg) })
		return true
	case wire.CmdSetKeepAlive:
		c.Dispatch("handleSetKeepAlive", msg.ID, func() { handleSetKeepAlive(a, c, msg) })
		return true
	case wire.CmdSetSessionModels:
		c.Dispatch("handleSetSessionModels", msg.ID, func() { handleSetSessionModels(ctx, a, c, msg) })
		return true
	case wire.CmdRemoveRecentModel:
		c.Dispatch("handleRemoveRecentModel", msg.ID, func() { handleRemoveRecentModel(a, c, msg) })
		return true
	case wire.CmdTrackModelUsage:
		c.Dispatch("handleTrackModelUsage", msg.ID, func() { handleTrackModelUsage(a, c, msg) })
		return true
	case wire.CmdGetScopedModels:
		c.Dispatch("handleGetScopedModels", msg.ID, func() { handleGetScopedModels(a, c, msg) })
		return true
	case wire.CmdSetScopedModel:
		c.Dispatch("handleSetScopedModel", msg.ID, func() { handleSetScopedModel(a, c, msg) })
		return true
	case wire.CmdClearScopedModel:
		c.Dispatch("handleClearScopedModel", msg.ID, func() { handleClearScopedModel(a, c, msg) })
		return true
	case wire.CmdGetCustomRoles:
		c.Dispatch("handleGetCustomRoles", msg.ID, func() { handleGetCustomRoles(a, c, msg) })
		return true
	case wire.CmdSetCustomRole:
		c.Dispatch("handleSetCustomRole", msg.ID, func() { handleSetCustomRole(a, c, msg) })
		return true
	case wire.CmdRemoveCustomRole:
		c.Dispatch("handleRemoveCustomRole", msg.ID, func() { handleRemoveCustomRole(a, c, msg) })
		return true
	case wire.CmdSetProviderKey:
		c.Dispatch("handleSetProviderKey", msg.ID, func() { handleSetProviderKey(a, c, msg) })
		return true
	case wire.CmdRemoveProviderKey:
		c.Dispatch("handleRemoveProviderKey", msg.ID, func() { handleRemoveProviderKey(a, c, msg) })
		return true
	case wire.CmdSetMCPDisabled:
		c.Dispatch("handleSetMCPDisabled", msg.ID, func() { SetMCPDisabled(ctx, a, c, msg) })
		return true
	case wire.CmdAddMCPServer:
		c.Dispatch("handleAddMCPServer", msg.ID, func() { handleAddMCPServer(ctx, a, c, msg) })
		return true
	case wire.CmdRemoveMCPServer:
		c.Dispatch("handleRemoveMCPServer", msg.ID, func() { handleRemoveMCPServer(a, c, msg) })
		return true
	case wire.CmdUpdateMCPServer:
		c.Dispatch("handleUpdateMCPServer", msg.ID, func() { handleUpdateMCPServer(ctx, a, c, msg) })
		return true
	case wire.CmdSetDebug:
		c.Dispatch("handleSetDebug", msg.ID, func() { handleSetDebug(a, c, msg) })
		return true
	case wire.CmdAddContextPath:
		c.Dispatch("handleAddContextPath", msg.ID, func() { handleAddContextPath(a, c, msg) })
		return true
	case wire.CmdRemoveContextPath:
		c.Dispatch("handleRemoveContextPath", msg.ID, func() { handleRemoveContextPath(a, c, msg) })
		return true
	case wire.CmdGetSkills:
		c.Dispatch("handleGetSkills", msg.ID, func() { handleGetSkills(a, c, msg) })
		return true
	case wire.CmdAddSkillsPath:
		c.Dispatch("handleAddSkillsPath", msg.ID, func() { handleAddSkillsPath(a, c, msg) })
		return true
	case wire.CmdRemoveSkillsPath:
		c.Dispatch("handleRemoveSkillsPath", msg.ID, func() { handleRemoveSkillsPath(a, c, msg) })
		return true
	case wire.CmdAddCustomProvider:
		c.Dispatch("handleAddCustomProvider", msg.ID, func() { handleAddCustomProvider(a, c, msg) })
		return true
	case wire.CmdRemoveCustomProvider:
		c.Dispatch("handleRemoveCustomProvider", msg.ID, func() { handleRemoveCustomProvider(a, c, msg) })
		return true
	case wire.CmdUpdateCustomProvider:
		c.Dispatch("handleUpdateCustomProvider", msg.ID, func() { handleUpdateCustomProvider(a, c, msg) })
		return true
	case wire.CmdSetProviderPeakHours:
		c.Dispatch("handleSetProviderPeakHours", msg.ID, func() { handleSetProviderPeakHours(a, c, msg) })
		return true
	case wire.CmdPauseProviderPeakHours:
		c.Dispatch("handlePauseProviderPeakHours", msg.ID, func() { handlePauseProviderPeakHours(a, c, msg) })
		return true
	case wire.CmdResumeProviderPeakHours:
		c.Dispatch("handleResumeProviderPeakHours", msg.ID, func() { handleResumeProviderPeakHours(a, c, msg) })
		return true
	case wire.CmdGetPeakHoursStatus:
		c.Dispatch("handleGetPeakHoursStatus", msg.ID, func() { handleGetPeakHoursStatus(a, c, msg) })
		return true
	default:
		return false
	}
}
