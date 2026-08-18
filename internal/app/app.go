// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

// coordinatorAdapterImpl wraps agent.Coordinator to satisfy session.Coordinator,
// avoiding an import cycle (session → agent → session).
//
// Reads app.AgentCoordinator lazily, on every Run() call, rather than
// capturing it once at construction time.
//
// History (P0-1, found in the final @oh review of tasks #337-349): this
// adapter used to be constructed with the run queue pump BEFORE
// InitCoderAgent assigned app.AgentCoordinator, so an eager-capture struct
// permanently captured nil, silently disabling the pump for the entire
// process lifetime. The PRIMARY fix was reordering App.New so the pump is
// now only constructed and started AFTER InitCoderAgent succeeds (see the
// pump construction site near the end of New) — by the time this adapter
// is built, app.AgentCoordinator is already correctly assigned, so an
// eager capture would work correctly too.
//
// The lazy read here is kept anyway as defense-in-depth, not as the
// primary fix — verified directly: reverting ONLY this adapter to eager
// capture (while keeping the corrected pump-start ordering) does NOT
// reproduce the original bug. It costs nothing and removes any future
// ordering hazard if App.New's construction sequence changes again,
// without depending on that ordering being preserved by every future
// edit.
type coordinatorAdapterImpl struct {
	app *App
}

func (a *coordinatorAdapterImpl) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	coord := a.app.AgentCoordinator
	if coord == nil {
		return nil, fmt.Errorf("app: coordinator not ready yet")
	}

	// Convert session.SessionAgentCallData to agent.SessionAgentCall
	// This requires rebuilding the full Model from the serialized ModelCfg,
	// which only the coordinator can do (it has access to provider configs
	// and catwalk registry). Delegate to coordinator.RebuildSessionAgentCall.
	call, err := coord.RebuildSessionAgentCall(ctx, callData)
	if err != nil {
		return nil, fmt.Errorf("failed to rebuild session agent call: %w", err)
	}

	result, err := coord.RunSessionAgentCall(ctx, call)
	if result == nil && err == nil {
		// The session was already owned by a live, in-process turn: the
		// call was appended to that owner's mailbox queue rather than
		// executed by this call (agent.sessionAgent.Run returns (nil, nil)
		// for this case — see tryReserveSession). Report this distinctly
		// so the pump does not treat it as an ordinary success (which
		// would Ack/delete a durable row for work that has not actually
		// run) — see session.ErrCallQueuedNotExecuted's doc for the full
		// rationale, found by the third @oh review pass over #337-349.
		return nil, session.ErrCallQueuedNotExecuted
	}
	// Return as any - we only care about error handling, not the result
	var anyResult any
	if result != nil {
		anyResult = result
	}
	return &anyResult, err
}

type App struct {
	Sessions    session.Service
	Messages    message.Service
	History     history.Service
	Permissions permission.Service
	FileTracker filetracker.Service

	AgentCoordinator agent.Coordinator

	// RunQueuePump is the background pump for durable orphaned/detached calls (task #340).
	// It scans session_run_queue periodically and executes pending work.
	RunQueuePump *session.RunQueuePump

	config *config.ConfigStore

	// DB is the underlying SQLite connection. Exposed for queue and other
	// raw-SQL features that don't have their own sqlc-generated package.
	DB func() *sql.DB

	// dataDir is the path to .crush/ where the database lives. Stored here
	// so Shutdown() can call db.Release() with knowledge of whether shutdown
	// was graceful or forced.
	dataDir string

	// dbReleasesNeeded tracks how many db.Release() calls to make on shutdown.
	// One for each Connect/ConnectRead call during app startup.
	dbReleasesNeeded int

	// global context and cleanup functions
	globalCtx          context.Context
	cleanupFuncs       []func(context.Context) error
	agentNotifications *pubsub.Broker[notify.Notification]
	events             *pubsub.Broker[any]

	// recoveryOrphanAge — internal test seam for recoverInterruptedTurns.
	// nil = use the production default (30s). Tests set it to 0 so they
	// don't have to sleep waiting for fresh messages to "age out" before
	// recovery considers them orphans.
	recoveryOrphanAge *time.Duration

	// recoveryDataDir — internal test seam for recoverInterruptedTurns'
	// cross-process liveness probe (task #287). nil = resolve from the
	// app's own config, as production does. Tests point it at a temp dir
	// so they can seed a real session lock and drive the live-holder
	// branch without standing up a full config.
	recoveryDataDir *string
}

