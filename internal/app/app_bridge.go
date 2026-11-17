// Temporary bridge for the 0b-a seams of internal/app; deleted by design lane
// D2-4 (docs/plans/2026-11-15-sprint0-split-app.md section 5).

package app

import (
	"context"
	"database/sql"
	"io"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app/appbatch"
	"github.com/PHPCraftdream/rush/internal/app/apprecovery"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
)

// Maintenance limits retain their original package-private locations. The
// appbatch provider closes over these values and reads them on demand.
var (
	batchMaintenanceInterval   = 60 * time.Second
	batchMaintenancePassBudget = 1 * time.Second
	batchMaintenanceMaxRoots   = 64
	batchMaintenancePageSize   = 64
)

// recoverSessionListSeam / recoverSessionPreWriteSeam are test-only seams of
// the startup recovery sweep (moved to apprecovery in lane D2-2); they are
// threaded into apprecovery.RecoverParams by recoverInterruptedTurns so
// package-app white tests keep setting them. Nil in production.
var recoverSessionListSeam func()

// recoverSessionPreWriteSeam is the blocking pre-write seam of the recovery
// sweep (task #777); see apprecovery.RecoverParams.PreWriteSeam. Nil in
// production.
var recoverSessionPreWriteSeam func()

// recoverInterruptedTurns delegates the startup recovery sweep to
// apprecovery.RecoverInterruptedTurns. Called from New (app.go) after the
// mcpOwner setup and before InitCoderAgent; the call position must not move.
// Closures resolve App fields at call time, matching the old direct reads
// (nil-field meaning preserved for bare &App{}).
func (app *App) recoverInterruptedTurns(ctx context.Context) {
	apprecovery.RecoverInterruptedTurns(ctx, apprecovery.RecoverParams{
		Messages:        app.Messages,
		Config:          app.config,
		OrphanAge:       app.recoveryOrphanAge,
		DataDirOverride: app.recoveryDataDir,
		ListSeam:        recoverSessionListSeam,
		PreWriteSeam:    recoverSessionPreWriteSeam,
	})
}

// Thin same-name private wrappers over the Sh2 run-cluster free functions,
// kept so cliLoop/executeRunLoop call sites and tests stay unchanged.
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (coresession.Session, error) {
	return resolveSession(app.runEnv(), ctx, continueSessionID, useLast)
}

func (app *App) resolveRunRole(overrides *RunOverrides) error {
	return resolveRunRole(app.runEnv(), overrides)
}

func (app *App) drainPendingBeforeRun(ctx context.Context, sessionID string) error {
	return drainPendingBeforeRun(app.runEnv(), ctx, sessionID)
}

func (app *App) waitLocalOwnerReleased(ctx context.Context, sessionID string) error {
	return waitLocalOwnerReleased(app.runEnv(), ctx, sessionID)
}

func (app *App) disableSubAgentToolsInConfig() {
	disableSubAgentToolsInConfig(app.runEnv())
}

func (app *App) disableToolsInConfig(toolNames []string) {
	disableToolsInConfig(app.runEnv(), toolNames)
}

func (app *App) captureReviewBasis(ctx context.Context, role config.SelectedModelType, prompt string, start time.Time) *reviewBasis {
	return captureReviewBasis(app.runEnv(), ctx, role, prompt, start)
}

func (app *App) reviewerTurnPrompt(ctx context.Context, sessionID string, basis *reviewBasis) string {
	return reviewerTurnPrompt(app.runEnv(), ctx, sessionID, basis)
}

func (app *App) reviewerPassBlocked(ctx context.Context, source agent.ReactionDebtSource, sessionID string, stderr io.Writer) bool {
	return reviewerPassBlocked(app.runEnv(), ctx, source, sessionID, stderr)
}

func (app *App) attachReview(ctx context.Context, final *RunResult, sessionID, reviewText string, stderr io.Writer) {
	attachReview(app.runEnv(), ctx, final, sessionID, reviewText, stderr)
}

