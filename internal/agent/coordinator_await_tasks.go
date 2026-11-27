package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchflow"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

// Coordinator-side implementation of tools.AwaitControl — the await_tasks
// tool (#1270): the top-level agent's way to fall asleep while it has live
// work. The tool layer is thin (await_tasks.go); this file owns the state:
// the "until: all" sleep flag in the arbiter (rule 4b of the turn arbiter),
// the optional max_wait once schedule (an uncancelled schedule keeps
// OnceWakeOpen set and would hold the run open, so the pending schedule id
// is remembered and cancelled on every clear path), and the live-work read,
// which reads the batch live reader (batch_nodes only).

var _ tools.AwaitControl = (*coordinator)(nil)

// awaitTasksMaxWaitBounds are await_tasks' max_wait_seconds bounds: a floor
// high enough to be useful and a ceiling of six hours.
const (
	awaitTasksMinWaitSeconds = 60
	awaitTasksMaxWaitSeconds = 21600
)

// workerAwaitBlockCap bounds every worker in-tool wait, with or without
// max_wait: it must stay under the 45m single-tool watchdog (agent.go
// toolExecutionMaxDefault). A var so tests can shrink it.
var workerAwaitBlockCap = 40 * time.Minute

// awaitTasksWorkerPollInterval is the worker path's live-work re-read cadence;
// a package var so tests can shrink it.
var awaitTasksWorkerPollInterval = time.Second

// awaitTasksWorkerSnapshotHook is a test-only synchronization seam. Production
// leaves it nil; callbacks run after each complete legacy snapshot.
var awaitTasksWorkerSnapshotHook atomic.Pointer[func(string, string)]

var (
	awaitTasksWorkerBatchPollIntervalOverride time.Duration
	awaitTasksWorkerBatchWaitCapOverride      time.Duration
	awaitTasksWorkerBatchMinWaitOverride      int
	awaitTasksWorkerBatchMaxWaitOverride      int
)

// Root-private test overrides for the values awaitTasksForBatchWorker injects
// into batchflow.BatchAwaitParams (poll interval, wait cap, min bound, max
// bound). Production leaves them all zero: zero PollInterval/WaitCap falls
// back to the lower batchAwaitPollInterval/batchAwaitWorkerCap defaults, and
// zero MinWaitSeconds/MaxWaitBounds keeps the lower 60..21600 validation
// defaults. Tests set them to shrink timing without exporting any test-only
// production symbol in batchflow.

// Тестовый барьер после первого чтения held-вопросов, не до него.
var awaitTasksWorkerQuestionReadHook atomic.Pointer[func(string, string)]

// AwaitTasks implements tools.AwaitControl. Refuses with a model-safe error
// when nothing is running (open wake schedules do NOT count as work);
// otherwise reports the live rows and, for "all", arms the arbiter's
// sleepAll flag so the launch decision defers turns until the work finishes.
func (c *coordinator) AwaitTasks(ctx context.Context, sessionID, mode string, maxWaitSeconds int) (tools.AwaitTasksResult, error) {
	if bind := coordinatorBatchBindingFor(c); bind == nil || bind.live == nil {
		return tools.AwaitTasksResult{}, errors.New("batch live reader unavailable")
	}
	if maxWaitSeconds != 0 && (maxWaitSeconds < awaitTasksMinWaitSeconds || maxWaitSeconds > awaitTasksMaxWaitSeconds) {
		return tools.AwaitTasksResult{}, fmt.Errorf(
			"max_wait_seconds must be between %d and %d, or omitted",
			awaitTasksMinWaitSeconds, awaitTasksMaxWaitSeconds)
	}
	// A delegated child gets BLOCKING semantics: its turn runs synchronously
	// inside the orchestrator's agent tool call, so a StopTurn-shaped result
	// would falsely report the delegation as finished. Workers block inside
	// the tool call instead; the arbiter is not involved.
	if c.sessions != nil {
		isWorker, err := c.isDurableDelegationChild(ctx, sessionID)
		if err != nil {
			return tools.AwaitTasksResult{}, err
		}
		if isWorker {
			return c.awaitTasksForBatchWorker(ctx, sessionID, mode, maxWaitSeconds)
		}
	}
	bind := coordinatorBatchBindingFor(c)
	waiting := make([]tools.AwaitTaskRef, 0)
	reader := bind.live
	captured := reader.BatchCaptured(ctx, []string{sessionID})
	if !captured.Complete() {
		return tools.AwaitTasksResult{}, errors.New("batch live-work capture incomplete")
	}
	for _, ref := range captured.Refs() {
		level := reader.BatchLevel(ctx, ref, ref.RootID)
		if !level.Complete || level.Disappeared {
			return tools.AwaitTasksResult{}, errors.New("batch level read incomplete")
		}
		for _, node := range level.Subtree {
			if node.Kind != "task" || batchflow.BatchAwaitTerminalState(node.State) {
				continue
			}
			waiting = append(waiting, tools.AwaitTaskRef{ToolName: node.TaskKind, ToolCallID: level.Node.ToolCallID, AgeSeconds: int64(time.Since(node.StartedAt).Seconds()), BatchRootID: ref.RootID, BatchNodeID: node.NodeID})
		}
	}

	// A held child question refuses BOTH modes before any sleep/timer is
	// armed: the parent must answer (inject_agent) rather than sleep past
	// its own child. The tool layer (internal/agent/tools/await_tasks.go)
	// turns any error into NewTextErrorResponse WITHOUT the StopTurn flag,
	// so the refusal stays a model-visible turn-continuing message.
	if refusal := c.heldQuestionAwaitRefusal(ctx, sessionID); refusal != nil {
		return tools.AwaitTasksResult{}, refusal
	}

	if len(waiting) == 0 {
		return tools.AwaitTasksResult{}, errors.New("nothing is running; continue or finish")
	}
	res := tools.AwaitTasksResult{Mode: mode, WaitingFor: waiting}
	var create func() (string, error)
	if maxWaitSeconds > 0 {
		create = func() (string, error) {
			view, err := c.ScheduleWakeOnceDelay(ctx, sessionID, int64(maxWaitSeconds),
				fmt.Sprintf(sessionwake.AwaitTasksMaxWaitPrefix+" %d task(s) may still be running: %s",
					len(waiting), batchlife.AwaitTaskSummary(waiting)))
			return view.ScheduleID, err
		}
	}
	id, err := c.armAwait(ctx, sessionID, mode == "all", create)
	if err != nil {
		return tools.AwaitTasksResult{}, err
	}
	res.MaxWaitScheduleID = id
	return res, nil
}

