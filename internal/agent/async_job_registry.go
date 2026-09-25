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
}

// AsyncCompletionSource exposes session completion events to the CLI runner.
type AsyncCompletionSource interface {
	NextAsyncCompletion(context.Context, string) (AsyncCompletion, bool, error)
	HasPendingAsyncJobs(string) bool
}

type asyncJobState struct {
	cancel       context.CancelFunc
	cli          bool
	acknowledged bool
	completion   *AsyncCompletion
}

type asyncJobSession struct {
	jobs    map[string]*asyncJobState
	ready   []AsyncCompletion
	changed chan struct{}
}

type asyncJobRegistry struct {
	mu        sync.Mutex
	sessions  map[string]*asyncJobSession
	onWebDone func(AsyncCompletion)
	// onJobCompleted fires for EVERY async job that reaches a terminal
	// state, in both origin flavors. It is the origin-independent
	// re-check trigger for parked sub-agent outcomes (see
	// subagent_outcome.go): onWebDone only ever fires for web-origin
	// jobs, while a CLI-origin job merely lands on the session's ready
	// queue — which nobody drains for a child session, so a hook here
	// is the only way a CLI child's job completion can release a parked
	// delegation. The bool is the job's cli flag (true when only the
	// ready queue will ever see it). Fired AFTER the row's own release,
	// never from finishParked (which is the release path itself).
	onJobCompleted func(AsyncCompletion, bool)
	closed         bool
}

func newAsyncJobRegistry(onWebDone func(AsyncCompletion)) *asyncJobRegistry {
	return &asyncJobRegistry{
		sessions:  make(map[string]*asyncJobSession),
		onWebDone: onWebDone,
	}
}

func (r *asyncJobRegistry) sessionLocked(sessionID string) *asyncJobSession {
	s := r.sessions[sessionID]
	if s == nil {
		s = &asyncJobSession{jobs: make(map[string]*asyncJobState), changed: make(chan struct{}, 1)}
		r.sessions[sessionID] = s
	}
	return s
}

