package batchlife

import (
	"context"
	"sync"
	"time"
)

// DriverHints is ONE struct per coordinator aggregating the per-session
// hint counter (BumpHint/HintSeqOf/WaitForHint) and the external-driver
// registration mirror (ClaimExternalDriver/ReleaseExternalDriver/
// IsExternalDriver). The hint counter is the watermark the arbiter launch
// gate compares (Gate.HintNow vs Gate.HintSeen); the external-driver mirror
// is this process's fast path -- the DURABLE marker lives in session_drivers
// via the HostStore, and the mirror only answers "this process drives it" for
// the hot-path wake/step checks.
type DriverHints struct {
	mu       sync.Mutex
	seq      map[string]uint64
	waiters  map[string]map[chan struct{}]struct{}
	external map[string]bool
}

func NewDriverHints() *DriverHints {
	return &DriverHints{
		seq:      make(map[string]uint64),
		waiters:  make(map[string]map[chan struct{}]struct{}),
		external: make(map[string]bool),
	}
}

// BumpHint advances sessionID's counter and wakes every waiter. Buffered
// channels: a waiter not parked at this instant still sees the newer
// sequence number on its next read.
func (h *DriverHints) BumpHint(owner string) {
	if h == nil || owner == "" {
		return
	}
	h.mu.Lock()
	h.seq[owner]++
	var wake []chan struct{}
	for ch := range h.waiters[owner] {
		wake = append(wake, ch)
	}
	h.mu.Unlock()
	for _, ch := range wake {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *DriverHints) HintSeqOf(owner string) uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq[owner]
}

// WaitForHint blocks until sessionID's counter advances past since, ctx is
// done, or a newer fact bumped the counter while the waiter was registering
// (the registered-channel receive is best-effort: the re-read after wake is
// the authority).
func (h *DriverHints) WaitForHint(ctx context.Context, owner string, since uint64) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	if h.seq[owner] != since {
		h.mu.Unlock()
		return true
	}
	ch := make(chan struct{}, 1)
	if h.waiters[owner] == nil {
		h.waiters[owner] = make(map[chan struct{}]struct{})
	}
	h.waiters[owner][ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.waiters[owner], ch)
		h.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	case <-time.After(reactionWaitHintTimeout):
		return h.HintSeqOf(owner) != since
	}
}

// reactionWaitHintTimeout bounds one hint wait: the reaction wait must never
// outlive the re-check pass that would retry the launch anyway.
var reactionWaitHintTimeout = 60 * time.Second

func (h *DriverHints) ClaimExternalDriver(owner string) {
	if h == nil || owner == "" {
		return
	}
	h.mu.Lock()
	h.external[owner] = true
	h.mu.Unlock()
}

func (h *DriverHints) ReleaseExternalDriver(owner string) {
	if h == nil || owner == "" {
		return
	}
	h.mu.Lock()
	delete(h.external, owner)
	h.mu.Unlock()
}

func (h *DriverHints) IsExternalDriver(owner string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.external[owner]
}

// IdleCandidates lists the session ids with no external-driver mirror entry
// (the 60s pass's sweep input).
func (h *DriverHints) IdleCandidates() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for id := range h.seq {
		if !h.external[id] {
			out = append(out, id)
		}
	}
	return out
}

// DropIfIdle deletes id's hint counter and mirror entry unless a driver
// arrived in between; reports whether anything was dropped.
func (h *DriverHints) DropIfIdle(id string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.external[id] {
		return false
	}
	_, hadSeq := h.seq[id]
	delete(h.seq, id)
	delete(h.external, id)
	return hadSeq
}

// Подписка использует тот же hint ledger; после сигнала факты перечитываются.
func (h *DriverHints) Subscribe(owner string) (<-chan struct{}, func()) {
	if h == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.waiters == nil {
		h.waiters = make(map[string]map[chan struct{}]struct{})
	}
	if h.waiters[owner] == nil {
		h.waiters[owner] = make(map[chan struct{}]struct{})
	}
	h.waiters[owner][ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { h.mu.Lock(); delete(h.waiters[owner], ch); h.mu.Unlock() }) }
}