func batchAwaitNotifyModes(levels session.BatchCapturedLevels) map[session.BatchLiveRef]string {
	modes := make(map[session.BatchLiveRef]string, len(levels.Roots))
	for _, root := range levels.Roots {
		mode := root.Node.Notify.String
		if !root.Node.Notify.Valid || mode == "" {
			mode = batchflow.BatchNotifyAll
		}
		modes[root.Ref] = mode
	}
	return modes
}

func (c *coordinator) awaitTasksForBatchWorker(ctx context.Context, sessionID, mode string, maxWaitSeconds int) (tools.AwaitTasksResult, error) {
	bind := coordinatorBatchBindingFor(c)
	if bind == nil || bind.live == nil {
		return tools.AwaitTasksResult{}, errors.New("batch live reader unavailable")
	}
	var captured session.BatchCapturedRoots
	capturedHook := false
	questionHook := false
	notifyByRef := make(map[session.BatchLiveRef]string)
	result, err := batchflow.BatchAwaitWorker(ctx, batchflow.BatchAwaitParams{
		Mode: mode, MaxWaitSeconds: maxWaitSeconds,
		Capture: func(ctx context.Context) session.BatchCapturedRoots {
			captured = bind.live.BatchCaptured(ctx, []string{sessionID})
			levels := bind.live.BatchCapturedLevel(ctx, captured)
			if !capturedHook {
				capturedHook = true
				if hook := awaitTasksWorkerSnapshotHook.Load(); hook != nil {
					(*hook)(sessionID, mode)
				}
			}
			notifyByRef = batchAwaitNotifyModes(levels)
			return captured
		},
		Levels: func(ctx context.Context) session.BatchCapturedLevels {
			return bind.live.BatchCapturedLevel(ctx, captured)
		},
		NotifyModeOf: func(ref session.BatchLiveRef) string { return notifyByRef[ref] },
		Hints:        func() (<-chan struct{}, func()) { return c.driverHints.subscribe(sessionID) },
		// Zero overrides fall back to the lower defaults; see the override
		// var block above.
		PollInterval:   awaitTasksWorkerBatchPollIntervalOverride,
		WaitCap:        awaitTasksWorkerBatchWaitCapOverride,
		MinWaitSeconds: awaitTasksWorkerBatchMinWaitOverride,
		MaxWaitBounds:  awaitTasksWorkerBatchMaxWaitOverride,
		// Reaction-independent held-question gate: an unanswered child
		// question (even one marked reacted by the owner's reaction flow)
		// breaks the wait instead of blocking in-tool for up to the 40m cap.
		// unknown facts (capability absent) never gate: nil, nil.
		HeldQuestions: func(ctx context.Context) ([]batchlife.BatchHeldQuestionRoute, error) {
			src, ok := coordinatorBatchCutoverFacts(c).(batchlife.BatchHeldQuestionSource)
			if !ok {
				return nil, nil
			}
			routes, err := src.ActionableHeldQuestions(ctx, sessionID)
			if !questionHook {
				questionHook = true
				if hook := awaitTasksWorkerQuestionReadHook.Load(); hook != nil {
					(*hook)(sessionID, mode)
				}
			}
			return routes, err
		},
	})
	if err != nil {
		return tools.AwaitTasksResult{}, err
	}
	refs := make([]tools.AwaitTaskRef, 0, len(result.Remaining))
	for _, unit := range result.Remaining {
		levels := bind.live.BatchCapturedLevel(ctx, captured)
		for _, root := range levels.Roots {
			if root.Ref == unit.Ref {
				for _, node := range root.Children {
					if node.Kind == "task" && !batchflow.BatchAwaitTerminalState(node.State) {
						refs = append(refs, tools.AwaitTaskRef{ToolName: node.TaskKind, ToolCallID: unit.ToolCallID, AgeSeconds: int64(time.Since(node.StartedAt).Seconds()), BatchRootID: unit.Ref.RootID, BatchNodeID: node.NodeID})
					}
				}
			}
		}
	}
	return tools.AwaitTasksResult{Mode: mode, Blocked: true, TimedOut: result.TimedOut, FinishedCount: result.FinishedCount, WaitingFor: refs}, nil
}