func signalAsyncSession(s *asyncJobSession) {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (r *asyncJobRegistry) start(sessionID, toolCallID string, cli bool, cancel context.CancelFunc) error {
	if sessionID == "" || toolCallID == "" {
		return errors.New("async job requires a session and tool call ID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("async job registry is closed")
	}
	s := r.sessionLocked(sessionID)
	if _, exists := s.jobs[toolCallID]; exists {
		return fmt.Errorf("async job %s is already running", toolCallID)
	}
	if len(s.jobs) >= maxAsyncJobsPerSession {
		return fmt.Errorf("maximum number of async jobs (%d) reached", maxAsyncJobsPerSession)
	}
	s.jobs[toolCallID] = &asyncJobState{cancel: cancel, cli: cli}
	signalAsyncSession(s)
	return nil
}

func (r *asyncJobRegistry) acknowledged(sessionID, toolCallID string) {
	r.mu.Lock()
	s := r.sessions[sessionID]
	if s == nil {
		r.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	if job == nil {
		r.mu.Unlock()
		return
	}
	job.acknowledged = true
	completion, callback := r.releaseLocked(s, toolCallID)
	r.mu.Unlock()
	if callback {
		r.onWebDone(completion)
	}
}

func (r *asyncJobRegistry) finish(completion AsyncCompletion) {
	r.mu.Lock()
	s := r.sessions[completion.SessionID]
	if s == nil {
		r.mu.Unlock()
		return
	}
	job := s.jobs[completion.ToolCallID]
	if job == nil || job.completion != nil {
		r.mu.Unlock()
		return
	}
	job.completion = &completion
	ready, callback := r.releaseLocked(s, completion.ToolCallID)
	hook := r.onJobCompleted
	jobCLI := job.cli
	r.mu.Unlock()
	if callback {
		r.onWebDone(ready)
	}
	if hook != nil {
		hook(completion, jobCLI)
	}
}

// finishParked is the single emission point for a delegated sub-agent
// completion that was parked outside this registry while the child drained
// its own async work (see subagent_outcome.go). It behaves exactly like
// finish for the normal case, plus one extra branch:
//
// If the job row has already been dropped — by cancelSession, close, or an
// abort from a failed tool-result write — finish would silently return and
// the parked notice would be LOST, which is precisely the failure this
// registry exists to prevent. So a missing row is re-inserted as an
// acknowledged, already-completed row and released through the very same
// releaseLocked path, which still delivers it exactly once (releaseLocked
// deletes the row, so a later acknowledged() for the same toolCallID finds
// nothing and no-ops).
//
// cli is recorded by the parking site from the originating tool call's
// origin, and decides whether the completion lands on this session's CLI
// ready queue or on the web auto-resume callback.
func (r *asyncJobRegistry) finishParked(completion AsyncCompletion, cli bool) {
	r.mu.Lock()
	s := r.sessionLocked(completion.SessionID)
	job := s.jobs[completion.ToolCallID]
	switch {
	case job == nil:
		job = &asyncJobState{
			cli:          cli,
			acknowledged: true,
			completion:   &completion,
		}
		s.jobs[completion.ToolCallID] = job
	case job.completion != nil:
		r.mu.Unlock()
		return
	default:
		job.completion = &completion
	}
	ready, callback := r.releaseLocked(s, completion.ToolCallID)
	r.mu.Unlock()
	if callback {
		r.onWebDone(ready)
	}
}

func (r *asyncJobRegistry) releaseLocked(s *asyncJobSession, toolCallID string) (AsyncCompletion, bool) {
	job := s.jobs[toolCallID]
	if job == nil || !job.acknowledged || job.completion == nil {
		return AsyncCompletion{}, false
	}
	completion := *job.completion
	delete(s.jobs, toolCallID)
	if job.cli {
		s.ready = append(s.ready, completion)
	}
	signalAsyncSession(s)
	return completion, !job.cli && r.onWebDone != nil
}

// setJobCompletedHook installs (or, with nil, removes) the
// origin-independent completion hook. Installed by
// coordinator.installSubAgentOutcomeHooks once both registries exist.
func (r *asyncJobRegistry) setJobCompletedHook(fn func(AsyncCompletion, bool)) {
	r.mu.Lock()
	r.onJobCompleted = fn
	r.mu.Unlock()
}

func (r *asyncJobRegistry) abort(sessionID, toolCallID string) {
	r.mu.Lock()
	s := r.sessions[sessionID]
	if s == nil {
		r.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	delete(s.jobs, toolCallID)
	signalAsyncSession(s)
	r.mu.Unlock()
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

func (r *asyncJobRegistry) next(ctx context.Context, sessionID string) (AsyncCompletion, bool, error) {
	for {
		r.mu.Lock()
		s := r.sessions[sessionID]
		if s == nil {
			r.mu.Unlock()
			return AsyncCompletion{}, false, nil
		}
		if len(s.ready) > 0 {
			completion := s.ready[0]
			s.ready[0] = AsyncCompletion{}
			s.ready = s.ready[1:]
			r.mu.Unlock()
			return completion, true, nil
		}
		if len(s.jobs) == 0 {
			r.mu.Unlock()
			return AsyncCompletion{}, false, nil
		}
		changed := s.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return AsyncCompletion{}, false, ctx.Err()
		case <-changed:
		}
	}
}

func (r *asyncJobRegistry) pending(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sessionID]
	return s != nil && (len(s.jobs) > 0 || len(s.ready) > 0)
}

// running reports whether sessionID still has an async job that has NOT
// reached a terminal state, i.e. one still present in the jobs map.
//
// This is deliberately NOT pending(): pending() also counts entries sitting
// in the ready queue, and a ready-queue entry is a result that has ALREADY
// reached a terminal state and is merely waiting to be drained by the CLI
// root loop. For a child session nobody ever drains that queue, so counting
// ready entries would make a child look like it still owns live work
// forever, and any release gate built on it would never open.
func (r *asyncJobRegistry) running(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sessionID]
	return s != nil && len(s.jobs) > 0
}

// anyRunning is the process-wide form of running: is there any unfinished
// async job for ANY session? Used by the cheap in-memory pre-check in
// DescendantWorkPending so the common "nothing is pending" case costs no DB
// access.
func (r *asyncJobRegistry) anyRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if len(s.jobs) > 0 {
			return true
		}
	}
	return false
}

func (r *asyncJobRegistry) cancelSession(sessionID string) {
	r.mu.Lock()
	s := r.sessions[sessionID]
	if s == nil {
		r.mu.Unlock()
		return
	}
	jobs := s.jobs
	s.jobs = make(map[string]*asyncJobState)
	signalAsyncSession(s)
	r.mu.Unlock()
	for _, job := range jobs {
		if job.cancel != nil {
			job.cancel()
		}
	}
}

func (r *asyncJobRegistry) close() {
	r.mu.Lock()
	r.closed = true
	sessions := r.sessions
	for _, s := range sessions {
		signalAsyncSession(s)
	}
	r.mu.Unlock()
	for sessionID := range sessions {
		r.cancelSession(sessionID)
	}
}
