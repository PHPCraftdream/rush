// workLedger is the single in-memory work registry (phase 1 of the async
// structured-concurrency rewrite). It replaces asyncJobRegistry (former
// async_job_registry.go, 353 lines) and subAgentOutcomeRegistry (former
// subagent_outcome.go, 740 lines) with one type: the delegation-parking half
// lives in work_ledger_delegation.go, the plain-job half here.
//
// Delivery routing is unchanged from today (docs/plans/2026-09-27-async-
// phase1-spec.md §1.6): a job's completion lands on its owner's ready queue
// when the owner is a drained CLI loop (or there is no web callback at all),
// otherwise it is handed to onWebDone. See deliverLocked.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

const maxAsyncJobsPerSession = 50

// AsyncCompletion is the result of a tool that returned before its work ended.
type AsyncCompletion struct {
	SessionID  string
	ToolCallID string
	ToolName   string
	Content    string
	IsError    bool
	// cli is the origin of the job that produced this completion. Set only
	// on the wake-callback path, so the woken turn keeps the job's origin.
	cli bool
}

// AsyncCompletionSource exposes session completion events to the CLI runner.
// The runner claims its root session first; completions of every other
// session wake that session instead of queueing where nobody reads them.
type AsyncCompletionSource interface {
	ClaimAsyncCompletions(string)
	NextAsyncCompletion(context.Context, string) (AsyncCompletion, bool, error)
	HasPendingAsyncJobs(string) bool
}

// sessionJobs is one owner's view of the ledger: jobs it still owns (present
// == "not yet delivered"), plus its drained ready queue.
type sessionJobs struct {
	jobs    map[string]*asyncJob
	ready   []AsyncCompletion
	changed chan struct{}
	// drained marks a session whose ready queue a CLI loop consumes (the
	// root of a `rush run`). Sticky: completions arriving after that loop
	// returned still queue instead of starting a turn in a finished run.
	drained bool
}

// workLedger is the single owner of in-memory work state: plain async jobs
// (bash/run_command) and delegation jobs (agent/agentic_fetch), sharing one
// mutex and one per-job state machine (asyncJob.transitionToTerminal).
type workLedger struct {
	mu        sync.Mutex
	bySession map[string]*sessionJobs // owner -> its jobs
	// byChild indexes delegation jobs (childSession != "") by the child
	// session they ran in, FIFO per child. See work_ledger_delegation.go.
	byChild   map[string][]*asyncJob
	onWebDone func(AsyncCompletion)

	// coord backs childScopeDrained's background/mailbox-busy checks (see
	// work_ledger_delegation.go) and recheckChild's DB refresh. Nil-safe:
	// isolated ledger tests never set it. Same pattern as
	// subAgentOutcomeRegistry.coord before it.
	coord *coordinator

	// tickStop is the safety-net ticker (subAgentOutcomeTickInterval).
	// Phase 1 keeps it -- the design doc assigns its removal to phase 3,
	// alongside the event-driven scope accounting that replaces it.
	tickStop chan struct{}
	closed   bool
}

func newWorkLedger(onWebDone func(AsyncCompletion)) *workLedger {
	return &workLedger{
		bySession: make(map[string]*sessionJobs),
		byChild:   make(map[string][]*asyncJob),
		onWebDone: onWebDone,
	}
}

func (l *workLedger) sessionLocked(owner string) *sessionJobs {
	s := l.bySession[owner]
	if s == nil {
		s = &sessionJobs{jobs: make(map[string]*asyncJob), changed: make(chan struct{}, 1)}
		l.bySession[owner] = s
	}
	return s
}

