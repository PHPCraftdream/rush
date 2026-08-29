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

// recoverSessionListSeam is a test-only hook fired once per candidate
// session actually processed by recoverInterruptedTurns (task #774). nil in
// production. Tests use it to assert the recovery path's cost is
// proportional to the number of CANDIDATES (sessions whose last message is
// an unfinished assistant message) rather than the total number of sessions
// in the data directory -- the whole point of routing discovery through
// ListCandidateInterruptedAssistantSessions instead of Sessions.ListAll +
// per-session Messages.List.
var recoverSessionListSeam func()

// recoverSessionPreWriteSeam is a test-only hook fired once per candidate,
// immediately after all guards (finished re-check, liveness lock, age
// filter) have passed but BEFORE the conditional stamp write
// (StampInterruptedAssistantIfStillLast) is issued (task #777). nil in
// production.
//
// Unlike recoverSessionListSeam, this is deliberately a BLOCKING seam
// (invoked in-line, expected to itself block via a channel handshake) rather
// than fire-and-forget: a fire-and-forget hook cannot force a specific
// interleaving, so a test built on one can only ever observe "the write
// happened after some unspecified point," which is vacuous against exactly
// the TOCTOU bug this task fixes -- the whole defect is that a message can
// land in the narrow gap between the last guard check and the write. This
// codebase has a documented history of vacuous seam tests passing against
// deliberately-broken code, so this seam's contract requires deterministic
// interleaving: a test sets this to a function that signals "the guards have
// passed, safe to inject the race now" and then blocks until told to
// proceed.
var recoverSessionPreWriteSeam func()

