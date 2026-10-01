// Coordinator-side implementation of tools.WakeControl — the
// wakein/wakeon/loop/wake_list/wake_cancel tool family (stage 4c of
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md; contract:
// docs/plans/2026-09-27-wake-tools-contract.md §1/§5/§6). Validation follows
// §1's exact error texts; persistence goes through the stage-4a
// WakeScheduleStore (SCHED-01..05) and every successful mutation recomputes
// the stage-4b worker's timer via Notify (SCHED-06). A coordinator without a
// running worker (SDK/test coordinators) answers with an honest "scheduler
// unavailable" error instead of silently accepting a schedule nobody will
// ever fire.
package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

var _ tools.WakeControl = (*coordinator)(nil)

const (
	// wakePastTolerance is the clock-skew allowance when rejecting a past
	// at/unil timestamp (contract §1.2: "допуск 2 с").
	wakePastTolerance = 2 * time.Second
	// wakeMaxDelaySeconds is wakein's upper bound (§1.1: 100 years in
	// seconds — a unit-mistake guard, not a product limit).
	wakeMaxDelaySeconds = int64(3155760000)
)

// wakeSchedulerFor returns the running worker and its store, or an honest
// model-safe error when this coordinator has no wake-schedule worker.
func (c *coordinator) wakeSchedulerFor() (*wakeScheduler, error) {
	if c == nil || c.wakeScheduler == nil || c.wakeScheduler.store == nil {
		return nil, errors.New("wake scheduler is unavailable: this session has no durable wake-schedule worker, so nothing would ever fire")
	}
	return c.wakeScheduler, nil
}

// validateWakeMessage enforces §6's message rules (shared by all three
// creators).
func validateWakeMessage(message string) error {
	n := len([]rune(message))
	if n == 0 {
		return errors.New("message is required")
	}
	if n > session.MaxWakeMessageLength {
		return fmt.Errorf(
			"message exceeds %d characters (%d); shorten it — this becomes part of the next turn's context, not a task payload",
			session.MaxWakeMessageLength, n,
		)
	}
	return nil
}

// validateWakeDelay enforces §1.1's delay_seconds bounds.
func validateWakeDelay(delaySeconds int64) error {
	if delaySeconds <= 0 {
		return errors.New("delay_seconds must be positive")
	}
	if delaySeconds < 5 {
		return errors.New("delay_seconds must be at least 5 (use it to avoid immediately re-waking yourself)")
	}
	if delaySeconds > wakeMaxDelaySeconds {
		return errors.New("delay_seconds is implausibly large (over 100 years); check your units")
	}
	return nil
}

// parseWakeTimestamp enforces §1.2's RFC3339-with-zone rule.
func parseWakeTimestamp(value, field string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%s must be RFC3339 with an explicit offset or Z, e.g. %q or %q: %s",
			field, "2026-09-28T09:00:00+03:00", "2026-09-28T06:00:00Z", err,
		)
	}
	return t, nil
}

// checkWakeTimestamp rejects a past timestamp (with the 2s skew allowance)
// and an implausibly far one (§1.2), for both wakeon.at and loop.until.
func checkWakeTimestamp(t time.Time, now time.Time, field string) error {
	if t.Before(now.Add(-wakePastTolerance)) {
		return fmt.Errorf(
			"%s (%s) is in the past (server time is %s); use wakein for a relative delay instead",
			field, t.Format(time.RFC3339), now.Format(time.RFC3339),
		)
	}
	if t.Sub(now) > session.MaxWakeHorizon {
		return fmt.Errorf("%s is implausibly far in the future", field)
	}
	if t.Sub(now) < session.MinWakeOnceDelay {
		return fmt.Errorf("%s is too soon (schedule at least %s ahead); use wakein for a short delay", field, session.MinWakeOnceDelay)
	}
	return nil
}