// Background-job completion notices, Phase-4 autonomous idle-resume, and
// busy-state queries: bodies moved to background_component.go (the
// backgroundComponent host pattern); this file keeps the thin coordinator
// delegates.

func (c *coordinator) notifyAsyncCompletion(completion batchdispatch.AsyncCompletion) {
	newBackgroundComponent(c).notifyAsyncCompletion(completion)
}

func (c *coordinator) notifyBackgroundJobDone(sessionID string, sh *shellbg.BackgroundShell) {
	newBackgroundComponent(c).notifyBackgroundJobDone(sessionID, sh)
}

func (c *coordinator) IsBusy() bool {
	return newBackgroundComponent(c).isBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return newBackgroundComponent(c).isSessionBusy(sessionID)
}

func (c *coordinator) delegatedChildDriven(ctx context.Context, sessionID string) (bool, error) {
	return newBackgroundComponent(c).delegatedChildDriven(ctx, sessionID)
}

func (c *coordinator) notifyOnBackgroundJobDone() bool {
	return newBackgroundComponent(c).notifyOnBackgroundJobDone()
}

func (c *coordinator) persistBGShellCompletion(sessionID, shellID, summary string) (claimed bool) {
	return newBackgroundComponent(c).persistBGShellCompletion(sessionID, shellID, summary)
}

func (c *coordinator) pruneBGShellOverCap(sessionID string) {
	newBackgroundComponent(c).pruneBGShellOverCap(sessionID)
}

func (c *coordinator) bgShellOverCapCount(sessionID string) int {
	return newBackgroundComponent(c).bgShellOverCapCount(sessionID)
}

// stopBatchOwners durably fences local batch roots before issuing best-effort
// executor stops. The fence is authoritative even when cancellation fails.
func (c *coordinator) stopBatchOwners(ids []string) {
	store := batchCutoverReactionStore(c)
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	targets, err := store.BatchStopOwners(ctx, ids)
	cancel()
	if err != nil {
		slog.Error("coordinator.Cancel: failed to fence batch roots", "owners", ids, "err", err)
		return
	}
	if len(targets) == 0 {
		return
	}
	runtimeRunner, localHost, _ := batchRerunRuntime(c)
	if runtimeRunner == nil {
		return
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	for _, target := range targets {
		if target.Host != "" && (localHost == "" || target.Host != localHost) {
			continue
		}
		if out := batchmeta.CancelCall(stopCtx, runtimeRunner, jobbatch.ID(target.Root)); out.Err != nil {
			// A failed cancellation must leave the durable request pending for retry.
			slog.Error("coordinator.Cancel: batch stop failed", "owner", target.Owner, "root", target.Root, "err", out.Err)
			continue
		}
		answerCtx, answerCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.BatchAnswerOwnerStop(answerCtx, target); err != nil {
			slog.Error("coordinator.Cancel: failed to answer batch stop", "owner", target.Owner, "root", target.Root, "request_id", target.RequestID, "err", err)
		}
		answerCancel()
	}
}

// Per-invocation call-result identity reporting for internal/app's terminal
// reconciliation (R8-3, round-8 audit).
//
// Terminal reconciliation used to accept ANY assistant row newer than its
// baseline snapshot as this call's own result. A committed A can release
// its session, then a legitimately later B claims it and finishes its OWN
// turn before A's event loop gets around to reconciling -- baseline alone
// cannot tell A's own row apart from B's, since both are "new" relative to
// A's snapshot taken before A's turn started. Scoping reconciliation to the
// assistant message ID THIS runInternal invocation actually produced closes
// that window; the caller's baseline stays an additional filter; this
// closes a different window than R3-1/R7-1's own-ID fixes, which protect
// retry classification inside runInternal, not the app-layer consumer of
// its result.

// CallResultRecorder captures the assistant message ID that ONE runInternal
// invocation (Run/RunWithOverrides/RunWithCredentials, they all funnel
// through it) ultimately produced. Exported because internal/app -- the
// only intended caller -- arms one per ExecuteRun phase and reads it after
// the phase's turn goroutine has returned.
type CallResultRecorder struct {
	mu         sync.Mutex
	id         string
	ids        map[string]struct{}
	generation uint64
	sealed     bool
}

// NewCallResultRecorder returns a recorder that has not yet captured an ID.
func NewCallResultRecorder() *CallResultRecorder { return &CallResultRecorder{} }

// BeginCall clears identity accumulated by an earlier call using the same
// context, as can happen when a durable drain executes several queue rows.
func (r *CallResultRecorder) BeginCall() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.id = ""
	r.ids = nil
	r.generation++
	r.sealed = false
	r.mu.Unlock()
}

