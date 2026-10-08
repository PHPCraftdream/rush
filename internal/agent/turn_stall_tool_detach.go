package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// This file is the tool-detach layer of the turn-stall policy (chunk 3).
// When the policy fires on a stalled SYNCHRONOUS tool phase, the wrapper
// below (installed on every tool by wrapToolsWithStallDetach) makes the
// blocked Run return an immediate "still running ... job <id>" tool result
// so fantasy's sequential executeTools proceeds to the remaining calls of
// the same assistant message; the real execution keeps running and its
// outcome stays reachable through job_output/job_kill. Delegations
// (agent/agentic_fetch) and background/async jobs (bash/run_command, whose
// own machinery already detaches long runs) are never touched, and we
// never kill a tool on our own — only an explicit model job_kill does.

// stallDetachExcludedTools are never detached: the delegation tools are
// asyncTool-wrapped work-ledger jobs, and bash/run_command already detach
// long runs themselves (auto-background / work ledger), so a stalled call
// there is either a ledger job or an SDK-sync await that this layer must
// not double-handle.
var stallDetachExcludedTools = map[string]bool{
	AgentToolName:              true,
	tools.AgenticFetchToolName: true,
	tools.BashToolName:         true,
	tools.RunCommandToolName:   true,
	// A worker's await_tasks blocks by design until its jobs finish.
	tools.AwaitTasksToolName: true,
}

// stallDetachPending holds the in-flight (not yet detached) synchronous
// tool calls, keyed by sessionID NUL callID; stallDetachJobs holds the
// detached ones under the same key.
var (
	stallDetachPending sync.Map // string -> *stallDetachEntry
	stallDetachJobs    sync.Map // string -> *stallDetachEntry
)

// stallDetachKey builds both registries' key.
func stallDetachKey(sessionID, callID string) string {
	return sessionID + "\x00" + callID
}

// stallDetacherTool wraps a fantasy.AgentTool so a stalled synchronous Run
// can be detached: the inner Run moves to its own goroutine (cancellable
// only via job_kill after a detach) and this Run returns the wake-up
// notice the moment the stall policy fires.
type stallDetacherTool struct {
	inner fantasy.AgentTool
}

// wrapToolsWithStallDetach wraps every tool with a stallDetacherTool.
// One-line hook target for the tool-assembly boundary.
func wrapToolsWithStallDetach(toolsIn []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(toolsIn))
	for i, t := range toolsIn {
		out[i] = &stallDetacherTool{inner: t}
	}
	return out
}

func (t *stallDetacherTool) Info() fantasy.ToolInfo {
	return t.inner.Info()
}

func (t *stallDetacherTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *stallDetacherTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *stallDetacherTool) RestrictedRunAction() string {
	provider, ok := t.inner.(interface{ RestrictedRunAction() string })
	if !ok {
		return ""
	}
	return provider.RestrictedRunAction()
}

// Run executes the inner tool, ready to detach it on the stall policy's
// signal. Normal completions are indistinguishable from an unwrapped Run.
func (t *stallDetacherTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	name := t.inner.Info().Name
	if stallDetachExcludedTools[name] {
		return t.inner.Run(ctx, call)
	}
	// The detached run must outlive the triggering turn's ctx (mirroring
	// the asyncTool CLI/web pattern), with cancellation handed over to
	// job_kill only.
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	stopPropagate := context.AfterFunc(ctx, runCancel)
	entry := &stallDetachEntry{
		sessionID: tools.GetSessionFromContext(ctx),
		callID:    call.ID,
		toolName:  name,
		shortArgs: stallShortArgs(call.Input),
		started:   stallNow(),
		detach:    make(chan struct{}),
		cancel:    runCancel,
	}
	stallDetachPending.Store(stallDetachKey(entry.sessionID, entry.callID), entry)
	type runResult struct {
		resp fantasy.ToolResponse
		err  error
	}
	done := make(chan runResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				resp := fantasy.NewTextErrorResponse(fmt.Sprintf("%s panicked: %v", name, r))
				entry.complete(resp, nil)
				done <- runResult{resp, nil}
			}
		}()
		resp, err := t.inner.Run(runCtx, call)
		entry.complete(resp, err)
		done <- runResult{resp, err}
	}()
	select {
	case r := <-done:
		stopPropagate()
		runCancel()
		stallDetachPending.Delete(stallDetachKey(entry.sessionID, entry.callID))
		return r.resp, r.err
	case <-entry.detach:
		stopPropagate()
		stallDetachPending.Delete(stallDetachKey(entry.sessionID, entry.callID))
		stallDetachJobs.Store(stallDetachKey(entry.sessionID, entry.callID), entry)
		return fantasy.NewTextResponse(entry.notice()), nil
	case <-ctx.Done():
		// The turn is ending (cancel/interrupt): the inner run follows the
		// caller's ctx as before — this is not a stall detach.
		stopPropagate()
		runCancel()
		stallDetachPending.Delete(stallDetachKey(entry.sessionID, entry.callID))
		<-done
		return fantasy.ToolResponse{}, ctx.Err()
	}
}

// stallDetachEntry is one detachable synchronous tool run, first pending
// and then (after a policy fire) a background job.
type stallDetachEntry struct {
	mu        sync.Mutex
	sessionID string
	callID    string
	toolName  string
	shortArgs string
	started   time.Time
	detach    chan struct{}
	cancel    context.CancelFunc
	detached  bool
	// killRequested latches an explicit job_kill so complete() can record
	// the stop cause; only job_kill ever cancels the run.
	killRequested bool
	done          bool
	content       string
}

