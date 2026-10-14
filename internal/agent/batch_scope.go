package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// batchAgentScope is the per-incarnation scope state of one agent leaf: the
// node's scope closes only when its turn loop ended AND no question is held
// AND every descendant is confirmed terminal. A confirmed closure happens
// exactly once; all transitions are serialized by mu. Inert until R11.
type batchAgentScope struct {
	mu                   sync.Mutex
	ended                bool // The agent turn loop ended.
	stopped              bool // A Stop request reached this incarnation.
	held                 *batchAgentHeldQuestion
	closed               bool // Confirmed closure; never unset.
	live                 bool // Descendants observed live.
	readsIn              bool // At least one descendant read completed.
	questionSequence     uint64
	questionPublishError error
	changed              chan struct{}
}

// batchAgentHeldQuestion is one question awaiting an injected answer.
type batchAgentHeldQuestion struct {
	depth    int
	answer   chan json.RawMessage
	resolved bool
}

// batchAgentInjectMessage is the wire shape of an injected answer: it names
// the nesting depth it addresses so an answer can never reach another depth.
type batchAgentInjectMessage struct {
	Depth int             `json:"depth"`
	Text  json.RawMessage `json:"text,omitempty"`
}

// Errors of Inject and the question channel. All are deterministic: a caller
// can branch on them instead of losing a message silently.
var (
	// ErrBatchAgentNotHeld: no question is held, or the addressed depth does
	// not match the held one.
	ErrBatchAgentNotHeld = errors.New("batch agent: no held question at that depth")
	// ErrBatchAgentStopped: the incarnation is stopping or stopped; an
	// injected message would be lost, so it is rejected instead.
	ErrBatchAgentStopped = errors.New("batch agent: incarnation is stopped")
	// ErrBatchAgentStaleGeneration: the incarnation is terminal; a message
	// must never reach a new incarnation through the old handle.
	ErrBatchAgentStaleGeneration = errors.New("batch agent: stale incarnation")
	// ErrBatchAgentQuestionHeld: a second question cannot be held at the
	// same time.
	ErrBatchAgentQuestionHeld = errors.New("batch agent: question already held")
)

func newBatchAgentScope() *batchAgentScope {
	return &batchAgentScope{changed: make(chan struct{}, 1)}
}

// signal wakes every waiter conservatively; waiters re-read all facts.
func (s *batchAgentScope) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// beginStop records the stop intent before the run context is cancelled.
func (s *batchAgentScope) beginStop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.signal()
}

// stop reports whether a Stop request reached this incarnation.
func (s *batchAgentScope) stop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// turnEnded records that the turn loop finished.
func (s *batchAgentScope) turnEnded() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	s.signal()
}

// descendants records a live-descendant observation. A read that is not
// complete never makes the scope look finished: incomplete keeps it open.
func (s *batchAgentScope) descendants(live, complete bool) {
	s.mu.Lock()
	s.readsIn = true
	s.live = live || !complete // Conservative read: unknown looks live.
	s.mu.Unlock()
	s.signal()
}

// hold registers a question at the given depth. It fails while another
// question is held or the incarnation is stopping or already closed.
func (s *batchAgentScope) hold(depth int) (*batchAgentHeldQuestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return nil, ErrBatchAgentStaleGeneration
	case s.stopped:
		return nil, ErrBatchAgentStopped
	case s.held != nil:
		return nil, ErrBatchAgentQuestionHeld
	}
	h := &batchAgentHeldQuestion{depth: depth, answer: make(chan json.RawMessage, 1)}
	s.held = h
	s.signal()
	return h, nil
}

// discardHold releases a held question whose asker went away unanswered.
func (s *batchAgentScope) discardHold(h *batchAgentHeldQuestion) {
	s.mu.Lock()
	if s.held == h {
		s.held = nil
	}
	s.mu.Unlock()
	s.signal()
}

// deliver routes an injected message to the held question at exactly its
// depth. Any other depth is rejected, never buffered for later depths.
func (s *batchAgentScope) deliver(msg batchAgentInjectMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stopped {
		return ErrBatchAgentStopped
	}
	if s.held == nil || s.held.depth != msg.Depth {
		return ErrBatchAgentNotHeld
	}
	s.held.resolved = true
	s.held.answer <- msg.Text
	s.held = nil
	s.signal()
	return nil
}

// closedNow reports whether every closure fact holds: the turn ended, no
// question is held and descendants are confirmed terminal.
func (s *batchAgentScope) closedNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended && s.held == nil && s.readsIn && !s.live
}

// markClosed latches the confirmed closure exactly once.
func (s *batchAgentScope) markClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	return true
}

// waitChange blocks until a fact changed or ctx is done.
func (s *batchAgentScope) waitChange(ctx context.Context) error {
	select {
	case <-s.changed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
