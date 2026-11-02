package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	apiRunMaxRetained = 1000
	apiRunTTL         = 24 * time.Hour
)

var (
	ErrAPIRunNotFound     = errors.New("API run not found")
	ErrAPIRunInvalidState = errors.New("invalid API run state transition")
	ErrAPIRunRegistryFull = errors.New("API run registry is full")
	ErrAPIRunIDGeneration = errors.New("could not generate API run ID")
)

type APIRunState string

const (
	APIRunQueued         APIRunState = "queued"
	APIRunRunning        APIRunState = "running"
	APIRunSucceeded      APIRunState = "succeeded"
	APIRunFailed         APIRunState = "failed"
	APIRunCancelled      APIRunState = "cancelled"
	APIRunAwaitingAnswer APIRunState = "awaiting_answer"
)

type APIRunError struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
}

type APIRun struct {
	ID         string       `json:"id"`
	SessionID  string       `json:"session_id"`
	TokenID    string       `json:"-"`
	State      APIRunState  `json:"state"`
	CreatedAt  time.Time    `json:"created_at"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Error      *APIRunError `json:"error,omitempty"`
	MessageID  string       `json:"message_id,omitempty"`
}

type apiRunEntry struct {
	run      APIRun
	notify   chan struct{}
	sequence uint64
}

// APIRunRegistry is process-local and should be created once per API route set.
type APIRunRegistry struct {
	mu       sync.Mutex
	runs     map[string]*apiRunEntry
	now      func() time.Time
	sequence uint64
}

func NewAPIRunRegistry(now func() time.Time) *APIRunRegistry {
	if now == nil {
		now = time.Now
	}
	return &APIRunRegistry{runs: make(map[string]*apiRunEntry), now: now}
}

func (r *APIRunRegistry) Create(sessionID, tokenID string) (APIRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	r.evictExpiredLocked(now)
	if len(r.runs) >= apiRunMaxRetained {
		r.evictOldestFinishedLocked()
	}
	if len(r.runs) >= apiRunMaxRetained {
		return APIRun{}, ErrAPIRunRegistryFull
	}
	id, err := r.newIDLocked()
	if err != nil {
		return APIRun{}, err
	}
	r.sequence++
	run := APIRun{ID: id, SessionID: sessionID, TokenID: tokenID, State: APIRunQueued, CreatedAt: now}
	r.runs[id] = &apiRunEntry{run: run, notify: make(chan struct{}), sequence: r.sequence}
	return cloneAPIRun(run), nil
}

func (r *APIRunRegistry) newIDLocked() (string, error) {
	for range apiRunMaxRetained + 1 {
		id, err := newAPIRunID()
		if err != nil {
			return "", err
		}
		if _, exists := r.runs[id]; !exists {
			return id, nil
		}
	}
	return "", ErrAPIRunIDGeneration
}

func newAPIRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.Join(ErrAPIRunIDGeneration, err)
	}
	return "run_" + hex.EncodeToString(raw[:]), nil
}

func (r *APIRunRegistry) Get(id string) (APIRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked(r.now())
	entry, ok := r.runs[id]
	if !ok {
		return APIRun{}, false
	}
	return cloneAPIRun(entry.run), true
}

func (r *APIRunRegistry) GetOwned(id, tokenID string) (APIRun, bool) {
	run, ok := r.Get(id)
	return run, ok && run.TokenID == tokenID
}

// Transition updates a run only when the requested state is a legal successor.
func (r *APIRunRegistry) Transition(id string, state APIRunState, errorCode, errorSummary, resultMessageID string) (APIRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked(r.now())
	entry, ok := r.runs[id]
	if !ok {
		return APIRun{}, ErrAPIRunNotFound
	}
	if !validAPIRunTransition(entry.run.State, state) {
		return cloneAPIRun(entry.run), ErrAPIRunInvalidState
	}
	now := r.now().UTC()
	entry.run.State = state
	if state == APIRunRunning && entry.run.StartedAt == nil {
		entry.run.StartedAt = timePointer(now.UTC())
	}
	if terminalAPIRunState(state) {
		entry.run.FinishedAt = timePointer(now.UTC())
	}
	if errorCode != "" || errorSummary != "" {
		entry.run.Error = &APIRunError{Code: errorCode, Summary: errorSummary}
	} else {
		entry.run.Error = nil
	}
	entry.run.MessageID = resultMessageID
	close(entry.notify)
	entry.notify = make(chan struct{})
	return cloneAPIRun(entry.run), nil
}

func validAPIRunTransition(from, to APIRunState) bool {
	if terminalAPIRunState(from) {
		return false
	}
	switch from {
	case APIRunQueued:
		return to == APIRunRunning || to == APIRunCancelled || to == APIRunFailed
	case APIRunRunning:
		return to == APIRunAwaitingAnswer || to == APIRunSucceeded || to == APIRunFailed || to == APIRunCancelled
	case APIRunAwaitingAnswer:
		return to == APIRunRunning || to == APIRunFailed || to == APIRunCancelled
	default:
		return false
	}
}

func terminalAPIRunState(state APIRunState) bool {
	return state == APIRunSucceeded || state == APIRunFailed || state == APIRunCancelled
}

func (r *APIRunRegistry) SnapshotAndWait(id string) (APIRun, <-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked(r.now().UTC())
	entry, ok := r.runs[id]
	if !ok {
		return APIRun{}, nil, false
	}
	return cloneAPIRun(entry.run), entry.notify, true
}

func (r *APIRunRegistry) WaitChannel(id string) (<-chan struct{}, bool) {
	_, wait, ok := r.SnapshotAndWait(id)
	return wait, ok
}

func (r *APIRunRegistry) IsSessionActive(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked(r.now())
	for _, entry := range r.runs {
		if entry.run.SessionID == id && (entry.run.State == APIRunQueued || entry.run.State == APIRunRunning) {
			return true
		}
	}
	return false
}

func (r *APIRunRegistry) ActiveSessionIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpiredLocked(r.now())
	active := make(map[string]struct{})
	for _, entry := range r.runs {
		if entry.run.State == APIRunQueued || entry.run.State == APIRunRunning {
			active[entry.run.SessionID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *APIRunRegistry) evictExpiredLocked(now time.Time) {
	for id, entry := range r.runs {
		if terminalAPIRunState(entry.run.State) && entry.run.FinishedAt != nil && now.Sub(*entry.run.FinishedAt) >= apiRunTTL {
			delete(r.runs, id)
		}
	}
}

func (r *APIRunRegistry) evictOldestFinishedLocked() {
	var oldestID string
	var oldestFinished time.Time
	for id, entry := range r.runs {
		if !terminalAPIRunState(entry.run.State) || entry.run.FinishedAt == nil {
			continue
		}
		if oldestID == "" || entry.run.FinishedAt.Before(oldestFinished) {
			oldestID, oldestFinished = id, *entry.run.FinishedAt
		}
	}
	if oldestID != "" {
		delete(r.runs, oldestID)
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func cloneAPIRun(run APIRun) APIRun {
	run.CreatedAt = run.CreatedAt.UTC()
	if run.StartedAt != nil {
		run.StartedAt = timePointer(run.StartedAt.UTC())
	}
	if run.FinishedAt != nil {
		run.FinishedAt = timePointer(run.FinishedAt.UTC())
	}
	if run.Error != nil {
		errorCopy := *run.Error
		run.Error = &errorCopy
	}
	return run
}
