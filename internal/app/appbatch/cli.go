package appbatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
)

// BatchCLIParams is the explicit capability set the batch free functions
// read from the composition root (Sh5 port). Every closure is resolved
// lazily by the App-side constructor at the same moment the old method
// body read the App field; a zero params (nil closures) keeps the
// configless-App behaviour through the nil guards in the bodies.
type BatchCLIParams struct {
	WorkspaceRoot       string
	Sessions            func() session.Service
	Messages            func() message.Service
	Coord               func() agent.Coordinator
	Perms               func() permission.Service
	DB                  func() *sql.DB
	DataDir             func() string
	ProcessHome         func() bool
	GetBatchFaultOutput func() func(error)
	DBConns             func() []*sql.DB
	ReadDB              func() *sql.DB
	DBReleasesNeeded    func() int
	ActivityConn        func() *db.Queries
	Owner               func() *Owner
	MaintenanceBounds   func() MaintenanceBounds
	BatchInstall        func() *agent.BatchInstall
	BatchRuntime        func() *BatchRuntime
	BatchWorkIngress    func() agent.BatchWorkIngress
	BatchTimeoutWake    func() *TimeoutWakeDispatcher
	SetBatchTimeoutWake func(*TimeoutWakeDispatcher)
}

// SessionExecutionProviderIntent records which disk provider a session's
// execution was admitted under (the session execution requirement fence).
type SessionExecutionProviderIntent uint8

const (
	SessionExecutionProviderOmitted SessionExecutionProviderIntent = iota
	SessionExecutionProviderCustom
	SessionExecutionProviderOS
)

type BatchCLIStore struct {
	Store  *session.BatchStore
	Reader *session.BatchLiveReader
}

func NewBatchCLIStore(p BatchCLIParams) (*BatchCLIStore, error) {
	if p.DB == nil {
		return nil, errors.New("batch store unavailable")
	}
	writer := p.DB()
	if conns := p.DBConns(); len(conns) > 0 {
		writer = conns[0]
	}
	if writer == nil {
		return nil, errors.New("batch store unavailable")
	}
	read := p.ReadDB()
	if read == nil {
		read = writer
	}
	return &BatchCLIStore{session.NewBatchStoreWithReader(writer, read), session.NewBatchLiveReader(db.New(read), p.DataDir())}, nil
}

func BatchOwnerRuntime(p BatchCLIParams) bool {
	return p.BatchRuntime != nil && p.BatchRuntime() != nil && p.BatchInstall() != nil && p.BatchInstall().Runner != nil
}

func DataDirectory(p BatchCLIParams) string {
	if p.DataDir == nil {
		return ""
	}
	return p.DataDir()
}