func (app *App) reconcileTerminalMessage(
	ctx context.Context,
	sessionID string,
	baselineIDs map[string]struct{},
	baselineKnown bool,
	runStart time.Time,
	ownID string,
	ownIDs map[string]struct{},
) (terminalReconciliation, error) {
	return reconcileTerminalMessage(app.runEnv(), ctx, sessionID, baselineIDs, baselineKnown, runStart, ownID, ownIDs)
}

func (app *App) collectSubAgentOutputs(ctx context.Context, parentSessionID string) []SubAgentOutput {
	return collectSubAgentOutputs(app.runEnv(), ctx, parentSessionID)
}

func (app *App) subAgentSummaryStats(ctx context.Context, parentSessionID string) (count int, totalChars int) {
	return subAgentSummaryStats(app.runEnv(), ctx, parentSessionID)
}

func (app *App) prepareExecuteRun(ctx context.Context, req RunRequest) (_ context.Context, cancel context.CancelFunc, setup *runExecSetup, err error) {
	return prepareExecuteRun(app.runEnv(), ctx, req)
}

// batchCLIParams snapshots the batch capability set at wrapper entry (the
// Sh5 port input, appbatch.BatchCLIParams). Every closure resolves its App
// field lazily at the same moment the old method body read it. A nil App
// yields a zero params (nil closures); the appbatch bodies keep the
// nil-App behaviour through their own guards.
func (app *App) batchCLIParams() appbatch.BatchCLIParams {
	if app == nil {
		return appbatch.BatchCLIParams{}
	}
	env := app.runEnv()
	return appbatch.BatchCLIParams{
		WorkspaceRoot:       env.WorkspaceRoot,
		Sessions:            env.Sessions,
		Messages:            env.Messages,
		Coord:               env.Coord,
		Perms:               env.Perms,
		DB:                  env.DB,
		DataDir:             env.DataDir,
		ProcessHome:         env.ProcessHome,
		GetBatchFaultOutput: env.GetBatchFaultOutput,
		DBConns:             func() []*sql.DB { return app.dbConns },
		ReadDB:              func() *sql.DB { return app.readDB },
		DBReleasesNeeded:    func() int { return app.dbReleasesNeeded },
		ActivityConn:        app.activityConn,
		Owner:               app.batchOwnerHandle,
		MaintenanceBounds: func() appbatch.MaintenanceBounds {
			return appbatch.MaintenanceBounds{
				Interval:   batchMaintenanceInterval,
				PassBudget: batchMaintenancePassBudget,
				MaxRoots:   batchMaintenanceMaxRoots,
				PageSize:   batchMaintenancePageSize,
			}
		},
		BatchInstall:        func() *agent.BatchInstall { return app.batchInstall },
		BatchRuntime:        func() *appbatch.BatchRuntime { return app.batchRuntime },
		BatchWorkIngress:    func() agent.BatchWorkIngress { return app.batchWorkIngress },
		BatchTimeoutWake:    func() *appbatch.TimeoutWakeDispatcher { return app.batchTimeoutWake },
		SetBatchTimeoutWake: func(d *appbatch.TimeoutWakeDispatcher) { app.batchTimeoutWake = d },
	}
}

// batchOwnerHandle returns this App's registry key, creating it on first
// use. Safe for concurrent callers and for bare App literals; a nil App
// yields a nil key, as the nil *App key did before.
func (app *App) batchOwnerHandle() *appbatch.Owner {
	if app == nil {
		return nil
	}
	if o := app.batchOwner.Load(); o != nil {
		return o
	}
	app.batchOwner.CompareAndSwap(nil, &appbatch.Owner{})
	return app.batchOwner.Load()
}

