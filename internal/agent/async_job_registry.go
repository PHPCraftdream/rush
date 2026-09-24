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
	closed    bool
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
