package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsapi"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsconfig"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wswork"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	core "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/gorilla/websocket"
)

// TEMPORARY BRIDGE (design docs/plans/2026-11-15-sprint0-split-server.md, seam S1).
//
// Type aliases and constant re-exports of the wire leaf package
// (internal/serve/server/wire) so the ~150 files of this package keep using
// the unqualified protocol names. The bridge is deleted by the lanes that
// migrate the consumers to the wire. L4 (hub), L5 (ws families:
// wsapi/wswork/wsconfig), L6 (rest: api/apibatch) and finally L7 (server
// composer), the last consumer of a name removes it from this file.

type (
	WSMessage                      = wire.WSMessage
	ModelOverrideWire              = wire.ModelOverrideWire
	SetSessionModelsPayload        = wire.SetSessionModelsPayload
	SetSessionModelsResult         = wire.SetSessionModelsResult
	ScopedModelSlotWire            = wire.ScopedModelSlotWire
	ScopedModelsWire               = wire.ScopedModelsWire
	SetScopedModelPayload          = wire.SetScopedModelPayload
	ClearScopedModelPayload        = wire.ClearScopedModelPayload
	SendMessagePayload             = wire.SendMessagePayload
	CancelAgentPayload             = wire.CancelAgentPayload
	CreateSessionPayload           = wire.CreateSessionPayload
	ForkSessionPayload             = wire.ForkSessionPayload
	DeleteSessionPayload           = wire.DeleteSessionPayload
	DeleteOtherSessionsPayload     = wire.DeleteOtherSessionsPayload
	DeleteOtherSessionsResult      = wire.DeleteOtherSessionsResult
	LoadMessagesPayload            = wire.LoadMessagesPayload
	SetThemePayload                = wire.SetThemePayload
	SetKeepAlivePayload            = wire.SetKeepAlivePayload
	RenameSessionPayload           = wire.RenameSessionPayload
	RerunMessagePayload            = wire.RerunMessagePayload
	AgentBusyPayload               = wire.AgentBusyPayload
	SetDebugPayload                = wire.SetDebugPayload
	AddContextPathPayload          = wire.AddContextPathPayload
	RemoveContextPathPayload       = wire.RemoveContextPathPayload
	AddSkillsPathPayload           = wire.AddSkillsPathPayload
	RemoveSkillsPathPayload        = wire.RemoveSkillsPathPayload
	SkillInfo                      = wire.SkillInfo
	SkillsSnapshot                 = wire.SkillsSnapshot
	CustomModelPayload             = wire.CustomModelPayload
	PeakHoursWirePayload           = wire.PeakHoursWirePayload
	AddCustomProviderPayload       = wire.AddCustomProviderPayload
	RemoveCustomProviderPayload    = wire.RemoveCustomProviderPayload
	UpdateCustomProviderPayload    = wire.UpdateCustomProviderPayload
	SetProviderPeakHoursPayload    = wire.SetProviderPeakHoursPayload
	MCPServerInfo                  = wire.MCPServerInfo
	MCPSnapshot                    = wire.MCPSnapshot
	LSPServerInfo                  = wire.LSPServerInfo
	LSPSnapshot                    = wire.LSPSnapshot
	SetLSPDisabledPayload          = wire.SetLSPDisabledPayload
	AddLSPServerPayload            = wire.AddLSPServerPayload
	RemoveLSPServerPayload         = wire.RemoveLSPServerPayload
	UpdateLSPServerPayload         = wire.UpdateLSPServerPayload
	RemoveRecentModelPayload       = wire.RemoveRecentModelPayload
	TrackModelUsagePayload         = wire.TrackModelUsagePayload
	SetProviderKeyPayload          = wire.SetProviderKeyPayload
	RemoveProviderKeyPayload       = wire.RemoveProviderKeyPayload
	DeleteMessagePayload           = wire.DeleteMessagePayload
	DeleteMessagesPayload          = wire.DeleteMessagesPayload
	UpdateMessageContentPayload    = wire.UpdateMessageContentPayload
	UpdateMessageThinkingPayload   = wire.UpdateMessageThinkingPayload
	ConfigWire                     = wire.ConfigWire
	ModelEntryWire                 = wire.ModelEntryWire
	ProviderWire                   = wire.ProviderWire
	ModelInfoWire                  = wire.ModelInfoWire
	GetSystemPromptPayload         = wire.GetSystemPromptPayload
	GetLogsPayload                 = wire.GetLogsPayload
	SetSystemPromptPayload         = wire.SetSystemPromptPayload
	SummarizeSessionPayload        = wire.SummarizeSessionPayload
	SummarizeQueuedPayload         = wire.SummarizeQueuedPayload
	CancelQueuedSummarizePayload   = wire.CancelQueuedSummarizePayload
	LogClientEventPayload          = wire.LogClientEventPayload
	LogClientErrorPayload          = wire.LogClientErrorPayload
	DeleteMessagePartPayload       = wire.DeleteMessagePartPayload
	UpdateMessagePartPayload       = wire.UpdateMessagePartPayload
	TogglePinMessagePayload        = wire.TogglePinMessagePayload
	SetMCPDisabledPayload          = wire.SetMCPDisabledPayload
	AddMCPServerPayload            = wire.AddMCPServerPayload
	RemoveMCPServerPayload         = wire.RemoveMCPServerPayload
	UpdateMCPServerPayload         = wire.UpdateMCPServerPayload
	UpdateTodosPayload             = wire.UpdateTodosPayload
	TodoWire                       = wire.TodoWire
	UpdateAvailableWire            = wire.UpdateAvailableWire
	CustomRoleWire                 = wire.CustomRoleWire
	CustomRolesWire                = wire.CustomRolesWire
	SetCustomRolePayload           = wire.SetCustomRolePayload
	RemoveCustomRolePayload        = wire.RemoveCustomRolePayload
	PeakHoursWindowWire            = wire.PeakHoursWindowWire
	PauseProviderPeakHoursPayload  = wire.PauseProviderPeakHoursPayload
	ResumeProviderPeakHoursPayload = wire.ResumeProviderPeakHoursPayload
	GetPeakHoursStatusPayload      = wire.GetPeakHoursStatusPayload
	PeakHoursStatusWire            = wire.PeakHoursStatusWire
	PeakHoursStatusesWire          = wire.PeakHoursStatusesWire
	GetSessionLiveWorkPayload      = wire.GetSessionLiveWorkPayload
	LiveWorkItemWire               = wire.LiveWorkItemWire
	SessionLiveWorkPayload         = wire.SessionLiveWorkPayload
	GetSessionBatchWorkPayload     = wire.GetSessionBatchWorkPayload
	GetSessionWakeSchedulesPayload = wire.GetSessionWakeSchedulesPayload
	CancelWakeSchedulePayload      = wire.CancelWakeSchedulePayload
	WakeScheduleWire               = wire.WakeScheduleWire
	SessionWakeSchedulesPayload    = wire.SessionWakeSchedulesPayload
	APIEvent                       = wire.APIEvent
	APIRunState                    = wire.APIRunState
	HelloPayload                   = wire.HelloPayload
	PartWire                       = wire.PartWire
	UsageWire                      = wire.UsageWire
	MessageWire                    = wire.MessageWire
	APISession                     = wire.APISession
	APIMessage                     = wire.APIMessage
	APIMessagePart                 = wire.APIMessagePart
	APIModel                       = wire.APIModel
)

