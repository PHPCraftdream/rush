package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrFinalizationFactsUnavailable = errors.New("finalization_facts_unavailable")
	ErrFinalizationSealed           = errors.New("finalization_sealed")
)

// finalizationFacts excludes the owner's driver, delegation node, and own gate.
type finalizationFacts struct {
	complete      bool
	own           int
	descendants   int
	physicalHolds int
	debt          int
	queuedCalls   int
	heldQuestion  bool
	cancelled     bool
}

func (f finalizationFacts) quiet() bool {
	return f.own == 0 && f.descendants == 0 && f.physicalHolds == 0 && f.debt == 0 && f.queuedCalls == 0 && !f.heldQuestion && !f.cancelled
}

func (f finalizationFacts) valid() bool {
	return f.complete && f.own >= 0 && f.descendants >= 0 && f.physicalHolds >= 0 && f.debt >= 0 && f.queuedCalls >= 0
}

type finalizationVerdict uint8

const (
	finalizationWaitWork finalizationVerdict = iota
	finalizationPauseQuestion
	finalizationCompose
	finalizationAccepted
)

// finalizationStamp identifies the facts visible when a provider step began.
type finalizationStamp struct {
	owner    *finalizationGate
	epoch    uint64
	serial   uint64
	revision uint64
	ready    bool
}

type finalizationCandidate struct {
	messageID string
	hash      string
}

// finalizationGate serializes admitted scope activity with final publication.
// Callers must acquire mutation admission before side effects, not afterwards.
// The same gate covers every producer for one logical owner incarnation.
type finalizationGate struct {
	mu              sync.Mutex
	epoch           uint64
	candidateSerial uint64
	revision        uint64
	mutations       int
	mustCompose     bool
	sealed          bool
	cancelled       bool
	failure         error
	accepted        finalizationCandidate
	changed         chan struct{}
}

func newFinalizationGate(epoch uint64) *finalizationGate {
	return &finalizationGate{epoch: epoch, changed: make(chan struct{})}
}

func (g *finalizationGate) signalLocked() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}

// admitMutation keeps an in-progress durable write visible until it completes.
func (g *finalizationGate) admitMutation(ownedWork bool) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancelled {
		return nil, context.Canceled
	}
	if g.sealed {
		return nil, ErrFinalizationSealed
	}
	if g.failure != nil {
		return nil, g.failure
	}
	g.revision++
	g.mutations++
	g.mustCompose = g.mustCompose || ownedWork
	g.signalLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.mutations--
			g.revision++
			g.signalLocked()
			g.mu.Unlock()
		})
	}, nil
}

func (g *finalizationGate) cancel() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.sealed {
		g.cancelled = true
		g.revision++
		g.signalLocked()
	}
}

// readLocked must use read-only facts; re-entering this gate is not permitted.
func (g *finalizationGate) readLocked(ctx context.Context, read func(context.Context) (finalizationFacts, error)) (finalizationFacts, error) {
	if err := ctx.Err(); err != nil {
		return finalizationFacts{}, err
	}
	if g.cancelled {
		return finalizationFacts{}, context.Canceled
	}
	if g.failure != nil {
		return finalizationFacts{}, g.failure
	}
	if read == nil {
		return finalizationFacts{}, ErrFinalizationFactsUnavailable
	}
	facts, err := read(ctx)
	if ctx.Err() != nil {
		return finalizationFacts{}, ctx.Err()
	}
	if err != nil {
		return finalizationFacts{}, fmt.Errorf("%w: %w", ErrFinalizationFactsUnavailable, err)
	}
	if !facts.valid() {
		return finalizationFacts{}, ErrFinalizationFactsUnavailable
	}
	if facts.cancelled {
		return finalizationFacts{}, context.Canceled
	}
	return facts, nil
}

func (g *finalizationGate) beginCandidate(ctx context.Context, read func(context.Context) (finalizationFacts, error)) (finalizationStamp, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return finalizationStamp{}, ErrFinalizationSealed
	}
	facts, err := g.readLocked(ctx, read)
	if err != nil {
		return finalizationStamp{}, err
	}
	ready := facts.quiet() && g.mutations == 0
	if !ready {
		g.mustCompose = true
	}
	g.candidateSerial++
	return finalizationStamp{owner: g, epoch: g.epoch, serial: g.candidateSerial, revision: g.revision, ready: ready}, nil
}

// trySeal performs the final facts read and publication under the admission gate.
// publish must atomically persist acceptance before emitting a terminal event.
func (g *finalizationGate) trySeal(ctx context.Context, stamp finalizationStamp, candidate finalizationCandidate, read func(context.Context) (finalizationFacts, error), publish func(context.Context, finalizationCandidate) error) (finalizationVerdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return finalizationAccepted, ErrFinalizationSealed
	}
	facts, err := g.readLocked(ctx, read)
	if err != nil {
		return finalizationWaitWork, err
	}
	if facts.heldQuestion {
		g.mustCompose = true
		g.revision++
		g.signalLocked()
		return finalizationPauseQuestion, nil
	}
	if !facts.quiet() || g.mutations != 0 {
		g.mustCompose = true
		g.revision++
		g.signalLocked()
		return finalizationWaitWork, nil
	}
	if stamp.owner != g || stamp.serial == 0 || stamp.serial != g.candidateSerial || stamp.epoch != g.epoch || stamp.revision != g.revision || g.mustCompose && !stamp.ready {
		g.mustCompose = true
		return finalizationCompose, nil
	}
	if candidate.messageID == "" || candidate.hash == "" {
		return finalizationCompose, errors.New("finalization candidate identity is missing")
	}
	if publish == nil {
		g.failure = fmt.Errorf("%w: final publication unavailable", ErrFinalizationFactsUnavailable)
		g.signalLocked()
		return finalizationWaitWork, g.failure
	}
	if err := publish(ctx, candidate); err != nil {
		if errors.Is(err, ErrFinalizationScopeChanged) {
			g.mustCompose = true
			g.revision++
			g.signalLocked()
			return finalizationCompose, nil
		}
		g.failure = fmt.Errorf("finalization publication failed: %w", err)
		g.signalLocked()
		return finalizationWaitWork, g.failure
	}
	g.accepted = candidate
	g.sealed = true
	g.signalLocked()
	return finalizationAccepted, nil
}

// waitState reads facts and arms their change signal under the same admission lock.
func (g *finalizationGate) waitState(ctx context.Context, read func(context.Context) (finalizationFacts, error)) (finalizationVerdict, <-chan struct{}, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return finalizationAccepted, nil, nil
	}
	facts, err := g.readLocked(ctx, read)
	if err != nil {
		return finalizationWaitWork, nil, err
	}
	if facts.heldQuestion {
		g.mustCompose = true
		g.revision++
		g.signalLocked()
		return finalizationPauseQuestion, nil, nil
	}
	if facts.quiet() && g.mutations == 0 {
		return finalizationCompose, nil, nil
	}
	g.mustCompose = true
	g.revision++
	g.signalLocked()
	return finalizationWaitWork, g.changed, nil
}
