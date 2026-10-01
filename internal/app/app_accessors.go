// Small read-only accessors on *App: config/config-store exposure,
// the event-broker subscription helpers, and the agent-notification
// broker. Kept out of app.go so the type and constructor stand alone
// there.

package app

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
)

// Config returns the pure-data configuration.
func (app *App) Config() *config.Config {
	return app.config.Config()
}

// Store returns the config store.
func (app *App) Store() *config.ConfigStore {
	return app.config
}

// Events returns a per-caller subscription channel for application events.
// Each caller receives its own channel; all callers receive every event.
func (app *App) Events(ctx context.Context) <-chan pubsub.Event[any] {
	return app.events.Subscribe(ctx)
}

func (app *App) SendEvent(msg any) {
	app.events.Publish(pubsub.UpdatedEvent, msg)
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

// AsyncJobStore returns the phase-4 durable job store App.New builds
// unconditionally whenever this App has a data dir (docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.5 step 7's readers: `sessions jobs`, the
// LiveJobs-based STATUS surfaces) -- available even when no provider is
// configured, since it is a pure DB/lock-file reader, not the agent
// runtime. nil under SkipAgentSetup or when this App has no data dir;
// callers must treat a nil store as "no async job data available", not
// panic.
func (app *App) AsyncJobStore() *session.AsyncJobStore {
	return app.asyncJobStore
}

// SetAsyncJobStoreForTest wires an AsyncJobStore into an App built WITHOUT
// InitCoderAgent -- a lightweight &App{...} literal, the pattern several
// `sessions why`/`sessions list` unit tests use to test the command's own
// logic without standing up a full agent coordinator. Test-only; mirrors
// AsyncJobStore's own SimulateCrashForTest seam.
func (app *App) SetAsyncJobStoreForTest(s *session.AsyncJobStore) {
	app.asyncJobStore = s
}

// WakeScheduleStore returns the stage-4b durable wake-schedule store
// (same construction rules as AsyncJobStore: nil under SkipAgentSetup or
// without a data dir; readers treat nil as "no schedule data available").
func (app *App) WakeScheduleStore() *session.WakeScheduleStore {
	return app.wakeScheduleStore
}

// SetWakeScheduleStoreForTest wires a WakeScheduleStore into an App built
// WITHOUT InitCoderAgent (the &App{...} literal pattern); mirrors
// SetAsyncJobStoreForTest.
func (app *App) SetWakeScheduleStoreForTest(s *session.WakeScheduleStore) {
	app.wakeScheduleStore = s
}