const (
	EventHello                 = wire.EventHello
	EventMessageCreated        = wire.EventMessageCreated
	EventMessageUpdated        = wire.EventMessageUpdated
	EventMessageDeleted        = wire.EventMessageDeleted
	EventSessionCreated        = wire.EventSessionCreated
	EventSessionUpdated        = wire.EventSessionUpdated
	EventSessionDeleted        = wire.EventSessionDeleted
	EventMCPState              = wire.EventMCPState
	EventLSPState              = wire.EventLSPState
	EventAgentBusy             = wire.EventAgentBusy
	EventSessionsList          = wire.EventSessionsList
	EventMessagesList          = wire.EventMessagesList
	EventConfig                = wire.EventConfig
	EventLogs                  = wire.EventLogs
	EventUpdateAvailable       = wire.EventUpdateAvailable
	EventResponse              = wire.EventResponse
	EventError                 = wire.EventError
	EventSystemPrompt          = wire.EventSystemPrompt
	EventSkills                = wire.EventSkills
	EventSummarizeQueued       = wire.EventSummarizeQueued
	EventScopedModels          = wire.EventScopedModels
	EventSessionLiveWork       = wire.EventSessionLiveWork
	EventSessionWakeSchedules  = wire.EventSessionWakeSchedules
	EventSessionBatchWork      = wire.EventSessionBatchWork
	EventBatchControlResult    = wire.EventBatchControlResult
	CmdSendMessage             = wire.CmdSendMessage
	CmdInterruptAndSend        = wire.CmdInterruptAndSend
	CmdInjectMessage           = wire.CmdInjectMessage
	CmdCancelAgent             = wire.CmdCancelAgent
	CmdCreateSession           = wire.CmdCreateSession
	CmdForkSession             = wire.CmdForkSession
	CmdDeleteSession           = wire.CmdDeleteSession
	CmdDeleteOtherSessions     = wire.CmdDeleteOtherSessions
	CmdListSessions            = wire.CmdListSessions
	CmdLoadMessages            = wire.CmdLoadMessages
	CmdGetConfig               = wire.CmdGetConfig
	CmdGetLogs                 = wire.CmdGetLogs
	CmdSetTheme                = wire.CmdSetTheme
	CmdSetKeepAlive            = wire.CmdSetKeepAlive
	CmdRenameSession           = wire.CmdRenameSession
	CmdSetSessionModels        = wire.CmdSetSessionModels
	CmdRemoveRecentModel       = wire.CmdRemoveRecentModel
	CmdTrackModelUsage         = wire.CmdTrackModelUsage
	CmdDeleteMessage           = wire.CmdDeleteMessage
	CmdDeleteMessages          = wire.CmdDeleteMessages
	CmdUpdateMessageContent    = wire.CmdUpdateMessageContent
	CmdUpdateMessageThinking   = wire.CmdUpdateMessageThinking
	CmdGetSystemPrompt         = wire.CmdGetSystemPrompt
	CmdSetSystemPrompt         = wire.CmdSetSystemPrompt
	CmdSummarizeSession        = wire.CmdSummarizeSession
	CmdCancelQueuedSummarize   = wire.CmdCancelQueuedSummarize
	CmdDeleteMessagePart       = wire.CmdDeleteMessagePart
	CmdUpdateMessagePart       = wire.CmdUpdateMessagePart
	CmdTogglePinMessage        = wire.CmdTogglePinMessage
	CmdRerunMessage            = wire.CmdRerunMessage
	CmdGetScopedModels         = wire.CmdGetScopedModels
	CmdSetScopedModel          = wire.CmdSetScopedModel
	CmdClearScopedModel        = wire.CmdClearScopedModel
	CmdGetCustomRoles          = wire.CmdGetCustomRoles
	CmdSetCustomRole           = wire.CmdSetCustomRole
	CmdRemoveCustomRole        = wire.CmdRemoveCustomRole
	CmdGetSessionLiveWork      = wire.CmdGetSessionLiveWork
	CmdGetSessionWakeSchedules = wire.CmdGetSessionWakeSchedules
	CmdGetSessionBatchWork     = wire.CmdGetSessionBatchWork
	CmdBatchStatus             = wire.CmdBatchStatus
	CmdBatchOutput             = wire.CmdBatchOutput
	CmdBatchStop               = wire.CmdBatchStop
	CmdBatchAdd                = wire.CmdBatchAdd
	CmdBatchDrain              = wire.CmdBatchDrain
	CmdCancelWakeSchedule      = wire.CmdCancelWakeSchedule
	CmdShutdownServer          = wire.CmdShutdownServer
	ReasoningEffortClear       = wire.ReasoningEffortClear
	CmdSetDebug                = wire.CmdSetDebug
	CmdAddContextPath          = wire.CmdAddContextPath
	CmdRemoveContextPath       = wire.CmdRemoveContextPath
	CmdGetSkills               = wire.CmdGetSkills
	CmdAddSkillsPath           = wire.CmdAddSkillsPath
	CmdRemoveSkillsPath        = wire.CmdRemoveSkillsPath
	CmdInitializeProject       = wire.CmdInitializeProject
	CmdAddCustomProvider       = wire.CmdAddCustomProvider
	CmdRemoveCustomProvider    = wire.CmdRemoveCustomProvider
	CmdUpdateCustomProvider    = wire.CmdUpdateCustomProvider
	CmdSetProviderPeakHours    = wire.CmdSetProviderPeakHours
	CmdPauseProviderPeakHours  = wire.CmdPauseProviderPeakHours
	CmdResumeProviderPeakHours = wire.CmdResumeProviderPeakHours
	CmdGetPeakHoursStatus      = wire.CmdGetPeakHoursStatus
	CmdUpdateTodos             = wire.CmdUpdateTodos
	CmdSetProviderKey          = wire.CmdSetProviderKey
	CmdRemoveProviderKey       = wire.CmdRemoveProviderKey
	CmdLogClientEvent          = wire.CmdLogClientEvent
	CmdLogClientError          = wire.CmdLogClientError
	CmdSetMCPDisabled          = wire.CmdSetMCPDisabled
	CmdAddMCPServer            = wire.CmdAddMCPServer
	CmdRemoveMCPServer         = wire.CmdRemoveMCPServer
	CmdUpdateMCPServer         = wire.CmdUpdateMCPServer
	CmdSetLSPDisabled          = wire.CmdSetLSPDisabled
	CmdAddLSPServer            = wire.CmdAddLSPServer
	CmdRemoveLSPServer         = wire.CmdRemoveLSPServer
	CmdUpdateLSPServer         = wire.CmdUpdateLSPServer
	EventCustomRoles           = wire.EventCustomRoles
	APIRunQueued               = wire.APIRunQueued
	APIRunRunning              = wire.APIRunRunning
	APIRunSucceeded            = wire.APIRunSucceeded
	APIRunFailed               = wire.APIRunFailed
	APIRunCancelled            = wire.APIRunCancelled
	APIRunAwaitingAnswer       = wire.APIRunAwaitingAnswer
)

