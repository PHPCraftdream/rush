// Startup recovery: the sweep that adds error finishes to orphaned
// assistant messages from dead processes, and findOrphanPartial,
// which surfaces recovered partial text in the run envelope.

package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// recoverInterruptedTurns is the startup safety net for the "silent dying"
// pattern: a previous crush process that died ungracefully (kill -9, power
// loss, OS reboot, panic without recovery, or even a graceful Ctrl-C during
// the brief window where ctx.Canceled would bypass the in-flight Update)
// can leave assistant messages in the DB with tool calls but no finish
// part. Without recovery, the WUI renders those as "still streaming"
// forever, and `crush sessions reset` is the only escape.
//
// This sweep runs once at app start, before the coordinator is wired up.
// For every session, it finds the LAST assistant message and, if it has no
// finish part, adds a FinishReasonError marking it as a process-restart
// interruption. Cheap (O(sessions × 1 query each)), non-fatal on error,
// silent when there is nothing to recover.
func (app *App) recoverInterruptedTurns(ctx context.Context) {
	// Bound the whole sweep so a slow disk (network mount, AV scan,
	// fsync stall) cannot block app startup. 10s is generous for a
	// linear scan of sessions + targeted updates against SQLite; if it
	// trips we'd rather skip recovery than hang the user's first
	// `crush run`.
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
	sessions, err := app.Sessions.List(ctx)
	if err != nil {
		slog.Warn("startup recovery: failed to list sessions", "err", err)
		return
	}
	var recovered, skippedFresh, skippedLive int
	for _, sess := range sessions {
		msgs, err := app.Messages.List(ctx, sess.ID)
		if err != nil {
			slog.Debug("startup recovery: skipping session, list failed",
				"session_id", sess.ID, "err", err)
			continue
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
			continue
		}
		// PRIMARY GUARD (task #287, release blocker): never touch a session
		// that another LIVE crush process still owns. This sweep runs at the
		// start of EVERY crush process and iterates EVERY session in the data
		// directory — not just this process's own — so without a real
		// liveness probe it happily stamps "Process restarted" onto turns
		// that are genuinely mid-flight in a sibling process. Because
		// message.Update rewrites the whole Parts blob from the snapshot read
		// here, that stamp also CLOBBERS whatever the live owner streamed in
		// between our read and our write. This fork's entire model is N
		// concurrent `crush run` sessions sharing one data directory, so that
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
				skippedLive++
				continue
			}
		}
		// Age filter: secondary belt-and-braces only. Skips messages another
		// concurrent crush process might have just created in the window
		// before it managed to write its lock file at all.
		if lastAssistant.CreatedAt > staleBefore {
			skippedFresh++
			continue
		}
		lastAssistant.AddFinish(
			message.FinishReasonError,
			"Process restarted",
			"The previous crush process exited before this turn completed (silent dying — see CHANGELOG.fork.md section 4.D). The assistant message had tool calls but no finish part. Cleanly recovered on startup; you can retry from the previous user message.",
		)
		if err := app.Messages.Update(ctx, *lastAssistant); err != nil {
			slog.Warn(
				"startup recovery: failed to mark orphan assistant",
				"session_id", sess.ID,
				"message_id", lastAssistant.ID,
				"err", err,
			)
			continue
		}
		recovered++
	}
	elapsed := time.Since(start)
	if recovered > 0 || skippedFresh > 0 || skippedLive > 0 {
		slog.Info(
			"startup recovery: completed",
			"recovered", recovered,
			"skipped_fresh", skippedFresh,
			"skipped_live", skippedLive,
			"total_sessions_scanned", len(sessions),
			"elapsed", elapsed.String(),
		)
	} else if elapsed > time.Second {
		// Silent normally, but if the sweep took noticeable time
		// (10k+ sessions on slow disk), surface it so the user can
		// diagnose a slow startup without enabling debug logs.
		slog.Info(
			"startup recovery: nothing to recover",
			"total_sessions_scanned", len(sessions),
			"elapsed", elapsed.String(),
		)
	}
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
