package agent

// This file is the R10b mapping table: every old await/arbiter/scope
// function or fact and its batch counterpart, or why it was dropped or
// deferred. Anchors are hints; the old code lives untouched until R12.

// awaitTasksWorker (internal/agent/coordinator_await_tasks.go) ->
// BatchAwaitWorker. Captured (owner, tool_call_id) pairs become captured
// BatchLiveRef roots; complete-read disappearance of a pair becomes the
// Disappeared/Undetermined classification; incomplete reads still remove
// nothing; timeout still reports remaining without stopping; one return per
// call is preserved as a single return point at the end of the select loop.
// NEW vs old: NotifyEach roots settle on a terminal direct child of a later
// Add (visible via the level read); a disappeared root breaks ANY and ALL
// immediately instead of being counted as one finished unit.

// AwaitTasks top-level path (coordinator_await_tasks.go AwaitTasks) ->
// dropped here, deferred: the top-level StopTurn/sleep-arming wrapper needs
// the arbiter and wake scheduler wiring, which is R11F (arbiter
// integration). BatchAwaitWorker covers the worker blocking half only.

// awaitTasksMaxWaitBounds (awaitTasksMinWaitSeconds=60,
// awaitTasksMaxWaitSeconds=21600) -> injected BatchAwaitParams.
// MinWaitSeconds and MaxWaitSeconds with 60/21600 defaults
// (batchAwaitMinWaitSeconds, batchAwaitMaxWaitSeconds); 0 max_wait stays
// omitted; out of bounds stays an error.

// workerAwaitBlockCap (40m, under the 45m single-tool watchdog) ->
// batchAwaitWorkerCap, a new-file var, injectable via BatchAwaitParams.
// WaitCap; deadline = min(effective max_wait, cap) as before.

// awaitTasksWorkerPollInterval (1s) -> batchAwaitPollInterval, injectable
// via BatchAwaitParams.PollInterval; the poll tick is now only a fallback,
// the primary wake being the level-triggered hint channel.

// arb.setSleepAll and max-wake scheduling (clearSleepAll, ScheduleWakeOnceDelay,
// arb.setMaxWaitWake) -> deferred to R11F: the arbiter integration itself.
// BatchAwaitWorker arms nothing and schedules nothing; it holds no execution
// admission.

// turn_arbiter rule 1 (Debt.PendingIncl, VNone when empty) -> ReadBatchArbiterFacts'
// DebtPendingIncl via BatchStore.BatchReactionPendingInclusiveDebtRows
// (pending-inclusive). Unknown debt stays true: a false "no debt" would
// produce a false VNone.

// turn_arbiter rule 4b (SleepAll && CompletionOnly && (RunningOwnJobs>0 ||
// RunningDelegation)) -> BatchArbiterFacts.SleepHolds(sleepAll, breakers):
// SleepAll && CompletionOnly && LiveRoots && !breaker, where breakers are
// QuestionDebt, WakeFired and MaxWaitExpired (question, wake_only and
// max_wait can break sleep). The deferral decision itself stays in R11F.

// Session facts RunningOwnJobs>0 -> BatchArbiterFacts.LiveRoots, own-scope
// half: BatchLiveResult.HasLive over BatchLiveForOwner's own roots.

// Session fact RunningDelegation -> LiveRoots, descendant half: the R10a
// read covers delegation descendants through child_session_id node links,
// so both old counts collapse into one HasLive fact.

// DebtFacts.CompletionOnly (turn_arbiter.go readTurnFacts) -> CompletionOnly:
// selected batch completion notices are completion-class (any node kind,
// like legacy job debt rows); child_question is not; stop intent never
// appears (BatchArbiterFacts.StopIntentIsNotCompletion documents AW-b).

// readTurnFacts' LiveWorkForRoots read (rule 4b running rows) -> LiveRoots
// via the R10a read; the sleep-armed-only read optimization is a caller
// concern, deferred to R11F.

// CLIScopeState.WorkOpen work half (coordinator_reaction_source.go
// CLIScope/runningWorkOpen) -> ReadBatchScopeFacts: LiveRoots (HasLive) ||
// OpenDebt (pending-inclusive debt), incomplete reads keep it open.
// CLIScopeState.WorkOpen (old) -> replaced per caller in R10c/R11 wiring.

// OnceWakeOpen -> kept OUT of BatchScopeFacts deliberately: WorkOpen and
// OnceWakeOpen are independent facts (WorkOpen vs OnceWakeOpen are
// independent in CLIScopeState too); the CLI caller combines them.

// LiveWorkForRoots consumers (sessions reset/rerun/jobs/why, server
// handlers, web) -> deferred to R10c where those consumers belong; this
// package only provides the await/arbiter/scope facts above.
