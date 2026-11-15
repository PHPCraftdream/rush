// Wake-schedule snapshot + cancel handlers (stage 5b): the server half of
// the web panel's Schedules tab. Reads and writes go through
// session.WakeScheduleStore over this App's DB handle — the same durable
// wake_schedules table the stage-4b scheduler worker claims from, so a
// snapshot is correct no matter which process created or fired the
// schedules. Only the REQUESTED session's rows are ever read (isolation:
// no delegation-tree walk, unlike live work).

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// wakeScheduleStoreFor builds a store handle over the App's DB. The store
// is stateless over the pool, so a per-call handle is safe; nil (with a
// logged warning at the caller) means this App has no DB, which no
// reachable server does.
func wakeScheduleStoreFor(a *appPkg.App) *session.WakeScheduleStore {
	if a == nil || a.DB == nil || a.DB() == nil {
		return nil
	}
	return session.NewWakeScheduleStore(a.DB())
}

// notifyWakeScheduler prods the stage-4b scheduler worker to recompute its
// timer after a schedule change. Best-effort via an optional interface: the
// concrete coordinator implements it, test fakes may not, and an App
// without an agent setup has nothing to notify (the worker's next tick
// reads the cancelled row from the DB and skips it regardless).
func notifyWakeScheduler(a *appPkg.App) {
	if n, ok := a.AgentCoordinator.(interface{ WakeSchedulerNotify() }); ok {
		n.WakeSchedulerNotify()
	}
}

// handleGetSessionWakeSchedules replies to CmdGetSessionWakeSchedules with
// one full EventSessionWakeSchedules snapshot for the requested session. A
// session that does not exist is an explicit error — same rule as
// handleGetSessionLiveWork.
func handleGetSessionWakeSchedules(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p GetSessionWakeSchedulesPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, "session not found")
		return
	}
	store := wakeScheduleStoreFor(a)
	if store == nil {
		c.reply(msg.ID, EventSessionWakeSchedules, SessionWakeSchedulesPayload{SessionID: p.SessionID, Schedules: []WakeScheduleWire{}}, "")
		return
	}
	snap, err := buildSessionWakeSchedules(ctx, store, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventSessionWakeSchedules, snap, "")
}

// handleCancelWakeSchedule cancels one schedule owned by the requested
// session and replies with a fresh snapshot. An unknown or foreign
// ScheduleID is an explicit "not found" error (the same single shape the
// wake_cancel tool returns); a second cancel of an already-cancelled row
// succeeds idempotently. On success the stage-4b worker's timer is
// recomputed so a due-but-not-yet-claimed occurrence never fires.
func handleCancelWakeSchedule(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p CancelWakeSchedulePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" || p.ScheduleID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload: sessionID and scheduleID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, "session not found")
		return
	}
	store := wakeScheduleStoreFor(a)
	if store == nil {
		c.reply(msg.ID, EventError, nil, "wake schedules are not available")
		return
	}
	// CancelSchedule alone cannot distinguish "unknown id" from "foreign
	// id" — both must read as the same not-found error anyway, and both
	// must be REFUSED, not silently swallowed — so membership in the
	// owner's own list is the ownership check (SCHED-5).
	rows, err := store.ListSchedules(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	known := false
	for _, row := range rows {
		if row.ID == p.ScheduleID {
			known = true
			break
		}
	}
	if !known {
		c.reply(msg.ID, EventError, nil, "wake schedule not found")
		return
	}
	if err := store.CancelSchedule(ctx, p.SessionID, p.ScheduleID, time.Now()); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	notifyWakeScheduler(a)
	snap, err := buildSessionWakeSchedules(ctx, store, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventSessionWakeSchedules, snap, "")
}

// buildSessionWakeSchedules reads the owner's wake_schedules rows (ANY
// state — the panel shows done/cancelled history too) into the wire shape.
// Message text is truncated with the shared title budget.
func buildSessionWakeSchedules(ctx context.Context, store *session.WakeScheduleStore, sessionID string) (SessionWakeSchedulesPayload, error) {
	rows, err := store.ListSchedules(ctx, sessionID)
	if err != nil {
		return SessionWakeSchedulesPayload{}, err
	}
	items := make([]WakeScheduleWire, 0, len(rows))
	for _, row := range rows {
		var untilAt int64
		if row.UntilAt.Valid {
			untilAt = row.UntilAt.Int64 * 1000
		}
		items = append(items, WakeScheduleWire{
			ID:         row.ID,
			Kind:       row.Kind,
			Message:    truncateTitle(oneLine(row.Message)),
			NextRunAt:  row.NextRunAt * 1000,
			EveryMs:    row.EveryMs,
			MaxRuns:    row.MaxRuns.Int64,
			UntilAt:    untilAt,
			State:      row.State,
			Occurrence: row.Occurrence,
			CreatedAt:  row.CreatedAt * 1000,
		})
	}
	return SessionWakeSchedulesPayload{SessionID: sessionID, Schedules: items}, nil
}

// pushWakeSchedules broadcasts one snapshot for sessionID off the hot path
// (called only from the live-work pusher's sweep). Best-effort: a failure
// is logged and the next change event re-sends.
func pushWakeSchedules(ctx context.Context, a *appPkg.App, h *Hub, sessionID string) {
	store := wakeScheduleStoreFor(a)
	if store == nil {
		return
	}
	snap, err := buildSessionWakeSchedules(ctx, store, sessionID)
	if err != nil {
		slog.Warn("wake_schedules: could not build the change snapshot", "session", sessionID, "err", err)
		return
	}
	h.Broadcast(EventSessionWakeSchedules, snap)
}