// ScheduleWakeOnceDelay implements tools.WakeControl (§1.1). wakein is NOT
// idempotent by input: two identical calls create two schedules (§1.1).
func (c *coordinator) ScheduleWakeOnceDelay(ctx context.Context, sessionID string, delaySeconds int64, message string) (tools.WakeScheduleView, error) {
	sched, err := c.wakeSchedulerFor()
	if err != nil {
		return tools.WakeScheduleView{}, err
	}
	if err := validateWakeDelay(delaySeconds); err != nil {
		return tools.WakeScheduleView{}, err
	}
	if err := validateWakeMessage(message); err != nil {
		return tools.WakeScheduleView{}, err
	}
	now := sched.now()
	row, err := sched.store.CreateSchedule(ctx, session.CreateWakeScheduleParams{
		Owner: sessionID, Kind: session.WakeKindOnce, Message: message,
		RunAt: now.Add(time.Duration(delaySeconds) * time.Second),
	}, now)
	if err != nil {
		return tools.WakeScheduleView{}, c.wrapWakeCreateError(err)
	}
	sched.Notify()
	return wakeScheduleView(row), nil
}

// ScheduleWakeOnceAt implements tools.WakeControl (§1.2).
func (c *coordinator) ScheduleWakeOnceAt(ctx context.Context, sessionID, at, message string) (tools.WakeScheduleView, error) {
	sched, err := c.wakeSchedulerFor()
	if err != nil {
		return tools.WakeScheduleView{}, err
	}
	if err := validateWakeMessage(message); err != nil {
		return tools.WakeScheduleView{}, err
	}
	parsed, err := parseWakeTimestamp(at, "at")
	if err != nil {
		return tools.WakeScheduleView{}, err
	}
	now := sched.now()
	if err := checkWakeTimestamp(parsed, now, "at"); err != nil {
		return tools.WakeScheduleView{}, err
	}
	row, err := sched.store.CreateSchedule(ctx, session.CreateWakeScheduleParams{
		Owner: sessionID, Kind: session.WakeKindOnce, Message: message, RunAt: parsed,
	}, now)
	if err != nil {
		return tools.WakeScheduleView{}, c.wrapWakeCreateError(err)
	}
	sched.Notify()
	return wakeScheduleView(row), nil
}

// ScheduleWakeLoop implements tools.WakeControl (§1.3).
func (c *coordinator) ScheduleWakeLoop(ctx context.Context, sessionID string, everySeconds int64, message string, maxRuns *int64, until *string) (tools.WakeScheduleView, error) {
	sched, err := c.wakeSchedulerFor()
	if err != nil {
		return tools.WakeScheduleView{}, err
	}
	if everySeconds*int64(time.Second) < int64(session.MinLoopInterval) {
		return tools.WakeScheduleView{}, errors.New(
			"every_seconds must be at least 300 (5 minutes); a tighter loop would wake you more often than the system's own supervision tick and is rarely what you want — use wakein for a single near-term check instead",
		)
	}
	if err := validateWakeMessage(message); err != nil {
		return tools.WakeScheduleView{}, err
	}
	if maxRuns != nil {
		if *maxRuns <= 0 {
			return tools.WakeScheduleView{}, errors.New("max_runs must be positive")
		}
		if *maxRuns > session.MaxLoopRuns {
			return tools.WakeScheduleView{}, fmt.Errorf(
				"max_runs exceeds the limit (%d); use wake_cancel to stop the loop manually instead of an enormous max_runs", session.MaxLoopRuns,
			)
		}
	}
	now := sched.now()
	params := session.CreateWakeScheduleParams{
		Owner: sessionID, Kind: session.WakeKindLoop, Message: message,
		RunAt: now.Add(time.Duration(everySeconds) * time.Second),
		Every: time.Duration(everySeconds) * time.Second,
	}
	if maxRuns != nil {
		params.MaxRuns = int(*maxRuns)
	}
	var untilT *time.Time
	if until != nil {
		parsed, err := parseWakeTimestamp(*until, "until")
		if err != nil {
			return tools.WakeScheduleView{}, err
		}
		if err := checkWakeTimestamp(parsed, now, "until"); err != nil {
			return tools.WakeScheduleView{}, err
		}
		if parsed.Before(params.RunAt) {
			return tools.WakeScheduleView{}, fmt.Errorf(
				"until (%s) is earlier than the first scheduled firing (%s)",
				parsed.Format(time.RFC3339), params.RunAt.Format(time.RFC3339),
			)
		}
		untilT = &parsed
		params.Until = untilT
	}
	row, err := sched.store.CreateSchedule(ctx, params, now)
	if err != nil {
		return tools.WakeScheduleView{}, c.wrapWakeCreateError(err)
	}
	sched.Notify()
	return wakeScheduleView(row), nil
}