func BatchCLISession(p BatchCLIParams, ctx context.Context, id string) (session.Session, error) {
	if id == "" {
		return p.Sessions().Create(ctx, "rush batch")
	}
	if p.Sessions().IsAgentToolSession(id) {
		return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", id)
	}
	sess, err := p.Sessions().Get(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	sess, err = ValidateResolvedSession(sess, id, p.WorkspaceRoot, p.ProcessHome())
	if err != nil {
		return session.Session{}, err
	}
	if p.Coord() != nil && p.Coord().IsSessionBusy(sess.ID) {
		return session.Session{}, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
	}
	if p.DataDir() == "" {
		return sess, nil
	}
	// Another process (rush run, web) holds the session's OS lock while it
	// drives a turn; probe it and release at once: the batch's own leaves
	// may need the lock.
	lock, err := session.TryAcquireSessionLock(p.DataDir(), sess.ID)
	if err != nil {
		var busyErr *session.SessionLockBusyError
		if errors.As(err, &busyErr) {
			return session.Session{}, fmt.Errorf("session %q is already processing another request (held by PID %d): %w", sess.ID, busyErr.HolderPID, agent.ErrSessionBusy)
		}
		return session.Session{}, fmt.Errorf("check session %q lock: %w", sess.ID, err)
	}
	if err := lock.Release(); err != nil {
		return session.Session{}, fmt.Errorf("release session %q lock probe: %w", sess.ID, err)
	}
	return sess, nil
}

func BatchCLISpecWithQuestionRoute(p BatchCLIParams, ctx context.Context, owner, messageID string, spec jobbatch.Spec, questionRoute bool) (jobbatch.Spec, func(), error) {
	if !BatchOwnerRuntime(p) || p.Coord() == nil {
		return jobbatch.Spec{}, nil, errors.New("batch runtime unavailable")
	}
	admitted, err := agent.AdmitBatchRoleContracts(ctx, p.Coord(), spec)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	ctx = agent.WithAdmittedBatchRoleContracts(ctx)
	spec = admitted
	if err := PersistSessionExecutionIntent(ctx, p.Sessions(), owner, SessionExecutionProviderOmitted, nil, false); err != nil {
		return jobbatch.Spec{}, nil, err
	}
	requestedDepth, depthErr := agent.MaxAgentDepthFrom(ctx)
	if depthErr != nil {
		return jobbatch.Spec{}, nil, depthErr
	}
	remainingDepth, depthErr := agent.PersistSessionAgentDepth(ctx, p.Sessions(), owner, requestedDepth)
	if depthErr != nil {
		return jobbatch.Spec{}, nil, depthErr
	}
	ctx, err = agent.WithAgentDepth(ctx, remainingDepth)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	b, err := agent.NewBatchCLIBuilderWithQuestionRoute(p.Coord(), p.BatchWorkIngress(), questionRoute)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	return b.TranslateSpec(ctx, owner, messageID, spec)
}

func BatchAutoApproveOwner(p BatchCLIParams, owner string) {
	if p.Perms == nil || p.Perms() == nil {
		return
	}
	p.Perms().AutoApproveSession(owner)
}

func BatchSubmitRefWithQuestionRoute(p BatchCLIParams, ctx context.Context, owner string, spec jobbatch.Spec, ref func(root string) string, questionRoute bool) (string, error) {
	if !BatchOwnerRuntime(p) {
		return "", errors.New("batch submit requires owning runtime")
	}
	if spec.Batch == nil || spec.Task != nil {
		return "", errors.New("batch submit requires batch root")
	}
	translated, release, err := BatchCLISpecWithQuestionRoute(p, ctx, owner, uuid.NewString(), spec, questionRoute)
	if err != nil {
		return "", err
	}
	batch, err := p.BatchInstall().SubmitCLIRef(ctx, owner, translated, ref)
	if err != nil {
		release()
		return "", err
	}
	if err := p.BatchInstall().RetainCLIConstruction(batch.ID(), release); err != nil {
		release()
		return "", err
	}
	return string(batch.ID()), nil
}

func BatchNodeOutput(p BatchCLIParams, ctx context.Context, owner, root, node string) (string, error) {
	if p.BatchInstall() == nil {
		return "", errors.New("batch runtime unavailable")
	}
	if p.BatchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return "", errors.New("batch CLI: root is not owned by this session")
	}
	chunk, err := p.BatchInstall().CLIOutput(ctx, owner, jobbatch.ID(root), jobbatch.ID(node))
	if err != nil {
		return "", err
	}
	return string(chunk.Data), nil
}

func BatchAnswerHeld(p BatchCLIParams, ctx context.Context, owner, childSessionID, text string) error {
	answerer, ok := p.Coord().(agent.BatchHeldAnswerer)
	if !ok {
		return errors.New("batch runtime unavailable")
	}
	_, err := answerer.AnswerBatchHeldQuestion(ctx, owner, childSessionID, text)
	return err
}

func BatchSettleDebt(p BatchCLIParams, ctx context.Context, owner, root string) error {
	if p.BatchInstall() == nil || p.BatchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return errors.New("batch CLI: root is not owned by this session")
	}
	fac, err := NewBatchCLIStore(p)
	if err != nil {
		return err
	}
	return fac.Store.BatchSettleCLIWakeDebt(ctx, owner, root)
}

