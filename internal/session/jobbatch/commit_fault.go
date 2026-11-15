package jobbatch

import (
	"context"
	"sync"
	"time"
)

// CommitPhase identifies an owning-layer boundary, never request content.
type CommitPhase string

const (
	CommitPhaseWaitingHook CommitPhase = "waiting_hook"
	CommitPhaseHook        CommitPhase = "hook"
	CommitPhasePrepare     CommitPhase = "prepare"
	CommitPhaseAcquire     CommitPhase = "acquire_transaction"
	CommitPhaseStatements  CommitPhase = "statements"
	CommitPhaseCommit      CommitPhase = "commit_transaction"
)

// CommitFaultDetails is the immutable first-claim diagnostic snapshot.
type CommitFaultDetails struct {
	Kind         string
	Sequence     uint64
	Event        EventType
	Budget       time.Duration
	Elapsed      time.Duration
	Phase        CommitPhase
	PhaseElapsed time.Duration
}

// CommitFault preserves error identity without exposing hook error text.
type CommitFault struct {
	details CommitFaultDetails
	cause   error
}

func (e *CommitFault) Error() string { return ErrCommitFailed.Error() + ": " + e.details.Kind }

func (e *CommitFault) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrCommitFailed}
	}
	return []error{ErrCommitFailed, e.cause}
}

func (e *CommitFault) Details() CommitFaultDetails { return e.details }

type commitProgressKey struct{}

type commitProgress struct {
	mu      sync.Mutex
	started time.Time
	changed time.Time
	phase   CommitPhase
}

func newCommitProgress() *commitProgress {
	now := time.Now()
	return &commitProgress{started: now, changed: now, phase: CommitPhaseWaitingHook}
}

// MarkCommitPhase is inert outside a Runner-owned commit context.
func MarkCommitPhase(ctx context.Context, phase CommitPhase) {
	switch phase {
	case CommitPhaseHook, CommitPhasePrepare, CommitPhaseAcquire, CommitPhaseStatements, CommitPhaseCommit:
	default:
		return
	}
	progress, _ := ctx.Value(commitProgressKey{}).(*commitProgress)
	if progress == nil {
		return
	}
	progress.mu.Lock()
	progress.phase = phase
	progress.changed = time.Now()
	progress.mu.Unlock()
}

func (p *commitProgress) fault(kind string, tr Transition, budget time.Duration, cause error) *CommitFault {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	return &CommitFault{details: CommitFaultDetails{
		Kind: kind, Sequence: tr.Sequence, Event: tr.Event.Type, Budget: budget,
		Elapsed: now.Sub(p.started), Phase: p.phase, PhaseElapsed: now.Sub(p.changed),
	}, cause: cause}
}
