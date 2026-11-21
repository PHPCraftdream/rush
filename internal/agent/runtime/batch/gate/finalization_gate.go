package gate

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

// Facts excludes the owner's driver, delegation node, and own gate.
type Facts struct {
	DurableRevision uint64
	Complete        bool
	Own             int
	Descendants     int
	PhysicalHolds   int
	Debt            int
	QueuedCalls     int
	HeldQuestion    bool
	Cancelled       bool
}

func (f Facts) quiet() bool {
	return f.Own == 0 && f.Descendants == 0 && f.PhysicalHolds == 0 && f.Debt == 0 && f.QueuedCalls == 0 && !f.HeldQuestion && !f.Cancelled
}

func (f Facts) valid() bool {
	return f.Complete && f.DurableRevision <= uint64(1<<63-1) && f.Own >= 0 && f.Descendants >= 0 && f.PhysicalHolds >= 0 && f.Debt >= 0 && f.QueuedCalls >= 0
}

type Verdict uint8

const (
	WaitWork Verdict = iota
	PauseQuestion
	Compose
	Accepted
)

// Stamp identifies the facts visible when a provider step began.
type Stamp struct {
	owner           *Gate
	epoch           uint64
	serial          uint64
	revision        uint64
	durableRevision uint64
	ready           bool
}

type Candidate struct {
	MessageID string
	Hash      string
}

// Gate serializes admitted scope activity with final publication.
// Callers must acquire mutation admission before side effects, not afterwards.
// The same gate covers every producer for one logical owner incarnation.
type Gate struct {
	mu              sync.Mutex
	epoch           uint64
	candidateSerial uint64
	revision        uint64
	mutations       int
	mustCompose     bool
	sealed          bool
	cancelled       bool
	failure         error
	accepted        Candidate
	changed         chan struct{}
}

func New(epoch uint64) *Gate {
	return &Gate{epoch: epoch, changed: make(chan struct{})}
}

func (g *Gate) signalLocked() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}

// AdmitMutation keeps an in-progress durable write visible until it completes.
func (g *Gate) AdmitMutation(ownedWork bool) (func(), error) {
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

func (g *Gate) Cancel() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.sealed {
		g.cancelled = true
		g.revision++
		g.signalLocked()
	}
}

// readLocked must use read-only facts; re-entering this gate is not permitted.
func (g *Gate) readLocked(ctx context.Context, read func(context.Context) (Facts, error)) (Facts, error) {
	if err := ctx.Err(); err != nil {
		return Facts{}, err
	}
	if g.cancelled {
		return Facts{}, context.Canceled
	}
	if g.failure != nil {
		return Facts{}, g.failure
	}
	if read == nil {
		return Facts{}, ErrFinalizationFactsUnavailable
	}
	facts, err := read(ctx)
	if ctx.Err() != nil {
		return Facts{}, ctx.Err()
	}
	if err != nil {
		return Facts{}, fmt.Errorf("%w: %w", ErrFinalizationFactsUnavailable, err)
	}
	if !facts.valid() {
		return Facts{}, ErrFinalizationFactsUnavailable
	}
	if facts.Cancelled {
		return Facts{}, context.Canceled
	}
	return facts, nil
}

func (g *Gate) BeginCandidate(ctx context.Context, read func(context.Context) (Facts, error)) (Stamp, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return Stamp{}, ErrFinalizationSealed
	}
	facts, err := g.readLocked(ctx, read)
	if err != nil {
		return Stamp{}, err
	}
	ready := facts.quiet() && g.mutations == 0
	if !ready {
		g.mustCompose = true
	}
	g.candidateSerial++
	return Stamp{owner: g, epoch: g.epoch, serial: g.candidateSerial, revision: g.revision, durableRevision: facts.DurableRevision, ready: ready}, nil
}

// TrySeal performs the final facts read and publication under the admission gate.
// publish must atomically persist acceptance before emitting a terminal event.
func (g *Gate) TrySeal(ctx context.Context, stamp Stamp, candidate Candidate, read func(context.Context) (Facts, error), publish func(context.Context, Candidate) error) (Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return Accepted, ErrFinalizationSealed
	}
	facts, err := g.readLocked(ctx, read)
	if err != nil {
		return WaitWork, err
	}
	if facts.HeldQuestion {
		g.mustCompose = true
		g.revision++
		g.signalLocked()
		return PauseQuestion, nil
	}
	if !facts.quiet() || g.mutations != 0 {
		g.mustCompose = true
		g.revision++
		g.signalLocked()
		return WaitWork, nil
	}
	if stamp.owner != g || stamp.serial == 0 || stamp.serial != g.candidateSerial || stamp.epoch != g.epoch || stamp.revision != g.revision || stamp.durableRevision != facts.DurableRevision || g.mustCompose && !stamp.ready {
		g.mustCompose = true
		return Compose, nil
	}
	if candidate.MessageID == "" || candidate.Hash == "" {
		return Compose, errors.New("finalization candidate identity is missing")
	}
	if publish == nil {
		g.failure = fmt.Errorf("%w: final publication unavailable", ErrFinalizationFactsUnavailable)
		g.signalLocked()
		return WaitWork, g.failure
	}
	if err := publish(ctx, candidate); err != nil {
		if errors.Is(err, ErrFinalizationScopeChanged) {
			g.mustCompose = true
			g.revision++
			g.signalLocked()
			return Compose, nil
		}
		g.failure = fmt.Errorf("finalization publication failed: %w", err)
		g.signalLocked()
		return WaitWork, g.failure
	}
	g.accepted = candidate
	g.sealed = true
	g.signalLocked()
	return Accepted, nil
}

// WaitState reads facts and arms their change signal under the same admission lock.
func (g *Gate) WaitState(ctx context.Context, read func(context.Context) (Facts, error)) (Verdict, <-chan struct{}, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return Accepted, nil, nil
	}
	facts, err := g.readLocked(ctx, read)
	if err != nil {
		return WaitWork, nil, err
	}
	if facts.HeldQuestion {
		g.mustCompose = true
		g.revision++
		g.signalLocked()
		return PauseQuestion, nil, nil
	}
	if facts.quiet() && g.mutations == 0 {
		return Compose, nil, nil
	}
	g.mustCompose = true
	g.revision++
	g.signalLocked()
	return WaitWork, g.changed, nil
}

// Epoch is the gate's immutable owner incarnation.
func (g *Gate) Epoch() uint64 { return g.epoch }

// BelongsTo reports whether the stamp was issued by g.
func (s Stamp) BelongsTo(g *Gate) bool { return s.owner == g }

func (s Stamp) Epoch() uint64           { return s.epoch }
func (s Stamp) Serial() uint64          { return s.serial }
func (s Stamp) DurableRevision() uint64 { return s.durableRevision }