// Seal rejects callbacks that arrive after this call has completed.
func (r *CallResultRecorder) Seal() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
}

// Capture returns a callback restricted to the current call generation.
func (r *CallResultRecorder) Capture() func(string) {
	if r == nil {
		return func(string) {}
	}
	r.mu.Lock()
	generation := r.generation
	r.mu.Unlock()
	return func(id string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if id == "" || r.generation != generation || r.sealed {
			return
		}
		r.id = id
		if r.ids == nil {
			r.ids = make(map[string]struct{})
		}
		r.ids[id] = struct{}{}
	}
}

// Owns reports whether id was created by this recorded call.
func (r *CallResultRecorder) Owns(id string) bool {
	if r == nil || id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ids[id]
	return ok
}

// IDs returns the assistant rows created by the current call.
func (r *CallResultRecorder) IDs() map[string]struct{} {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make(map[string]struct{}, len(r.ids))
	for id := range r.ids {
		ids[id] = struct{}{}
	}
	return ids
}

// RecordConfirmed adopts an identity confirmed by a durable queue execution.
func (r *CallResultRecorder) RecordConfirmed(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids == nil {
		r.ids = make(map[string]struct{})
	}
	r.id = id
	r.ids[id] = struct{}{}
}

// Resolve reads the captured ID ("" if runInternal never reached the point
// of recording one, e.g. an early refusal before any message was created).
// internal/app calls this only after the turn goroutine that was handed
// this recorder's ctx has returned -- the channel send that reports that
// completion is itself a happens-before edge, so no additional
// synchronization beyond the mutex is required.
func (r *CallResultRecorder) Resolve() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id
}

type callResultRecorderCtxKey struct{}

// WithCallResultRecorder arms rec as the call-result recorder for the
// DIRECT runInternal invocation ctx is handed to. internal/app is the only
// intended caller.
func WithCallResultRecorder(ctx context.Context, rec *CallResultRecorder) context.Context {
	return context.WithValue(ctx, callResultRecorderCtxKey{}, rec)
}

// callResultRecorderFrom returns the recorder armed on ctx, or nil when the
// caller did not arm one (every non-ExecuteRun caller: interrupt replay,
// compaction, sub-agent delegation).
func callResultRecorderFrom(ctx context.Context) *CallResultRecorder {
	rec, _ := ctx.Value(callResultRecorderCtxKey{}).(*CallResultRecorder)
	return rec
}

// withoutCallResultRecorder detaches any recorder from ctx so a nested
// runInternal reached through this turn's tools (a sub-agent's child
// session shares ctx) observes nil and cannot overwrite the top-level
// caller's own captured ID with a child session's unrelated message.
func withoutCallResultRecorder(ctx context.Context) context.Context {
	return context.WithValue(ctx, callResultRecorderCtxKey{}, (*CallResultRecorder)(nil))
}

// ExternalInterruptSource обслуживает interrupt между ходами CLI-драйвера.
type ExternalInterruptSource interface {
	PollExternalInterrupt(context.Context, string, func() error) (string, error)
}

func (c *coordinator) PollExternalInterrupt(ctx context.Context, sessionID string, beforeDelivery func() error) (string, error) {
	if !c.driverHints.isExternalDriver(sessionID) || c.IsSessionBusy(sessionID) {
		return "", nil
	}
	tickCtx, cancel := context.WithTimeout(ctx, interruptTickOperationTimeout)
	defer cancel()
	var messageID string
	// Существующий atomic consume/enqueue сохраняет generation и policy fences.
	fired, err := c.handleInterruptTickObserved(tickCtx, sessionID, func(id string) { messageID = id }, beforeDelivery)
	if fired {
		c.clearSleepAll(sessionID)
		c.driverHints.bumpHint(sessionID)
	}
	return messageID, err
}