// jobID is the id the model sees in the notice and passes to
// job_output/job_kill.
func (e *stallDetachEntry) jobID() string {
	return "stall-" + e.callID
}

// notice builds the immediate wake-up tool result.
func (e *stallDetachEntry) notice() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fmt.Sprintf(
		"tool %s (%s) is still running after %s with no progress; it continues in the background as job %s. Wait with job_output or stop it with job_kill.",
		e.toolName, e.shortArgs, stallNow().Sub(e.started).Truncate(time.Second), e.jobID(),
	)
}

// complete records the inner run's outcome so job_output can serve it.
func (e *stallDetachEntry) complete(resp fantasy.ToolResponse, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.done = true
	switch {
	case err != nil:
		e.content = err.Error()
	case e.killRequested:
		e.content = "stopped by job_kill\n" + resp.Content
	default:
		e.content = resp.Content
	}
}

// stallToolStalled detaches the stalled synchronous call, if one is
// registered and not yet detached. Returns the job id, or "" when the call
// is not detachable (delegation, background/async, already detached).
func stallToolStalled(sessionID, callID string, since time.Duration) string {
	if callID == "" {
		return ""
	}
	key := stallDetachKey(sessionID, callID)
	v, ok := stallDetachPending.Load(key)
	if !ok {
		return ""
	}
	e := v.(*stallDetachEntry)
	e.mu.Lock()
	if e.detached {
		e.mu.Unlock()
		return ""
	}
	e.detached = true
	e.mu.Unlock()
	close(e.detach)
	slog.Warn("turn-stall policy: detached stalled synchronous tool into background job",
		"session_id", sessionID,
		"tool", e.toolName,
		"job_id", e.jobID(),
		"since_progress", since.Truncate(time.Second),
	)
	return e.jobID()
}

// stallShortArgs collapses call input to a short, single-line summary.
func stallShortArgs(input string) string {
	input = strings.Join(strings.Fields(input), " ")
	if len(input) > 80 {
		return input[:77] + "..."
	}
	return input
}

// stallDetachJobController serves the detached jobs to job_output/job_kill
// through the tools package's pluggable controller.
type stallDetachJobController struct{}

// init installs the controller; job_output/job_kill consult it before the
// shell/ledger paths.
func init() { tools.StallDetachedJobs = stallDetachJobController{} }

// StallDetachOutput reports a detached job's captured output. Running jobs
// report done=false (no progressive output is captured for an arbitrary
// in-flight tool); ok=false means the id is not a detached job.
func (stallDetachJobController) StallDetachOutput(sessionID, jobID string, cursor int64) (string, bool, int64, bool) {
	v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, strings.TrimPrefix(jobID, "stall-")))
	if !ok {
		return "", false, 0, false
	}
	e := v.(*stallDetachEntry)
	e.mu.Lock()
	if !e.done {
		e.mu.Unlock()
		return "still running (final output is kept here when the detached tool finishes)", false, 0, true
	}
	if cursor < 0 || cursor > int64(len(e.content)) {
		cursor = 0
	}
	out, total := tools.TruncateOutput(e.content[cursor:]), int64(len(e.content))
	// A kill-requested job's final output stays readable (the model keeps
	// polling after job_kill); others are served once and dropped.
	if !e.killRequested {
		defer deleteStallDetachJob(sessionID, jobID)
	}
	e.mu.Unlock()
	return out, true, total, true
}

// StallDetachKill requests a stop for a detached job. This is the ONLY
// cancellation path — rush never kills a detached tool on its own.
func (stallDetachJobController) StallDetachKill(sessionID, jobID string) (string, bool) {
	v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, strings.TrimPrefix(jobID, "stall-")))
	if !ok {
		return "", false
	}
	e := v.(*stallDetachEntry)
	e.mu.Lock()
	if e.done {
		e.mu.Unlock()
		deleteStallDetachJob(sessionID, jobID)
		return fmt.Sprintf("detached job %s already finished", jobID), true
	}
	defer e.mu.Unlock()
	e.killRequested = true
	e.cancel()
	return fmt.Sprintf("detached job %s (%s) stop requested; its final output is kept under job_output", jobID, e.toolName), true
}

// deleteStallDetachJob removes one finished detached job from the registry.
func deleteStallDetachJob(sessionID, jobID string) {
	stallDetachJobs.Delete(stallDetachKey(sessionID, strings.TrimPrefix(jobID, "stall-")))
}

// stallDetachCleanupSession drops a session's pending entries and finished
// detached jobs when its run ends; still-running jobs stay reachable.
func stallDetachCleanupSession(sessionID string) {
	prefix := sessionID + "\x00"
	stallDetachPending.Range(func(k, v any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			stallDetachPending.Delete(k)
		}
		return true
	})
	stallDetachJobs.Range(func(k, v any) bool {
		if !strings.HasPrefix(k.(string), prefix) {
			return true
		}
		e := v.(*stallDetachEntry)
		e.mu.Lock()
		done := e.done
		e.mu.Unlock()
		if done {
			stallDetachJobs.Delete(k)
		}
		return true
	})
}
