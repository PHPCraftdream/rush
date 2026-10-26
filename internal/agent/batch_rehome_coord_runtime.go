// Rehomed retained in-process state from the deleted workLedger
// (work_ledger_reaction.go, work_ledger_driver.go): the per-session fact-hint
// counter + waiter set, the external-driver registration mirror, the durable
// foreign-driver read, and the SessionNoticeStore binding the completion
// notice writers (supervision tick, reaction-chain marker, bg-shell done,
// held child question) use. None of this is ledger state: the hint counter
// and the driver mirror are per-process coordination memory that outlives the
// ledger, the driver READ is the retained session_drivers host marker, and
// session_notices is retained notice infrastructure.
package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// batchDriverHints is ONE struct per coordinator aggregating the per-session
// hint counter (bumpHint/hintSeqOf/waitForHint) and the external-driver
// registration mirror (claimExternalDriver/releaseExternalDriver/
// isExternalDriver). The hint counter is the watermark the arbiter launch
// gate compares (Gate.HintNow vs Gate.HintSeen); the external-driver mirror
// is this process's fast path -- the DURABLE marker lives in session_drivers
// via the HostStore, and the mirror only answers "this process drives it" for
// the hot-path wake/step checks.
type batchDriverHints struct {
	mu       sync.Mutex
	seq      map[string]uint64
	waiters  map[string]map[chan struct{}]struct{}
	external map[string]bool
}

func newBatchDriverHints() *batchDriverHints {
	return &batchDriverHints{
		seq:      make(map[string]uint64),
		waiters:  make(map[string]map[chan struct{}]struct{}),
		external: make(map[string]bool),
	}
}

// bumpHint advances sessionID's counter and wakes every waiter. Buffered
// channels: a waiter not parked at this instant still sees the newer
// sequence number on its next read.
func (h *batchDriverHints) bumpHint(owner string) {
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

func (h *batchDriverHints) hintSeqOf(owner string) uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq[owner]
}

// waitForHint blocks until sessionID's counter advances past since, ctx is
// done, or a newer fact bumped the counter while the waiter was registering
// (the registered-channel receive is best-effort: the re-read after wake is
// the authority).
func (h *batchDriverHints) waitForHint(ctx context.Context, owner string, since uint64) bool {
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
		return h.hintSeqOf(owner) != since
	}
}

// reactionWaitHintTimeout bounds one hint wait: the reaction wait must never
// outlive the re-check pass that would retry the launch anyway.
var reactionWaitHintTimeout = 60 * time.Second

func (h *batchDriverHints) claimExternalDriver(owner string) {
	if h == nil || owner == "" {
		return
	}
	h.mu.Lock()
	h.external[owner] = true
	h.mu.Unlock()
}

func (h *batchDriverHints) releaseExternalDriver(owner string) {
	if h == nil || owner == "" {
		return
	}
	h.mu.Lock()
	delete(h.external, owner)
	h.mu.Unlock()
}

func (h *batchDriverHints) isExternalDriver(owner string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.external[owner]
}

// idleCandidates lists the session ids with no external-driver mirror entry
// (the 60s pass's sweep input).
func (h *batchDriverHints) idleCandidates() []string {
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

// dropIfIdle deletes id's hint counter and mirror entry unless a driver
// arrived in between; reports whether anything was dropped.
func (h *batchDriverHints) dropIfIdle(id string) bool {
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

// foreignLiveDriver reports whether another live process drives owner's
// session (a `rush run` loop whose durable session_drivers marker names a
// host that is alive or not provably dead). Rehome of work_ledger_driver.go:
// the read goes through the bound HostStore (SetBatchDriverClaimer wires it;
// the claim/release and the foreign read are the same store in production).
// A coordinator without a HostStore-shaped claimer (bare fixtures) reports
// false -- the conservative answer is live/fact-driven, but a fixture has no
// other process to be foreign.
func (c *coordinator) foreignLiveDriver(ctx context.Context, owner string) (bool, error) {
	if c == nil {
		return false, nil
	}
	hs, ok := c.batchDriverClaimer.(*session.HostStore)
	if !ok || hs == nil {
		return false, nil
	}
	_, foreign, err := hs.ForeignLiveDriver(ctx, owner)
	return foreign, err
}

// SetCoordinatorSessionNoticeStore binds the retained SessionNoticeStore
// onto a coordinator (internal/app composition root). The completion-notice
// writers (supervision tick, reaction-chain marker, bg-shell done, held
// child question) persist through it; nil store or coordinator is refused.
func SetCoordinatorSessionNoticeStore(c Coordinator, s *session.SessionNoticeStore) error {
	if c == nil {
		return errors.New("session notice store: nil coordinator")
	}
	if s == nil {
		return errors.New("session notice store: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("session notice store: unsupported coordinator type")
	}
	co.noticeStore = s
	return nil
}

// sessionNoticeStore returns the bound SessionNoticeStore, or nil.
func (c *coordinator) sessionNoticeStore() *session.SessionNoticeStore {
	if c == nil {
		return nil
	}
	return c.noticeStore
}