// hub_bridge.go: temporary compatibility surface for package server after the
// S4 seam moved the WS hub to internal/serve/server/hub (sprint 0 split,
// design docs/plans/2026-11-15-sprint0-split-server.md §2.2/§3 S4). Deleter
// lanes: L5 (ws families migrate to hub.Client methods / hub.Hub accessors)
// and L7 (composer + remaining fixture tests). Type aliases and one-line
// delegating wrappers only — no mutable var re-exports.
//
// Trimmed in L4: none. The S4 candidates were ResolvePinnedCommand and
// ResolveBatchOnlyCommand (their production consumer, the handlers.go
// authorization block, moved into hub.Client.AuthorizeIncoming), but the
// staying white tests api_session_resolution_test.go:51 and
// api_batch_only_test.go:151-158 still use them, so the apiauth_bridge
// entries remain (L6/L7 delete them with their tests). StillValid/RealTicker
// keep their production consumer api_events_revoke.go:11,18. wire_bridge.go
// and listener_bridge.go: all entries remain (the moved hub files now
// qualify wire/apiauth directly, but every moved name still has
// package-server consumers).

type (
	// Core S4 surface (design §2.2).
	Hub    = hub.Hub
	Client = hub.Client
	// titleCache is the §2.2 TitleCache under its HEAD name: the live-work
	// files that stay in package server until L5 (handlers_livework.go:65,
	// handlers_livework_batch.go:72) keep their unqualified parameter types.
	titleCache = hub.TitleCache
)

