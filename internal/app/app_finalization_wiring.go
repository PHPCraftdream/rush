// Agent finalization wiring (W1a): builds the finalization driver wiring from
// this App's configuration and database and binds it to the agent
// coordinator. Everything is flag-gated: with options.finalization.enabled
// off this is a no-op that neither allocates, nor opens connections, nor
// touches the terminal gate.

package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// installAgentFinalizationWiring wires the finalization driver when the flag
// is on. It runs once from InitCoderAgent, after the batch runtime install,
// so the batch install and the notice store are already available. Flag off:
// immediate nil, zero side effects. Flag on: any missing collaborator fails
// closed and aborts agent setup.
func (app *App) installAgentFinalizationWiring() error {
	if app.config == nil || app.AgentCoordinator == nil {
		return nil
	}
	if !app.Config().Options.AgentFinalizationEnabled() {
		return nil
	}
	if app.DB == nil {
		return errors.New("finalization wiring: database accessor is unavailable")
	}
	conn := app.DB()
	if conn == nil {
		return errors.New("finalization wiring: database is unavailable")
	}
	noticeStore := app.batchNoticeStore()
	if noticeStore == nil {
		return errors.New("finalization wiring: session notice store is unavailable")
	}
	return agent.SetCoordinatorFinalizationWiring(app.AgentCoordinator, agent.FinalizationWiringDeps{
		Enabled:     true,
		Conn:        conn,
		Messages:    app.Messages,
		NoticeStore: noticeStore,
		DataDir:     app.dataDir,
		Install:     app.batchInstall,
		// W1c: the same registry the batch install put into the commit
		// identity; the root bind registers each bound authority here.
		Authorities: app.finalizationAuthorityRegistry(),
		ResultGate:  app.FinalizationTerminalGate(),
	})
}

// appFinalizationGateEntry caches one App's terminal gate behind a sync.Once
// so repeated subscription wiring shares one gate without re-reading the
// config or rebuilding the store. Entries are keyed per App because tests
// assemble many App values in one process.
type appFinalizationGateEntry struct {
	once sync.Once
	gate session.FinalizationTerminalGate
}

// appFinalizationGates holds the per-App gate cache entries.
var appFinalizationGates sync.Map // *App -> *appFinalizationGateEntry

// FinalizationTerminalGate returns the durable terminal gate used to delay
// terminal deliveries (CLI final output, WS session events, SSE run events)
// until the owner's finalization record is sealed. With
// options.finalization.enabled off — or without a usable config or database
// writer — it returns nil: every consumer treats nil as "no gate" and keeps
// the legacy byte-for-byte behavior. With the flag on it polls the App's
// agent finalization store; the poll interval 0 selects the gate's built-in
// 500ms default, because the supervision interval is a minutes-scale silence
// window, far too coarse for a terminal-delivery poll. The result is cached
// per App; no goroutines are created.
func (a *App) FinalizationTerminalGate() session.FinalizationTerminalGate {
	if a == nil {
		return nil
	}
	entryAny, _ := appFinalizationGates.LoadOrStore(a, &appFinalizationGateEntry{})
	entry := entryAny.(*appFinalizationGateEntry)
	entry.once.Do(func() { entry.gate = a.newFinalizationTerminalGate() })
	return entry.gate
}

// newFinalizationTerminalGate builds the gate once per App: nil unless the
// finalization flag is on and a database writer is available.
func (a *App) newFinalizationTerminalGate() session.FinalizationTerminalGate {
	if a.config == nil {
		return nil
	}
	cfg := a.Config()
	if cfg == nil || !cfg.Options.AgentFinalizationEnabled() {
		return nil
	}
	if a.DB == nil {
		return nil
	}
	conn := a.DB()
	if conn == nil {
		return nil
	}
	return session.NewPollingFinalizationTerminalGate(session.NewAgentFinalizationStore(conn), 0)
}

func runMarkedFinalizationQueue(ctx context.Context, coordinator agent.Coordinator, data session.SessionAgentCallData) (bool, *any, error) {
	if data.FinalizationRequirement == nil {
		return false, nil, nil
	}
	combined, ok := coordinator.(agent.FinalizationQueueCoordinator)
	if !ok {
		return false, nil, nil
	}
	result, err := combined.RunFinalizationQueuedCall(ctx, runq.RunQueueRowIDFrom(ctx), data)
	if result == nil && err == nil {
		return true, nil, session.ErrCallQueuedNotExecuted
	}
	var value any
	if result != nil {
		value = result
	}
	return true, &value, err
}

// finalizationAuthorityRegistry returns the App's single owner-to-authority
// registry when the finalization flag is on, building it on first use. The
// batch install runs before the agent wiring, so the instance the commit
// hook sees is the same one the wiring binds authorities into. A disabled
// flag (or a config-less App) returns nil and allocates nothing.
func (app *App) finalizationAuthorityRegistry() *agent.FinalizationAuthorityRegistry {
	if app.finalizationAuthorities != nil {
		return app.finalizationAuthorities
	}
	if app.config == nil || !app.Config().Options.AgentFinalizationEnabled() {
		return nil
	}
	registry := agent.NewFinalizationAuthorityRegistry()
	app.finalizationAuthorities = registry
	return registry
}

// turnStallCallTimeout resolves the call's turn-stall threshold: the
// configured Options.TurnStallTimeoutSeconds, or the 30-minute default when
// unset (0).
func turnStallCallTimeout(opts *config.Options) time.Duration {
	if opts == nil || opts.TurnStallTimeoutSeconds <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(opts.TurnStallTimeoutSeconds) * time.Second
}

// applyTurnStallCallTimeout pins the resolved threshold onto CallOptions.
func applyTurnStallCallTimeout(call *agent.CallOptions, opts *config.Options) {
	call.TurnStallTimeout = turnStallCallTimeout(opts)
}

// Ephemeral (dataDir == "") library-mode Apps run with the batch runtime
// always installed (since H4), but they have no batchInstall.DiskBindings,
// so a custom-DiskProvider run has nothing to bind against. These two
// helpers give such an App its own App-owned registry, without any
// filesystem-backed host infra.
//
// resolveDiskBindings returns the DiskProviderRegistry a custom-DiskProvider
// run binds its per-call Call owner against (R12-H2
// §1.2, see app_run_setup.go's BindCallDiskProvider site). A persisted App
// (dataDir != "") uses the batch runtime install's registry, exactly as
// before this seam existed; an ephemeral library-mode App (dataDir == "")
// falls back to the in-memory registry built in New. A nil return means no
// registry is available and the caller must keep failing closed with
// agent.ErrDiskProviderUnavailable.
func (app *App) resolveDiskBindings() *agent.DiskProviderRegistry {
	if app.batchInstall != nil && app.batchInstall.DiskBindings != nil {
		return app.batchInstall.DiskBindings
	}
	return app.ephemeralDiskBindings
}

// newEphemeralDiskBindings builds the ephemeral App's binding registry.
// DiskProviderRegistry (internal/agent/disk_provider_capability.go) is a
// purely process-local in-memory capability manager: it keeps no hosts/
// directory, takes no lock files and touches no database, so constructing
// one for an ephemeral (dataDir == "") App adds no filesystem-backed host
// infra. It has no Close method (GC-managed); it is closed implicitly with
// the App itself.
func newEphemeralDiskBindings() *agent.DiskProviderRegistry {
	return agent.NewDiskProviderRegistry()
}