func BatchRelease(p BatchCLIParams, ctx context.Context, owner, root string) error {
	if p.BatchInstall() == nil || p.BatchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return errors.New("batch CLI: root is not owned by this session")
	}
	fac, err := NewBatchCLIStore(p)
	if err != nil {
		return err
	}
	if err = fac.Store.BatchSettleCLIWakeDebt(ctx, owner, root); err != nil {
		return err
	}
	return p.BatchInstall().ReleaseCLIAfterResult(jobbatch.ID(root))
}

func BatchResult(p BatchCLIParams, ctx context.Context, owner, root string) ([]db.BatchNode, map[string]string, error) {
	if p.BatchInstall() == nil {
		return nil, nil, errors.New("batch runtime unavailable")
	}
	if p.BatchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return nil, nil, errors.New("batch CLI: root is not owned by this session")
	}
	fac, err := NewBatchCLIStore(p)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := fac.Store.Read(ctx, owner, root)
	if err != nil {
		return nil, nil, err
	}
	outputs := make(map[string]string, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if node.Kind != "task" {
			continue
		}
		chunk, outputErr := p.BatchInstall().CLIOutput(ctx, owner, jobbatch.ID(root), jobbatch.ID(node.NodeID))
		if outputErr != nil {
			// Never started, or released and no longer cached: the outcome
			// still reports, without output.
			if errors.Is(outputErr, jobbatch.ErrNotStarted) || errors.Is(outputErr, agent.ErrBatchCLIOutputExpired) {
				continue
			}
			return nil, nil, fmt.Errorf("batch CLI output for %s: %w", node.NodeID, outputErr)
		}
		if len(chunk.Data) > 0 {
			outputs[node.NodeID] = string(chunk.Data)
		}
	}
	return snapshot.Nodes, outputs, nil
}

func BatchWait(p BatchCLIParams, ctx context.Context, owner, root string) (db.BatchNode, error) {
	if p.BatchInstall() == nil {
		return db.BatchNode{}, errors.New("batch runtime unavailable")
	}
	if p.BatchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return db.BatchNode{}, errors.New("batch CLI: root is not owned by this session")
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.BatchInstall().WaitCLI(waitCtx, jobbatch.ID(root)); done <- err }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return db.BatchNode{}, err
			}
			goto completed
		case <-ctx.Done():
			return db.BatchNode{}, ctx.Err()
		case <-ticker.C:
			if err := p.BatchInstall().PumpCLIControls(ctx, owner, jobbatch.ID(root)); err != nil {
				return db.BatchNode{}, err
			}
		}
	}
completed:
	fac, err := NewBatchCLIStore(p)
	if err != nil {
		return db.BatchNode{}, err
	}
	snapshot, err := fac.Store.Read(ctx, owner, root)
	if err != nil {
		return db.BatchNode{}, err
	}
	return snapshot.Root, nil
}

