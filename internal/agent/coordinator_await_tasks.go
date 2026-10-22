// Coordinator-side implementation of tools.AwaitControl — the await_tasks
// tool (#1270): the top-level agent's way to fall asleep while it has live
// work. The tool layer is thin (await_tasks.go); this file owns the state:
// the "until: all" sleep flag in the arbiter (rule 4b of the turn arbiter),
// the optional max_wait once schedule (an uncancelled schedule keeps
// OnceWakeOpen set and would hold the run open, so the pending schedule id
// is remembered and cancelled on every clear path), and the live-work read,
// which reads the batch live reader (batch_nodes only).
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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

// awaitTasksWorkerSnapshotHook is a test-only synchronization seam. Production
// leaves it nil; callbacks run after each complete legacy snapshot.
var awaitTasksWorkerSnapshotHook atomic.Pointer[func(string, string)]

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
			if node.Kind != "task" || batchAwaitTerminalState(node.State) {
				continue
			}
			waiting = append(waiting, tools.AwaitTaskRef{ToolName: node.TaskKind, ToolCallID: level.Node.ToolCallID, AgeSeconds: int64(time.Since(node.StartedAt).Seconds()), BatchRootID: ref.RootID, BatchNodeID: node.NodeID})
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
	capturedHook := false
	notifyByRef := make(map[session.BatchLiveRef]string)
	result, err := BatchAwaitWorker(ctx, BatchAwaitParams{
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
