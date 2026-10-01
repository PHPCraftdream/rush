// The reaction chain guard (#1113, docs/plans/2026-10-01-reaction-chain-guard.
// md): a session that waits on async commands by re-launching pure wait
// commands (`sleep 90`, `echo tick`, ...) chains Drain turns that look healthy
// -- each completion pays its own reaction debt -- while achieving nothing.
// The count of consecutive no-progress links lives HERE, in the coordinator
// next to bgShellOverCap (not in drainGate/sessionJobs: a successful reaction
// closes the debt and would reset the counter with exactly the event that
// masks the chain). State dies with a human message (resetConsecutiveResume),
// real progress, or the session; an app restart simply costs up to N more
// links.
package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/session"
)

// ReactionChainLimit is N: the number of consecutive no-progress links after
// which the guard defers the session's automatic turns. Exported for the CLI
// loop's stderr/warning wording (internal/app), so the text cannot drift.
const ReactionChainLimit = 3

// reactionChainReason is the Deferred reason the guard reports.
const reactionChainReason = "reaction chain without progress"

// reactionChainMarkerText is the advisory marker inserted once when the guard
// fires in a web-driven session.
var reactionChainMarkerText = fmt.Sprintf(
	"Автоматические ходы остановлены: %d подряд попытки реакции запускали только sleep/echo без какого-либо прогресса. Чтобы дождаться результата — заверши ход; чтобы подождать и проверить — сделай это одной командой вида `sleep N && <проверка>`.",
	ReactionChainLimit)

// reactionChainLinkLocked accounts one finished Drain leg (att) under
// autoResumeMu. att.chainIdleClaims/chainProgress come from the leg's step
// history (recordStepHistory); snap is the leg's own debt snapshot. Called
// from accountDrainAttempt for every successful paid attempt, before the
// debt's rows are read -- a successful reaction (rows.open == 0) deliberately
// does NOT reset the count: closing the chain's own completions is the very
// event that masks the chain. A leg with progress resets; a leg with idle
// launches whose snapshot is entirely completions of the chain's own idle
// launches increments; a leg with idle launches answering another fact starts
// a fresh chain at 1; anything else leaves the count as it is.
func (c *coordinator) reactionChainLinkLocked(sessionID string, att *drainAttempt, snap session.DebtSnapshot) {
	if att.chainProgress {
		c.resetReactionChainLocked(sessionID)
		return
	}
	if len(att.chainIdleClaims) == 0 {
		return // a text-only or neutral-tool leg breaks nothing and adds nothing
	}
	if c.reactionChainClaims == nil {
		c.reactionChainClaims = make(map[string]map[string]struct{})
	}
	claims := c.reactionChainClaims[sessionID]
	if claims == nil {
		claims = make(map[string]struct{}, len(att.chainIdleClaims))
		c.reactionChainClaims[sessionID] = claims
	}
	selfSustaining := true
	for _, ref := range snap.Jobs {
		if _, ok := claims[ref.ClaimID]; !ok {
			selfSustaining = false
			break
		}
	}
	if len(snap.Notices) > 0 {
		selfSustaining = false // another fact (bg-shell done, supervision, ...)
	}
	if selfSustaining {
		if c.consecutiveDrainLinks == nil {
			c.consecutiveDrainLinks = make(map[string]int)
		}
		c.consecutiveDrainLinks[sessionID]++
	} else {
		if c.consecutiveDrainLinks == nil {
			c.consecutiveDrainLinks = make(map[string]int)
		}
		c.consecutiveDrainLinks[sessionID] = 1
	}
	for _, claim := range att.chainIdleClaims {
		claims[claim] = struct{}{}
	}
}

// resetReactionChainLocked clears the guard's state for sessionID. Caller
// holds autoResumeMu.
func (c *coordinator) resetReactionChainLocked(sessionID string) {
	delete(c.consecutiveDrainLinks, sessionID)
	delete(c.reactionChainClaims, sessionID)
	delete(c.reactionChainNoticed, sessionID)
}

// reactionChainCount returns the current link count for sessionID.
func (c *coordinator) reactionChainCount(sessionID string) int {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	return c.consecutiveDrainLinks[sessionID]
}

// chainGuardDeferred is drainPolicy's branch: deferred when the chain hit N
// links AND the session's entire pending-inclusive debt is completions of the
// chain's own idle launches -- any other row (a delegation result, a
// supervision tick, a bg-shell completion) means a real fact awaits and gets
// its turn. An unreadable debt is an error (the policy fails closed). For a
// web-driven session the first hit also inserts one marker notice (wake=0)
// and logs; a CLI session's loop reports the guard itself (CLIScopeState.
// ChainGuard), so the marker is skipped there.
func (c *coordinator) chainGuardDeferred(ctx context.Context, sessionID string, externallyDriven bool) (bool, error) {
	c.autoResumeMu.Lock()
	count := c.consecutiveDrainLinks[sessionID]
	c.autoResumeMu.Unlock()
	if count < ReactionChainLimit {
		return false, nil
	}
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return false, nil
	}
	_, notices, jobClaims, err := c.asyncJobs.store.PendingInclusiveDebtRows(ctx, sessionID)
	if err != nil {
		return false, err
	}
	// hasJobDebt only says job ROWS exist -- the idle completions ARE job
	// rows; the other-fact signal is a session_notices row or a job claim
	// outside the chain's set.
	if len(notices) > 0 || len(jobClaims) == 0 {
		return false, nil
	}
	c.autoResumeMu.Lock()
	claims := c.reactionChainClaims[sessionID]
	for _, claim := range jobClaims {
		if _, ok := claims[claim]; !ok {
			c.autoResumeMu.Unlock()
			return false, nil
		}
	}
	_, noticed := c.reactionChainNoticed[sessionID]
	if !externallyDriven {
		if c.reactionChainNoticed == nil {
			c.reactionChainNoticed = make(map[string]struct{})
		}
		c.reactionChainNoticed[sessionID] = struct{}{}
	}
	c.autoResumeMu.Unlock()
	if !externallyDriven && !noticed {
		slog.Warn("reaction chain without progress: automatic turns for this session are deferred",
			"session_id", sessionID, "links", count)
		insertCtx, cancel := context.WithTimeout(context.Background(), drainAccountBudget())
		defer cancel()
		if err := c.asyncJobs.store.InsertSessionNotice(insertCtx, sessionID, session.NoticeKindReactionChain, reactionChainMarkerText, false, ""); err != nil {
			slog.Error("failed to persist the reaction-chain marker notice", "session_id", sessionID, "err", err)
		}
	}
	return true, nil
}

// reactionChainHasClaim reports whether claim belongs to the session's
// current idle-launch chain (pullPendingNotices uses it to keep a pulled link
// completion from resetting the supervision backoff).
func (c *coordinator) reactionChainHasClaim(sessionID, claim string) bool {
	if claim == "" {
		return false
	}
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	_, ok := c.reactionChainClaims[sessionID][claim]
	return ok
}