// batchInstallParams is the explicit input of the Sh5 install port
// (appbatch.InstallParams): InstallRuntime, ReleaseBatchRuntimeGate and
// their release seams.
func (app *App) batchInstallParams() appbatch.InstallParams {
	if app == nil {
		return appbatch.InstallParams{}
	}
	cli := app.batchCLIParams()
	return appbatch.InstallParams{
		CLI:                    cli,
		HostEnsure:             func(ctx context.Context) (string, error) { return appbatch.BatchHostEnsure(cli, ctx) },
		HostRelease:            func(ctx context.Context) error { return appbatch.BatchHostRelease(cli, ctx) },
		HostReleaseKeepLock:    func() { appbatch.BatchHostReleaseKeepLock(cli) },
		DrainResidual:          func() { appbatch.DrainResidualConstructions(cli) },
		HandleFaultOutput:      func(fault error) { appbatch.HandleBatchFaultOutput(cli, fault) },
		ReleaseWriter:          func() error { return releaseBatchWriterParams(cli) },
		FinalizationAuth:       app.finalizationAuthorityRegistry,
		GetBatchHeldAnswerSink: func() agent.BatchHeldAnswerSink { return app.batchHeldAnswerSink },
		SetBatchHeldAnswerSink: func(sink agent.BatchHeldAnswerSink) { app.batchHeldAnswerSink = sink },
		SetBatchWorkIngress:    func(ingress agent.BatchWorkIngress) { app.batchWorkIngress = ingress },
		SetBatchModelControl:   func(mc agent.BatchModelControl) { app.batchModelControl = mc },
		SetBatchLiveReader:     func(r *session.BatchLiveReader) { app.batchLiveReader = r },
		SetBatchRuntime:        func(rt *appbatch.BatchRuntime) { app.batchRuntime = rt },
		SetBatchInstall:        func(install *agent.BatchInstall) { app.batchInstall = install },
	}
}

// releaseBatchWriterParams releases the App's pooled DB references exactly
// like the graceful branch of releaseResources' DB block (per
// Connect/ConnectRead call, or the legacy dbReleasesNeeded count for
// hand-assembled Apps). Delegates to the appbatch pool-release helper.
func releaseBatchWriterParams(cli appbatch.BatchCLIParams) error {
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if conns := cli.DBConns(); len(conns) > 0 {
		for _, conn := range conns {
			record(db.ReleaseConn(conn))
		}
		return firstErr
	}
	for i := 0; i < cli.DBReleasesNeeded(); i++ {
		record(db.Release(cli.DataDir()))
	}
	return firstErr
}

// batchFaultRunResult builds the fault envelope payload for the batch-fault
// JSON writer; it stays in package app because it constructs the run-result
// protocol types. appbatch.FaultEnvelopeWriter receives it as the injected
// fault-result builder.
func batchFaultRunResult(sessionID string, fault error) any {
	text := "batch runner fault"
	if fault != nil {
		text = fault.Error()
	}
	return &RunResult{SessionID: sessionID, ExitReason: "batch_fault", Error: text, ToolCalls: []ToolCallStat{}, FinalText: "", Usage: UsageInfo{}}
}

// Thin same-signature wrappers over the appbatch exported free functions,
// kept so the composition root, lifecycle, agent setup and tests keep
// calling the App methods. The bodies live in internal/app/appbatch.
func (app *App) BatchOwnerRuntime() bool {
	return appbatch.BatchOwnerRuntime(app.batchCLIParams())
}

func (app *App) DataDirectory() string {
	return appbatch.DataDirectory(app.batchCLIParams())
}

func (app *App) BatchCLISession(ctx context.Context, id string) (coresession.Session, error) {
	return appbatch.BatchCLISession(app.batchCLIParams(), ctx, id)
}

func (app *App) BatchCLISpec(ctx context.Context, owner, messageID string, spec jobbatch.Spec) (jobbatch.Spec, func(), error) {
	return appbatch.BatchCLISpecWithQuestionRoute(app.batchCLIParams(), ctx, owner, messageID, spec, true)
}

func (app *App) BatchSubmit(ctx context.Context, owner string, spec jobbatch.Spec) (string, error) {
	return appbatch.BatchSubmitRefWithQuestionRoute(app.batchCLIParams(), ctx, owner, spec, nil, true)
}

// BatchAutoApproveOwner lets the tool calls of an API-owned batch service
// session run without a prompt. Only the "auto" token permission mode asks for
// it; the service session has no user to answer a prompt, and it is never a
// conversational session.
func (app *App) BatchAutoApproveOwner(owner string) {
	appbatch.BatchAutoApproveOwner(app.batchCLIParams(), owner)
}

