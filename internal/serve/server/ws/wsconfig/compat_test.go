package wsconfig

import (
	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/gorilla/websocket"
)

type (
	AddContextPathPayload          = wire.AddContextPathPayload
	AddCustomProviderPayload       = wire.AddCustomProviderPayload
	AddSkillsPathPayload           = wire.AddSkillsPathPayload
	ClearScopedModelPayload        = wire.ClearScopedModelPayload
	ConfigWire                     = wire.ConfigWire
	CreateSessionPayload           = wire.CreateSessionPayload
	CustomModelPayload             = wire.CustomModelPayload
	ModelEntryWire                 = wire.ModelEntryWire
	ModelInfoWire                  = wire.ModelInfoWire
	ModelOverrideWire              = wire.ModelOverrideWire
	PeakHoursWirePayload           = wire.PeakHoursWirePayload
	ProviderWire                   = wire.ProviderWire
	RemoveContextPathPayload       = wire.RemoveContextPathPayload
	RemoveCustomProviderPayload    = wire.RemoveCustomProviderPayload
	RemoveProviderKeyPayload       = wire.RemoveProviderKeyPayload
	RemoveSkillsPathPayload        = wire.RemoveSkillsPathPayload
	ResumeProviderPeakHoursPayload = wire.ResumeProviderPeakHoursPayload
	SetCustomRolePayload           = wire.SetCustomRolePayload
	SetDebugPayload                = wire.SetDebugPayload
	SetKeepAlivePayload            = wire.SetKeepAlivePayload
	SetMCPDisabledPayload          = wire.SetMCPDisabledPayload
	SetProviderKeyPayload          = wire.SetProviderKeyPayload
	SetProviderPeakHoursPayload    = wire.SetProviderPeakHoursPayload
	SetScopedModelPayload          = wire.SetScopedModelPayload
	SetSessionModelsPayload        = wire.SetSessionModelsPayload
	SetSessionModelsResult         = wire.SetSessionModelsResult
	SetThemePayload                = wire.SetThemePayload
	TrackModelUsagePayload         = wire.TrackModelUsagePayload
	UpdateCustomProviderPayload    = wire.UpdateCustomProviderPayload
	WSMessage                      = wire.WSMessage
	Client                         = hub.Client
	Hub                            = hub.Hub
	CommandPolicy                  = hub.CommandPolicy
	Principal                      = apiauth.Principal
)

const (
	EventConfig             = wire.EventConfig
	EventCustomRoles        = wire.EventCustomRoles
	EventError              = wire.EventError
	EventResponse           = wire.EventResponse
	EventScopedModels       = wire.EventScopedModels
	EventSessionUpdated     = wire.EventSessionUpdated
	CmdAddCustomProvider    = wire.CmdAddCustomProvider
	CmdClearScopedModel     = wire.CmdClearScopedModel
	CmdCreateSession        = wire.CmdCreateSession
	CmdGetCustomRoles       = wire.CmdGetCustomRoles
	CmdRemoveCustomRole     = wire.CmdRemoveCustomRole
	CmdSetCustomRole        = wire.CmdSetCustomRole
	CmdSetScopedModel       = wire.CmdSetScopedModel
	CmdSetSessionModels     = wire.CmdSetSessionModels
	CmdTrackModelUsage      = wire.CmdTrackModelUsage
	CmdUpdateCustomProvider = wire.CmdUpdateCustomProvider
	ReasoningEffortClear    = wire.ReasoningEffortClear
	cookieName              = "rush_session"
)

var (
	ErrScopeForbidden = hub.ErrScopeForbidden
	commandScopes     = hub.CommandScopesSnapshot()
)

func newHub() *hub.Hub                                       { return hub.NewHub() }
func newClient(h *hub.Hub, conn *websocket.Conn) *hub.Client { return hub.NewClient(h, conn) }
func newClientAuth(p Principal, policy CommandPolicy) *hub.ClientAuth {
	return hub.NewClientAuth(p, policy)
}

func AuthorizeCommand(p Principal, command string, policy CommandPolicy) error {
	return hub.AuthorizeCommand(p, command, policy)
}

func SessionResolutionFor(command string) (field, class string, ok bool) {
	return apiauth.SessionResolutionFor(command)
}

// newTestClient mirrors the package-server detached handler fixture.
func newTestClient() *hub.Client {
	c := newClient(newHub(), nil)
	c.ResizeSendChan(8)
	return c
}
