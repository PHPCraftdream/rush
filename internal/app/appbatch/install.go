package appbatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/google/uuid"
)

// InstallParams is the explicit input of the Sh5 install port:
// InstallRuntime, ReleaseBatchRuntimeGate and their release seams.
type InstallParams struct {
	CLI                    BatchCLIParams
	HostEnsure             func(context.Context) (string, error)
	HostRelease            func(context.Context) error
	HostReleaseKeepLock    func()
	DrainResidual          func()
	HandleFaultOutput      func(error)
	ReleaseWriter          func() error
	FinalizationAuth       func() *agent.FinalizationAuthorityRegistry
	GetBatchHeldAnswerSink func() agent.BatchHeldAnswerSink
	SetBatchHeldAnswerSink func(agent.BatchHeldAnswerSink)
	SetBatchWorkIngress    func(agent.BatchWorkIngress)
	SetBatchModelControl   func(agent.BatchModelControl)
	SetBatchLiveReader     func(*session.BatchLiveReader)
	SetBatchRuntime        func(*BatchRuntime)
	SetBatchInstall        func(*agent.BatchInstall)
	RuntimeConfigHook      func(*BatchRuntimeConfig)
	InstallDepsHook        func(*agent.BatchInstallDeps)
	InstallWiringHook      func(agent.BatchInstallDeps)
}

// R11F (M2) batch-runtime installation: App.New builds the batch runtime
// via the M1 factory unconditionally for every persisted agent-setup App,
// and the shutdown path replaces the "release host identity and writer on
// agent-idle" sites with the R11E drain gate (BatchRuntime.DrainAndRelease).
// BatchDrainTimeout bounds the Runner drain inside an ordinary
// (graceful) App shutdown. A Close that outlives it makes the gate retain
// the still-draining host identity and writer and report Forced — the same
// verdict the agent CancelAll grace produces, and the same consequence:
// the caller must exit its process. 5s matches the per-cleanup shutdownCtx
// budget of releaseResources.
const BatchDrainTimeout = 5 * time.Second