var (
	testHookMu      sync.RWMutex
	testBeforeWrite func([]byte)
)

func beforeClientWrite(msg []byte) {
	testHookMu.RLock()
	hook := testBeforeWrite
	testHookMu.RUnlock()
	if hook != nil {
		hook(msg)
	}
}

// NewHub wraps hub.NewHub for the ~30 package-server tests/fixtures that call
// the unqualified constructor until L5/L7.
func NewHub() *Hub { return hub.NewHub() }

// newClient mirrors the HEAD unqualified constructor used by server-side test
// fixtures (readpump_control_starvation_test.go dialTestWS,
// handlers_shutdown_test.go, ~30 more) until L5/L7.
func newClient(h *Hub, conn *websocket.Conn) *Client { return hub.NewClient(h, conn) }

// newTitleCache mirrors the HEAD unqualified constructor (handlers_livework_*
// tests) until L5.
func newTitleCache() *titleCache { return hub.NewTitleCache() }

// newClientAuth mirrors the HEAD unqualified constructor; two staying tests
// assign auth through Client.SetAuth (handlers_config_lock_trusted_test.go:575,
// handlers_custom_roles_test.go:95) until L5.
func newClientAuth(principal Principal, policy hub.CommandPolicy) *hub.ClientAuth {
	return hub.NewClientAuth(principal, policy)
}

