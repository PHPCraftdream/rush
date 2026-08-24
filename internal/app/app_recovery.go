// Startup recovery: the sweep that adds error finishes to orphaned
// assistant messages from dead processes, and findOrphanPartial,
// which surfaces recovered partial text in the run envelope.

package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// recoverInterruptedTurns is the startup safety net for the "silent dying"
// pattern: a previous rush process that died ungracefully (kill -9, power
// loss, OS reboot, panic without recovery, or even a graceful Ctrl-C during
// the brief window where ctx.Canceled would bypass the in-flight Update)
// can leave assistant messages in the DB with tool calls but no finish
// part. Without recovery, the WUI renders those as "still streaming"
// forever, and `rush sessions reset` is the only escape.
//
// This sweep runs once at app start, before the coordinator is wired up.
// It performs a two-pass scan over ALL sessions (Sessions.ListAll):
//
//   - PASS 1: top-level sessions (parent_session_id IS NULL), iterated
//     first. This preserves the original behavior and guarantees top-level
//     sessions are always recovered before children.
//
//   - PASS 2: child sessions (parent_session_id IS NOT NULL), iterated
//     after all top-level sessions have been processed. Children include
//     sub-agent task sessions (created via CreateTaskSession), title children
//     (CreateTitleSession), and forks (ForkSessionTx with ParentID set).
//
// For each session, the sweep finds the LAST assistant message and, if it
// has no finish part, adds a FinishReasonError marking it as a process-restart
// interruption. The same liveness guard (InspectSessionLock) protects both
// passes: every Run() — child sessions included — acquires its own
// per-session-ID lock (see agent_run.go:~320-360), so the lock check
// correctly identifies live children and leaves them alone.
//
// Cost: one full Messages.List per session. For top-level sessions this is
// cheap (bounded by the number of top-level conversations). For child
// sessions it's unbounded (every delegation ever recorded), which is why
// children are processed in pass 2 after all top-level sessions and are
// bounded by the shared 10s deadline — they can never crowd out top-level
// recovery. The deadline is enforced by early-out checks at the top of
// each pass loop (a tripped deadline would fail every Messages.List
// anyway, so we skip wasted iterations).
//
// The sweep is non-fatal on error and silent when there is nothing to recover.
func (app *App) recoverInterruptedTurns(ctx context.Context) {
	// Bound the whole sweep so a slow disk (network mount, AV scan,
	// fsync stall) cannot block app startup. 10s is generous for a
	// linear scan of sessions + targeted updates against SQLite; if it
	// trips we'd rather skip recovery than hang the user's first
	// `rush run`.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	// Age threshold for "this orphan is really orphan" vs "this is a
	// fresh assistant another concurrent process just created". 30s is
	// long enough to cover startup + the first inter-process
	// notification roundtrip; short enough that recovery doesn't wait
	// 5 minutes for stale orphans. See bug-analyzer audit, #7
	// "Recovery vs new turn race".
	orphanAgeThreshold := 30 * time.Second
	if app.recoveryOrphanAge != nil {
		orphanAgeThreshold = *app.recoveryOrphanAge
	}
	staleBefore := start.Add(-orphanAgeThreshold).Unix()
	// Data directory for the cross-process liveness probe below. Empty
	// only in degenerate/test configs; when empty we fall back to the age
	// heuristic alone rather than skipping recovery entirely.
	var dataDir string
	switch {
	case app.recoveryDataDir != nil:
		dataDir = *app.recoveryDataDir
	case app.config != nil:
		if cfg := app.Config(); cfg != nil && cfg.Options != nil {
			dataDir = cfg.Options.DataDirectory
		}
	}
	allSessions, err := app.Sessions.ListAll(ctx)
	if err != nil {
		slog.Warn("startup recovery: failed to list sessions", "err", err)
		return
	}
	// Partition sessions into top-level (ParentSessionID == "") and children.
	var topLevel, children []session.Session
	for _, sess := range allSessions {
		if sess.ParentSessionID == "" {
			topLevel = append(topLevel, sess)
		} else {
			children = append(children, sess)
		}
	}
	var recovered, skippedFresh, skippedLive, recoveredChildren, childrenScanned int
	// PASS 1: top-level sessions, iterated first. Existing behavior is
	// bit-for-bit preserved.
	for _, sess := range topLevel {
		// Early-out if the deadline has already tripped. This is
		// especially important for the unbounded child pass below, but we
		// check it here too to keep the pattern consistent.
		if ctx.Err() != nil {
			break
		}
		outcome := app.recoverSessionInterruptedTurn(ctx, sess, staleBefore, dataDir)
		switch outcome {
		case recoveryOutcomeRecovered:
			recovered++
		case recoveryOutcomeSkippedFresh:
			skippedFresh++
		case recoveryOutcomeSkippedLive:
			skippedLive++
		}
	}
	// PASS 2: child sessions, only after pass 1 completes. Same per-session
	// logic verbatim — the only difference is which sessions we visit.
	//
	// The early-out at the top prevents wasted iterations after the deadline:
	// a tripped deadline means every Messages.List would fail, so we stop
	// immediately rather than spinning through thousands of children.
	for _, sess := range children {
		if ctx.Err() != nil {
			break
		}
		childrenScanned++
		outcome := app.recoverSessionInterruptedTurn(ctx, sess, staleBefore, dataDir)
		switch outcome {
		case recoveryOutcomeRecovered:
			recoveredChildren++
		case recoveryOutcomeSkippedFresh:
			skippedFresh++
		case recoveryOutcomeSkippedLive:
			skippedLive++
		}
	}
	elapsed := time.Since(start)
	totalScanned := len(topLevel) + childrenScanned
	if recovered > 0 || recoveredChildren > 0 || skippedFresh > 0 || skippedLive > 0 {
		slog.Info(
			"startup recovery: completed",
			"recovered", recovered,
			"recovered_children", recoveredChildren,
			"skipped_fresh", skippedFresh,
			"skipped_live", skippedLive,
			"children_scanned", childrenScanned,
			"total_sessions_scanned", totalScanned,
			"elapsed", elapsed.String(),
		)
	} else if elapsed > time.Second {
		// Silent normally, but if the sweep took noticeable time
		// (10k+ sessions on slow disk), surface it so the user can
		// diagnose a slow startup without enabling debug logs.
		slog.Info(
			"startup recovery: nothing to recover",
			"children_scanned", childrenScanned,
			"total_sessions_scanned", totalScanned,
			"elapsed", elapsed.String(),
		)
	}
}

