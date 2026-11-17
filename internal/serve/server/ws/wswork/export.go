package wswork

// export.go: narrow L5 cutover surface (design docs/plans/2026-11-15-sprint0-
// split-server.md §3 S5). Only what the root package's bridge (ws_bridge.go)
// and its staying white tests actually call — exported type aliases over the
// package-private wire/panel types and one-line delegating wrappers that
// thread Seams. No mutable seam global is exported; the root package keeps
// owning its seam vars and passes them in as Seams.

import (
	"context"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	wire "github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// ── Wire type aliases (field names identical to the HEAD root wire) ─────────

type (
	BatchCountsWire       = batchCountsWire
	BatchDeliveryWire     = batchDeliveryWire
	BatchNodeWire         = batchNodeWire
	BatchRootWire         = batchRootWire
	BatchCompletionWire   = batchCompletionWire
	BatchWorkSnapshotWire = batchWorkSnapshotWire
	BatchControlResult    = batchControlResult
	BatchControlPayload   = batchControlPayload
	BatchRootMeta         = batchRootMeta
	LiveWorkPusher        = liveWorkPusher
)

// ── Constants re-exports ────────────────────────────────────────────────────

const (
	TitleMaxLen               = titleMaxLen
	LiveWorkTerminalLimit     = liveWorkTerminalLimit
	BatchHistoryDepth         = batchHistoryDepth
	BatchHistoryTerminalBound = batchHistoryTerminalBound
)

// ── Batch snapshot / control ────────────────────────────────────────────────

func BuildBatchWorkSnapshot(ctx context.Context, a *appPkg.App, sessionID string, seams Seams) BatchWorkSnapshotWire {
	return buildBatchWorkSnapshot(ctx, a, sessionID, seams)
}

func HandleGetSessionBatchWork(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, seams Seams) {
	handleGetSessionBatchWork(ctx, a, c, msg, seams)
}

func HandleBatchControl(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, seams Seams) {
	handleBatchControl(ctx, a, c, msg, seams)
}

func BatchCollectRows(ctx context.Context, src BatchNodeSource, sessionID string) (rowsByOwner map[string][]db.BatchNode, visited []string, complete bool) {
	return batchCollectRows(ctx, src, sessionID)
}

// ── Live work ───────────────────────────────────────────────────────────────

func HandleGetSessionLiveWork(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage, seams Seams) {
	handleGetSessionLiveWork(ctx, a, c, msg, seams)
}

func BuildSessionLiveWork(ctx context.Context, a *appPkg.App, cache *hub.TitleCache, sessionID string, s Seams) (wire.SessionLiveWorkPayload, error) {
	return buildSessionLiveWork(ctx, a, cache, sessionID, s)
}

func BuildSessionLiveWorkFromBatch(ctx context.Context, a *appPkg.App, cache *hub.TitleCache, sessionID string, seams Seams) (wire.SessionLiveWorkPayload, error) {
	return buildSessionLiveWorkFromBatch(ctx, a, cache, sessionID, seams)
}

func TruncateTitle(s string) string { return truncateTitle(s) }

func ShouldPushLiveWork(m message.Message) bool { return shouldPushLiveWork(m) }

func StartLiveWorkPusher(ctx context.Context, a *appPkg.App, h *hub.Hub, seams ...Seams) *LiveWorkPusher {
	return startLiveWorkPusher(ctx, a, h, seams...)
}

// Stopped re-exports the pusher's exit channel for the staying root tests
// (<-p.stopped) without exporting the mutable struct fields.
func (p *liveWorkPusher) Stopped() <-chan struct{} { return p.stopped }

// Mark re-exports mark for the staying root tests and the root bridge.
func (p *liveWorkPusher) Mark(sessionID string) { p.mark(sessionID) }

// ── batchRootMeta crossing ──────────────────────────────────────────────────

// NewBatchRootMeta builds the panel meta from plain values so the root bridge
// can convert its own test-facing struct without exposing this package's
// fields.
func NewBatchRootMeta(root db.BatchNode, taskKind, child, askNodeID string, taskChildren int, taskPayload string) BatchRootMeta {
	return batchRootMeta{
		root:         root,
		taskKind:     taskKind,
		child:        child,
		askNodeID:    askNodeID,
		taskChildren: taskChildren,
		taskPayload:  taskPayload,
	}
}

// Field accessors for the root bridge's wswork→root conversion.
func (m batchRootMeta) RootRow() db.BatchNode   { return m.root }
func (m batchRootMeta) MetaTaskKind() string    { return m.taskKind }
func (m batchRootMeta) MetaChild() string       { return m.child }
func (m batchRootMeta) MetaAskNodeID() string   { return m.askNodeID }
func (m batchRootMeta) MetaTaskChildren() int   { return m.taskChildren }
func (m batchRootMeta) MetaTaskPayload() string { return m.taskPayload }

func CollectBatchHistoryTerminalRoots(ctx context.Context, src BatchNodeSource, visited []string, liveRootIDs map[string]bool) ([]BatchRootMeta, bool) {
	return collectBatchHistoryTerminalRoots(ctx, src, visited, liveRootIDs)
}

func BatchRootItem(m BatchRootMeta, titles map[hub.TitleKey]string, questions map[string]session.ChildQuestion) wire.LiveWorkItemWire {
	return batchRootItem(m, titles, questions)
}

func BatchPendingQuestions(ctx context.Context, a *appPkg.App, visited []string) (map[string]session.ChildQuestion, bool) {
	return batchPendingQuestions(ctx, a, visited)
}

// ── Wake schedules ──────────────────────────────────────────────────────────

func HandleGetSessionWakeSchedules(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage) {
	handleGetSessionWakeSchedules(ctx, a, c, msg)
}

func HandleCancelWakeSchedule(ctx context.Context, a *appPkg.App, c *hub.Client, msg wire.WSMessage) {
	handleCancelWakeSchedule(ctx, a, c, msg)
}

func BuildSessionWakeSchedules(ctx context.Context, store *session.WakeScheduleStore, sessionID string) (wire.SessionWakeSchedulesPayload, error) {
	return buildSessionWakeSchedules(ctx, store, sessionID)
}