// filterSessionsForPinnedClient re-exports the §2.2 rename for its staying
// consumer handlers_sessions.go:332 until L5.
func filterSessionsForPinnedClient(sessions []core.Session, c *Client) []core.Session {
	return hub.FilterSessionsForPinnedClient(sessions, c)
}

// pinnedSessionsOnly re-exports the exported method for its staying root
// consumer api_read_handlers.go:123 until L6.
func pinnedSessionsOnly(sessions []core.Session, pin string) []core.Session {
	return hub.PinnedSessionsOnly(sessions, pin)
}

// ServeWSGate builds the S4 WS gateway for the server-side test fixture that
// mounts the same chain the composer builds
// (readpump_control_starvation_test.go dialTestWS until L7). Not a delegating
// method on Server: the composer (server.go) qualifies hub.NewGate directly.
func ServeWSGate(s *Server, ctx context.Context) http.Handler {
	return hub.NewGate(hub.GateOptions{
		Hub:         s.hub,
		Ctx:         ctx,
		CheckOrigin: s.checkOrigin,
		UIAuth:      s.uiAuth,
		AllowAdmin:  s.policy.AllowAdmin,
		OnIncoming:  func(ctx context.Context, c *Client, raw []byte) { handleIncoming(ctx, s, c, raw) },
		BeforeWrite: beforeClientWrite,
		Workers:     &s.wsWorkers,
	})
}

// Re-exported production sizes for root call sites; see
// hub.WorkerPoolSize/MaxMessageSize.
const (
	workerPoolSize = hub.WorkerPoolSize
	maxMessageSize = hub.MaxMessageSize
)

// Re-exported ring capacity for root call sites; see hub.MaxBufferSize.
const maxBufferSize = hub.MaxBufferSize

// commandScopes re-exports the moved scope table for its staying white-test
// consumers (api_session_resolution_test.go) until L6/L7; snapshot copy — no
// mutable var crosses the boundary.
var commandScopes = hub.CommandScopesSnapshot()

// newHub is the HEAD unqualified constructor used by ~30 staying tests
// (descendant_work_*, p-family, handlers-family fixtures) until L5/L7.
func newHub() *Hub { return hub.NewHub() }

// newDetachedTestClient builds a Client detached from any real connection
// with an n-slot send channel for root tests that wrote
// &Client{send: make(chan []byte, n)} struct literals before the hub split
// (events_config_test.go:74, p569_*, p595_* at HEAD). Provenance:
// p460_model_scope_test.go newTestClient pattern. L5/L7 remove the need.
func newDetachedTestClient(sendBuf int) *Client {
	c := hub.NewDetachedClient(hub.NewHub(), sendBuf)
	return c
}

type (
	// CommandPolicy and ClientAuth re-export the moved scope-table types for
	// their staying test consumers (handlers_config_lock_trusted_test.go:575,
	// handlers_custom_roles_test.go:95) until L5.
	CommandPolicy = hub.CommandPolicy
	ClientAuth    = hub.ClientAuth
)

// Scope-table function re-exports (design §2.2 renames) for their staying
// test consumers (handlers_custom_roles_test.go:110-114,
// peak_hours_wire_v2_test.go:241-251, ws_serve_scope_matrix_test.go) until
// L5/L7.

func AuthorizeCommand(p Principal, cmd string, policy CommandPolicy) error {
	return hub.AuthorizeCommand(p, cmd, policy)
}

func ScopeFor(cmd string) (string, bool) { return hub.ScopeFor(cmd) }

var (
	ErrScopeForbidden  = hub.ErrScopeForbidden
	ErrAdminDisallowed = hub.ErrAdminDisallowed
	ErrCommandUnmapped = hub.ErrCommandUnmapped
)

// startServeRevocationWatch re-exports the moved WS revocation watcher
// (ws_serve_revoke.go, design §2.4 row 81) for its staying white test
// ws_serve_revoke_test.go until L7.
func startServeRevocationWatch(ctx context.Context, conn *websocket.Conn, principal Principal) {
	hub.StartServeRevocationWatch(ctx, conn, principal)
}

