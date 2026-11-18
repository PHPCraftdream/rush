package server

// ws_bridge.go: temporary compatibility seam for WS-family packages moved
// under server/ws. Preserve wsworkSeams while root callers migrate through
// narrow wsconfig/wsapi exports. L7 removes the remaining root wrappers and
// this bridge once the composer and staying tests no longer need them.

import (
	"context"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsapi"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsconfig"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wswork"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	core "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

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
