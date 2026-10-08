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
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return tools.AwaitTasksResult{}, errors.New(
			"live-work facts are unavailable in this session; continue or finish instead")
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
			return c.awaitTasksForWorker(ctx, sessionID, mode, maxWaitSeconds)
		}
	}
	work := c.asyncJobs.store.LiveWorkForRoots(ctx, []string{sessionID})[sessionID]
	waiting := make([]tools.AwaitTaskRef, 0, len(work.Own)+len(work.Descendants))
	for _, job := range work.Own {
		waiting = append(waiting, awaitTaskRef(job))
	}
	for _, job := range work.Descendants {
		waiting = append(waiting, awaitTaskRef(job))
	}
	if len(waiting) == 0 {
		return tools.AwaitTasksResult{}, errors.New("nothing is running; continue or finish")
	}
	res := tools.AwaitTasksResult{Mode: mode, WaitingFor: waiting}
	if mode == "all" {
		// Arm the sleep BEFORE the schedule so a clear racing this call
		// always finds the flag it must lift.
		c.arb.setSleepAll(sessionID, "")
	}
	if maxWaitSeconds > 0 {
		view, err := c.ScheduleWakeOnceDelay(ctx, sessionID, int64(maxWaitSeconds),
			fmt.Sprintf("await_tasks max_wait elapsed: %d task(s) may still be running: %s",
				len(waiting), awaitTaskSummary(waiting)))
		if err != nil {
			// The call failed: do not leave the sleep armed behind it.
			c.arb.clearSleepAll(sessionID)
			return tools.AwaitTasksResult{}, err
		}
		res.MaxWaitScheduleID = view.ScheduleID
		if mode == "all" {
			// Remember the schedule so every clear path cancels it; an
			// uncancelled once schedule keeps OnceWakeOpen set and holds
			// the run open after the sleep is over.
			c.arb.setSleepAll(sessionID, view.ScheduleID)
		}
	}
	return res, nil
}

// awaitTasksForWorker is the delegated-worker half of AwaitTasks: it blocks
// inside the tool call until the wake condition holds, the deadline expires,
// or the generation context is cancelled. It never arms the arbiter's sleep
// nor schedules wake schedules.
func (c *coordinator) awaitTasksForWorker(ctx context.Context, sessionID, mode string, maxWaitSeconds int) (tools.AwaitTasksResult, error) {
	count := func() (int, []tools.AwaitTaskRef, bool) {
		work := c.asyncJobs.store.LiveWorkForRoots(ctx, []string{sessionID})[sessionID]
		if work.OwnIncomplete || work.DescendantsIncomplete {
			// A failed/cut-off read (e.g. a cancelled ctx) must never
			// look like "the work finished".
			return 0, nil, false
		}
		refs := make([]tools.AwaitTaskRef, 0, len(work.Own)+len(work.Descendants))
		for _, job := range work.Own {
			refs = append(refs, awaitTaskRef(job))
		}
		for _, job := range work.Descendants {
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
	ticker := time.NewTicker(awaitTasksWorkerPollInterval)
	defer ticker.Stop()
	// Computed once: a deadline re-created per loop iteration would drift.
	deadlineCh := time.After(waitCap)
	for {
		select {
		case <-ctx.Done():
			return tools.AwaitTasksResult{}, ctx.Err()
		case <-ticker.C:
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
	if _, err := c.CancelWakeSchedule(ctx, sessionID, id); err != nil {
		slog.Warn("await_tasks: failed to cancel the max_wait schedule",
			"session_id", sessionID, "schedule_id", id, "err", err)
	}
}
