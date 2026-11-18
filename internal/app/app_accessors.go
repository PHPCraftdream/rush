// Small read-only accessors on *App: config/config-store exposure,
// the event-broker subscription helpers, and the agent-notification
// broker. Kept out of app.go so the type and constructor stand alone
// there.

package app

import (
	"context"
	"database/sql"
	"io"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/safety/notify"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/app/appbatch"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
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
// (sessionhosts.HostStore): the job-independent host identity the batch
// runtime install registers and releases. Nil without a database writer
// or without a data dir; callers treat nil as "no batch host identity",
// never panic.
func (app *App) BatchHostStore() *sessionhosts.HostStore {
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
func (app *App) WakeScheduleStore() *sessionwake.WakeScheduleStore {
	return app.wakeScheduleStore
}

// SetWakeScheduleStoreForTest wires a WakeScheduleStore into an App built
// WITHOUT InitCoderAgent (the &App{...} literal pattern); test-only.
func (app *App) SetWakeScheduleStoreForTest(s *sessionwake.WakeScheduleStore) {
	app.wakeScheduleStore = s
}

// SetDataDirForTest points the session-activity reader's lock inspection at
// a data directory chosen by the test (the &App{...} literal pattern; the
// reader derives the lock path itself, decision 10).
func (app *App) SetDataDirForTest(dir string) {
	app.dataDir = dir
}

// runEnv snapshots the run cluster's capability set at wrapper entry.
// Closure fields resolve their App field lazily on purpose: the underlying
// fields can be reassigned after New (test fakes, InitCoderAgent's
// self-heal, the lazily built batch-fault writer), so each read must
// happen at the same moment the old method-body read did.
func (app *App) runEnv() RunEnv {
	// A bare &App{} literal has no DB func: env.DB stays nil so every
	// `env.DB == nil` check keeps the old `app.DB == nil` meaning.
	var db func() *sql.DB
	if app.DB != nil {
		db = func() *sql.DB { return app.DB() }
	}
	return RunEnv{
		WorkspaceRoot:         app.workspaceRoot,
		Config:                app.config,
		Sessions:              func() coresession.Service { return app.Sessions },
		Messages:              func() message.Service { return app.Messages },
		Coord:                 func() agent.Coordinator { return app.AgentCoordinator },
		Perms:                 func() permission.Service { return app.Permissions },
		Pump:                  func() *runq.RunQueuePump { return app.RunQueuePump },
		WakeStore:             func() *sessionwake.WakeScheduleStore { return app.wakeScheduleStore },
		DB:                    db,
		DataDir:               func() string { return app.dataDir },
		MCPOwner:              func() *mcp.Owner { return app.mcpOwner },
		ProcessHome:           app.ProcessHome,
		SessionActivity:       app.SessionActivity,
		DiskBindings:          app.resolveDiskBindings,
		ResolveModelOverrides: app.resolveModelOverridesForNonInteractive,
		BatchFaultWriter:      func() *appbatch.FaultEnvelopeWriter { return app.batchFaultWriter },
		SetBatchFaultOutput: func(f func(error)) {
			app.batchFaultOutputMu.Lock()
			app.batchFaultOutput = f
			app.batchFaultOutputMu.Unlock()
		},
		GetBatchFaultOutput: func() func(error) {
			app.batchFaultOutputMu.Lock()
			defer app.batchFaultOutputMu.Unlock()
			return app.batchFaultOutput
		},
		BatchCutoverWriter: app.batchCutoverWriter,
		BatchCutoverReader: app.batchCutoverReader,
		BatchFinishedWork:  app.batchFinishedWork,
	}
}

// ExecuteRun computes a single agent turn and returns the result envelope
// without rendering it (see RunMode for the output shapes that shape the
// streaming behaviour). Streaming output goes to req.Stdout, diagnostics to
// req.Stderr; nil falls back to io.Discard. For RunModeJSON the caller is
// responsible for encoding the returned *RunResult.
func (app *App) ExecuteRun(ctx context.Context, req RunRequest) (result *RunResult, runErr error) {
	return executeRun(app.runEnv(), ctx, req)
}

// RunNonInteractive runs a single agent turn and writes its result to
// `output`. See RunMode for the available output shapes. It is a thin
// wrapper over ExecuteRun: it supplies the process streams as defaults
// and renders the JSON envelope for RunModeJSON.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	_, err := runNonInteractiveWithAsyncResults(app.runEnv(), ctx, output, prompt, overrides, hideSpinner, mode, continueSessionID, useLast)
	return err
}

// RunNonInteractiveWithResult runs one agent turn, writes its output, and
// returns the structured result when one is available.
func (app *App) RunNonInteractiveWithResult(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) (*RunResult, error) {
	return runNonInteractiveWithAsyncResults(app.runEnv(), ctx, output, prompt, overrides, hideSpinner, mode, continueSessionID, useLast)
}

// ProcessHome reports whether this process owns its own data directory
// (WS-1, #1142 step C) -- the flag that decides whether legacy unbound (”)
// rows are drivable here. It is app.New's copy of the single source
// config.WorkspaceHome() (false only for a linked worktree on the shared data directory, SD-D #1143), NOT a derivation from the
// workspace root: a git checkout has a non-empty root and is still a home
// process today. A nil App (test literals) reads as home, the pre-WS-1 world.
func (app *App) ProcessHome() bool {
	return processHome(app)
}

// Permanent batch-CLI accessors: thin same-signature wrappers over the Sh5
// batch free functions (chunk 4 of lane 0b-a).
func (app *App) BatchCLIStore() (*appbatch.BatchCLIStore, error) {
	return appbatch.NewBatchCLIStore(app.batchCLIParams())
}

// BatchCLISpecWithQuestionRoute stamps the route chosen by trusted intake.
func (app *App) BatchCLISpecWithQuestionRoute(ctx context.Context, owner, messageID string, spec jobbatch.Spec, questionRoute bool) (jobbatch.Spec, func(), error) {
	return appbatch.BatchCLISpecWithQuestionRoute(app.batchCLIParams(), ctx, owner, messageID, spec, questionRoute)
}

// BatchSubmitRefWithQuestionRoute is BatchSubmit with a caller-chosen submit
// reference (see agent.BatchInstall.SubmitCLIRef) and the intake's answer-route
// decision.
func (app *App) BatchSubmitRefWithQuestionRoute(ctx context.Context, owner string, spec jobbatch.Spec, ref func(root string) string, questionRoute bool) (string, error) {
	return appbatch.BatchSubmitRefWithQuestionRoute(app.batchCLIParams(), ctx, owner, spec, ref, questionRoute)
}

// BatchSettleDebt settles the owner's notice debt for a terminal root without
// releasing the root, so its live output stays readable.
func (app *App) BatchSettleDebt(ctx context.Context, owner, root string) error {
	return appbatch.BatchSettleDebt(app.batchCLIParams(), ctx, owner, root)
}

func (app *App) BatchWait(ctx context.Context, owner, root string) (db.BatchNode, error) {
	return appbatch.BatchWait(app.batchCLIParams(), ctx, owner, root)
}