// recoveryOutcome is the result of attempting to recover a single session.
type recoveryOutcome int

const (
	recoveryOutcomeRecovered    recoveryOutcome = iota // Orphan was marked as errored.
	recoveryOutcomeSkippedFresh                        // Too recent to touch.
	recoveryOutcomeSkippedLive                         // Live lock holder protects it.
	recoveryOutcomeNone                                // List failed or no assistant message.
)

// recoverSessionInterruptedTurn attempts to recover a single session's
// orphaned assistant message. It returns the outcome so the caller can
// maintain counters.
func (app *App) recoverSessionInterruptedTurn(ctx context.Context, sess session.Session, staleBefore int64, dataDir string) recoveryOutcome {
	msgs, err := app.Messages.List(ctx, sess.ID)
	if err != nil {
		slog.Debug("startup recovery: skipping session, list failed",
			"session_id", sess.ID, "err", err)
		return recoveryOutcomeNone
	}
	var lastAssistant *message.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant {
			m := msgs[i]
			lastAssistant = &m
			break
		}
	}
	if lastAssistant == nil || lastAssistant.IsFinished() {
		return recoveryOutcomeNone
	}
	// PRIMARY GUARD (task #287, release blocker): never touch a session
	// that another LIVE rush process still owns. This sweep runs at the
	// start of EVERY rush process and iterates EVERY session in the data
	// directory — not just this process's own — so without a real
	// liveness probe it happily stamps "Process restarted" onto turns
	// that are genuinely mid-flight in a sibling process. Because
	// message.Update rewrites the whole Parts blob from the snapshot read
	// here, that stamp also CLOBBERS whatever the live owner streamed in
	// between our read and our write. This fork's entire model is N
	// concurrent `rush run` sessions sharing one data directory, so that
	// was routine, not rare: an observed 38-minute sub-agent delegation
	// was marked errored by an unrelated process merely starting up.
	//
	// The age filter below is NOT sufficient for this — it only covers
	// the first 30 seconds of a turn, while a delegation is bounded at 45
	// minutes. Same discipline `sessions kill`/`locks`/`reap` already
	// apply (tasks #219/#224/#241): prove the holder is gone before
	// acting on its session. InspectSessionLock reports Live for a fresh
	// heartbeat AND, when the mtime looks stale, for a still-running
	// recorded PID (bounded by MaxPidFallbackAge), so a healthy session
	// blocked on one long tool call is correctly protected too.
	//
	// This guard works for child sessions too: agent_run.go:~320-360
	// documents that every Run() acquires its own per-session-ID lock,
	// regardless of whether it's a sub-agent. A live child's lock protects
	// it exactly like a live top-level session.
	if dataDir != "" {
		st := session.InspectSessionLock(dataDir, sess.ID, session.LockStaleDuration)
		// Fail CLOSED on "could not check": if the lock file's
		// existence could not be determined at all (permission
		// denied, I/O error, unreachable path component, ...),
		// StatErr is non-nil and Live is false — which without this
		// check would read as "no live owner" and let the sweep
		// clobber a session another process may own. "Could not
		// look" must not read as "looked and found nothing" (same
		// principle as #622/#631 on the rerun/delete guards).
		// Skipping a genuinely orphaned session here only delays its
		// recovery to a later startup; wrongly stamping a live one
		// destroys streamed content (#287).
		if st.Live || st.StatErr != nil {
			return recoveryOutcomeSkippedLive
		}
	}
	// Age filter: secondary belt-and-braces only. Skips messages another
	// concurrent rush process might have just created in the window
	// before it managed to write its lock file at all.
	if lastAssistant.CreatedAt > staleBefore {
		return recoveryOutcomeSkippedFresh
	}
	lastAssistant.AddFinish(
		message.FinishReasonError,
		"Process restarted",
		"The previous rush process exited before this turn completed (silent dying — see CHANGELOG.fork.md section 4.D). The assistant message had tool calls but no finish part. Cleanly recovered on startup; you can retry from the previous user message.",
	)
	if err := app.Messages.Update(ctx, *lastAssistant); err != nil {
		slog.Warn(
			"startup recovery: failed to mark orphan assistant",
			"session_id", sess.ID,
			"message_id", lastAssistant.ID,
			"err", err,
		)
		return recoveryOutcomeNone
	}
	return recoveryOutcomeRecovered
}

// findOrphanPartial scans the session for the latest assistant message that
// has a Partial finish (mid-stream checkpoint) and is unfinished. Used by
// RunNonInteractive to surface recovered text in the envelope.
// Returns nil if no orphan found. Fork patch: batch 8.
func (app *App) findOrphanPartial(ctx context.Context, sessionID string) *recoveredPartial {
	msgs, err := app.Messages.List(ctx, sessionID)
	if err != nil {
		return nil
	}
	// Find the LATEST assistant message that is partial and unfinished.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != message.Assistant {
			continue
		}
		if m.IsPartial() && !m.IsFinished() {
			text := m.FullText()
			var lastFlushAt int64
			if f := m.FinishPart(); f != nil {
				lastFlushAt = f.Time
			}
			return &recoveredPartial{
				MessageID:   m.ID,
				Chars:       len(text),
				LastFlushAt: lastFlushAt,
				Text:        text,
			}
		}
		// Only surface the LATEST orphan — stop at the first assistant
		// message we encounter (going backwards).
		break
	}
	return nil
}