// ws_bridge.go: temporary compatibility seam for WS-family packages moved
// under server/ws. Preserve wsworkSeams while root callers migrate through
// narrow wsconfig/wsapi exports. L7 removes the remaining root wrappers and
// this bridge once the composer and staying tests no longer need them.

// buildConfigWire retains the package-server snapshot entry point used by
// production callers; ConfigWire is the same wire-package alias as before.
func buildConfigWire(a *appPkg.App) (ConfigWire, bool) {
	return wsconfig.BuildConfigWire(a)
}

func holdExternalSilenceProof(a *appPkg.App, sessionID string) (*sessionlock.SharedLockProbe, bool, string) {
	return wsapi.HoldExternalSilenceProof(a, sessionID)
}

func appOwnsSession(a *appPkg.App, sess core.Session) bool {
	return wsapi.AppOwnsSession(a, sess)
}

// Thin delegates retained for the staying root-package tests that call the
// pre-move private handler names directly (rule-18 narrow compiler-proven
// cross-family seams; L7 removes these with the bridge).
func handleListSessions(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wsapi.HandleListSessions(ctx, a, c, msg)
}

func handleCreateSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wsapi.HandleCreateSession(ctx, a, c, msg)
}

func handleDeleteSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wsapi.HandleDeleteSession(ctx, a, c, msg)
}

func handleSendMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wsapi.HandleSendMessage(ctx, a, c, msg)
}

func saveAttachmentToDisk(dataDir, fileName string, data []byte) (string, error) {
	return wsapi.SaveAttachmentToDisk(dataDir, fileName, data)
}

func attachmentsDataDir(a *appPkg.App) string {
	return wsapi.AttachmentsDataDir(a)
}

// The peak-hours handlers still in package server use the same legacy scope
// parsing rules as the migrated provider handlers.
func scopeFromWire(s string) config.Scope {
	return wsconfig.ScopeFromWire(s)
}

func peakHoursFromWire(p *PeakHoursWirePayload) (*config.ProviderPeakHours, error) {
	return wsconfig.PeakHoursFromWire(p)
}

func peakHoursToWire(p *config.ProviderPeakHours) *PeakHoursWirePayload {
	return wsconfig.PeakHoursToWire(p)
}

func handleSetSessionModels(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleSetSessionModels(ctx, a, c, msg)
}

func handleSetScopedModel(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleSetScopedModel(a, c, msg)
}

func handleClearScopedModel(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleClearScopedModel(a, c, msg)
}

func handleSetTheme(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleSetTheme(a, c, msg)
}

func handleSetKeepAlive(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleSetKeepAlive(a, c, msg)
}

func handleSetDebug(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleSetDebug(a, c, msg)
}

func handleAddContextPath(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleAddContextPath(a, c, msg)
}

func handleRemoveContextPath(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleRemoveContextPath(a, c, msg)
}

func handleAddSkillsPath(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleAddSkillsPath(a, c, msg)
}

func handleRemoveSkillsPath(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleRemoveSkillsPath(a, c, msg)
}

func handleSetProviderKey(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleSetProviderKey(a, c, msg)
}

func handleRemoveProviderKey(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleRemoveProviderKey(a, c, msg)
}

func handleAddCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleAddCustomProvider(a, c, msg)
}

func handleUpdateCustomProvider(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleUpdateCustomProvider(a, c, msg)
}

func handleSetProviderPeakHours(a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.HandleSetProviderPeakHours(a, c, msg)
}

// handleSetMCPDisabled is the live wsconfig handler; the root-package copy
// (deleted with handlers_mcp.go) was a dead duplicate. Caller:
// handlers_config_lock_trusted_test.go:536.
func handleSetMCPDisabled(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wsconfig.SetMCPDisabled(ctx, a, c, msg)
}

