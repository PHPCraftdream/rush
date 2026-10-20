// R11F app factory (dormant): assembles the batch runtime from App-owned
// resources. Nothing here touches the database, starts a watcher or registers
// a session — Start arms the fault watcher, and no Runner input exists until
// a session invokes the ingress.

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchRuntimeFactory builds the R11E BatchRuntime plus the adapted R11C/D
// seams from App-owned resources: the host identity, the DB writer, the
// history service. Host, store and messages come from the App; the remaining
// install deps (profile, pinned registries, runner budgets, agent family
// seams) are supplied by the flagged-install wiring (M2/M3).
type batchRuntimeFactory struct {
	deps   agent.BatchInstallDeps
	live   *session.BatchLiveReader
	writer *sql.DB
	wake   func(context.Context, string)
}

// newBatchRuntimeFactory resolves the App-owned resources. It fails closed
// when the App has no agent store or message service (SkipAgentSetup, empty
// data directory): the runtime never installs half-wired.
func newBatchRuntimeFactory(a *App, deps agent.BatchInstallDeps) (*batchRuntimeFactory, error) {
	if a == nil {
		return nil, errors.New("batch runtime factory: nil app")
	}
	if a.asyncJobStore == nil && !(batchCutoverOn() && a.batchHostStore() != nil) {
		return nil, errors.New("batch runtime factory: no agent store (agent setup skipped)")
	}
	if a.Messages == nil {
		return nil, errors.New("batch runtime factory: no message service")
	}
	writer := a.DB()
	if writer == nil {
		return nil, errors.New("batch runtime factory: no db writer")
	}
	if deps.Host == "" {
		if batchCutoverOn() {
			// R12-F: the flag-ON identity is the HostStore's.
			if hs := a.batchHostStore(); hs != nil {
				deps.Host = hs.HostID()
			}
		} else {
			deps.Host = a.asyncJobStore.HostID()
		}
	}
	deps.Store = session.NewBatchStore(writer)
	deps.Messages = a.Messages
	deps.Sessions = a.Sessions
	// R11F (M3): closed defaults so a bare install never builds half-wired.
	// Flag-off Apps never reach the factory; the flag-on install keeps its
	// own idempotent guards on top.
	if deps.Profile == nil {
		profile, err := agent.NewBatchProfileDefault()
		if err != nil {
			return nil, err
		}
		deps.Profile = profile
	}
	if deps.Registries == nil {
		deps.Registries = agent.NewBatchLeafRegistryStore()
	}
	if deps.AgentDeps.Runs == nil {
		// R11F round-2 (P1-5b): the flagged install wires the R10a
		// store-backed live-descendant reader over the same writer pool the
		// batch store uses; a nil reader would report an incomplete read
		// forever (scope never closes).
		reader := session.NewBatchLiveReader(db.New(writer), a.dataDir)
		// TxStarter: every live read runs in ONE read-only tx on the writer.
		reader.TxStarter = writer
		// Package S (jb/psc): keep the delegation open through the child's
		// actionable/paced completion-reaction debt (CLIScope) and settle
		// with the child's refreshed final outcome (review 4 §4.3 items
		// 2-3). The coordinator is assigned later by InitCoderAgent, so
		// the facts resolve it lazily per read.
		deps.AgentDeps = agent.CoordinatorBatchAgentDepsWithChildScope(reader,
			agent.PscCoordinatorChildReactionFactsLate(func() agent.Coordinator { return a.AgentCoordinator }))
	}
	if deps.Resolver == nil {
		deps.Resolver = agent.BatchNonReentrantMCPResolver()
	}
	// Engine defaults: jobbatch.NewRunner fails closed on zero budgets, so a
	// bare install gets the documented one-item direct-call shape (mirrors
	// the R11A admission defaults) and ordinary control/commit budgets.
	if deps.Limits.MaxConcurrent <= 0 {
		deps.Limits = jobbatch.Limits{MaxConcurrent: 16, MaxDepth: 8, MaxNodes: 256}
	}
	if deps.ControlTimeout <= 0 {
		deps.ControlTimeout = 60 * time.Second
	}
	if deps.CommitTimeout <= 0 {
		deps.CommitTimeout = 5 * time.Second
	}
	if deps.RecordRetention <= 0 {
		deps.RecordRetention = 256
	}
	live := session.NewBatchLiveReader(db.New(writer), a.dataDir)
	// TxStarter: every live read runs in ONE read-only tx on the writer.
	live.TxStarter = writer
	f := &batchRuntimeFactory{deps: deps, live: live, writer: writer}
	return f, nil
}

// build constructs the adapted install and a new DORMANT BatchRuntime
// without a Runner (R11F P1-6): the Runner is constructed by
// BatchRuntime.Start through the wired RunnerFactory, so build has NO side
// effects at all — no engine goroutines, no store row, no notice pull, and
// the fault watcher stays disarmed until Start. The install's ingress stays
// fail-closed until Start constructs the Runner (the runner handle adapter
// refuses Submit until then). Runner construction is idempotent per install
// (BuildRunner first-wins); a faulted or closed App must not call build.
func (f *batchRuntimeFactory) build(ctx context.Context) (rt *BatchRuntime, install *agent.BatchInstall, err error) {
	install, err = agent.InstallBatchRuntimeDeferred(ctx, f.deps)
	if err != nil {
		return nil, nil, fmt.Errorf("batch runtime factory: %w", err)
	}
	if batchCutoverOn() {
		checkin := session.NewBatchTimeoutCheckin(f.writer)
		if f.wake != nil {
			checkin.SetWakeFunc(f.wake)
		}
		rt = NewBatchRuntime(BatchRuntimeConfig{CommitHook: install.CommitHook, TimeoutCheckin: checkin})
	} else {
		rt = NewBatchRuntime(BatchRuntimeConfig{CommitHook: install.CommitHook})
	}
	// R11F P1-6: Start constructs the Runner from the install's deps before
	// any Invoke can run (Start happens at New-time install; Invoke only
	// after the App is built).
	rt.cfg.RunnerFactory = func() (batchRunner, error) {
		return install.BuildRunner(context.Background())
	}
	return rt, install, nil
}
