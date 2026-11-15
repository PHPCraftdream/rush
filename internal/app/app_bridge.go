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
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// recoverParams are the explicit inputs of the startup recovery sweep.
// OrphanAge and DataDirOverride are the test seams of the same names on
// App (recoveryOrphanAge, recoveryDataDir).
type recoverParams struct {
	Messages        message.Service
	Config          *config.ConfigStore
	OrphanAge       *time.Duration
	DataDirOverride *string
}

// recoverInterruptedTurns delegates the startup recovery sweep to the free
// function.
func (app *App) recoverInterruptedTurns(ctx context.Context) {
	recoverInterruptedTurns(ctx, recoverParams{
		Messages:        app.Messages,
		Config:          app.config,
		OrphanAge:       app.recoveryOrphanAge,
		DataDirOverride: app.recoveryDataDir,
	})
}

// Thin same-name private wrappers over the Sh2 run-cluster free functions,
// kept so cliLoop/executeRunLoop call sites and tests stay unchanged.
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
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

// batchCLIParams is the explicit capability set the batch free functions
// (Sh5 port) read from *App. env carries the run-cluster closures (DB,
// DataDir, Sessions, Coord, Perms, ProcessHome, fault-output access); the
// remaining fields close over the App fields that are mutable after New
// (batch install pieces, pools, registry owner key, activity conn).
type batchCLIParams struct {
	env                 RunEnv
	dbConns             func() []*sql.DB
	readDB              func() *sql.DB
	dbReleasesNeeded    func() int
	activityConn        func() *db.Queries
	owner               func() *batchOwner
	batchInstall        func() *agent.BatchInstall
	batchRuntime        func() *BatchRuntime
	batchWorkIngress    func() agent.BatchWorkIngress
	batchTimeoutWake    func() *batchTimeoutWakeDispatcher
	setBatchTimeoutWake func(*batchTimeoutWakeDispatcher)
}

// batchCLIParams snapshots the batch capability set at wrapper entry.
// Every closure resolves its App field lazily at the same moment the old
// method body read it. A nil App yields a zero params (nil closures);
// the converted bodies keep the nil-App behaviour through their own guards.
func (app *App) batchCLIParams() batchCLIParams {
	if app == nil {
		return batchCLIParams{}
	}
	return batchCLIParams{
		env:                 app.runEnv(),
		dbConns:             func() []*sql.DB { return app.dbConns },
		readDB:              func() *sql.DB { return app.readDB },
		dbReleasesNeeded:    func() int { return app.dbReleasesNeeded },
		activityConn:        app.activityConn,
		owner:               app.batchOwnerHandle,
		batchInstall:        func() *agent.BatchInstall { return app.batchInstall },
		batchRuntime:        func() *BatchRuntime { return app.batchRuntime },
		batchWorkIngress:    func() agent.BatchWorkIngress { return app.batchWorkIngress },
		batchTimeoutWake:    func() *batchTimeoutWakeDispatcher { return app.batchTimeoutWake },
		setBatchTimeoutWake: func(d *batchTimeoutWakeDispatcher) { app.batchTimeoutWake = d },
	}
}

// batchInstallParams is the explicit input of the Sh5 install port:
// installBatchRuntime, releaseBatchRuntimeGate and their release seams.
type batchInstallParams struct {
	cli                    batchCLIParams
	sessions               func() session.Service
	messages               func() message.Service
	coord                  func() agent.Coordinator
	hostEnsure             func(context.Context) (string, error)
	hostRelease            func(context.Context) error
	hostReleaseKeepLock    func()
	drainResidual          func()
	handleFaultOutput      func(error)
	releaseWriter          func() error
	finalizationAuth       func() *agent.FinalizationAuthorityRegistry
	getBatchHeldAnswerSink func() agent.BatchHeldAnswerSink
	setBatchHeldAnswerSink func(agent.BatchHeldAnswerSink)
	setBatchWorkIngress    func(agent.BatchWorkIngress)
	setBatchModelControl   func(agent.BatchModelControl)
	setBatchLiveReader     func(*session.BatchLiveReader)
	setBatchRuntime        func(*BatchRuntime)
	setBatchInstall        func(*agent.BatchInstall)
}