// BatchSubmitRef is kept for the served intake and other route-ful callers.
func (app *App) BatchSubmitRef(ctx context.Context, owner string, spec jobbatch.Spec, ref func(root string) string) (string, error) {
	return appbatch.BatchSubmitRefWithQuestionRoute(app.batchCLIParams(), ctx, owner, spec, ref, true)
}

// BatchSubmitRefUnattended serves the standalone unattended CLI batch surface:
// no answering process is bound, so agent leaves must not get ask_question.
func (app *App) BatchSubmitRefUnattended(ctx context.Context, owner string, spec jobbatch.Spec) (string, error) {
	return appbatch.BatchSubmitRefWithQuestionRoute(app.batchCLIParams(), ctx, owner, spec, nil, false)
}

// BatchNodeOutput returns the live output of one node of a root this process
// still holds, or an error when it is released, expired or not started. It is
// bounded by ctx: the output of a node that is still running blocks until it
// ends, so callers pass only terminal nodes with a short deadline.
func (app *App) BatchNodeOutput(ctx context.Context, owner, root, node string) (string, error) {
	return appbatch.BatchNodeOutput(app.batchCLIParams(), ctx, owner, root, node)
}

// BatchAnswerHeld answers the held question of a child of an owner session in
// the same batch execution (never as a new generation). The error is
// agent.ErrBatchHeldAnswerNone or a stale/stopped sentinel when there is no
// current question to answer.
func (app *App) BatchAnswerHeld(ctx context.Context, owner, childSessionID, text string) error {
	return appbatch.BatchAnswerHeld(app.batchCLIParams(), ctx, owner, childSessionID, text)
}

func (app *App) BatchRelease(ctx context.Context, owner, root string) error {
	return appbatch.BatchRelease(app.batchCLIParams(), ctx, owner, root)
}

func (app *App) BatchResult(ctx context.Context, owner, root string) ([]db.BatchNode, map[string]string, error) {
	return appbatch.BatchResult(app.batchCLIParams(), ctx, owner, root)
}

func (app *App) BatchAdd(ctx context.Context, root, node string, items []jobbatch.Spec) error {
	return appbatch.BatchAdd(app.batchCLIParams(), ctx, root, node, items)
}

func (app *App) findOrphanPartial(ctx context.Context, sessionID string) *RecoveredPartial {
	return findOrphanPartial(app.runEnv(), ctx, sessionID)
}

// findOrphanPartial scans the session for the latest assistant message that
// has a Partial finish (mid-stream checkpoint) and is unfinished. Used by
// RunNonInteractive to surface recovered text in the envelope.
// Returns nil if no orphan found. Fork patch: batch 8.
// It stays in package app: it consumes RunEnv and produces the run-result
// protocol type RecoveredPartial.
func findOrphanPartial(env RunEnv, ctx context.Context, sessionID string) *RecoveredPartial {
	msgs, err := env.Messages().List(ctx, sessionID)
	if err != nil {
		return nil
	}
	// Find the LATEST assistant message that is partial and unfinished.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != message.Assistant {
			continue
		}
		if m.IsPartial() && !m.IsFinished() {
			text := m.FullText()
			var lastFlushAt int64
			if f := m.FinishPart(); f != nil {
				lastFlushAt = f.Time
			}
			return &RecoveredPartial{
				MessageID:   m.ID,
				Chars:       len(text),
				LastFlushAt: lastFlushAt,
				Text:        text,
			}
		}
		// Only surface the LATEST orphan — stop at the first assistant
		// message we encounter (going backwards).
		break
	}
	return nil
}

func (app *App) batchCutoverWriter() *sql.DB {
	return appbatch.CutoverWriter(app.batchCLIParams())
}

func (app *App) batchCutoverReader(writer *sql.DB) *sql.DB {
	return appbatch.CutoverReader(app.batchCLIParams(), writer)
}

func (app *App) batchFinishedWork(ctx context.Context, sessionID string) ([]appbatch.QuotaWorkRow, bool, error) {
	return appbatch.BatchFinishedWork(app.batchCLIParams(), ctx, sessionID)
}

func (app *App) batchHostStore() *sessionhosts.HostStore {
	return appbatch.BatchHostStore(app.batchCLIParams())
}

func (app *App) batchNoticeStore() *session.SessionNoticeStore {
	return appbatch.BatchNoticeStore(app.batchCLIParams())
}

