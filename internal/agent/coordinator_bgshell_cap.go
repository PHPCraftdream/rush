// The web bg-shell auto-turn cap (docs/async-invariants.md ASYNC-09; plan
// amendments (aa), (ad)). A finished SDK background shell spends one of
// maxConsecutiveAutoResumes slots per human message at admission
// (claimAutoResumeSlot -- the arbiter's one writer of the cap, R-ARB-2); a
// completion that finds every slot spent is recorded by its notice row id
// (the arbiter state's overCap set) and never launches a turn. A release or
// tick re-check, and a queued Drain's turn-start check, may still act on the
// rows whose slot WAS spent (paced, refused, held or deferred launches), but
// never on an over-cap row: the arbiter (rule 11 of decide) defers only when
// EVERY row of the debt is an over-cap id.
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

// readTurnFactsGateSeam is a test-only hook called in readTurnFacts right
// before it takes bgArrival: the seam between the DB half and the arrival
// gate (what an over-cap completion can slip through).
var readTurnFactsGateSeam atomic.Pointer[func()]

// persistBGShellCompletion records a finished background shell and takes (or
// refuses) its auto-resume slot as ONE step relative to the cap check (the
// arbiter cap rule): the check then never sees a row without its slot
// decision. The row's id is what an over-cap completion is remembered by. A
// failed write is logged and the slot decision is made anyway, as before, but
// with no row id: nothing exists to defer, so nothing is recorded as over-cap.
//
// The shell's own async_jobs row (kind=bg_shell, claimed by
// asyncTool.claimBackgroundShellRow at the background escape) is NOT written
// here: its terminal Transition belongs to the observer registered at claim
// time (bgshell_claim.go), so it happens whether or not a notification
// callback exists. This function writes only the wake channel -- a
// kind=bg_shell_done notice, whose id is what the arbiter's over-cap set is
// keyed by -- and takes the slot decision on it. Shells without a row (started
// before the claim existed, and store-less test fixtures) get the same notice,
// which drains through the existing pull.
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
	eligible := c.persistentMode.Load() && c.autonomyEnabled()
	return c.claimAutoResumeSlot(sessionID, rowID, eligible)
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
	c.arb.pruneOverCap(sessionID, notices)
}

// bgShellOverCapCount returns how many over-cap completion rows sessionID
// currently remembers (since its last human message).
func (c *coordinator) bgShellOverCapCount(sessionID string) int {
	return c.arb.overCapCount(sessionID)
}