// recoverInterruptedTurns is the startup safety net for the "silent dying"
// pattern: a previous rush process that died ungracefully (kill -9, power
// loss, OS reboot, panic without recovery, or even a graceful Ctrl-C during
// the brief window where ctx.Canceled would bypass the in-flight Update)
// can leave assistant messages in the DB with tool calls but no finish
// part. Without recovery, the WUI renders those as "still streaming"
// forever, and `rush sessions reset` is the only escape.
//
// This sweep runs once at app start, before the coordinator is wired up.
//
// Discovery (task #774): rather than listing EVERY session
// (Sessions.ListAll) and running a full Messages.List against each one just
// to find its last assistant message, the sweep asks SQL directly, via
// message.Service.ListCandidateInterruptedAssistantSessions, for the small
// set of sessions whose chronologically last message is an unfinished
// assistant message. That query is backed by a partial index
// (idx_messages_unfinished_assistant) that SQLite auto-maintains on every
// insert/update/delete, so the sweep's cost is now proportional to the
// number of CANDIDATES, not the total number of sessions ever created.
// Measured on a real dev DB this eliminated a 10s+ scan (137 sessions) that
// was tripping the sweep's own deadline below.
//
// The candidate set is then partitioned exactly as before:
//
//   - PASS 1: top-level candidates (ParentSessionID == ""), iterated
//     first. This preserves the original behavior and guarantees top-level
//     sessions are always recovered before children.
//
//   - PASS 2: child candidates (ParentSessionID != ""), iterated after all
//     top-level candidates have been processed. Children include sub-agent
//     task sessions (created via CreateTaskSession), title children
//     (CreateTitleSession), and forks (ForkSessionTx with ParentID set).
//
// For each candidate, the sweep loads just that one message (Messages.Get,
// by the id the query already identified as the last assistant message) and,
// since discovery already proved it has no finish part, adds a
// FinishReasonError marking it as a process-restart interruption. The same
// liveness guard (InspectSessionLock) protects both passes: every Run() —
// child sessions included — acquires its own per-session-ID lock (see
// agent_run.go:~320-360), so the lock check correctly identifies live
// children and leaves them alone.
//
// The sweep is non-fatal on error and silent when there is nothing to recover.
func (app *App) recoverInterruptedTurns(ctx context.Context) {
	// Bound the whole sweep so a slow disk (network mount, AV scan,
	// fsync stall) cannot block app startup. 10s is generous for a
	// candidate-proportional scan against SQLite; if it trips we'd rather
	// skip recovery than hang the user's first `rush run`.
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
	candidates, err := app.Messages.ListCandidateInterruptedAssistantSessions(ctx)
	if err != nil {
		slog.Warn("startup recovery: failed to list recovery candidates", "err", err)
		return
	}
	// Partition candidates into top-level (ParentSessionID == "") and children.
	var topLevel, children []message.InterruptedAssistantCandidate
	for _, cand := range candidates {
		if cand.ParentSessionID == "" {
			topLevel = append(topLevel, cand)
		} else {
			children = append(children, cand)
		}
	}
	var recovered, skippedFresh, skippedLive, recoveredChildren, childrenScanned int
	// PASS 1: top-level candidates, iterated first. Existing behavior is
	// bit-for-bit preserved.
	for _, cand := range topLevel {
		// Early-out if the deadline has already tripped. This is
		// especially important for the child pass below, but we check it
		// here too to keep the pattern consistent.
		if ctx.Err() != nil {
			break
		}
		outcome := app.recoverSessionInterruptedTurn(ctx, cand, staleBefore, dataDir)
		switch outcome {
		case recoveryOutcomeRecovered:
			recovered++
		case recoveryOutcomeSkippedFresh:
			skippedFresh++
		case recoveryOutcomeSkippedLive:
			skippedLive++
		}
	}
	// PASS 2: child candidates, only after pass 1 completes. Same per-session
	// logic verbatim — the only difference is which candidates we visit.
	//
	// The early-out at the top prevents wasted iterations after the deadline.
	for _, cand := range children {
		if ctx.Err() != nil {
			break
		}
		childrenScanned++
		outcome := app.recoverSessionInterruptedTurn(ctx, cand, staleBefore, dataDir)
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
		// (10k+ candidates on slow disk), surface it so the user can
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
	recoveryOutcomeNone                                // Get failed or message no longer unfinished.
)

// recoverSessionInterruptedTurn attempts to recover a single candidate's
// orphaned assistant message. It returns the outcome so the caller can
// maintain counters.
//
// cand.MessageID is already known to be the session's chronologically last
// message and, as of the discovery query's read, an unfinished assistant
// message — so this only needs to fetch that ONE message (not the session's
// full history) to re-check and act on it.
func (app *App) recoverSessionInterruptedTurn(ctx context.Context, cand message.InterruptedAssistantCandidate, staleBefore int64, dataDir string) recoveryOutcome {
	if recoverSessionListSeam != nil {
		recoverSessionListSeam()
	}
	lastAssistant, err := app.Messages.Get(ctx, cand.MessageID)
	if err != nil {
		slog.Debug("startup recovery: skipping candidate, get failed",
			"session_id", cand.SessionID, "message_id", cand.MessageID, "err", err)
		return recoveryOutcomeNone
	}
	// Re-check finished state: the discovery query's read and this Get are
	// two round trips, so a concurrent write landing in between (the live
	// owner finishing its own turn) could have finished the message since.
	if lastAssistant.IsFinished() {
		return recoveryOutcomeNone
	}
	// PRIMARY GUARD (task #287, release blocker): never touch a session
	// that another LIVE rush process still owns. This sweep runs at the
	// start of EVERY rush process and iterates EVERY candidate in the data
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
		st := session.InspectSessionLock(dataDir, cand.SessionID, session.LockStaleDuration)
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
	if recoverSessionPreWriteSeam != nil {
		recoverSessionPreWriteSeam()
	}
	// task #777 (P1 release blocker): a plain Update here rewrites the whole
	// Parts blob unconditionally, which is unsafe against two remaining
	// TOCTOU windows even after the IsFinished/lock/age checks above: (1)
	// cand.MessageID was proven to be the session's LAST message only at the
	// instant the discovery query ran -- a new message landing between then
	// and this write makes the candidate stale, and (2) a plain UPDATE has no
	// way to notice a live owner's write landing in the gap between the Get
	// above and this write, so it would clobber it. StampInterruptedAssistant-
	// IfStillLast folds "still unfinished AND still the session's last
	// message" into the WHERE clause of the write itself, closing both gaps
	// atomically. applied=false means the candidate went stale in that
	// window: skip, do not retry-stamp (see the query's own comment in
	// internal/db/sql/messages.sql for the full rationale).
	applied, err := app.Messages.StampInterruptedAssistantIfStillLast(ctx, cand.SessionID, lastAssistant)
	if err != nil {
		slog.Warn(
			"startup recovery: failed to mark orphan assistant",
			"session_id", cand.SessionID,
			"message_id", lastAssistant.ID,
			"err", err,
		)
		return recoveryOutcomeNone
	}
	if !applied {
		slog.Debug(
			"startup recovery: skipping candidate, went stale between discovery and write",
			"session_id", cand.SessionID,
			"message_id", lastAssistant.ID,
		)
		return recoveryOutcomeNone
	}
	return recoveryOutcomeRecovered
}

// findOrphanPartial scans the session for the latest assistant message that
// has a Partial finish (mid-stream checkpoint) and is unfinished. Used by
// RunNonInteractive to surface recovered text in the envelope.
// Returns nil if no orphan found. Fork patch: batch 8.
func (app *App) findOrphanPartial(ctx context.Context, sessionID string) *RecoveredPartial {
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
			return &RecoveredPartial{
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