// wsworkSeams keeps the migrated work handlers connected to the root package's
// mutable test seams. Each provider reads its current root value when invoked.
func wsworkSeams() wswork.Seams {
	return wswork.Seams{
		SourceFor: func() func(*appPkg.App) wswork.BatchNodeSource {
			return func(a *appPkg.App) wswork.BatchNodeSource { return batchSourceFor(a) }
		},
		DataDir: func() func(*appPkg.App) string {
			return func(a *appPkg.App) string { return batchDataDir(a) }
		},
		ProbeHost: func() func(string, string) sessionhosts.HostLockStatus {
			return func(dataDir, host string) sessionhosts.HostLockStatus { return batchProbeHost(dataDir, host) }
		},
		IsOwnHost: func() func(string) bool {
			return func(host string) bool { return batchIsOwnHost(host) }
		},
		PendingQuestions: func() func(context.Context, *appPkg.App, []string) (map[string]sessionnotice.ChildQuestion, bool) {
			return func(ctx context.Context, a *appPkg.App, visited []string) (map[string]sessionnotice.ChildQuestion, bool) {
				return batchPendingQuestionsFor(ctx, a, visited)
			}
		},
		AddForWeb: func() func(context.Context, *appPkg.App, string, string, []jobbatch.Spec) error {
			return func(ctx context.Context, a *appPkg.App, root, node string, items []jobbatch.Spec) error {
				return batchAddForWeb(ctx, a, root, node, items)
			}
		},
	}
}

// ── L5 cutover delegates (design §3 S5) ─────────────────────────────────────
//
// The wswork-family handler files moved to ws/wswork; what remains here is
// only what the STAYING root white tests and events.go actually call: the
// root-owned mutable seam vars (never exported across the boundary), thin
// type aliases over the wswork wire types, and one-line delegates that thread
// wsworkSeams(). L7 deletes these with the bridge.

// batchNodeSource re-exports the wswork read boundary under its HEAD name.
type batchNodeSource = wswork.BatchNodeSource

type (
	// Wire type aliases: the wswork copies keep the HEAD field names, so the
	// staying tests' literals and assertions compile unchanged.
	BatchCompletionWire   = wswork.BatchCompletionWire
	BatchWorkSnapshotWire = wswork.BatchWorkSnapshotWire
	BatchControlResult    = wswork.BatchControlResult
	// batchControlPayload re-exports the control request envelope; only its
	// exported fields are touched from root tests.
	batchControlPayload = wswork.BatchControlPayload
)

// Constants re-exported for the staying white tests (provenance:
// handlers_livework.go / handlers_batch.go / handlers_livework_batch.go).
const (
	titleMaxLen               = wswork.TitleMaxLen
	liveWorkTerminalLimit     = wswork.LiveWorkTerminalLimit
	liveWorkCoalesceDelay     = 250 * time.Millisecond
	batchHistoryDepth         = wswork.BatchHistoryDepth
	batchHistoryTerminalBound = wswork.BatchHistoryTerminalBound
)

// Production seam wiring moved verbatim from handlers_batch.go /
// handlers_batch_control.go / handlers_livework_batch.go. Package vars only
// so the staying root tests can fake them; production never reassigns.
var (
	batchSourceFor = func(a *appPkg.App) batchNodeSource {
		if a == nil || a.DB == nil {
			return nil
		}
		return db.New(a.DB())
	}
	batchDataDir = func(a *appPkg.App) string {
		if a == nil || a.Config() == nil {
			return ""
		}
		return a.Config().Options.DataDirectory
	}
	batchProbeHost = func(dataDir, host string) sessionhosts.HostLockStatus {
		if dataDir == "" || host == "" {
			return session.HostStatusUnknown
		}
		st, err := sessionhosts.ProbeHostShared(dataDir, host)
		if err != nil || (st != session.HostStatusAlive && st != session.HostStatusDead) {
			return session.HostStatusUnknown
		}
		return st
	}
	batchIsOwnHost = sessionhosts.IsOwnHostID
	// batchAddForWeb preserves the public App boundary while allowing focused
	// validation/argument tests; production always delegates to App.BatchAdd.
	batchAddForWeb = func(ctx context.Context, a *appPkg.App, root, node string, items []jobbatch.Spec) error {
		return a.BatchAdd(ctx, root, node, items)
	}
)

// batchCollectRows delegates the delegation-tree row walk.
func batchCollectRows(ctx context.Context, src batchNodeSource, sessionID string) (rowsByOwner map[string][]db.BatchNode, visited []string, complete bool) {
	return wswork.BatchCollectRows(ctx, src, sessionID)
}

func buildBatchWorkSnapshot(ctx context.Context, a *appPkg.App, sessionID string) BatchWorkSnapshotWire {
	return wswork.BuildBatchWorkSnapshot(ctx, a, sessionID, wsworkSeams())
}

