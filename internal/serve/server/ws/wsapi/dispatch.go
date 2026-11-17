package wsapi

import (
	"context"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	hub "github.com/PHPCraftdream/rush/internal/serve/server/hub"
	wire "github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

func Dispatch(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage) bool {
	switch msg.Type {
	case wire.CmdSendMessage:
		c.Dispatch("handleSendMessage", msg.ID, func() { handleSendMessage(ctx, a, c, msg) })
		return true
	case wire.CmdInterruptAndSend:
		c.DispatchControl("handleInterruptAndSend", msg.ID, func() { handleInterruptAndSend(ctx, a, c, msg) })
		return true
	case wire.CmdInjectMessage:
		c.Dispatch("handleInjectMessage", msg.ID, func() { handleInjectMessage(ctx, a, c, msg) })
		return true
	case wire.CmdCancelAgent:
		c.DispatchControl("handleCancelAgent", msg.ID, func() { handleCancelAgent(ctx, a, c, msg) })
		return true
	case wire.CmdCreateSession:
		c.Dispatch("handleCreateSession", msg.ID, func() { handleCreateSession(ctx, a, c, msg) })
		return true
	case wire.CmdForkSession:
		c.Dispatch("handleForkSession", msg.ID, func() { handleForkSession(ctx, a, c, msg) })
		return true
	case wire.CmdDeleteSession:
		c.Dispatch("handleDeleteSession", msg.ID, func() { handleDeleteSession(ctx, a, c, msg) })
		return true
	case wire.CmdDeleteOtherSessions:
		c.Dispatch("handleDeleteOtherSessions", msg.ID, func() { handleDeleteOtherSessions(ctx, a, c, msg) })
		return true
	case wire.CmdListSessions:
		c.Dispatch("handleListSessions", msg.ID, func() { handleListSessions(ctx, a, c, msg) })
		return true
	case wire.CmdLoadMessages:
		c.Dispatch("handleLoadMessages", msg.ID, func() { handleLoadMessages(ctx, a, c, msg) })
		return true
	case wire.CmdRenameSession:
		c.Dispatch("handleRenameSession", msg.ID, func() { handleRenameSession(ctx, a, c, msg) })
		return true
	case wire.CmdDeleteMessage:
		c.Dispatch("handleDeleteMessage", msg.ID, func() { handleDeleteMessage(ctx, a, c, msg) })
		return true
	case wire.CmdDeleteMessages:
		c.Dispatch("handleDeleteMessages", msg.ID, func() { handleDeleteMessages(ctx, a, c, msg) })
		return true
	case wire.CmdUpdateMessageContent:
		c.Dispatch("handleUpdateMessageContent", msg.ID, func() { handleUpdateMessageContent(ctx, a, c, msg) })
		return true
	case wire.CmdUpdateMessageThinking:
		c.Dispatch("handleUpdateMessageThinking", msg.ID, func() { handleUpdateMessageThinking(ctx, a, c, msg) })
		return true
	case wire.CmdGetSystemPrompt:
		c.Dispatch("handleGetSystemPrompt", msg.ID, func() { handleGetSystemPrompt(ctx, a, c, msg) })
		return true
	case wire.CmdSetSystemPrompt:
		c.Dispatch("handleSetSystemPrompt", msg.ID, func() { handleSetSystemPrompt(ctx, a, c, msg) })
		return true
	case wire.CmdSummarizeSession:
		c.Dispatch("handleSummarizeSession", msg.ID, func() { handleSummarizeSession(ctx, a, c, msg) })
		return true
	case wire.CmdCancelQueuedSummarize:
		c.Dispatch("handleCancelQueuedSummarize", msg.ID, func() { handleCancelQueuedSummarize(a, c, msg) })
		return true
	case wire.CmdDeleteMessagePart:
		c.Dispatch("handleDeleteMessagePart", msg.ID, func() { handleDeleteMessagePart(ctx, a, c, msg) })
		return true
	case wire.CmdUpdateMessagePart:
		c.Dispatch("handleUpdateMessagePart", msg.ID, func() { handleUpdateMessagePart(ctx, a, c, msg) })
		return true
	case wire.CmdTogglePinMessage:
		c.Dispatch("handleTogglePinMessage", msg.ID, func() { handleTogglePinMessage(ctx, a, c, msg) })
		return true
	case wire.CmdRerunMessage:
		c.Dispatch("handleRerunMessage", msg.ID, func() { handleRerunMessage(ctx, a, c, msg) })
		return true
	case wire.CmdInitializeProject:
		c.Dispatch("handleInitializeProject", msg.ID, func() { handleInitializeProject(ctx, a, c, msg) })
		return true
	case wire.CmdUpdateTodos:
		c.Dispatch("handleUpdateTodos", msg.ID, func() { handleUpdateTodos(ctx, a, c, msg) })
		return true
	default:
		return false
	}
}