func BatchAdd(p BatchCLIParams, ctx context.Context, root, node string, items []jobbatch.Spec) error {
	if !BatchOwnerRuntime(p) {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	fac, err := NewBatchCLIStore(p)
	if err != nil {
		return err
	}
	rootIdentity, rootID, err := fac.Store.BatchCLIFind(ctx, root)
	if err != nil {
		return err
	}
	if rootID != rootIdentity.Root {
		return errors.New("first add argument must identify exact root")
	}
	nodeIdentity, nodeID, err := fac.Store.BatchCLIFind(ctx, node)
	if err != nil {
		return err
	}
	if nodeIdentity.Owner != rootIdentity.Owner || nodeIdentity.Root != rootIdentity.Root {
		return errors.New("node does not belong to selected root")
	}
	if hs := BatchHostStore(p); hs == nil || hs.HostID() != rootIdentity.Host {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	if p.BatchInstall().CLIOwner(jobbatch.ID(rootID)) != rootIdentity.Owner {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	// CLI owner-process add = the same unattended surface.
	b, err := agent.NewBatchCLIBuilderWithQuestionRoute(p.Coord(), p.BatchWorkIngress(), false)
	if err != nil {
		return err
	}
	owner := rootIdentity.Owner
	translated, release, err := b.TranslateItems(ctx, owner, uuid.NewString(), items)
	if err != nil {
		return err
	}
	if err := p.BatchInstall().AddCLI(ctx, jobbatch.ID(rootID), jobbatch.ID(nodeID), translated); err != nil {
		release()
		return err
	}
	if err := p.BatchInstall().RetainCLIConstruction(jobbatch.ID(rootID), release); err != nil {
		release()
		return err
	}
	return nil
}

func ValidateResolvedSession(sess session.Session, id, workspaceRoot string, home bool) (session.Session, error) {
	if sess.ParentSessionID != "" {
		return session.Session{}, fmt.Errorf("cannot continue a child session: %s", id)
	}
	if !session.Owns(sess.WorkspaceRoot, workspaceRoot, home) {
		return session.Session{}, agent.ForeignWorkspaceError(sess)
	}
	return sess, nil
}

// resolveSession resolves which session to use for a non-interactive run
func PersistSessionExecutionIntent(ctx context.Context, sessions session.Service, sessionID string, intent SessionExecutionProviderIntent, requirement *session.SessionExecutionRequirement, customFenceEnabled bool) error {
	reqStore, hasStore := sessions.(session.SessionExecutionRequirementStore)
	switch intent {
	case SessionExecutionProviderCustom:
		if !customFenceEnabled {
			return nil
		}
		if !hasStore {
			return fmt.Errorf("%w: session service cannot persist the session execution requirement", agent.ErrDiskProviderUnavailable)
		}
		if requirement == nil {
			return fmt.Errorf("%w: admitted provider run carries no execution requirement envelope", agent.ErrDiskProviderUnavailable)
		}
		if err := reqStore.StoreSessionExecutionRequirement(ctx, sessionID, *requirement); err != nil {
			return fmt.Errorf("%w: failed to persist the session execution requirement: %v", agent.ErrDiskProviderUnavailable, err)
		}
	case SessionExecutionProviderOS:
		if hasStore {
			if err := reqStore.DeleteSessionExecutionRequirement(ctx, sessionID); err != nil {
				return fmt.Errorf("%w: failed to supersede the session execution requirement: %v", agent.ErrDiskProviderUnavailable, err)
			}
		}
	case SessionExecutionProviderOmitted:
		if hasStore {
			fence, err := reqStore.LoadSessionExecutionRequirement(ctx, sessionID)
			if err != nil {
				return fmt.Errorf("%w: session execution requirement cannot be validated: %v", agent.ErrDiskProviderUnavailable, err)
			}
			if fence != nil {
				return fmt.Errorf("%w: session has a persistent custom-provider requirement", agent.ErrDiskProviderUnavailable)
			}
		}
	}
	return nil
}

// Package R11E — dormant host lifecycle scaffolding (decomposition row E,
// docs/research/2026-10-12-r11-ingress-decomposition.md). This file is
// documentation only; it holds no code and compiles to nothing.
//
// Old -> new mapping (all anchors re-verified at f7d1bf27):
//
//   app_lifecycle.go releaseResources (stillBusy path) releases the host
//   identity via asyncJobStore.CloseKeepLock/Close and the writer via
//   db.ReleaseConn/Release on agent-idle — which is NOT proof of a drained
//   Runner, because no Runner exists in the lifecycle yet. R11F replaces
//   those two release sites with a single call to
//   BatchRuntime.DrainAndRelease, the drain gate in app_batch_drain.go,
//   which drains the Runner first and only then releases.
//
//   app_lifecycle.go cancelAgentsBeforeRelease stays authoritative for agent
//   cancellation; the gate runs after it (R11F wires the order).
//
//   D4 fatal exit (docs/plans/2026-10-12-job-batches-rush-integration.md,
//   decision D4; decomposition row E): app_batch_fault.go owns the
//   process-wide exit-70 handler for every host kind (CLI `rush run`,
//   long-lived server, SDK host — an SDK host also exits the process, not
//   only returns an error). R11F installs the handler alongside Runner
//   construction; today nothing calls it from production paths.
//
// Ownership transfer plan for R11F (files/lines are anchors at f7d1bf27):
//
//   1. app.go New: construct BatchRuntime with the R11B commit hook
//      (session.NewBatchCommitHook) and release funcs bound to
//      app.asyncJobStore / app.dbConns; store it in a new App field
//      (e.g. app.batchRuntime). No behavior change.
//   2. app_lifecycle.go cancelAgentsBeforeRelease: after agent
//      cancellation, call app.batchRuntime.Start (installs the fault
//      watcher) — or start it at first Runner set. Start and SetRunner are
//      order-independent: Start may be called while dormant, and SetRunner
//      wakes the dormant watcher. SetRunner is FIRST WINS: the first
//      Runner attaches and any later SetRunner with a different Runner is
//      refused. Fenced() does not depend on the watcher having run — it
//      also observes the Runner's closed Faulted channel directly.
//   3. app_lifecycle.go releaseResources: replace the asyncJobStore
//      CloseKeepLock/Close block and the db release block with
//      app.batchRuntime.DrainAndRelease(ctx, drainTimeout); the gate
//      drains the Runner, then releases host identity then writer.
//   4. Runner construction site (R11F, coordinator/ingress install):
//      call BatchRuntime.SetRunner so the narrow BatchRunner interface
//      points at the live *jobbatch.Runner.
//
// Assumed engine contracts (verified against internal/jobbatch at
// f7d1bf27):
//
//   - Runner.Faulted() <-chan struct{} closes exactly once on the first
//     commit fault; FaultErr() returns the fault error thereafter
//     (runner_commit.go:26-31).
//   - Runner.Close(ctx) publishes closing intent exactly once, waits for
//     Final bounded by ctx, returns ErrCloseTimeout-wrapped
//     *CloseTimeoutError on timeout without fault, DisposeError for
//     dispose failures, or the fault error (runner_requests.go:350).
//   - On a fault the Runner contains all attempts and never publishes
//     failed transitions; the process is expected to exit (R4 fault
//     semantics, gate 3 of the decomposition doc).
//
// NOT-DONE (left for R11F or later packages):
//
//   - No production call sites: RESOLVED — R11F wired the runtime (app.go
//     InstallRuntime), and H4 W2 added the App-owned dead-host
//     maintenance pass (app_batch_maintenance.go) started at install and
//     joined in releaseResources.
//   - No Runner construction, no commit-hook installation (RESOLVED by
//     R11F; listed as non-goals here). The store schema ships as ONE
//     embedded migration in the store file rush_store_v1.db
//     (storepath.DataDirPath(dir, storepath.ActiveFilename)); opening the
//     data dir applies it automatically — no separate migration step, and
//     no async_jobs table.
//   - No per-host-kind wiring: CLI/server/SDK all share the same handler;
//     only the CLI subprocess is exercised end-to-end in tests because
//     server/SDK hosts only exist after installation (R11F).
//   - No integration with R9 recovery/probe: RESOLVED — H4 W2 wires the W1
//     bounded recovery primitives (BatchRecoverPass/RecoverHostsOnce) into
//     App startup/periodic maintenance, the turn preamble, runningWorkOpen,
//     CLIActiveWork and the batch reset entry; the gate's fault/timeout
//     retention semantics are unchanged.
//   - No metrics, no configurable exit code (70 is fixed by D4).
