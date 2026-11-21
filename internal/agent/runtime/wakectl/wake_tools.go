package wakectl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

func (scheduler *Scheduler) OpenOnceWakeSchedules(ctx context.Context, sessionID string) ([]db.WakeSchedule, error) {
	if scheduler == nil {
		return nil, nil
	}
	rows, err := scheduler.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var once []db.WakeSchedule
	for _, row := range rows {
		if row.Kind == string(sessionwake.WakeKindOnce) && row.State == "active" {
			once = append(once, row)
		}
	}
	return once, nil
}

func (scheduler *Scheduler) CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error) {
	if scheduler == nil {
		return 0, nil
	}
	rows, err := scheduler.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	n := 0
	now := time.Now()
	for _, row := range rows {
		if row.Kind != string(sessionwake.WakeKindLoop) || row.State != "active" {
			continue
		}
		if err := scheduler.store.CancelSchedule(ctx, sessionID, row.ID, now); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		scheduler.Notify()
	}
	return n, nil
}

func (scheduler *Scheduler) RequireScheduler() (*Scheduler, error) {
	if scheduler == nil || scheduler.store == nil {
		return nil, errors.New("wake scheduler is unavailable: this session has no durable wake-schedule worker, so nothing would ever fire")
	}
	return scheduler, nil
}

func (scheduler *Scheduler) ScheduleWakeOnceDelay(ctx context.Context, sessionID string, delaySeconds int64, message string) (tools.WakeScheduleView, error) {
	_, err := scheduler.RequireScheduler()
	if err != nil {
		return tools.WakeScheduleView{}, err
	}
	if err := validateWakeDelay(delaySeconds); err != nil {
		return tools.WakeScheduleView{}, err
	}
	if err := validateWakeMessage(message); err != nil {
		return tools.WakeScheduleView{}, err
	}
	now := scheduler.now()
	row, err := scheduler.store.CreateSchedule(ctx, sessionwake.CreateWakeScheduleParams{Owner: sessionID, Kind: sessionwake.WakeKindOnce, Message: message, RunAt: now.Add(time.Duration(delaySeconds) * time.Second)}, now)
	if err != nil {
		return tools.WakeScheduleView{}, wrapWakeCreateError(err)
	}
	scheduler.Notify()
	return wakeScheduleView(row), nil
}

func (scheduler *Scheduler) ScheduleWakeOnceAt(ctx context.Context, sessionID, at, message string) (tools.WakeScheduleView, error) {
	_, err := scheduler.RequireScheduler()
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
	now := scheduler.now()
	if err := checkWakeTimestamp(parsed, now, "at"); err != nil {
		return tools.WakeScheduleView{}, err
	}
	row, err := scheduler.store.CreateSchedule(ctx, sessionwake.CreateWakeScheduleParams{Owner: sessionID, Kind: sessionwake.WakeKindOnce, Message: message, RunAt: parsed}, now)
	if err != nil {
		return tools.WakeScheduleView{}, wrapWakeCreateError(err)
	}
	scheduler.Notify()
	return wakeScheduleView(row), nil
}

func (scheduler *Scheduler) ScheduleWakeLoop(ctx context.Context, sessionID string, everySeconds int64, message string, maxRuns *int64, until *string) (tools.WakeScheduleView, error) {
	_, err := scheduler.RequireScheduler()
	if err != nil {
		return tools.WakeScheduleView{}, err
	}
	if everySeconds*int64(time.Second) < int64(sessionwake.MinLoopInterval) {
		return tools.WakeScheduleView{}, errors.New("every_seconds must be at least 300 (5 minutes); a tighter loop would wake you more often than the system's own supervision tick and is rarely what you want — use wakein for a single near-term check instead")
	}
	if err := validateWakeMessage(message); err != nil {
		return tools.WakeScheduleView{}, err
	}
	if maxRuns != nil {
		if *maxRuns <= 0 {
			return tools.WakeScheduleView{}, errors.New("max_runs must be positive")
		}
		if *maxRuns > sessionwake.MaxLoopRuns {
			return tools.WakeScheduleView{}, fmt.Errorf("max_runs exceeds the limit (%d); use wake_cancel to stop the loop manually instead of an enormous max_runs", sessionwake.MaxLoopRuns)
		}
	}
	now := scheduler.now()
	params := sessionwake.CreateWakeScheduleParams{Owner: sessionID, Kind: sessionwake.WakeKindLoop, Message: message, RunAt: now.Add(time.Duration(everySeconds) * time.Second), Every: time.Duration(everySeconds) * time.Second}
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
			return tools.WakeScheduleView{}, fmt.Errorf("until (%s) is earlier than the first scheduled firing (%s)", parsed.Format(time.RFC3339), params.RunAt.Format(time.RFC3339))
		}
		untilT = &parsed
		params.Until = untilT
	}
	row, err := scheduler.store.CreateSchedule(ctx, params, now)
	if err != nil {
		return tools.WakeScheduleView{}, wrapWakeCreateError(err)
	}
	scheduler.Notify()
	return wakeScheduleView(row), nil
}

func (scheduler *Scheduler) ListWakeSchedules(ctx context.Context, sessionID string) ([]tools.WakeScheduleView, error) {
	_, err := scheduler.RequireScheduler()
	if err != nil {
		return nil, err
	}
	rows, err := scheduler.store.ListSchedules(ctx, sessionID)
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

func (scheduler *Scheduler) CancelWakeSchedule(ctx context.Context, sessionID, scheduleID string) (string, error) {
	_, err := scheduler.RequireScheduler()
	if err != nil {
		return "", err
	}
	notFound := fmt.Errorf("schedule %s not found (never existed, already fired, already cancelled, or belongs to another session)", scheduleID)
	cancelled, err := scheduler.store.CancelScheduleReport(ctx, sessionID, scheduleID, scheduler.now())
	if errors.Is(err, sessionwake.ErrWakeScheduleNotOwned) {
		return "", notFound
	}
	if err != nil {
		return "", fmt.Errorf("failed to cancel wake schedule: %w", err)
	}
	if !cancelled {
		return "", notFound
	}
	scheduler.Notify()
	return fmt.Sprintf("Schedule %s cancelled.", scheduleID), nil
}

func wrapWakeCreateError(err error) error {
	if errors.Is(err, sessionwake.ErrWakeScheduleLimit) {
		return errors.New("maximum number of active schedules (20) reached for this session; cancel one with wake_cancel or wait for a one-shot wake to fire")
	}
	return err
}

func wakeScheduleView(row db.WakeSchedule) tools.WakeScheduleView {
	view := tools.WakeScheduleView{ScheduleID: row.ID, Kind: row.Kind, Message: row.Message}
	if row.MaxRuns.Valid {
		mr := row.MaxRuns.Int64
		view.MaxRuns = &mr
	}
	if row.UntilAt.Valid {
		u := time.Unix(row.UntilAt.Int64, 0).UTC().Format(time.RFC3339)
		view.Until = &u
	}
	switch sessionwake.WakeKind(row.Kind) {
	case sessionwake.WakeKindOnce:
		view.FiresAt = time.Unix(row.NextRunAt, 0).UTC().Format(time.RFC3339)
	case sessionwake.WakeKindLoop:
		view.EverySeconds = row.EveryMs / 1000
		view.NextFireAt = time.Unix(row.NextRunAt, 0).UTC().Format(time.RFC3339)
		view.Occurrence = row.Occurrence
	}
	return view
}