// arms its fault watcher. It fails closed: any missing resource surfaces as
// a New error — the process never silently continues with a half-installed
// runtime. Caller must have ensured agent setup and a non-empty data dir.
func InstallRuntime(p InstallParams, ctx context.Context) error {
	if p.CLI.BatchRuntime() != nil {
		// First wins: one install, one authority per App. A second call
		// (only possible from tests/re-entry) never swaps the Runner.
		return nil
	}
	// R11F (M3): the commit-hook identity must exist BEFORE the first Claim,
	// so the install registers the host up front (HostID() is empty until
	// the lazy ensureHost registration). The identity is the job-independent
	// HostStore's.
	host, err := p.HostEnsure(ctx)
	if err != nil {
		return err
	}
	deps := agent.BatchInstallDeps{
		// R11F round-2 (P1-1) decision — delivery ownership: the install
		// carries NO fixed owner. Owner=host was an FK violation on the
		// first Submit (batch_nodes.owner_session_id REFERENCES
		// sessions(id); async_hosts rows are not sessions). The commit hook
		// resolves every Submit's owner from the INVOKING session carried
		// in the spec payload (BatchCommitIdentity.OwnerFor); deliveries
		// and elections fence by the invocation's session
		// (batchDeliveryImpl.owner); Host stays ONLY the fence identity.
		Host: host, RunnerID: uuid.NewString(),
		DiskBindings: agent.NewDiskProviderRegistry(),
	}
	// W1c: the flag-on registry must exist before the batch install so the
	// commit-hook identity carries the same lookup the finalization wiring
	// registers authorities into. Flag off: nil, byte-identical install.
	if registry := p.FinalizationAuth(); registry != nil {
		deps.FinalizationAuthorityFor = registry.Lookup
	}
	if p.InstallDepsHook != nil {
		p.InstallDepsHook(&deps)
	}
	factory, err := NewBatchRuntimeFactory(p, deps)
	if err != nil {
		return err
	}
	p.CLI.SetBatchTimeoutWake(NewTimeoutWakeDispatcher())
	// Ordinary completions ride the same bounded dispatcher: the commit
	// hook enqueues the touched root (nonblocking, off the Runner commit
	// stack); the dispatcher resolves the owner from the durable row and
	// hands it to the coordinator's existing wake gates. The dispatcher
	// is NOT a delivery authority — the durable selected notice stays
	// the only fact; the wake only schedules the reaction turn.
	p.CLI.BatchTimeoutWake().SetRootOwner(CompletionRootOwner(p.CLI))
	p.CLI.BatchTimeoutWake().setSkipOwner(apiServiceOwner(p.CLI))
	factory.wake = p.CLI.BatchTimeoutWake().Wake
	factory.Deps.CompletionWake = p.CLI.BatchTimeoutWake().WakeCompletion
	factory.Deps.QuestionWake = p.CLI.BatchTimeoutWake().Wake
	if p.InstallWiringHook != nil {
		p.InstallWiringHook(factory.Deps)
	}
	if factory.Deps.Profile == nil {
		profile, perr := agent.NewBatchProfileDefault()
		if perr != nil {
			if wake := p.CLI.BatchTimeoutWake(); wake != nil {
				wake.Stop()
				p.CLI.SetBatchTimeoutWake(nil)
			}
			return perr
		}
		factory.Deps.Profile = profile
	}
	if factory.Deps.Registries == nil {
		factory.Deps.Registries = agent.NewBatchLeafRegistryStore()
	}
	rt, install, err := factory.Build(ctx)
	if err != nil {
		if wake := p.CLI.BatchTimeoutWake(); wake != nil {
			wake.Stop()
			p.CLI.SetBatchTimeoutWake(nil)
		}
		return err
	}
	// R11F (M3): the exported one-authority ingress seam over the installed
	// runtime; InitCoderAgent binds it to the coordinator.
	ingress, err := agent.NewBatchWorkIngressWithDiskBindings(install.Runtime, factory.Deps.Registries, install.DiskBindings)
	if err != nil {
		if wake := p.CLI.BatchTimeoutWake(); wake != nil {
			wake.Stop()
			p.CLI.SetBatchTimeoutWake(nil)
		}
		return err
	}
	p.SetBatchWorkIngress(ingress)
	// R12-H2 P1-2/P1-4: bind the ingress into the runtime so batchLeafEnv
	// carries it — without this, provider-bound delegation fails closed.
	agent.BindRuntimeIngress(install.Runtime, p.CLI.BatchWorkIngress())
	p.SetBatchModelControl(agent.NewBatchModelControl(install.RunnerHandle, install.Delivery, factory.Deps.Store, factory.live, p.CLI.Sessions(), factory.Deps.Host, factory.Deps.RunnerID, factory.Deps.Profile, factory.Deps.Resolver, install.RootLeases))
	p.SetBatchLiveReader(factory.live)
	// Wire the R11E release seams onto the gate BEFORE the watcher starts:
	// RetainHostIdentity mirrors the forced path's CloseKeepLock,
	// ReleaseHostIdentity the graceful Close, and ReleaseWriter the
	// generation-safe per-reference DB release of releaseResources.
	// Host release goes through the HostStore -- CloseKeepLock pins the OS
	// lock, Close deletes via DeleteAsyncHostIfNoNodes, never async_jobs.
	rt.Config.BeforeExit = p.HandleFaultOutput
	rt.Config.RetainHostIdentity = p.HostReleaseKeepLock
	rt.Config.ReleaseHostIdentity = func(ctx context.Context) error {
		// R12-H2 §1.3: residual construction scopes close AFTER the
		// disposal drain and BEFORE host/writer release (the gate's
		// release order is host, then writer). Forced shutdown and the
		// fault fence never reach this seam, so the manager's remaining
		// owners are retained until process exit.
		p.DrainResidual()
		return p.HostRelease(ctx)
	}
	rt.Config.ReleaseWriter = func(ctx context.Context) error {
		return p.ReleaseWriter()
	}
	if p.RuntimeConfigHook != nil {
		p.RuntimeConfigHook(&rt.Config)
	}
	rt.Config.TimeoutCheckin = batchTimeoutWakeRuntime{checkin: rt.Config.TimeoutCheckin, dispatch: p.CLI.BatchTimeoutWake()}
	p.SetBatchRuntime(rt)
	p.SetBatchInstall(install)
	// Start arms the fault watcher (idempotent; SetRunner already ran inside
	// build — Start and SetRunner are order-independent).
	rt.Start()
	// Held-question inject bridge (D1): with a live Runner and writer,
	// install the sink inject_agent uses to release a child's
	// CURRENT-incarnation held question through Runner.Inject. A
	// runnerless install (Start logged the RunnerFactory failure) leaves
	// the sink nil and inject_agent keeps its legacy behaviour
	// byte-identically.
	if runner := install.Runner; runner != nil {
		sink, serr := agent.NewBatchHeldAnswerSink(CutoverWriter(p.CLI), runner)
		if serr != nil {
			slog.Warn("app: batch held-answer sink unavailable; inject_agent keeps the legacy path", "err", serr)
		} else {
			p.SetBatchHeldAnswerSink(sink)
		}
	}
	// H4 W2: arm the App-owned dead-host maintenance (startup pass + 60s
	// periodic pass) alongside the fault watcher. Covers CLI, server and
	// SDK hosts; stopped and joined by releaseResources.
	StartBatchMaintenance(p.CLI)
	slog.Info("app: batch runtime installed", "host", host)
	return nil
}