func (app *App) batchHostEnsure(ctx context.Context) (string, error) {
	return appbatch.BatchHostEnsure(app.batchCLIParams(), ctx)
}

func (app *App) batchHostRelease(ctx context.Context) error {
	return appbatch.BatchHostRelease(app.batchCLIParams(), ctx)
}

func (app *App) batchHostReleaseKeepLock() {
	appbatch.BatchHostReleaseKeepLock(app.batchCLIParams())
}

func (app *App) batchCutoverStoresForget() {
	appbatch.BatchCutoverStoresForget(app.batchCLIParams())
}

func (app *App) batchCutoverHostLiveness() func(string) sessionhosts.HostLockStatus {
	return appbatch.BatchCutoverHostLiveness(app.batchCLIParams())
}

func (app *App) batchCutoverDebtOwners(ctx context.Context, ids []string) (map[string]bool, error) {
	return appbatch.BatchCutoverDebtOwners(app.batchCLIParams(), ctx, ids)
}

func (app *App) batchCutoverLiveWork(ctx context.Context, ids []string) map[string]appbatch.BatchWorkFacts {
	return appbatch.BatchCutoverLiveWork(app.batchCLIParams(), ctx, ids)
}

func (app *App) drainResidualConstructions() {
	appbatch.DrainResidualConstructions(app.batchCLIParams())
}

func (app *App) startBatchMaintenance() {
	appbatch.StartBatchMaintenance(app.batchCLIParams())
}

func (app *App) batchMaintenancePass() {
	appbatch.BatchMaintenancePass(app.batchCLIParams())
}

func (app *App) stopBatchMaintenance() {
	appbatch.StopBatchMaintenance(app.batchCLIParams())
}

func (app *App) handleBatchFaultOutput(fault error) {
	appbatch.HandleBatchFaultOutput(app.batchCLIParams(), fault)
}

// batchRuntimeConfigHook is a test-only seam applied to the runtime config
// right after the factory build and BEFORE the fault watcher starts; it is
// threaded into appbatch.InstallRuntime through InstallParams so package-app
// white tests keep setting it. Nil in production.
var batchRuntimeConfigHook func(*appbatch.BatchRuntimeConfig)

// batchInstallDepsHook is a test-only, per-install dependency decorator.
var batchInstallDepsHook func(*agent.BatchInstallDeps)

// batchInstallWiringHook observes the completed app-owned wake wiring in tests.
var batchInstallWiringHook func(agent.BatchInstallDeps)

func (app *App) installBatchRuntime(ctx context.Context) error {
	params := app.batchInstallParams()
	params.RuntimeConfigHook = batchRuntimeConfigHook
	params.InstallDepsHook = batchInstallDepsHook
	params.InstallWiringHook = batchInstallWiringHook
	return appbatch.InstallRuntime(params, ctx)
}

func (app *App) releaseBatchRuntimeGate(ctx context.Context, stillBusy bool, record func(error)) bool {
	return appbatch.ReleaseBatchRuntimeGate(app.batchInstallParams(), ctx, stillBusy, record)
}

// Temporary bridge declarations (LoopHost seam and turnRunFunc); deleted by design lanes D2-3/D2-4.
// LoopHost is the pipeline seam the run loops (cliLoop, executeRunLoop) call
// back through: free-function-shaped methods over a RunEnv captured by the
// implementation. Unexported by design; package-app tests may use it.
type LoopHost interface {
	executeRun(ctx context.Context, req RunRequest) (*RunResult, error)
	drainPendingBeforeRun(ctx context.Context, sessionID string) error
	waitLocalOwnerReleased(ctx context.Context, sessionID string) error
	captureReviewBasis(ctx context.Context, role config.SelectedModelType, prompt string, start time.Time) *reviewBasis
	attachReview(ctx context.Context, final *RunResult, sessionID, reviewText string, stderr io.Writer)
	reconcileTerminalMessage(ctx context.Context, sessionID string, baselineIDs map[string]struct{}, baselineKnown bool, runStart time.Time, ownID string, ownIDs map[string]struct{}) (terminalReconciliation, error)
	collectSubAgentOutputs(ctx context.Context, parentSessionID string) []SubAgentOutput
	subAgentSummaryStats(ctx context.Context, parentSessionID string) (count int, totalChars int)
	findOrphanPartial(ctx context.Context, sessionID string) *RecoveredPartial
	batchCutoverWriter() *sql.DB
	batchCutoverReader(writer *sql.DB) *sql.DB
	batchFinishedWork(ctx context.Context, sessionID string) ([]appbatch.QuotaWorkRow, bool, error)
}

