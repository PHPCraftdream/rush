package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/google/uuid"
)

type wakeHost interface {
	wakeCoordinatorIsNil() bool
	WakeSession(context.Context, string, bool) error
	bumpWakeHint(string)
	isExternalDriver(string) bool
	drainDecision(context.Context, string, bool) (bool, drainVerdict, error)
	drainCallFor(context.Context, string) (SessionAgentCall, *preparedContinuation, error)
	handleDrainBuildError(string, error) error
	noteDrainRefused(string, error)
	addToRecheckSet(string)
	clearFromRecheckSet(string)
	agentFor(string) SessionAgent
	wakeScheduler() *wakeScheduler
	setWakeScheduler(*wakeScheduler)
}

type (
	wakeComponent       struct{ host wakeHost }
	coordinatorWakeHost struct{ coordinator *coordinator }
)

func newWakeComponent(c *coordinator) *wakeComponent {
	return &wakeComponent{host: coordinatorWakeHost{coordinator: c}}
}
func (h coordinatorWakeHost) wakeCoordinatorIsNil() bool { return h.coordinator == nil }
func (h coordinatorWakeHost) WakeSession(ctx context.Context, owner string, fact bool) error {
	return h.coordinator.wakeSession(ctx, owner, fact)
}
func (h coordinatorWakeHost) bumpWakeHint(id string) { h.coordinator.driverHints.bumpHint(id) }
func (h coordinatorWakeHost) isExternalDriver(id string) bool {
	return h.coordinator.driverHints.isExternalDriver(id)
}

func (h coordinatorWakeHost) drainDecision(ctx context.Context, id string, spent bool) (bool, drainVerdict, error) {
	return h.coordinator.drainDecision(ctx, id, spent)
}

func (h coordinatorWakeHost) drainCallFor(ctx context.Context, id string) (SessionAgentCall, *preparedContinuation, error) {
	return h.coordinator.drainCallFor(ctx, id)
}

func (h coordinatorWakeHost) handleDrainBuildError(id string, err error) error {
	return h.coordinator.handleDrainBuildError(id, err)
}

func (h coordinatorWakeHost) noteDrainRefused(id string, err error) {
	h.coordinator.noteDrainRefused(id, err)
}
func (h coordinatorWakeHost) addToRecheckSet(id string)       { h.coordinator.addToRecheckSet(id) }
func (h coordinatorWakeHost) clearFromRecheckSet(id string)   { h.coordinator.clearFromRecheckSet(id) }
func (h coordinatorWakeHost) agentFor(id string) SessionAgent { return h.coordinator.agentFor(id) }
func (h coordinatorWakeHost) wakeScheduler() *wakeScheduler {
	if h.coordinator == nil {
		return nil
	}
	return h.coordinator.wakeScheduler
}

func (h coordinatorWakeHost) setWakeScheduler(s *wakeScheduler) {
	if h.coordinator != nil {
		h.coordinator.wakeScheduler = s
	}
}

func (w *wakeComponent) wakeSession(ctx context.Context, sessionID string, fact bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("wakeSession panic", "session_id", sessionID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("wakeSession panic: %v", r)
		}
	}()
	if fact {
		w.host.bumpWakeHint(sessionID)
	}
	if w.host.isExternalDriver(sessionID) {
		return nil
	}
	decideCtx, cancel := context.WithTimeout(ctx, recheckDebtCheckBudget)
	debt, v, decErr := w.host.drainDecision(decideCtx, sessionID, fact)
	cancel()
	if decErr != nil {
		w.host.addToRecheckSet(sessionID)
		return fmt.Errorf("wakeSession: launch decision: %w", decErr)
	}
	if !debt {
		return nil
	}
	switch v.kind {
	case drainPaced:
		w.host.addToRecheckSet(sessionID)
		return nil
	case drainDeferred:
		if v.recheck {
			w.host.addToRecheckSet(sessionID)
		}
		return nil
	case drainStuck:
		return nil
	}
	call, prep, buildErr := w.host.drainCallFor(ctx, sessionID)
	if buildErr != nil {
		return w.handleDrainBuildError(sessionID, buildErr)
	}
	if prep != nil {
		defer prep.release()
	}
	if _, runErr := w.host.agentFor(sessionID).Run(ctx, call); runErr != nil {
		slog.Warn("drain call failed after its underlying notice was committed", "session_id", sessionID, "err", runErr)
		return runErr
	}
	return nil
}

