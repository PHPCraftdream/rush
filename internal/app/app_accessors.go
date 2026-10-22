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

// BatchHostStore returns the App host store
// (session.HostStore): the job-independent host identity the batch
// runtime install registers and releases. Nil without a database writer
// or without a data dir; callers treat nil as "no batch host identity",
// never panic.
func (app *App) BatchHostStore() *session.HostStore {
	return app.batchHostStore()
}

// BatchNoticeStore returns the job-independent session-notice store
// (session.SessionNoticeStore) wired to the BatchHostStore identity and
// liveness. Nil without a database writer.
func (app *App) BatchNoticeStore() *session.SessionNoticeStore {
	return app.batchNoticeStore()
}

// WakeScheduleStore returns the stage-4b durable wake-schedule store
// (same construction rules: nil under SkipAgentSetup or without a data
// dir; readers treat nil as "no schedule data available").
func (app *App) WakeScheduleStore() *session.WakeScheduleStore {
	return app.wakeScheduleStore
}

// SetWakeScheduleStoreForTest wires a WakeScheduleStore into an App built
// WITHOUT InitCoderAgent (the &App{...} literal pattern); test-only.
func (app *App) SetWakeScheduleStoreForTest(s *session.WakeScheduleStore) {
	app.wakeScheduleStore = s
}

// SetDataDirForTest points the session-activity reader's lock inspection at
// a data directory chosen by the test (the &App{...} literal pattern; the
// reader derives the lock path itself, decision 10).
func (app *App) SetDataDirForTest(dir string) {
	app.dataDir = dir
}
