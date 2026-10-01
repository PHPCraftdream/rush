// The reaction chain guard (#1113, docs/plans/2026-10-01-reaction-chain-guard.
// md): a session that waits on async commands by re-launching pure wait
// commands (`sleep 90`, `echo tick`, ...) chains Drain turns that look healthy
// -- each completion pays its own reaction debt -- while achieving nothing.
// The guard's COUNT and CLAIMS live in the arbiter state (R-ARB-2, one state
// under one mutex, turn_arbiter_state.go): a successful reaction closes the
// debt and must not reset the counter, which is exactly the event that masks
// the chain. State dies with a human message (resetConsecutiveResume), real
// progress, or the session; an app restart simply costs up to N more links.
// The condition itself is decide's rule 5 (turn_arbiter.go); this file keeps
// the marker executor and the read accessors.
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

// the old link accounting moved to arb.chainLink (R-ARB-2: one state,
// one mutex). accountDrainAttempt calls the arbiter directly.

// reactionChainCount returns the current link count for sessionID.
func (c *coordinator) reactionChainCount(sessionID string) int {
	return c.arb.chainLinksOf(sessionID)
}

// chainGuardMarker executes the guard's marker half (the ONLY part of the old
// chainGuardDeferred that survives R-ARB-2 -- the guard's CONDITION is decide's
// rule 5, computed in readTurnFacts): for a web-driven session's first hit it
// inserts one advisory marker notice (wake=0) and logs; a CLI session's loop
// reports the guard itself (CLIScopeState.ChainGuard), so the marker is
// skipped there.
func (c *coordinator) chainGuardMarker(ctx context.Context, sessionID string) {
	if c.arb.markChainNoticed(sessionID) {
		return // already warned this episode
	}
	slog.Warn("reaction chain without progress: automatic turns for this session are deferred",
		"session_id", sessionID, "links", c.arb.chainLinksOf(sessionID))
	insertCtx, cancel := context.WithTimeout(context.Background(), drainAccountBudget())
	defer cancel()
	if err := c.asyncJobs.store.InsertSessionNotice(insertCtx, sessionID, session.NoticeKindReactionChain, reactionChainMarkerText, false, ""); err != nil {
		slog.Error("failed to persist the reaction-chain marker notice", "session_id", sessionID, "err", err)
	}
}

// reactionChainHasClaim reports whether claim belongs to the session's
// current idle-launch chain (pullPendingNotices uses it to keep a pulled link
// completion from resetting the supervision backoff).
func (c *coordinator) reactionChainHasClaim(sessionID, claim string) bool {
	return c.arb.chainHasClaim(sessionID, claim)
}