func signalWorkSession(s *sessionJobs) {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Start registers a job for (owner, toolCallID), or -- if that key already
// has a row for the SAME tool and input -- returns the EXISTING job with
// existing=true. That idempotency (closing #1038) means the caller
// (asyncTool.Run) must not start a second executor for a repeated tool call:
// a provider retrying the same call must not repeat the underlying side
// effect (e.g. a second `rm -rf`). A reused id with a DIFFERENT tool or input
// is a distinct call colliding on the id and is refused, so it is never
// silently reported as started without running.
func (l *workLedger) Start(owner, toolCallID, input, toolName, childSession string, cli bool, cancel context.CancelFunc) (*asyncJob, bool, error) {
	if owner == "" || toolCallID == "" {
		return nil, false, errors.New("async job requires a session and tool call ID")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, false, errors.New("async job registry is closed")
	}
	s := l.sessionLocked(owner)
	if existing, ok := s.jobs[toolCallID]; ok {
		if existing.toolName != toolName || existing.input != input {
			return nil, false, fmt.Errorf("async job %s is already running", toolCallID)
		}
		return existing, true, nil
	}
	if len(s.jobs) >= maxAsyncJobsPerSession {
		return nil, false, fmt.Errorf("maximum number of async jobs (%d) reached", maxAsyncJobsPerSession)
	}
	job := &asyncJob{owner: owner, toolCallID: toolCallID, input: input, toolName: toolName, childSession: childSession, cli: cli, cancel: cancel}
	s.jobs[toolCallID] = job
	signalWorkSession(s)
	return job, false, nil
}

// markDrained records that a CLI loop consumes owner's ready queue. CLI
// completions of any other session wake that session through the callback
// instead, like web-origin ones: nobody drains its queue.
func (l *workLedger) markDrained(owner string) {
	if owner == "" {
		return
	}
	l.mu.Lock()
	l.sessionLocked(owner).drained = true
	l.mu.Unlock()
}

// deliverLocked delivers job if it is still present in bySession[owner].jobs
// (i.e. not already delivered by a concurrent winner of the state race),
// terminal, and announced. It is safe to call unconditionally after ANY
// transitionToTerminal attempt, win or lose: a losing caller's job is either
// already gone (this no-ops) or still mid-race (this reads whatever the
// CURRENT, single, coherent state+result is -- transitionToTerminal sets
// both under the same lock hold, so a reader here never sees a torn mix of
// two different outcomes' fields).
//
// Routing (ready queue vs onWebDone) is byte-for-byte the same condition as
// today's queuesLocked (async_job_registry.go): job.cli && (s.drained ||
// onWebDone == nil).
//
// Caller must hold l.mu, and must invoke the returned callback (if any)
// AFTER releasing it.
func (l *workLedger) deliverLocked(owner string, job *asyncJob) (AsyncCompletion, bool) {
	s := l.bySession[owner]
	if s == nil {
		return AsyncCompletion{}, false
	}
	current, present := s.jobs[job.toolCallID]
	if !present || current != job || !job.state.terminal() || !job.announced {
		return AsyncCompletion{}, false
	}
	completion := AsyncCompletion{
		SessionID:  owner,
		ToolCallID: job.toolCallID,
		ToolName:   job.toolName,
		Content:    job.result.content,
		IsError:    job.result.isError,
	}
	delete(s.jobs, job.toolCallID)
	if job.childSession != "" {
		l.gcChildLocked(job.childSession)
	}
	queued := job.cli && (s.drained || l.onWebDone == nil)
	if queued {
		s.ready = append(s.ready, completion)
	}
	signalWorkSession(s)
	completion.cli = job.cli
	return completion, !queued && l.onWebDone != nil
}

// acknowledged is the ack-gate: marks that the "started" tool result for
// (sessionID, toolCallID) is persisted (agent_turn_stream.go's onToolResult,
// called after a successful messages.Create), then attempts delivery. Name
// unchanged from today (async_job_registry.go's acknowledged) -- the caller
// is not touched by phase 1.
func (l *workLedger) acknowledged(sessionID, toolCallID string) {
	l.mu.Lock()
	s := l.bySession[sessionID]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	if job == nil {
		l.mu.Unlock()
		return
	}
	job.announced = true
	completion, callback := l.deliverLocked(sessionID, job)
	l.mu.Unlock()
	if callback {
		l.onWebDone(completion)
	}
}

// abort drops (sessionID, toolCallID) and cancels its executor context.
// Called ONLY when the "started" tool result write itself failed -- strictly
// before Announce could ever be called for this id, so there is nothing to
// preserve (ASYNC-05). Name unchanged from today (async_job_registry.go's
// abort).
func (l *workLedger) abort(sessionID, toolCallID string) {
	l.mu.Lock()
	s := l.bySession[sessionID]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	delete(s.jobs, toolCallID)
	signalWorkSession(s)
	l.mu.Unlock()
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

// finish is the terminal transition for a PLAIN job (bash/run_command). Not
// used for a delegation (agent/agentic_fetch) -- see armDelegation.
func (l *workLedger) finish(owner, toolCallID string, result jobResult) {
	l.mu.Lock()
	s := l.bySession[owner]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	if job == nil {
		l.mu.Unlock()
		return
	}
	state := phaseCompleted
	if result.isError {
		state = phaseFailed
	}
	job.transitionToTerminal(state, result)
	completion, callback := l.deliverLocked(owner, job)
	l.mu.Unlock()
	if callback {
		l.onWebDone(completion)
	}
}

func (l *workLedger) next(ctx context.Context, sessionID string) (AsyncCompletion, bool, error) {
	for {
		l.mu.Lock()
		s := l.bySession[sessionID]
		if s == nil {
			l.mu.Unlock()
			return AsyncCompletion{}, false, nil
		}
		if len(s.ready) > 0 {
			completion := s.ready[0]
			s.ready[0] = AsyncCompletion{}
			s.ready = s.ready[1:]
			l.mu.Unlock()
			return completion, true, nil
		}
		if len(s.jobs) == 0 {
			l.mu.Unlock()
			return AsyncCompletion{}, false, nil
		}
		changed := s.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return AsyncCompletion{}, false, ctx.Err()
		case <-changed:
		}
	}
}

func (l *workLedger) pending(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.bySession[sessionID]
	return s != nil && (len(s.jobs) > 0 || len(s.ready) > 0)
}

// running reports whether sessionID still has a job that has NOT reached
// delivery, i.e. one still present in the jobs map (running OR terminal but
// not yet announced/delivered). Deliberately NOT pending(): a ready-queue
// entry is already-delivered work waiting to be drained by the CLI root
// loop, and nobody drains a child session's queue, so counting it would make
// a child look like it owns live work forever.
func (l *workLedger) running(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.bySession[sessionID]
	return s != nil && len(s.jobs) > 0
}

// anyRunning is the process-wide form of running: the cheap in-memory
// pre-check DescendantWorkPending uses so the common "nothing pending" case
// costs no DB access.
func (l *workLedger) anyRunning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.bySession {
		if len(s.jobs) > 0 {
			return true
		}
	}
	return false
}

// close cancels every job in every session (as cancelSession does per
// session) and refuses further Start calls. Also stops the safety-net ticker.
func (l *workLedger) close() {
	l.mu.Lock()
	l.closed = true
	owners := make([]string, 0, len(l.bySession))
	for owner, s := range l.bySession {
		owners = append(owners, owner)
		signalWorkSession(s)
	}
	stop := l.tickStop
	l.tickStop = nil
	l.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	for _, owner := range owners {
		l.cancelSession(owner)
	}
}
