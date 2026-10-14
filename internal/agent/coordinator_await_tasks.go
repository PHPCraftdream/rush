// Coordinator-side implementation of tools.AwaitControl — the await_tasks
// tool (#1270): the top-level agent's way to fall asleep while it has live
// work. The tool layer is thin (await_tasks.go); this file owns the state:
// the "until: all" sleep flag in the arbiter (rule 4b of the turn arbiter),
// the optional max_wait once schedule (an uncancelled schedule keeps
// OnceWakeOpen set and would hold the run open, so the pending schedule id
// is remembered and cancelled on every clear path), and the live-work read,
// which reuses the same source as the session-activity facts
// (AsyncJobStore.LiveWorkForRoots).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
)

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

// AwaitTasks implements tools.AwaitControl. Refuses with a model-safe error
// when nothing is running (open wake schedules do NOT count as work);
// otherwise reports the live rows and, for "all", arms the arbiter's
// sleepAll flag so the launch decision defers turns until the work finishes.
func (c *coordinator) AwaitTasks(ctx context.Context, sessionID, mode string, maxWaitSeconds int) (tools.AwaitTasksResult, error) {
	if !batchFlagEnabled() && (c.asyncJobs == nil || c.asyncJobs.store == nil) {
		return tools.AwaitTasksResult{}, errors.New("live-work facts are unavailable in this session; continue or finish instead")
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
		if sess, err := c.sessions.Get(ctx, sessionID); err == nil && sess.ParentSessionID != "" {
			if batchFlagEnabled() {
				return c.awaitTasksForBatchWorker(ctx, sessionID, mode, maxWaitSeconds)
			}
			if c.asyncJobs == nil || c.asyncJobs.store == nil {
				return tools.AwaitTasksResult{}, errors.New("live-work facts are unavailable in this session; continue or finish instead")
			}
			return c.awaitTasksForWorker(ctx, sessionID, mode, maxWaitSeconds)
		}
	}
	bind := coordinatorBatchBindingFor(c)
	waiting := make([]tools.AwaitTaskRef, 0)
	if !batchFlagEnabled() {
		if c.asyncJobs == nil || c.asyncJobs.store == nil {
			return tools.AwaitTasksResult{}, errors.New("live-work facts are unavailable in this session; continue or finish instead")
		}
		work := c.asyncJobs.store.LiveWorkForRoots(ctx, []string{sessionID})[sessionID]
		waiting = make([]tools.AwaitTaskRef, 0, len(work.Own)+len(work.Descendants))
		for _, job := range work.Own {
			waiting = append(waiting, awaitTaskRef(job))
		}
		for _, job := range work.Descendants {
			waiting = append(waiting, awaitTaskRef(job))
		}
	}
	if batchFlagEnabled() {
		if bind == nil || bind.live == nil {
			return tools.AwaitTasksResult{}, errors.New("batch live reader unavailable")
		}
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
				if node.Kind != "task" || batchAwaitTerminalState(node.State) {
					continue
				}
				waiting = append(waiting, tools.AwaitTaskRef{ToolName: node.TaskKind, ToolCallID: level.Node.ToolCallID, AgeSeconds: int64(time.Since(node.StartedAt).Seconds()), BatchRootID: ref.RootID, BatchNodeID: node.NodeID})
			}
		}
	}

	if len(waiting) == 0 {
		return tools.AwaitTasksResult{}, errors.New("nothing is running; continue or finish")
	}
	res := tools.AwaitTasksResult{Mode: mode, WaitingFor: waiting}
	if mode == "all" {
		c.arb.setSleepAll(sessionID)
	}
	if maxWaitSeconds > 0 {
		view, err := c.ScheduleWakeOnceDelay(ctx, sessionID, int64(maxWaitSeconds),
			fmt.Sprintf(session.AwaitTasksMaxWaitPrefix+" %d task(s) may still be running: %s",
				len(waiting), awaitTaskSummary(waiting)))
		if err != nil {
			// The call failed: do not leave the sleep armed behind it.
			c.arb.clearSleepAll(sessionID)
			return tools.AwaitTasksResult{}, err
		}
		res.MaxWaitScheduleID = view.ScheduleID
		// Both modes own the timer; only all owns the sleep flag.
		c.arb.setMaxWaitWake(sessionID, view.ScheduleID)
	}
	return res, nil
}