// New initializes a new application instance.
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore) (*App, error) {
	q := db.New(conn)
	cfg := store.Config()

	// Open a separate read-only pool alongside the writer connection so the
	// heaviest standalone read paths (call-tree CTEs behind `sessions
	// list`/`why`/`watch`, transcript pagination, `sessions grep`) run
	// concurrently with the single writer connection instead of queuing
	// behind it and behind each other. See internal/db/connect.go's
	// ConnectRead doc and session/message's NewServiceWithReader doc for the
	// full rationale.
	//
	// This is additive to db.Connect, which the caller already invoked to
	// obtain conn — ConnectRead shares that same pool entry's refCount, so
	// the existing single db.Release(dataDir) call in the cleanup func
	// below must become two (one per Connect/ConnectRead call this process
	// made) to avoid leaking the reader's reference. If the reader fails to
	// open for any reason, we degrade to today's single-connection
	// behavior (qRead/readDB alias the writer) rather than failing
	// startup over a purely load-shedding optimization.
	var qRead *db.Queries
	var readConn *sql.DB
	dataDir := cfg.Options.DataDirectory
	if dataDir != "" {
		rc, err := db.ConnectRead(ctx, dataDir)
		if err != nil {
			slog.Warn("Failed to open read-only DB pool, reads will serialize with writes", "error", err)
		} else {
			readConn = rc
			qRead = db.New(rc)
		}
	}

	sessions := session.NewServiceWithReader(q, conn, qRead, readConn)
	messages := message.NewServiceWithReader(q, qRead)
	files := history.NewService(q, conn)
	skipPermissionsRequests := store.Overrides().SkipPermissionRequests
	var allowedTools []string
	if cfg.Permissions != nil && cfg.Permissions.AllowedTools != nil {
		allowedTools = cfg.Permissions.AllowedTools
	}

	app := &App{
		Sessions:    sessions,
		Messages:    messages,
		History:     files,
		Permissions: permission.NewPermissionService(ctx, store.WorkingDir(), skipPermissionsRequests, allowedTools, q),
		FileTracker: filetracker.NewService(q),

		DB:      func() *sql.DB { return conn },
		dataDir: dataDir,

		globalCtx: ctx,

		config:             store,
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		events:             pubsub.NewBroker[any](),
	}

	// NOTE: the restricted-run allowlist is deliberately NOT armed here.
	// app.New builds the App shared by BOTH `crush run` and the
	// interactive web/TUI server, and the gate would then leak into
	// interactive sessions — an auto-approved sub-agent (e.g.
	// agentic_fetch) would be denied-by-default even though interactive
	// mode must stay exempt. RunNonInteractive arms the gate itself from
	// config + CLI overrides on every run, so the run path is unaffected.
	// Fork patch (run allowlist).

	// Check for updates in the background.
	go app.checkForUpdates(ctx)

	// Startup recovery: any assistant message left without a finish part
	// from a previous run is treated as an interrupted turn — we add a
	// FinishReasonError to it so the UI/non-interactive callers don't see
	// it as still in-flight. Backs the "Codec must surface control"
	// invariant: even when the previous process died ungracefully (kill,
	// power loss, panic) we release the session on next startup. See
	// the 162-promise-all post-mortem in CHANGELOG.fork.md section 4.D.
	app.recoverInterruptedTurns(ctx)

	go mcp.Initialize(ctx, app.Permissions, store)

	// Release the shared database connection(s) on shutdown. The pool
	// closes the underlying *sql.DB when the last reference is released.
	// One Release call is needed per Connect/ConnectRead call this process
	// made against dataDir: the caller already did one Connect (for conn)
	// before calling New, and New itself did one ConnectRead above (when
	// dataDir != "" and it succeeded) — so we release twice, matching the
	// two increments, or once if the reader was never opened (dataDir=="").
	//
	// NOTE: DB cleanup is now handled directly in Shutdown() based on
	// whether shutdown was graceful or forced, NOT via cleanupFuncs.
	// This avoids the race where a timeout-abandoned Close() goroutine
	// continues running after the process has exited.
	releases := 1
	if readConn != nil {
		releases++
	}
	app.dbReleasesNeeded = releases
	// Run queue pump stop is now handled in Shutdown() synchronously (after CancelAll)
	// to capture the stillBusy return value, not in cleanupFuncs.
	app.cleanupFuncs = append(
		app.cleanupFuncs,
		func(ctx context.Context) error { return mcp.Close(ctx) },
	)

	// TODO: remove the concept of agent config, most likely.
	if !cfg.IsConfigured() {
		slog.Warn("No agent configuration found")
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
	}

	// Start the run queue pump (task #340) only AFTER InitCoderAgent has
	// assigned app.AgentCoordinator. This pump ensures that once a call is
	// enqueued as durable (including by a PREVIOUS process — the queue
	// survives restarts, which is the entire point of task #340), it will
	// eventually be executed. The pump lives for the lifetime of the
	// process and is stopped during Shutdown() (see the cleanupFuncs
	// registration above, which is nil-safe and already covers this).
	//
	// This ordering (not just coordinatorAdapterImpl's lazy field read) is
	// itself required, not merely nice-to-have: starting the pump earlier
	// — even with a lazy-reading adapter — raced a fresh AgentCoordinator
	// assignment against a concurrent pump goroutine reading the same
	// unsynchronized interface field the moment a restart-recovered queue
	// already had pending work (found in the final @oh review of
	// #337-349's own fix commit for P0-1 — a genuine data race per Go's
	// memory model on a plain struct field, not just "would eventually get
	// a working value"). Starting the pump only after InitCoderAgent's
	// synchronous assignment has already happened on this same goroutine
	// removes the race entirely, and also removes the separate failure
	// mode where a pump with a permanently-nil coordinator (config never
	// configured, or InitCoderAgent failing) would otherwise treat
	// "coordinator not ready" as an ordinary retryable failure and
	// eventually dead-letter (delete) durably-accepted work it was never
	// going to get a chance to run anyway.
	if dataDir != "" {
		app.RunQueuePump = session.NewRunQueuePump(session.RunQueuePumpConfig{
			Sessions:    app.Sessions,
			Coordinator: &coordinatorAdapterImpl{app: app},
		})
		app.RunQueuePump.Start()
		slog.Info("app: started run queue pump", "data_dir", dataDir)
	}

	return app, nil
}
