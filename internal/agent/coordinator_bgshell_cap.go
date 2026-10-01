// The web bg-shell auto-turn cap (docs/async-invariants.md ASYNC-09; plan
// amendments (aa), (ad)). A finished SDK background shell spends one of
// maxConsecutiveAutoResumes slots per human message at admission
// (claimAutoResume); a completion that finds every slot spent is recorded by
// its notice row id (bgShellOverCap) and never launches a turn. A release or
// tick re-check, and a queued Drain's turn-start check, may still act on the
// rows whose slot WAS spent (paced, refused, held or deferred launches), but
// never on an over-cap row: they read the durable debt and defer only when
// EVERY row of it is an over-cap id (bgShellCapDeferred).
package agent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// ctxMutex is a mutex whose waiter can give up with its context. The zero value
// is ready to use.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *ctxMutex) init() { m.once.Do(func() { m.ch = make(chan struct{}, 1) }) }

// lock acquires m, or returns ctx's error.
func (m *ctxMutex) lock(ctx context.Context) error {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *ctxMutex) unlock() { <-m.ch }

// bgArrivalInsertedSeam is a test-only hook called between the notice insert and
// the slot decision of persistBGShellCompletion, still under bgArrival.
var bgArrivalInsertedSeam atomic.Pointer[func()]

// bgArrivalEnteredSeam is a test-only hook called with the session id as
// persistBGShellCompletion is entered, before it takes bgArrival.
var bgArrivalEnteredSeam atomic.Pointer[func(sessionID string)]

// persistBGShellCompletion writes a finished background shell's notice row and
// takes (or refuses) its auto-resume slot as ONE step relative to the cap check
// (bgShellCapDeferred): the check then never sees a row without its slot
// decision. The row's id is what an over-cap completion is remembered by. A
// failed insert is logged and the slot decision is made anyway, as before, but
// with no row id: nothing exists to defer, so nothing is recorded as over-cap.
func (c *coordinator) persistBGShellCompletion(sessionID, shellID, summary string) (claimed bool) {
	if seam := bgArrivalEnteredSeam.Load(); seam != nil {
		(*seam)(sessionID)
	}
	_ = c.bgArrival.lock(context.Background()) // cannot fail: Background never ends
	defer c.bgArrival.unlock()
	var rowID int64
	if c.asyncJobs != nil && c.asyncJobs.store != nil {
		c.pruneBGShellOverCap(sessionID)
		insertCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		id, err := c.asyncJobs.store.InsertSessionNoticeReturningID(insertCtx, sessionID, session.NoticeKindBGShellDone, summary, true, "")
		cancel()
		if err != nil {
			slog.Error("failed to persist background-shell-done notice",
				"session_id", sessionID, "shell_id", shellID, "err", err)
		} else {
			rowID = id
		}
	}
	if seam := bgArrivalInsertedSeam.Load(); seam != nil {
		(*seam)()
	}
	return c.claimAutoResume(sessionID, rowID)
}

// bgShellOverCapCount returns how many over-cap completion rows sessionID
// currently remembers (since its last human message).
func (c *coordinator) bgShellOverCapCount(sessionID string) int {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	return len(c.bgShellOverCap[sessionID])
}

// pruneBGShellOverCapLocked drops the over-cap ids of sessionID that are not in
// debt (reacted, closed or void: the row no longer needs deferring), so the set
// follows the durable debt instead of growing with every completion. notices
// must be the COMPLETE notice debt: an id missing from a partial list would be
// forgotten while still owed. Caller holds autoResumeMu.
func (c *coordinator) pruneBGShellOverCapLocked(sessionID string, notices []session.PendingNoticeDebt) {
	over := c.bgShellOverCap[sessionID]
	if len(over) == 0 {
		return
	}
	owed := make(map[int64]struct{}, len(notices))
	for _, n := range notices {
		owed[n.ID] = struct{}{}
	}
	for id := range over {
		if _, ok := owed[id]; !ok {
			delete(over, id)
		}
	}
	if len(over) == 0 {
		delete(c.bgShellOverCap, sessionID)
	}
}

// pruneBGShellOverCap is the best-effort prune at a completion's arrival (under
// bgArrival): an unreadable debt leaves the set as it is.
func (c *coordinator) pruneBGShellOverCap(sessionID string) {
	if c.bgShellOverCapCount(sessionID) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recheckDebtCheckBudget)
	defer cancel()
	_, notices, _, err := c.asyncJobs.store.PendingInclusiveDebtRows(ctx, sessionID)
	if err != nil {
		return
	}
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	c.pruneBGShellOverCapLocked(sessionID, notices)
}

// bgShellCapDeferred decides a release, tick or turn-start re-check (a launch
// that spends no slot) of sessionID's debt when every slot is spent: deferred
// only when the ENTIRE debt is bg-shell notices and every one of them is an
// over-cap completion (its row id was recorded when it arrived with no slot
// left). A row whose slot was spent but whose launch was paced, refused, held
// or deferred, or whose pull keeps failing while newer rows are reacted, is not
// in the set, so it is retried (and closed at K=3) like any other debt; a
// slot Drain's own pulled-but-unreacted row is one of them, so a Drain is never
// refused for the slot it holds. The gate is held across the debt read and the
// set read: a completion is either fully visible to them or not at all. The
// read also prunes ids that left the debt. An unreadable input is an error
// (drainPolicy fails closed).
func (c *coordinator) bgShellCapDeferred(ctx context.Context, sessionID string) (bool, error) {
	if c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes {
		return false, nil
	}
	if err := c.bgArrival.lock(ctx); err != nil {
		return false, err
	}
	defer c.bgArrival.unlock()
	if c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes {
		return false, nil // a human message re-armed the cap meanwhile
	}
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return false, nil
	}
	hasJobDebt, notices, _, err := c.asyncJobs.store.PendingInclusiveDebtRows(ctx, sessionID)
	if err != nil {
		return false, err
	}
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	c.pruneBGShellOverCapLocked(sessionID, notices)
	if hasJobDebt || len(notices) == 0 {
		return false, nil
	}
	over := c.bgShellOverCap[sessionID]
	for _, n := range notices {
		if n.Kind != session.NoticeKindBGShellDone {
			return false, nil
		}
		if _, ok := over[n.ID]; !ok {
			return false, nil
		}
	}
	return true, nil
}
