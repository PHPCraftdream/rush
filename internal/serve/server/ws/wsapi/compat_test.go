package wsapi

import (
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/gorilla/websocket"
)

type (
	AgentBusyPayload           = wire.AgentBusyPayload
	ModelOverrideWire          = wire.ModelOverrideWire
	SendMessagePayload         = wire.SendMessagePayload
	DeleteMessagePartPayload   = wire.DeleteMessagePartPayload
	DeleteMessagePayload       = wire.DeleteMessagePayload
	DeleteMessagesPayload      = wire.DeleteMessagesPayload
	DeleteOtherSessionsPayload = wire.DeleteOtherSessionsPayload
	DeleteOtherSessionsResult  = wire.DeleteOtherSessionsResult
	RenameSessionPayload       = wire.RenameSessionPayload
	RerunMessagePayload        = wire.RerunMessagePayload
	SetSessionModelsPayload    = wire.SetSessionModelsPayload
	UpdateMessagePartPayload   = wire.UpdateMessagePartPayload
	WSMessage                  = wire.WSMessage
	Client                     = hub.Client
)

const (
	EventAgentBusy         = wire.EventAgentBusy
	EventError             = wire.EventError
	EventResponse          = wire.EventResponse
	EventSessionUpdated    = wire.EventSessionUpdated
	EventSessionsList      = wire.EventSessionsList
	CmdDeleteMessage       = wire.CmdDeleteMessage
	CmdDeleteMessagePart   = wire.CmdDeleteMessagePart
	CmdDeleteMessages      = wire.CmdDeleteMessages
	CmdDeleteOtherSessions = wire.CmdDeleteOtherSessions
	CmdListSessions        = wire.CmdListSessions
	CmdRenameSession       = wire.CmdRenameSession
	CmdRerunMessage        = wire.CmdRerunMessage
	CmdSendMessage         = wire.CmdSendMessage
	CmdSetSessionModels    = wire.CmdSetSessionModels
	CmdUpdateMessagePart   = wire.CmdUpdateMessagePart
)

// Package-local test constructors matching the former server hub fixtures.
func newHub() *hub.Hub                                       { return hub.NewHub() }
func newClient(h *hub.Hub, conn *websocket.Conn) *hub.Client { return hub.NewClient(h, conn) }
func newTitleCache() *hub.TitleCache                         { return hub.NewTitleCache() }
func newDetachedTestClient(sendBuf int) *hub.Client {
	return hub.NewDetachedClient(hub.NewHub(), sendBuf)
}

// newTestClient builds a Client detached from any real connection. Both
// c.reply and hub.Broadcast are non-blocking (select/default), so no hub
// event loop needs to be running for a handler to complete.
func newTestClient() *Client {
	c := newClient(newHub(), nil)
	c.ResizeSendChan(8)
	return c
}