func (app *App) batchInstallParams() batchInstallParams {
	if app == nil {
		return batchInstallParams{}
	}
	cli := app.batchCLIParams()
	return batchInstallParams{
		cli:                    cli,
		sessions:               cli.env.Sessions,
		messages:               cli.env.Messages,
		coord:                  cli.env.Coord,
		hostEnsure:             func(ctx context.Context) (string, error) { return batchHostEnsure(cli, ctx) },
		hostRelease:            func(ctx context.Context) error { return batchHostRelease(cli, ctx) },
		hostReleaseKeepLock:    func() { batchHostReleaseKeepLock(cli) },
		drainResidual:          func() { drainResidualConstructions(cli) },
		handleFaultOutput:      func(fault error) { handleBatchFaultOutput(cli, fault) },
		releaseWriter:          func() error { return releaseBatchWriter(cli) },
		finalizationAuth:       app.finalizationAuthorityRegistry,
		getBatchHeldAnswerSink: func() agent.BatchHeldAnswerSink { return app.batchHeldAnswerSink },
		setBatchHeldAnswerSink: func(sink agent.BatchHeldAnswerSink) { app.batchHeldAnswerSink = sink },
		setBatchWorkIngress:    func(ingress agent.BatchWorkIngress) { app.batchWorkIngress = ingress },
		setBatchModelControl:   func(mc agent.BatchModelControl) { app.batchModelControl = mc },
		setBatchLiveReader:     func(r *session.BatchLiveReader) { app.batchLiveReader = r },
		setBatchRuntime:        func(rt *BatchRuntime) { app.batchRuntime = rt },
		setBatchInstall:        func(install *agent.BatchInstall) { app.batchInstall = install },
	}
}

// Private same-signature wrappers over the Sh5 batch free functions, kept
// so the composition root, lifecycle, agent setup and tests keep calling
// the App methods. The public wrappers live in their production files.
func (app *App) batchCutoverWriter() *sql.DB {
	return batchCutoverWriter(app.batchCLIParams())
}

func (app *App) batchCutoverReader(writer *sql.DB) *sql.DB {
	return batchCutoverReader(app.batchCLIParams(), writer)
}

func (app *App) batchFinishedWork(ctx context.Context, sessionID string) ([]quotaWorkRow, bool, error) {
	return batchFinishedWork(app.batchCLIParams(), ctx, sessionID)
}

func (app *App) findOrphanPartial(ctx context.Context, sessionID string) *RecoveredPartial {
	return findOrphanPartial(app.runEnv(), ctx, sessionID)
}

func (app *App) batchHostStore() *session.HostStore {
	return batchHostStore(app.batchCLIParams())
}

func (app *App) batchNoticeStore() *session.SessionNoticeStore {
	return batchNoticeStore(app.batchCLIParams())
}

func (app *App) batchHostEnsure(ctx context.Context) (string, error) {
	return batchHostEnsure(app.batchCLIParams(), ctx)
}

func (app *App) batchHostRelease(ctx context.Context) error {
	return batchHostRelease(app.batchCLIParams(), ctx)
}

func (app *App) batchHostReleaseKeepLock() {
	batchHostReleaseKeepLock(app.batchCLIParams())
}

func (app *App) batchCutoverStoresForget() {
	batchCutoverStoresForget(app.batchCLIParams())
}

func (app *App) batchCutoverHostLiveness() func(string) session.HostLockStatus {
	return batchCutoverHostLiveness(app.batchCLIParams())
}

func (app *App) batchCutoverDebtOwners(ctx context.Context, ids []string) (map[string]bool, error) {
	return batchCutoverDebtOwners(app.batchCLIParams(), ctx, ids)
}

func (app *App) batchCutoverLiveWork(ctx context.Context, ids []string) map[string]batchWorkFacts {
	return batchCutoverLiveWork(app.batchCLIParams(), ctx, ids)
}

func (app *App) drainResidualConstructions() {
	drainResidualConstructions(app.batchCLIParams())
}

func (app *App) startBatchMaintenance() {
	startBatchMaintenance(app.batchCLIParams())
}

func (app *App) batchMaintenancePass() {
	batchMaintenancePass(app.batchCLIParams())
}

func (app *App) stopBatchMaintenance() {
	stopBatchMaintenance(app.batchCLIParams())
}

func (app *App) handleBatchFaultOutput(fault error) {
	handleBatchFaultOutput(app.batchCLIParams(), fault)
}

func (app *App) installBatchRuntime(ctx context.Context) error {
	return installBatchRuntime(app.batchInstallParams(), ctx)
}

func (app *App) releaseBatchRuntimeGate(ctx context.Context, stillBusy bool, record func(error)) bool {
	return releaseBatchRuntimeGate(app.batchInstallParams(), ctx, stillBusy, record)
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
	batchFinishedWork(ctx context.Context, sessionID string) ([]quotaWorkRow, bool, error)
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

func (h *loopHost) batchFinishedWork(ctx context.Context, sessionID string) ([]quotaWorkRow, bool, error) {
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