// DrainResidualConstructions closes residual construction scopes on the
// graceful path only: construction records whose only remaining owners are
// their Construction pins are dropped (R12-H2 §1.3) — the forced path never
// calls this, retaining the manager and its owners until process exit.
func DrainResidualConstructions(p BatchCLIParams) {
	if p.BatchInstall() != nil {
		p.BatchInstall().DrainDiskBindings()
	}
}

// ReleaseBatchRuntimeGate is the replacement for the "release host
// identity and writer on agent-idle" block of releaseResources: the Runner
// drains first, then host identity, then writer — all inside
// DrainAndRelease. It returns the (possibly promoted) stillBusy verdict.
//
//   - Graceful (stillBusy=false): drain bounded by BatchDrainTimeout;
//     a Close timeout or fault fence retains the draining resources and
//     promotes the shutdown to Forced — the DB block below then skips the
//     release exactly like the forced path.
//   - Forced (stillBusy=true): the watcher is disarmed, the host lock is
//     kept (CloseKeepLock) and the DB is never touched.
func ReleaseBatchRuntimeGate(p InstallParams, ctx context.Context, stillBusy bool, record func(error)) bool {
	// Disarm the held-question inject bridge first: once the drain begins no
	// answer may be routed into a draining Runner (the sink lookup is gone,
	// so a racing inject_agent takes the legacy path instead).
	// Close first: a sink already loaded by an in-flight inject_agent must
	// fail deterministically (jobbatch.ErrReleased-shaped) instead of
	// delivering into the draining Runner or silently degrading to the
	// mailbox with a half-delivered answer.
	if sink := p.GetBatchHeldAnswerSink(); sink != nil {
		sink.Close()
	}
	agent.SetCoordinatorBatchHeldAnswerSink(p.CLI.Coord(), nil)
	p.SetBatchHeldAnswerSink(nil)
	rt := p.CLI.BatchRuntime()
	if rt == nil {
		if wake := p.CLI.BatchTimeoutWake(); wake != nil {
			wake.Stop()
			p.CLI.SetBatchTimeoutWake(nil)
		}
		// Defensive: an App without an installed runtime must never take
		// this path; retain the host identity if it somehow does.
		p.HostReleaseKeepLock()
		return stillBusy
	}
	if stillBusy {
		rt.Stop()
		p.HostReleaseKeepLock()
		return true
	}
	rt.Stop()
	res := rt.DrainAndRelease(ctx, BatchDrainTimeout)
	if res.Err != nil {
		record(res.Err)
	}
	if res.Forced {
		slog.Warn("app: batch drain forced - host identity and writer retained; caller must exit the process")
		return true
	}
	return false
}

// R11F app factory (dormant): assembles the batch runtime from App-owned
// resources. Nothing here touches the database, starts a watcher or registers
// a session — Start arms the fault watcher, and no Runner input exists until
// a session invokes the ingress.

// defaultBatchCommitTimeout must outlast SQLite's own lock wait: a commit
// timeout is a runner fault that ends the process, so firing it while the
// write is still legitimately waiting on the database lock kills healthy
// runs whenever another writer holds the lock for a few seconds.
const defaultBatchCommitTimeout = db.BusyTimeout + 15*time.Second