func handleGetSessionLiveWork(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wswork.HandleGetSessionLiveWork(ctx, a, c, msg, wsworkSeams())
}

func buildSessionLiveWork(ctx context.Context, a *appPkg.App, cache *titleCache, sessionID string) (SessionLiveWorkPayload, error) {
	return wswork.BuildSessionLiveWork(ctx, a, cache, sessionID, wsworkSeams())
}

func buildSessionLiveWorkFromBatch(ctx context.Context, a *appPkg.App, cache *titleCache, sessionID string) (SessionLiveWorkPayload, error) {
	return wswork.BuildSessionLiveWorkFromBatch(ctx, a, cache, sessionID, wsworkSeams())
}

// truncateTitle bounds the abbreviated command/prompt shown in the panel
// (provenance: handlers_livework.go).
func truncateTitle(s string) string { return wswork.TruncateTitle(s) }

func shouldPushLiveWork(m message.Message) bool { return wswork.ShouldPushLiveWork(m) }

// liveWorkPusher wraps the wswork pusher for events.go and the staying tests:
// mark() and the stopped exit channel keep their HEAD names.
type liveWorkPusher struct {
	inner   *wswork.LiveWorkPusher
	stopped <-chan struct{}
}

func startLiveWorkPusher(ctx context.Context, a *appPkg.App, h *Hub) *liveWorkPusher {
	inner := wswork.StartLiveWorkPusher(ctx, a, h, wsworkSeams())
	return &liveWorkPusher{inner: inner, stopped: inner.Stopped()}
}

func (p *liveWorkPusher) mark(sessionID string) { p.inner.Mark(sessionID) }

// batchPendingQuestionsFor keeps the HEAD seam indirection: it reads the
// session_notices fallback for awaiting-answer badges.
var batchPendingQuestionsFor = func(ctx context.Context, a *appPkg.App, visited []string) (map[string]sessionnotice.ChildQuestion, bool) {
	return batchPendingQuestions(ctx, a, visited)
}

func batchPendingQuestions(ctx context.Context, a *appPkg.App, visited []string) (map[string]sessionnotice.ChildQuestion, bool) {
	return wswork.BatchPendingQuestions(ctx, a, visited)
}

// batchRootMeta is the root test-facing panel meta; the wswork field names
// are private to that package, so the staying terminal test constructs this
// struct and the delegates convert both directions.
type batchRootMeta struct {
	root         db.BatchNode
	taskKind     string
	child        string
	askNodeID    string
	taskChildren int
	taskPayload  string
}

func batchRootMetaFromWswork(m wswork.BatchRootMeta) batchRootMeta {
	return batchRootMeta{
		root:         m.RootRow(),
		taskKind:     m.MetaTaskKind(),
		child:        m.MetaChild(),
		askNodeID:    m.MetaAskNodeID(),
		taskChildren: m.MetaTaskChildren(),
		taskPayload:  m.MetaTaskPayload(),
	}
}

func collectBatchHistoryTerminalRoots(ctx context.Context, src batchNodeSource, visited []string, liveRootIDs map[string]bool) ([]batchRootMeta, bool) {
	metas, ok := wswork.CollectBatchHistoryTerminalRoots(ctx, src, visited, liveRootIDs)
	out := make([]batchRootMeta, len(metas))
	for i, m := range metas {
		out[i] = batchRootMetaFromWswork(m)
	}
	return out, ok
}

func batchRootItem(m batchRootMeta, titles map[hub.TitleKey]string, questions map[string]sessionnotice.ChildQuestion) LiveWorkItemWire {
	return wswork.BatchRootItem(wswork.NewBatchRootMeta(m.root, m.taskKind, m.child, m.askNodeID, m.taskChildren, m.taskPayload), titles, questions)
}

// ── Wake schedules (stage 5b) delegates ─────────────────────────────────────

func handleGetSessionWakeSchedules(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wswork.HandleGetSessionWakeSchedules(ctx, a, c, msg)
}

func handleCancelWakeSchedule(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	wswork.HandleCancelWakeSchedule(ctx, a, c, msg)
}

func buildSessionWakeSchedules(ctx context.Context, store *sessionwake.WakeScheduleStore, sessionID string) (SessionWakeSchedulesPayload, error) {
	return wswork.BuildSessionWakeSchedules(ctx, store, sessionID)
}