func batchAwaitNotifyModes(levels session.BatchCapturedLevels) map[session.BatchLiveRef]string {
	modes := make(map[session.BatchLiveRef]string, len(levels.Roots))
	for _, root := range levels.Roots {
		mode := root.Node.Notify.String
		if !root.Node.Notify.Valid || mode == "" {
			mode = batchNotifyAll
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
	notifyByRef := make(map[session.BatchLiveRef]string)
	result, err := BatchAwaitWorker(ctx, BatchAwaitParams{
		Mode: mode, MaxWaitSeconds: maxWaitSeconds,
		Capture: func(ctx context.Context) session.BatchCapturedRoots {
			captured = bind.live.BatchCaptured(ctx, []string{sessionID})
			levels := bind.live.BatchCapturedLevel(ctx, captured)
			notifyByRef = batchAwaitNotifyModes(levels)
			return captured
		},
		Levels: func(ctx context.Context) session.BatchCapturedLevels {
			return bind.live.BatchCapturedLevel(ctx, captured)
		},
		NotifyModeOf: func(ref session.BatchLiveRef) string { return notifyByRef[ref] },
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
					if node.Kind == "task" && !batchAwaitTerminalState(node.State) {
						refs = append(refs, tools.AwaitTaskRef{ToolName: node.TaskKind, ToolCallID: unit.ToolCallID, AgeSeconds: int64(time.Since(node.StartedAt).Seconds()), BatchRootID: unit.Ref.RootID, BatchNodeID: node.NodeID})
					}
				}
			}
		}
	}
	return tools.AwaitTasksResult{Mode: mode, Blocked: true, TimedOut: result.TimedOut, FinishedCount: result.FinishedCount, WaitingFor: refs}, nil
}

// awaitTasksForWorker is the delegated-worker half of AwaitTasks: it blocks
// inside the tool call until the wake condition holds, the deadline expires,
// or the generation context is cancelled. It never arms the arbiter's sleep
// nor schedules wake schedules.
func (c *coordinator) awaitTasksForWorker(ctx context.Context, sessionID, mode string, maxWaitSeconds int) (tools.AwaitTasksResult, error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return tools.AwaitTasksResult{}, errors.New("legacy live-work facts unavailable for mixed batch await")
	}
	return awaitTasksWorker(ctx, mode, maxWaitSeconds, func() session.RootLiveWork {
		return c.asyncJobs.store.LiveWorkForRoots(ctx, []string{sessionID})[sessionID]
	}, func(cap time.Duration) (<-chan time.Time, <-chan time.Time, func()) {
		ticker := time.NewTicker(awaitTasksWorkerPollInterval)
		return ticker.C, time.After(cap), ticker.Stop
	})
}

