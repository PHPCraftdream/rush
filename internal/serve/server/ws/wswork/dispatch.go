package wswork

import (
	"context"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	wire "github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

// Dispatch carries the WS work-family commands (design docs/plans/2026-11-15-sprint0-split-server.md §3 S5). The variadic seams parameter carries the root package's batch seam values so the staying root dispatch tests keep overriding them through the root skeleton; production callers pass the current root values and empty Seams behaves as this package's own defaults.
func Dispatch(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, seams Seams) bool {
	switch msg.Type {
	case wire.CmdGetSessionLiveWork:
		c.Dispatch("handleGetSessionLiveWork", msg.ID, func() { handleGetSessionLiveWork(ctx, a, c, msg, seams) })
		return true
	case wire.CmdGetSessionBatchWork:
		c.Dispatch("handleGetSessionBatchWork", msg.ID, func() { handleGetSessionBatchWork(ctx, a, c, msg, seams) })
		return true
	case wire.CmdBatchStatus, wire.CmdBatchOutput:
		c.Dispatch("handleBatchRead", msg.ID, func() { handleBatchControl(ctx, a, c, msg, seams) })
		return true
	case wire.CmdBatchStop, wire.CmdBatchAdd, wire.CmdBatchDrain:
		c.DispatchControl("handleBatchControl", msg.ID, func() { handleBatchControl(ctx, a, c, msg, seams) })
		return true
	case wire.CmdGetSessionWakeSchedules:
		c.Dispatch("handleGetSessionWakeSchedules", msg.ID, func() { handleGetSessionWakeSchedules(ctx, a, c, msg) })
		return true
	case wire.CmdCancelWakeSchedule:
		c.Dispatch("handleCancelWakeSchedule", msg.ID, func() { handleCancelWakeSchedule(ctx, a, c, msg) })
		return true
	default:
		return false
	}
}
