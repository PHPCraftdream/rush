// The web bg-shell auto-turn cap (docs/async-invariants.md ASYNC-09; plan
// amendment (aa)). A finished SDK background shell spends one of
// maxConsecutiveAutoResumes slots per human message at admission
// (claimAutoResume); a completion that finds every slot spent is counted
// (bgShellOverCap) and never launches a turn. A release or tick re-check may
// still retry the rows whose slot WAS spent (paced, refused, held or deferred
// launches), but never an over-cap row: the re-check reads the durable debt and
// compares its size with the over-cap count (bgShellCapDeferred).
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

// persistBGShellCompletion writes a finished background shell's notice row and
// takes (or refuses) its auto-resume slot as ONE step relative to the cap check
// (bgShellCapDeferred): the check then never sees a row without its slot
// decision, and rows reach the DB in slot-decision order, so the over-cap rows
// are the newest. A failed insert is logged and the slot decision is made
// anyway, as before.
func (c *coordinator) persistBGShellCompletion(sessionID, shellID, summary string) (claimed bool) {
	_ = c.bgArrival.lock(context.Background()) // cannot fail: Background never ends
	defer c.bgArrival.unlock()
	if c.asyncJobs != nil && c.asyncJobs.store != nil {
		insertCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := c.asyncJobs.store.InsertSessionNotice(insertCtx, sessionID, session.NoticeKindBGShellDone, summary, true, "")
		cancel()
		if err != nil {
			slog.Error("failed to persist background-shell-done notice",
				"session_id", sessionID, "shell_id", shellID, "err", err)
		}
	}
	if seam := bgArrivalInsertedSeam.Load(); seam != nil {
		(*seam)()
	}
	return c.claimAutoResume(sessionID)
}

// bgShellOverCapCount returns how many completions of sessionID were refused
// for lack of a slot since its last human message.
func (c *coordinator) bgShellOverCapCount(sessionID string) int {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	return c.bgShellOverCap[sessionID]
}

// bgShellCapDeferred decides a release or tick re-check (a launch that spends
// no slot) of sessionID's debt when every slot is spent: deferred only when the
// ENTIRE debt is bg-shell notices and none of them holds a slot. Debt is
// reacted to, and closed by failure, oldest first, so what remains is the
// newest rows; the over-cap completions are the newest of all (bgArrival), so
// the debt holds a slot's row exactly when it has more rows than
// bgShellOverCap counts. A row whose slot was spent but whose launch was
// paced, refused, held or deferred is therefore retried (and closed at K=3)
// like any other debt, while a completion that arrived after the cap never
// launches by re-check. The gate is held across the counter and debt reads: a
// completion is either fully visible to them or not at all. An unreadable
// input is an error (drainPolicy fails closed).
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
	bgOnly, rows, err := c.sessionDebtIsBGShellOnly(ctx, sessionID)
	if err != nil || !bgOnly {
		return false, err
	}
	return rows <= c.bgShellOverCapCount(sessionID), nil
}