// awaitTasksWorker accepts per-call reads and timers for deterministic tests.
func awaitTasksWorker(ctx context.Context, mode string, maxWaitSeconds int, read func() session.RootLiveWork, wait func(time.Duration) (<-chan time.Time, <-chan time.Time, func())) (tools.AwaitTasksResult, error) {
	// LiveJob exposes the durable row key, not claim_id. Snapshot owner/call
	// pairs at entry; once absent from a complete read, a pair stays finished
	// for this wait even if it reappears. Incomplete reads never remove it.
	type identity struct{ owner, call string }
	var pending map[identity]struct{}
	count := func() (int, []tools.AwaitTaskRef, bool) {
		work := read()
		if work.OwnIncomplete || work.DescendantsIncomplete {
			// A failed/cut-off read (e.g. a cancelled ctx) must never
			// look like "the work finished".
			return 0, nil, false
		}
		jobs := append(append([]session.LiveJob(nil), work.Own...), work.Descendants...)
		live := make(map[identity]struct{}, len(jobs))
		for _, job := range jobs {
			live[identity{job.SessionID, job.ToolCallID}] = struct{}{}
		}
		if pending == nil {
			pending = live
		} else {
			for id := range pending {
				if _, exists := live[id]; !exists {
					delete(pending, id)
				}
			}
		}
		refs := make([]tools.AwaitTaskRef, 0, len(pending))
		seen := make(map[identity]struct{}, len(pending))
		for _, job := range jobs {
			id := identity{job.SessionID, job.ToolCallID}
			if _, awaited := pending[id]; !awaited {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			refs = append(refs, awaitTaskRef(job))
		}
		return len(refs), refs, true
	}
	initial, _, ok := count()
	if !ok || initial == 0 {
		return tools.AwaitTasksResult{}, errors.New("nothing is running; continue or finish")
	}
	waitCap := workerAwaitBlockCap
	if maxWaitSeconds > 0 && time.Duration(maxWaitSeconds)*time.Second < waitCap {
		waitCap = time.Duration(maxWaitSeconds) * time.Second
	}
	pollCh, deadlineCh, stop := wait(waitCap)
	defer stop()
	// Computed once: a deadline re-created per loop iteration would drift.
	for {
		select {
		case <-ctx.Done():
			return tools.AwaitTasksResult{}, ctx.Err()
		case <-pollCh:
			if ctx.Err() != nil {
				// Cancellation must win over a same-tick stale poll.
				return tools.AwaitTasksResult{}, ctx.Err()
			}
			n, remaining, ok := count()
			if !ok {
				continue
			}
			if (mode == "all" && n == 0) || (mode == "any" && n < initial) {
				return tools.AwaitTasksResult{Mode: mode, WaitingFor: remaining, Blocked: true, FinishedCount: initial - n}, nil
			}
		case <-deadlineCh:
			n, remaining, ok := count()
			if !ok {
				n, remaining = initial, nil
			}
			return tools.AwaitTasksResult{Mode: mode, WaitingFor: remaining, Blocked: true, TimedOut: true, FinishedCount: initial - n}, nil
		}
	}
}

// awaitTaskRef maps a live-work row onto the tool's JSON shape.
func awaitTaskRef(job session.LiveJob) tools.AwaitTaskRef {
	ref := tools.AwaitTaskRef{
		ToolName:   job.ToolName,
		ToolCallID: job.ToolCallID,
		AgeSeconds: int64(time.Since(job.StartedAt).Seconds()),
	}
	if ref.ToolName == "" {
		ref.ToolName = job.Kind
	}
	ref.ChildSessionID = job.ChildSessionID
	return ref
}

// awaitTaskSummary names the awaited tasks compactly for the wake message.
func awaitTaskSummary(refs []tools.AwaitTaskRef) string {
	out := ""
	for i, ref := range refs {
		if i > 0 {
			out += ", "
		}
		out += ref.ToolName + "/" + ref.ToolCallID
		if len(out) > 200 {
			return out[:200] + "..."
		}
	}
	return out
}

// clearSleepAll disarms the session's await_tasks sleep and cancels its
// pending max_wait once schedule. Called on EVERY drain launch that is
// allowed, on a human message (resetConsecutiveResume) and on Stop
// (Cancel). Errors are logged, never fatal: the sleep flag itself is
// already cleared by the time the cancel runs.
func (c *coordinator) clearSleepAll(sessionID string) {
	id := c.arb.clearSleepAll(sessionID)
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sched, err := c.wakeSchedulerFor()
	if err == nil {
		// Internal cleanup is idempotent, unlike the model-facing tool.
		err = sched.store.CancelSchedule(ctx, sessionID, id, sched.now())
		if err == nil {
			sched.Notify()
		}
	}
	if err != nil {
		slog.Warn("await_tasks: failed to cancel the max_wait schedule",
			"session_id", sessionID, "schedule_id", id, "err", err)
	}
}