// batchRuntimeFactory builds the R11E BatchRuntime plus the adapted R11C/D
// seams from App-owned resources: the host identity, the DB writer, the
// history service. Host, store and messages come from the App; the remaining
// install deps (profile, pinned registries, runner budgets, agent family
// seams) are supplied by the install wiring (M2/M3).
type BatchRuntimeFactory struct {
	Deps   agent.BatchInstallDeps
	live   *session.BatchLiveReader
	Writer *sql.DB
	wake   func(context.Context, string)
}

// newBatchRuntimeFactory resolves the App-owned resources. It fails closed
// when the App has no host store or message service (SkipAgentSetup, empty
// data directory): the runtime never installs half-wired.
func NewBatchRuntimeFactory(p InstallParams, deps agent.BatchInstallDeps) (*BatchRuntimeFactory, error) {
	if p.CLI.Owner == nil {
		return nil, errors.New("batch runtime factory: nil app")
	}
	if BatchHostStore(p.CLI) == nil {
		return nil, errors.New("batch runtime factory: no host store (agent setup skipped)")
	}
	if p.CLI.Messages() == nil {
		return nil, errors.New("batch runtime factory: no message service")
	}
	writer := p.CLI.DB()
	if writer == nil {
		return nil, errors.New("batch runtime factory: no db writer")
	}
	if deps.Host == "" {
		if hs := BatchHostStore(p.CLI); hs != nil {
			deps.Host = hs.HostID()
		}
	}
	deps.Store = session.NewBatchStoreWithReader(writer, CutoverReader(p.CLI, writer))
	deps.Messages = p.CLI.Messages()
	deps.Sessions = p.CLI.Sessions()
	// Package psv: restore legacy supervision arming at the batch started
	// boundary. The coordinator is assigned later by InitCoderAgent, so the
	// getter re-reads it per call; a not-yet-bound coordinator is a no-op.
	deps.ArmSupervision = agent.PsvBatchSupervisionArmingLate(func() agent.Coordinator { return p.CLI.Coord() })
	// R11F (M3): closed defaults so a bare install never builds half-wired.
	// The install keeps its own idempotent guards on top.
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
		// R11F round-2 (P1-5b): the install wires the R10a
		// store-backed live-descendant reader over the same writer pool the
		// batch store uses; a nil reader would report an incomplete read
		// forever (scope never closes).
		reader := session.NewBatchLiveReader(db.New(writer), p.CLI.DataDir())
		// TxStarter: every live read runs in ONE read-only tx on the writer.
		reader.TxStarter = writer
		// Package S (jb/psc): keep the delegation open through the child's
		// actionable/paced completion-reaction debt (CLIScope) and settle
		// with the child's refreshed final outcome (review 4 §4.3 items
		// 2-3). The coordinator is assigned later by InitCoderAgent, so
		// the facts resolve it lazily per read.
		deps.AgentDeps = agent.CoordinatorBatchAgentDepsWithChildScope(reader,
			agent.PscCoordinatorChildReactionFactsLate(func() agent.Coordinator { return p.CLI.Coord() }))
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
		deps.CommitTimeout = defaultBatchCommitTimeout
	}
	if deps.RecordRetention <= 0 {
		deps.RecordRetention = 256
	}
	live := session.NewBatchLiveReader(db.New(writer), p.CLI.DataDir())
	// TxStarter: every live read runs in ONE read-only tx on the writer.
	live.TxStarter = writer
	f := &BatchRuntimeFactory{Deps: deps, live: live, Writer: writer}
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
func (f *BatchRuntimeFactory) Build(ctx context.Context) (rt *BatchRuntime, install *agent.BatchInstall, err error) {
	install, err = agent.InstallBatchRuntimeDeferred(ctx, f.Deps)
	if err != nil {
		return nil, nil, fmt.Errorf("batch runtime factory: %w", err)
	}
	checkin := session.NewBatchTimeoutCheckin(f.Writer)
	if f.wake != nil {
		checkin.SetWakeFunc(f.wake)
	}
	rt = NewBatchRuntime(BatchRuntimeConfig{CommitHook: install.CommitHook, TimeoutCheckin: checkin})
	rt.CommitDiagnostics = BatchCommitDiagnostics{Writer: f.Writer, Baseline: f.Writer.Stats()}
	// R11F P1-6: Start constructs the Runner from the install's deps before
	// any Invoke can run (Start happens at New-time install; Invoke only
	// after the App is built).
	rt.Config.RunnerFactory = func() (BatchRunner, error) {
		return install.BuildRunner(context.Background())
	}
	return rt, install, nil
}
