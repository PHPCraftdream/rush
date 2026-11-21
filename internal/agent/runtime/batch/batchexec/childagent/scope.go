package childagent

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
)

// Scope owns the per-incarnation closure, publication fence and answer state.
type Scope struct {
	mu                   sync.Mutex
	stopped, closed      bool
	held                 *HeldQuestion
	questionSequence     uint64
	ended, live, readsIn bool
	changed              chan struct{}
}

// HeldQuestion is one question awaiting an injected answer.
type HeldQuestion struct {
	depth    int
	sequence uint64 // Zero until publication stamps it; delivery fences on it.
	answer   chan json.RawMessage
	resolved bool
}

// Answer returns the receive side of the buffered answer channel.
func (h *HeldQuestion) Answer() <-chan json.RawMessage { return h.answer }

// Sequence returns the fence stamped by PublishQuestion. The asker reads it
// after publication returns.
func (h *HeldQuestion) Sequence() uint64 { return h.sequence }

func NewScope() *Scope { return &Scope{changed: make(chan struct{}, 1)} }

// HeldDepth reports the depth of the current hold under the scope lock.
func (s *Scope) HeldDepth() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		return 0, false
	}
	return s.held.depth, true
}

// PublishQuestion stamps the sequence and invokes publish under the same lock
// as delivery and stop. The callback must not re-enter the scope.
func (s *Scope) PublishQuestion(publish func(sequence uint64) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.closed {
		return batchinst.ErrBatchAgentStopped
	}
	s.questionSequence++
	sequence := s.questionSequence
	if s.held != nil {
		s.held.sequence = sequence
	}
	err := publish(sequence)
	if err != nil {
		s.held = nil
		s.Signal()
	}
	return err
}

// Signal queues a coalesced wake; waiters re-read all facts.
func (s *Scope) Signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// BeginStop records the stop intent before the run context is cancelled.
func (s *Scope) BeginStop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.Signal()
}

// Stop reports whether a Stop request reached this incarnation.
func (s *Scope) Stop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// TurnEnded records that the turn loop finished.
func (s *Scope) TurnEnded() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	s.Signal()
}

// Descendants records a live-descendant observation. A read that is not
// complete never makes the scope look finished: incomplete keeps it open.
func (s *Scope) Descendants(live, complete bool) {
	s.mu.Lock()
	s.readsIn = true
	s.live = live || !complete // Conservative read: unknown looks live.
	s.mu.Unlock()
	s.Signal()
}

// Hold registers a question at the given depth. It fails while another
// question is held or the incarnation is stopping or already closed.
func (s *Scope) Hold(depth int) (*HeldQuestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return nil, batchinst.ErrBatchAgentStaleGeneration
	case s.stopped:
		return nil, batchinst.ErrBatchAgentStopped
	case s.held != nil:
		return nil, batchinst.ErrBatchAgentQuestionHeld
	}
	h := &HeldQuestion{depth: depth, answer: make(chan json.RawMessage, 1)}
	s.held = h
	s.Signal()
	return h, nil
}

// DiscardHold releases a held question whose asker went away unanswered.
func (s *Scope) DiscardHold(h *HeldQuestion) {
	s.mu.Lock()
	if s.held == h {
		s.held = nil
	}
	s.mu.Unlock()
	s.Signal()
}

// Deliver routes an injected message to the held question at exactly its
// depth AND exactly the sequence publishQuestion stamped onto it: two
// questions of one incarnation at the same depth are distinguished, so a
// stale captured answer can never resolve a later question. Any other depth
// or sequence is rejected, never buffered for later.
func (s *Scope) Deliver(msg batchinst.AgentInjectMessage) error {
	return s.DeliverWithCommit(msg, nil)
}

func (s *Scope) DeliverWithCommit(msg batchinst.AgentInjectMessage, commit func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stopped {
		return batchinst.ErrBatchAgentStopped
	}
	held := s.held
	if held == nil || held.depth != msg.Depth {
		return batchinst.ErrBatchAgentNotHeld
	}
	if msg.Sequence == 0 || held.sequence != msg.Sequence {
		return batchinst.ErrBatchAgentNotHeld
	}
	if commit != nil {
		if err := commit(); err != nil {
			return err
		}
	}
	held.resolved = true
	held.answer <- msg.Text
	s.held = nil
	s.Signal()
	return nil
}

// ClosedNow reports whether every closure fact holds: the turn ended, no
// question is held and descendants are confirmed terminal.
func (s *Scope) ClosedNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended && s.held == nil && s.readsIn && !s.live
}

// MarkClosed latches the confirmed closure exactly once.
func (s *Scope) MarkClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	return true
}

// WaitChange blocks until a fact changed or ctx is done.
func (s *Scope) WaitChange(ctx context.Context) error {
	select {
	case <-s.changed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