// ListWakeSchedules implements tools.WakeControl (§1.4): active schedules of
// THIS session only (ownership, not a filter). ListWakeSchedulesForOwner
// returns terminal rows too (they are history); wake_list reports active
// ones only.
func (c *coordinator) ListWakeSchedules(ctx context.Context, sessionID string) ([]tools.WakeScheduleView, error) {
	sched, err := c.wakeSchedulerFor()
	if err != nil {
		return nil, err
	}
	rows, err := sched.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list wake schedules: %w", err)
	}
	views := make([]tools.WakeScheduleView, 0, len(rows))
	for _, row := range rows {
		if row.State != "active" {
			continue
		}
		views = append(views, wakeScheduleView(row))
	}
	return views, nil
}

// CancelWakeSchedule implements tools.WakeControl (§1.5). The single
// not-found error covers never-existed, foreign, already-fired and
// already-cancelled alike (§1.5's idempotency rule: a safe no-op with the
// same error, never a disguised success): "cancelled" is answered only when
// this call's own CAS moved the row (CancelScheduleReport).
func (c *coordinator) CancelWakeSchedule(ctx context.Context, sessionID, scheduleID string) (string, error) {
	sched, err := c.wakeSchedulerFor()
	if err != nil {
		return "", err
	}
	notFound := fmt.Errorf(
		"schedule %s not found (never existed, already fired, already cancelled, or belongs to another session)", scheduleID,
	)
	// The report is the CAS's own outcome: a schedule that fired between a
	// look and the cancel is "not found", never a disguised success.
	cancelled, err := sched.store.CancelScheduleReport(ctx, sessionID, scheduleID, sched.now())
	if errors.Is(err, session.ErrWakeScheduleNotOwned) {
		return "", notFound
	}
	if err != nil {
		return "", fmt.Errorf("failed to cancel wake schedule: %w", err)
	}
	if !cancelled {
		return "", notFound
	}
	sched.Notify()
	return fmt.Sprintf("Schedule %s cancelled.", scheduleID), nil
}

// wrapWakeCreateError maps store refusals onto the contract's model-facing
// texts (§1.1's limit message; SCHED-04's ownership comes from the session
// id in context, so a foreign-owner error here is an internal invariant
// breach and surfaces raw).
func (c *coordinator) wrapWakeCreateError(err error) error {
	if errors.Is(err, session.ErrWakeScheduleLimit) {
		return errors.New(
			"maximum number of active schedules (20) reached for this session; cancel one with wake_cancel or wait for a one-shot wake to fire",
		)
	}
	return err
}

// wakeScheduleView maps a store row onto the tools' JSON shape (§1). All
// times are normalized to UTC RFC3339 so wakeon/wakein/loop entries compare
// directly (§1.2).
func wakeScheduleView(row db.WakeSchedule) tools.WakeScheduleView {
	view := tools.WakeScheduleView{
		ScheduleID: row.ID,
		Kind:       row.Kind,
		Message:    row.Message,
	}
	if row.MaxRuns.Valid {
		mr := row.MaxRuns.Int64
		view.MaxRuns = &mr
	}
	if row.UntilAt.Valid {
		u := time.Unix(row.UntilAt.Int64, 0).UTC().Format(time.RFC3339)
		view.Until = &u
	}
	switch session.WakeKind(row.Kind) {
	case session.WakeKindOnce:
		view.FiresAt = time.Unix(row.NextRunAt, 0).UTC().Format(time.RFC3339)
	case session.WakeKindLoop:
		view.EverySeconds = row.EveryMs / 1000
		view.NextFireAt = time.Unix(row.NextRunAt, 0).UTC().Format(time.RFC3339)
		view.Occurrence = row.Occurrence
	}
	return view
}