func (w *wakeComponent) handleDrainBuildError(sessionID string, buildErr error) error {
	if errors.Is(buildErr, ErrDiskProviderUnavailable) || errors.Is(buildErr, ErrCredentialBindingUnavailable) {
		w.host.clearFromRecheckSet(sessionID)
		slog.Warn("drain launch refused: a process-bound drain dependency is unavailable; deferred until explicit re-supply", "session_id", sessionID, "err", buildErr)
		return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
	}
	w.host.noteDrainRefused(sessionID, buildErr)
	return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
}

func (w *wakeComponent) openOnceWakeSchedules(ctx context.Context, sessionID string) ([]db.WakeSchedule, error) {
	scheduler := w.host.wakeScheduler()
	if w.host.wakeCoordinatorIsNil() || scheduler == nil {
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

func (w *wakeComponent) HasOpenOnceWakeSchedule(ctx context.Context, sessionID string) (bool, error) {
	once, err := w.openOnceWakeSchedules(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return len(once) > 0, nil
}

func (w *wakeComponent) CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error) {
	scheduler := w.host.wakeScheduler()
	if w.host.wakeCoordinatorIsNil() || scheduler == nil {
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

func (w *wakeComponent) wakeSchedulerFor() (*wakeScheduler, error) {
	scheduler := w.host.wakeScheduler()
	if w.host.wakeCoordinatorIsNil() || scheduler == nil || scheduler.store == nil {
		return nil, errors.New("wake scheduler is unavailable: this session has no durable wake-schedule worker, so nothing would ever fire")
	}
	return scheduler, nil
}

func (w *wakeComponent) ScheduleWakeOnceDelay(ctx context.Context, sessionID string, delaySeconds int64, message string) (tools.WakeScheduleView, error) {
	scheduler, err := w.wakeSchedulerFor()
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
		return tools.WakeScheduleView{}, w.wrapWakeCreateError(err)
	}
	scheduler.Notify()
	return wakeScheduleView(row), nil
}

func (w *wakeComponent) ScheduleWakeOnceAt(ctx context.Context, sessionID, at, message string) (tools.WakeScheduleView, error) {
	scheduler, err := w.wakeSchedulerFor()
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
		return tools.WakeScheduleView{}, w.wrapWakeCreateError(err)
	}
	scheduler.Notify()
	return wakeScheduleView(row), nil
}

func (w *wakeComponent) ScheduleWakeLoop(ctx context.Context, sessionID string, everySeconds int64, message string, maxRuns *int64, until *string) (tools.WakeScheduleView, error) {
	scheduler, err := w.wakeSchedulerFor()
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
		return tools.WakeScheduleView{}, w.wrapWakeCreateError(err)
	}
	scheduler.Notify()
	return wakeScheduleView(row), nil
}

func (w *wakeComponent) ListWakeSchedules(ctx context.Context, sessionID string) ([]tools.WakeScheduleView, error) {
	scheduler, err := w.wakeSchedulerFor()
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

func (w *wakeComponent) CancelWakeSchedule(ctx context.Context, sessionID, scheduleID string) (string, error) {
	scheduler, err := w.wakeSchedulerFor()
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

func (w *wakeComponent) wrapWakeCreateError(err error) error {
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

func (w *wakeComponent) WakeSession(ctx context.Context, owner string, fact bool) error {
	return w.wakeSession(ctx, owner, fact)
}

func (w *wakeComponent) SetWakeScheduleStore(store *sessionwake.WakeScheduleStore) {
	if w.host.wakeCoordinatorIsNil() || store == nil || w.host.wakeScheduler() != nil {
		return
	}
	owner := "wakesched-" + uuid.NewString()
	scheduler := newWakeScheduler(store, owner, func(ctx context.Context, sessionID string) {
		if err := w.WakeSession(ctx, sessionID, true); err != nil {
			slog.Debug("wake scheduler: delivery attempt did not complete", "session_id", sessionID, "err", err)
		}
	})
	w.host.setWakeScheduler(scheduler)
	scheduler.Start()
}

func (w *wakeComponent) StopWakeScheduler() {
	if w.host.wakeCoordinatorIsNil() {
		return
	}
	w.host.wakeScheduler().Stop()
}

func (w *wakeComponent) WakeSchedulerNotify() {
	if w.host.wakeCoordinatorIsNil() {
		return
	}
	w.host.wakeScheduler().Notify()
}