// loopHost adapts a RunEnv to LoopHost: every method resolves through the
// env's closures, so the adapter carries no *App. App itself must not
// satisfy LoopHost: executeRun is a free function, not an App method.
type loopHost struct {
	env RunEnv
}

func newLoopHost(env RunEnv) *loopHost {
	return &loopHost{env: env}
}

func (app *App) loopHost() *loopHost {
	return newLoopHost(app.runEnv())
}

func (h *loopHost) executeRun(ctx context.Context, req RunRequest) (*RunResult, error) {
	return executeRun(h.env, ctx, req)
}

func (h *loopHost) drainPendingBeforeRun(ctx context.Context, sessionID string) error {
	return drainPendingBeforeRun(h.env, ctx, sessionID)
}

func (h *loopHost) waitLocalOwnerReleased(ctx context.Context, sessionID string) error {
	return waitLocalOwnerReleased(h.env, ctx, sessionID)
}

func (h *loopHost) captureReviewBasis(ctx context.Context, role config.SelectedModelType, prompt string, start time.Time) *reviewBasis {
	return captureReviewBasis(h.env, ctx, role, prompt, start)
}

func (h *loopHost) attachReview(ctx context.Context, final *RunResult, sessionID, reviewText string, stderr io.Writer) {
	attachReview(h.env, ctx, final, sessionID, reviewText, stderr)
}

func (h *loopHost) reconcileTerminalMessage(ctx context.Context, sessionID string, baselineIDs map[string]struct{}, baselineKnown bool, runStart time.Time, ownID string, ownIDs map[string]struct{}) (terminalReconciliation, error) {
	return reconcileTerminalMessage(h.env, ctx, sessionID, baselineIDs, baselineKnown, runStart, ownID, ownIDs)
}

func (h *loopHost) collectSubAgentOutputs(ctx context.Context, parentSessionID string) []SubAgentOutput {
	return collectSubAgentOutputs(h.env, ctx, parentSessionID)
}

func (h *loopHost) subAgentSummaryStats(ctx context.Context, parentSessionID string) (count int, totalChars int) {
	return subAgentSummaryStats(h.env, ctx, parentSessionID)
}

func (h *loopHost) findOrphanPartial(ctx context.Context, sessionID string) *RecoveredPartial {
	return findOrphanPartial(h.env, ctx, sessionID)
}

func (h *loopHost) batchCutoverWriter() *sql.DB {
	return h.env.BatchCutoverWriter()
}

func (h *loopHost) batchCutoverReader(writer *sql.DB) *sql.DB {
	return h.env.BatchCutoverReader(writer)
}

func (h *loopHost) batchFinishedWork(ctx context.Context, sessionID string) ([]appbatch.QuotaWorkRow, bool, error) {
	return h.env.BatchFinishedWork(ctx, sessionID)
}

// turnRunFunc is the shape of the per-turn runner the phase loop hands to
// its turn goroutine: the coordinator Run/RunWithOverrides/
// RunWithCredentials dispatch for the primary turn, or the reviewer pass's
// fixed RunWithOverrides call.
type turnRunFunc func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error)

// cliLoopTurnDoneSeam is a test-only hook called right after each loop turn's
// ExecuteRun returns, before its result is classified. Lets a test land a
// cancellation exactly between a finished turn and the loop's next decision.
// nil in every production path.
var cliLoopTurnDoneSeam func()

// cliLoopBeforeDrainSeam is a test-only hook called after the pre-launch
// checks passed and right before a Drain's ExecuteRun starts: the window in
// which an operator's `sessions cancel` can land after the check. nil in every
// production path.
var cliLoopBeforeDrainSeam func()
