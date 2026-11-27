// The reaction chain guard (#1113, docs/plans/2026-10-01-reaction-chain-guard.
// md): a session that waits on async commands by re-launching pure wait
// commands (`sleep 90`, `echo tick`, ...) chains Drain turns that look healthy
// -- each completion pays its own reaction debt -- while achieving nothing.
// Re-launching the byte-identical previous async command (any command, A36)
// is the same polling and counts as idle; a real action clears that memory.
// The guard's COUNT and CLAIMS live in the arbiter state (R-ARB-2, one state
// under one mutex, turn_arbiter_state.go): a successful reaction closes the
// debt and must not reset the counter, which is exactly the event that masks
// the chain. State dies with a human message (resetConsecutiveResume), real
// progress, or the session; an app restart simply costs up to N more links.

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
)

const (
	loopDetectionWindowSize = 10
	loopDetectionMaxRepeats = 5
)

// loopDetail carries specifics about a detected loop so the assistant
// message's Finish part can record a distinguishable, operator-visible
// explanation instead of looking identical to a model that voluntarily
// finished. The finish REASON stays FinishReasonEndTurn (a loop-detected
// stop legitimately IS a form of "done" — the turn ended cleanly, on
// purpose — and reclassifying it to a distinct enum value would break the
// reclassifyCrashedAsDone / sessions-why logic that keys on EndTurn); the
// distinction is carried in the Finish part's message/details text instead.
type loopDetail struct {
	// ToolName is the name of the tool whose call signature repeated. May
	// be empty if the tool name could not be extracted.
	ToolName string
	// Count is how many times the signature appeared in the window when the
	// threshold was crossed (the value that exceeded maxRepeats).
	Count int
	// Threshold is the maxRepeats value the detection used.
	Threshold int
}

// hasRepeatedToolCalls checks whether the agent is stuck in a loop by looking
// at recent steps. It examines the last windowSize steps and returns true if
// any tool-call signature appears more than maxRepeats times. When it returns
// true, the returned loopDetail is populated with the tool name and count so
// callers (agent.go's OnStepFinish) can record a specific, operator-visible
// explanation on the assistant message's Finish part.
func hasRepeatedToolCalls(steps []fantasy.StepResult, windowSize, maxRepeats int) (bool, loopDetail) {
	var detail loopDetail
	detail.Threshold = maxRepeats
	if len(steps) < windowSize {
		return false, detail
	}

	window := steps[len(steps)-windowSize:]
	counts := make(map[string]int)
	toolNames := make(map[string]string) // signature -> tool names for logging

	for _, step := range window {
		sig := getToolInteractionSignature(step.Content)
		if sig == "" {
			continue
		}
		counts[sig]++
		if _, exists := toolNames[sig]; !exists {
			// Extract tool names for logging
			calls := step.Content.ToolCalls()
			if len(calls) > 0 {
				toolNames[sig] = calls[0].ToolName
			}
		}
		if counts[sig] > maxRepeats {
			// Operator-visible: loop detection truncating a turn is a
			// meaningful event (a legitimate polling pattern may have been
			// cut short), not routine debug noise. Other notable agent
			// events in agent.go (max-cost/max-tokens aborts, empty-stream)
			// use Warn.
			slog.Warn("Loop detected — stopping to avoid infinite repeat",
				"tool", toolNames[sig], "count", counts[sig], "threshold", maxRepeats)
			detail.ToolName = toolNames[sig]
			detail.Count = counts[sig]
			return true, detail
		}
	}

	return false, detail
}

// loopDetectedFinishText builds the message + details strings for the Finish
// part recorded when loop detection stops a turn. Extracted as a named helper
// so it is directly unit-testable without driving a full agent.Run(). Both
// strings are non-empty whenever detail is non-zero — this is the property the
// regression test asserts, since it is what distinguishes a loop-detected stop
// from a voluntary model finish in the persisted message.
func loopDetectedFinishText(detail loopDetail) (msg, details string) {
	tool := detail.ToolName
	if tool == "" {
		tool = "(unknown)"
	}
	msg = fmt.Sprintf("Stopped: repeated identical `%s` tool calls (%d in a row)", tool, detail.Count)
	details = fmt.Sprintf(
		"The agent made the same `%s` tool call with the same result %d times "+
			"in a row (loop-detection threshold is > %d) and was stopped to avoid an "+
			"infinite loop. If this was legitimate polling of a background job or a "+
			"resource that legitimately returns byte-identical output, the task may "+
			"be unfinished — re-run it, possibly with a sleep or a varying argument "+
			"to avoid tripping the detector.",
		tool, detail.Count, detail.Threshold,
	)
	return msg, details
}

// Background launch acks embed a fresh per-launch shell ID (bash tool:
// "Background shell started with ID: %s" and the auto-background
// "Background shell ID: %s" variant). Canonicalize the ID before hashing so
// N identical background launches are recognized as repeats. The match is
// anchored to the FULL ack template, so an ordinary tool result that merely
// contains an ID-looking token still hashes raw.
var backgroundLaunchAckRes = []*regexp.Regexp{
	regexp.MustCompile(`\ABackground shell started with ID: [^\n]+\n\nUse batch_output \(root_id/node_id from await_tasks\) to view output or batch_stop to terminate\.\z`),
	regexp.MustCompile(`\ACommand is taking longer than expected and has been moved to background\.\n\nBackground shell ID: [^\n]+\n\nUse batch_output \(root_id/node_id from await_tasks\) to view output or batch_stop to terminate\.\z`),
}

var backgroundLaunchAckCanonical = []string{
	"Background shell started with ID: <shell-id>\n\nUse batch_output (root_id/node_id from await_tasks) to view output or batch_stop to terminate.",
	"Command is taking longer than expected and has been moved to background.\n\nBackground shell ID: <shell-id>\n\nUse batch_output (root_id/node_id from await_tasks) to view output or batch_stop to terminate.",
}

// canonicalizeBackgroundLaunchAck collapses a trusted background-launch ack
// to an ID-free canonical form. Non-matching output is returned unchanged.
func canonicalizeBackgroundLaunchAck(output string) string {
	for i, re := range backgroundLaunchAckRes {
		if re.MatchString(output) {
			return backgroundLaunchAckCanonical[i]
		}
	}
	return output
}

// getToolInteractionSignature computes a hash signature for the tool
// interactions in a single step's content. It pairs tool calls with their
// results (matched by ToolCallID) and returns a hex-encoded SHA-256 hash.
// If the step contains no tool calls, it returns "".
func getToolInteractionSignature(content fantasy.ResponseContent) string {
	toolCalls := content.ToolCalls()
	if len(toolCalls) == 0 {
		return ""
	}

	// Index tool results by their ToolCallID for fast lookup.
	resultsByID := make(map[string]fantasy.ToolResultContent)
	for _, tr := range content.ToolResults() {
		resultsByID[tr.ToolCallID] = tr
	}

	h := sha256.New()
	for _, tc := range toolCalls {
		output := ""
		if tr, ok := resultsByID[tc.ToolCallID]; ok {
			output = canonicalizeBackgroundLaunchAck(toolResultOutputString(tr.Result))
		}
		io.WriteString(h, tc.ToolName)
		io.WriteString(h, "\x00")
		io.WriteString(h, tc.Input)
		io.WriteString(h, "\x00")
		io.WriteString(h, output)
		io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// toolResultOutputString converts a ToolResultOutputContent to a stable string
// representation for signature comparison.
func toolResultOutputString(result fantasy.ToolResultOutputContent) string {
	if result == nil {
		return ""
	}
	if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result); ok {
		return text.Text
	}
	if errResult, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result); ok {
		if errResult.Error != nil {
			return errResult.Error.Error()
		}
		return ""
	}
	if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result); ok {
		return media.Data
	}
	return ""
}

// ReactionChainLimit is N: the number of consecutive no-progress links after
// which the guard defers the session's automatic turns. Exported for the CLI
// loop's stderr/warning wording (internal/app), so the text cannot drift.
const ReactionChainLimit = drainctl.ReactionChainLimit

// reactionChainReason is the Deferred reason the guard reports.
const reactionChainReason = drainctl.ReactionChainReason

// reactionChainMarkerText is the advisory marker inserted once when the guard
// fires in a web-driven session.
var reactionChainMarkerText = fmt.Sprintf(
	"Автоматические ходы остановлены: %d подряд попытки реакции запускали только sleep/echo без какого-либо прогресса. Чтобы дождаться результата — заверши ход; чтобы подождать и проверить — сделай это одной командой вида `sleep N && <проверка>`.",
	ReactionChainLimit)

// the old link accounting moved to arb.chainLink (R-ARB-2: one state,
// one mutex). accountDrainAttempt calls the arbiter directly.

// reactionChainCount returns the current link count for sessionID.
func (w *drainComponent) reactionChainCount(sessionID string) int {
	return w.host.arbiter().chainLinksOf(sessionID)
}

// chainGuardMarker executes the guard's marker half (the ONLY part of the old
// chainGuardDeferred that survives R-ARB-2 -- the guard's CONDITION is decide's
// rule 5, computed in readTurnFacts): for a web-driven session's first hit it
// inserts one advisory marker notice (wake=0) and logs; a CLI session's loop
// reports the guard itself (CLIScopeState.ChainGuard), so the marker is
// skipped there.
func (w *drainComponent) chainGuardMarker(ctx context.Context, sessionID string) {
	if w.host.arbiter().markChainNoticed(sessionID) {
		return // already warned this episode
	}
	slog.Warn("reaction chain without progress: automatic turns for this session are deferred",
		"session_id", sessionID, "links", w.host.arbiter().chainLinksOf(sessionID))
	store := w.host.sessionNoticeStore()
	if store == nil {
		slog.Error("reaction-chain marker not persisted: no session notice store bound", "session_id", sessionID)
		return
	}
	insertCtx, cancel := context.WithTimeout(context.Background(), drainAccountBudget())
	defer cancel()
	if err := store.InsertSessionNotice(insertCtx, sessionID, session.NoticeKindReactionChain, reactionChainMarkerText, false, ""); err != nil {
		slog.Error("failed to persist the reaction-chain marker notice", "session_id", sessionID, "err", err)
	}
}

// reactionChainHasClaim reports whether claim belongs to the session's
// current idle-launch chain (pullPendingNotices uses it to keep a pulled link
// completion from resetting the supervision backoff).
func (w *drainComponent) reactionChainHasClaim(sessionID, claim string) bool {
	return w.host.arbiter().chainHasClaim(sessionID, claim)
}

// DrainState is the launch verdict for a session's pending-inclusive reaction
// debt (drainPermitted): policy first, then the per-session launch gate.
type DrainState uint8

const (
	// DrainNone: no reaction debt.
	DrainNone DrainState = iota
	// DrainOwed: debt, and a Drain may be launched now.
	DrainOwed
	// DrainPaced: debt, retrying after an unreacted attempt or a refusal, or
	// held by a rerun in progress (a temporary hold: RetryAt is zero, the tick
	// and the release retry it; it reads as "open", never "drained"). The gate
	// opens at CLIScopeState.RetryAt; a newer event opens it sooner after a
	// refusal or a no-turn Drain, but NOT after a paid failure.
	DrainPaced
	// DrainDeferred: debt the policy will not allow a turn for (bg-shell-only
	// with AutoResumeOnJobDone off, or with the web auto-resume cap reached for
	// a re-check launch, a released delegation child, another live driver,
	// Stop's suspension, a pending question).
	DrainDeferred
	// DrainStuck: dormant gate. Three no-turn Drains over a pull that keeps
	// failing: a newer event or a human message reopens it. Three paid attempts
	// whose close kept failing: only a human message (or a restart) does.
	DrainStuck
)

// CLIScopeState is the ONE answer to "what now" for a session between turns
// (CLIScope), read by the CLI loop and by a delegation child's release.
type CLIScopeState struct {
	// WorkOpen: the session has a running task row on a host not provably
	// dead -- wait for it.
	WorkOpen bool
	// Drain is the launch verdict for its reaction debt.
	Drain DrainState
	// RetryAt is when a Paced gate reopens by itself.
	RetryAt time.Time
	// Reason names why the debt is Deferred/Stuck/Paced.
	Reason string
	// ChainGuard: the Deferred reason is the reaction chain guard (#1113) --
	// N consecutive no-progress links -- not the auto-resume policy. The CLI
	// loop reads this typed flag (never the Reason string) for its one
	// stderr line and envelope warning.
	ChainGuard bool
	// OnceWakeOpen: the session owns an ACTIVE once (wakein/wakeon) schedule
	// -- open work for the CLI lifetime policy (stage 5a): the run waits for
	// it to fire, and it never pulls the wait past the run's own deadline.
	OnceWakeOpen bool
	// OnceWakeID / OnceWakeAt name the earliest open once schedule, for the
	// wait heartbeat and `sessions why` (empty/zero without OnceWakeOpen).
	OnceWakeID string
	OnceWakeAt time.Time
}

// ReactionDebtSource is implemented by *coordinator; internal/app type-
// asserts for it exactly like it used to for AsyncCompletionSource.
type ReactionDebtSource interface {
	// ClaimExternalDriver marks sessionID as externally driven for the
	// lifetime of one `rush run` loop: the durable session_drivers marker
	// first (so no OTHER process starts a reaction turn for it -- see
	// drainPolicy), then the in-memory one wakeSession hints against.
	// A session already driven by another live loop is refused with
	// *sessionhosts.ErrSessionDrivenElsewhere and nothing is set. The ONLY
	// degraded outcome is a data dir that cannot host an OS lock file
	// (sessionhosts.ErrDriverMarkerUnavailable = the lock file cannot be created or
	// locked): a Warn, and the in-memory marker alone is set. Any other
	// failure -- a database error registering the host (SQLITE_BUSY/FULL), a
	// cancelled ctx, a failed marker write -- is returned and nothing is set:
	// the run fails before it touches the session (R3A-1).
	ClaimExternalDriver(ctx context.Context, sessionID string) error
	// ReleaseExternalDriver releases the durable marker once the loop exits
	// (always), and the in-memory one for a persistent coordinator (C4).
	ReleaseExternalDriver(ctx context.Context, sessionID string)
	// CLIScope is the CLI loop's between-turns decision (doc sec.3.5), also
	// read by a delegation child's release: it reads doc sec.3.4's debt
	// predicate PENDING-INCLUSIVE (delivery IN {pending, done} -- a
	// freshly-arrived notice is still 'pending' until a turn's own preamble
	// pull, so VISIBLE-only would wrongly report no debt) and answers with the
	// ONE launch predicate every Drain launch reads (policy, then the
	// per-session launch gate): Owed/Paced/Deferred/Stuck. Running rows are
	// read BEFORE debt: a job's terminal transition and its debt commit
	// atomically, so "row not running" then implies its debt is already
	// visible to the debt read that follows -- reversing the order could let
	// the loop exit between the two.
	CLIScope(ctx context.Context, sessionID string) (CLIScopeState, error)
	// WaitForHint blocks until sessionID's hint counter advances, ctx is
	// done, until (when non-zero) passes, or a bounded same-process fallback
	// elapses. The caller must still re-derive its decision from CLIScope
	// afterward -- a returned hint is a reason to re-check, not itself an
	// answer.
	WaitForHint(ctx context.Context, sessionID string, until time.Time)
	// RunMaintenanceSweep runs the dead-host sweep and retention purge halves
	// of the 60s host-level pass ONCE, best-effort (B12/C14 fix, part 3), at
	// the start of a `rush run` invocation; the 60s pass ClaimExternalDriver
	// starts repeats them (with the recheck-child/recheck-set halves) while
	// the loop waits.
	RunMaintenanceSweep(ctx context.Context)
}

// ClaimExternalDriver implements ReactionDebtSource: the durable claim first (a
// refusal or a real error returns before anything is set), then the in-memory
// marker. An unavailable durable marker (no OS-lock-capable data dir, and only
// that) is logged and the in-memory marker alone is kept -- the pre-durable
// behaviour; every other claim error is returned.
func (w *drainComponent) ClaimExternalDriver(ctx context.Context, sessionID string) error {
	if store := w.host.driverMarkerStore(); store != nil {
		if err := store.ClaimSessionDriver(ctx, sessionID); err != nil {
			if !errors.Is(err, sessionhosts.ErrDriverMarkerUnavailable) {
				return err
			}
			slog.Warn("external driver: durable marker unavailable; other processes cannot see this loop",
				"session_id", sessionID, "err", err)
		}
	}
	w.host.driverHints().claimExternalDriver(sessionID)
	// The same 60s pass the web process runs (recheck set, parked children,
	// maintenance): the CLI root itself is a hint-only no-op for it, but its
	// delegated children's retries and refusals ride it. Stopped by CancelAll.
	w.host.startRecheckTicker()
	return nil
}

// ReleaseExternalDriver implements ReactionDebtSource.
//
// C4 fix (docs/reviews/2026-09-29-async-phase4-round1.md): a NON-persistent
// coordinator (`rush run`, persistentMode never set true) keeps its root
// hint-only for the entire life of the process instead of ever releasing
// it. Before this fix, the CLI loop's own defer (app_run_async.go) released
// the marker the instant runNonInteractiveWithAsyncResults returned --
// strictly BEFORE the on-finish hook and before cmd/run.go's deferred
// App.Shutdown() actually runs (or completes CancelAll) -- so a background
// job finishing in that window found isExternalDriver false and started a
// full, PAID Drain turn on context.Background() in a process that is
// already shutting down. A one-shot CLI process has no "later web-driven
// wake for the same session id" within its own lifetime to restore ordinary
// routing FOR (a different process's coordinator has its own, separate
// in-memory marker) -- so simply never releasing it here is both safe and
// sufficient; the marker dies with the process either way.
func (w *drainComponent) ReleaseExternalDriver(ctx context.Context, sessionID string) {
	// The DURABLE marker is released unconditionally: it is what other
	// processes consult, and it must not keep naming a host that stays alive
	// until this process really exits (shutdown can take a while).
	if store := w.host.driverMarkerStore(); store != nil {
		if err := store.ReleaseSessionDriver(ctx, sessionID); err != nil {
			slog.Warn("external driver: releasing the durable marker failed; Close is the backstop",
				"session_id", sessionID, "err", err)
		}
	}
	if !w.host.persistentMode() {
		return // C4: the in-memory marker of a one-shot CLI process dies with it
	}
	w.host.driverHints().releaseExternalDriver(sessionID)
}

// ReactionDebtExists reads doc sec.3.4's pending-inclusive debt predicate for
// sessionID.
func (w *drainComponent) ReactionDebtExists(ctx context.Context, sessionID string) (bool, error) {
	// Pending-inclusive selected-notice debt from batch reads.
	return w.host.batchCutoverFacts().SelectedDebtExists(ctx, sessionID)
}

// reactionDebtSourceHintFallback bounds WaitForHint's wait even without a
// hint or ctx cancellation. Hints live inside one process, so this is also
// the CLI loop's cross-process fallback: it re-reads the DB at least this
// often while its scope is open.
const reactionDebtSourceHintFallback = 5 * time.Second

// WaitForHint implements ReactionDebtSource.
func (w *drainComponent) WaitForHint(ctx context.Context, sessionID string, until time.Time) {
	since := w.host.driverHints().hintSeqOf(sessionID)
	wait := reactionDebtSourceHintFallback
	if !until.IsZero() {
		wait = min(wait, max(time.Until(until), 0))
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	w.host.driverHints().waitForHint(waitCtx, sessionID, since)
}

// RunMaintenanceSweep implements ReactionDebtSource: the dead-host sweep and
// retention purge halves of the 60s pass (doc sec.3.6/3.7), factored out of
// RecheckPass so a `rush run` can run them once at its start.
func (w *drainComponent) RunMaintenanceSweep(ctx context.Context) {
	store := w.host.batchCutoverReactionStore()
	if store == nil {
		return
	}
	// Retention (doc sec.3.7): old terminal batch rows, same pass,
	// best-effort. Dead-host recovery is scope-driven
	// (recoverBatchScopeDeadCandidates / BatchRecoverPass), not a
	// maintenance-table sweep.
	if _, err := store.BatchPurgeExpired(ctx, session.BatchRetentionCutoff(time.Now())); err != nil {
		slog.Debug("coordinator: maintenance sweep: retention purge failed", "err", err)
	}
}

// CLIScope implements ReactionDebtSource (see the interface doc for the
// read order and the meaning of each field).
func (w *drainComponent) CLIScope(ctx context.Context, sessionID string) (CLIScopeState, error) {
	// Read the shell manager's completion hold BEFORE the DB reads: released
	// only after notifyBackgroundJobDone's notice insert commits, "hold gone"
	// implies the debt read below sees the notice; reading it after that read
	// would let the loop exit in the row-committed, notice-not-yet window.
	pending := w.host.backgroundPendingCompletions(sessionID)
	workOpen, err := w.runningWorkOpen(ctx, sessionID)
	if err != nil {
		return CLIScopeState{}, err
	}
	state := CLIScopeState{WorkOpen: workOpen}
	// Stage 5a: an ACTIVE once schedule is open work too. Read AFTER the
	// running rows (display fields, not a liveness factor) but BEFORE debt:
	// it must keep the scope open even when no debt and no row exists.
	once, err := w.host.openOnceWakeSchedules(ctx, sessionID)
	if err != nil {
		return CLIScopeState{}, err
	}
	for _, row := range once {
		at := time.Unix(row.NextRunAt, 0).UTC()
		if state.OnceWakeID == "" || at.Before(state.OnceWakeAt) {
			state.OnceWakeID, state.OnceWakeAt = row.ID, at
		}
	}
	state.OnceWakeOpen = len(once) > 0
	if state.OnceWakeOpen {
		state.WorkOpen = true
	}
	// A completion still in flight (its callback holds) is open work too.
	if pending {
		state.WorkOpen = true
	}
	debt, debtErr := w.host.batchCutoverFacts().SelectedDebtExists(ctx, sessionID)
	if debtErr != nil {
		return CLIScopeState{}, debtErr
	}
	if !debt {
		return state, nil
	}
	v, _, err := w.arbiterVerdict(ctx, sessionID, siteCLI)
	if err != nil {
		// An unreadable launch input is a read error the loop retries with a
		// pause, never a silent exit.
		return CLIScopeState{}, err
	}
	dv := drainVerdictOf(v)
	state.RetryAt, state.Reason = dv.RetryAt, dv.Reason
	state.ChainGuard = dv.Kind == drainDeferred && dv.Reason == reactionChainReason
	switch dv.Kind {
	case drainAllow:
		state.Drain = DrainOwed
	case drainPaced:
		state.Drain = DrainPaced
	case drainDeferred:
		state.Drain = DrainDeferred
	default:
		state.Drain = DrainStuck
	}
	return state, nil
}

// runningWorkOpen is the running-row half of doc sec.3.5's scope predicate:
// true iff sessionID owns a running task row on a host not provably dead.
func (w *drainComponent) runningWorkOpen(ctx context.Context, sessionID string) (bool, error) {
	// Phase-4 step 5 (doc sec.3.5/3.7): a scope check is also a "scope
	// evaluation" point -- recover this owner's own dead-host rows to
	// 'interrupted' before answering, so a row whose host just died is
	// treated as recoverable-and-gone rather than kept "open" forever (or,
	// once recovered, correctly reported closed if nothing else is
	// outstanding). Best-effort: recovery never fails this call.
	// Scope-time recovery first, then liveness from a FRESH batch live read
	// (conservative: an unreadable read reports open). A failed or
	// budget-exhausted recovery keeps the conservative live answer and the
	// App's background maintenance pass completes it later.
	w.host.recoverBatchScopeDeadCandidates(ctx, sessionID)
	entries, scopeErr := w.host.batchCutoverFacts().LiveScopeForOwners(ctx, []string{sessionID})
	if scopeErr != nil {
		return false, scopeErr
	}
	return len(entries) > 0 && entries[0].Live, nil
}

// batchCutoverReactionStore returns the node-backed BatchStore behind c's
// bound fact source, for the two WRITE paths the reaction sides own:
// the step/failure reaction marker (BatchReactionMarkReactedWithMessageUpdate,
// persistStepFinish/persistFailureFinish) and Stop's wake-zero pass
// (BatchReactionSetWakeZeroForOwners, stopTree). nil means "not bound to
// this coordinator, or the bound source is not the node-backed one (the
// conservative unknown source has no store)": the callers fall back to the
// plain message write so the finish still lands. Fact READS keep flowing
// through the BatchCutoverFacts seam.
func batchCutoverReactionStore(c *coordinator) *session.BatchStore {
	if c == nil {
		return nil
	}
	v, ok := coordinatorBatchCutoverFactsBindings.Load(c)
	if !ok {
		return nil
	}
	nbf, ok := v.(nodeBatchCutoverFacts)
	if !ok || nbf.store == nil {
		return nil
	}
	return nbf.store
}

// The in-turn progress guard (§2 of docs/plans/2026-10-01-in-turn-progress-guard.md).
// The POLICY — thresholds, streak folding, refusal verdicts, refusal/finish
// texts and the read-window math — lives in the leaf package
// (runtime/exec/turnstep). This file keeps the guard's original private root
// state (turnProgress, stepGuard) and the test seams the root tests and
// turnStream require; every shim below delegates to the leaf.
const (
	// inTurnRefuseAfter (S): from this many consecutive no-progress steps a
	// wait launch or an already-covered re-read is refused instead of run.
	inTurnRefuseAfter = turnstep.InTurnRefuseAfter
	// inTurnStopAfter (H): the turn is finished by the guard once the streak
	// reaches this length.
	inTurnStopAfter = turnstep.InTurnStopAfter
)

// InTurnGuardStopTitle opens the assistant finish message of a turn the guard
// stopped. UI/CLI recognize it through IsInTurnGuardStop, exactly as the run
// loop recognizes a coordinator continuation prompt.
const InTurnGuardStopTitle = turnstep.InTurnGuardStopTitle

// IsInTurnGuardStop reports whether title was written by the in-turn progress
// guard (see InTurnGuardStopTitle).
func IsInTurnGuardStop(title string) bool {
	return turnstep.IsInTurnGuardStop(title)
}

// refuseWait delegates the wait-leg verdict to the leaf policy
// (turnstep.RefuseWait).
func refuseWait(streak int, ownWork, async bool) bool {
	return turnstep.RefuseWait(streak, ownWork, async)
}

// refuseReread delegates the re-read leg verdict to the leaf policy
// (turnstep.RefuseReread).
func refuseReread(streak int) bool {
	return turnstep.RefuseReread(streak)
}

// nextStreak delegates the streak fold to the leaf policy (turnstep.NextStreak).
func nextStreak(streak int, class stepCallClass) int {
	return turnstep.NextStreak(streak, class)
}

// stopAfter delegates the hard-stop threshold to the leaf policy
// (turnstep.StopAfter).
func stopAfter(streak int) bool {
	return turnstep.StopAfter(streak)
}

// classOfStep delegates the per-step ranking to the leaf policy
// (turnstep.ClassOfStep).
func classOfStep(classes []stepCallClass) stepCallClass {
	return turnstep.ClassOfStep(classes)
}

func hasClass(classes []stepCallClass, want stepCallClass) bool {
	return turnstep.HasClass(classes, want)
}

// turnProgress is the guard's per-turn state (§1.3). One turn only; a new turn
// starts from zero. Written by prepareStep (coverage resets, guard snapshot)
// and by recordStepHistory (classes, streak, stopped), read by StopWhen — all
// sequential callbacks of one agent.Stream loop, so no lock.
type turnProgress struct {
	streak   int
	seen     readCoverage // line windows already returned in this turn; lazily built
	waits    int          // wait calls, for the finish text
	rereads  int          // re-read calls, for the finish text
	noWrites int          // native edit/multiedit calls proven not to write
	stopped  bool
	guard    stepGuard
}

// leafGuard lifts the root snapshot into the leaf's value type for the leaf
// text builders (they read only the streak/mode fields).
func leafGuard(g stepGuard) turnstep.StepGuard {
	return turnstep.StepGuard{
		Streak:      g.streak,
		OwnWork:     g.ownWork,
		Async:       g.async,
		CanEdit:     g.canEdit,
		CanDelegate: g.canDelegate,
	}
}

// leafTurnProgress lifts the root turn state into the leaf's value type for
// the finish text (which reads no coverage).
func leafTurnProgress(p turnProgress) turnstep.TurnProgress {
	return turnstep.TurnProgress{
		Streak:   p.streak,
		Waits:    p.waits,
		Rereads:  p.rereads,
		NoWrites: p.noWrites,
		Stopped:  p.stopped,
		Guard:    leafGuard(p.guard),
	}
}

// stepGuard is the immutable snapshot of one step's worth of guard state,
// built in prepareStep BEFORE the assistant message and handed to the tool
// wrappers of that step. Parallel calls of one step read one value — no lock,
// no shared mutable state (§1.3).
type stepGuard struct {
	streak               int
	ownWork, async       bool
	canEdit, canDelegate bool
	seen                 readCoverage // copy, only when a re-read could be refused
}

// stepGuardFor snapshots the turn state for one step. The coverage is copied
// at every step, not only from the refusal threshold on: recordStepHistory
// classifies each call against this very snapshot, and the streak must count a
// re-read as "no progress" from the FIRST one (§1.4 row 4) even when it is too
// early to refuse it. Files are small, so the copy is a plain map rebuild.
func stepGuardFor(p *turnProgress, ownWork, async bool) stepGuard {
	return stepGuard{
		streak:  p.streak,
		ownWork: ownWork,
		async:   async,
		seen:    p.seen.snapshot(),
	}
}

// refuseWaitCall reports whether this step refuses an async wait-only launch
// (leaf verdict turnstep.RefuseWait).
func (g stepGuard) refuseWaitCall() bool {
	return turnstep.RefuseWait(g.streak, g.ownWork, g.async)
}

// refuseRereadCall reports whether this step refuses a windowed read whose
// every window is already covered (leaf verdict turnstep.RefuseReread plus the
// per-path coverage check).
func (g stepGuard) refuseRereadCall(spans []namedSpan) bool {
	return refuseReread(g.streak) && coveredAll(g.seen, spans)
}

// coveredAll reports whether every window lies inside cov, short-circuiting on
// the first uncovered span. A nil map (no snapshot taken) never covers
// anything, so no call is ever accused of re-reading.
func coveredAll(cov readCoverage, spans []namedSpan) bool {
	if cov == nil || len(spans) == 0 {
		return false
	}
	for _, named := range spans {
		if !cov.covered(named.path, named.span) {
			return false
		}
	}
	return true
}

// applyStep folds one finished step's per-call classes into the turn state and
// returns how many of the step's calls the guard had refused. classes and
// spans are parallel: every read/re-read contributes the windows it asked for,
// added AFTER the classification — which must run against the coverage as of
// the step's START, not against this step's own reads (§1.2).
func (p *turnProgress) applyStep(classes []stepCallClass, spans [][]namedSpan) (refused int) {
	for i, class := range classes {
		switch class {
		case stepCallRead, stepCallReread:
			for _, named := range spans[i] {
				if p.seen == nil {
					p.seen = make(readCoverage)
				}
				p.seen.add(named.path, named.span)
			}
			if class == stepCallReread {
				p.rereads++
			}
		case stepCallWait:
			p.waits++
		case stepCallNoWrite:
			p.noWrites++
		case stepCallRefused:
			refused++
		}
	}
	// An act, a message insert or a trim all mean "the file may have
	// changed": coverage is dropped whatever the step also read (§1.2).
	if hasClass(classes, stepCallAct) {
		p.seen.clear()
	}
	p.streak = nextStreak(p.streak, classOfStep(classes))
	p.stopped = stopAfter(p.streak)
	return refused
}

// progressGuardMetadataJSON marshals the ClientMetadata tag stamped on a
// refusal; the classifier reads it to mark the call as refused (the tool never
// ran, so it neither progressed nor launched anything). The encode itself
// lives in the leaf package (runtime/exec/turnstep.ProgressGuardMetadataJSON).
func progressGuardMetadataJSON(refused string) string {
	return turnstep.ProgressGuardMetadataJSON(refused)
}

// progressGuardTool wraps one fantasy.AgentTool of one step and refuses, on
// that step's snapshot, the calls the plan §2.2 forbids: an async wait-only
// launch while the session has its own work (or past the threshold), and a
// windowed read whose windows were all already returned in this turn. Nothing
// runs on a refusal — no batch launch, no hook, no permission prompt — the
// model sees an ordinary error result carrying the guard's tag. The refusal
// VERDICTS and TEXTS come from the leaf policy; the wrapper itself stays in
// the root so policy_leaf.go's leaf-execution assertion can unwrap it and the
// test seams keep their concrete type.
type progressGuardTool struct {
	inner fantasy.AgentTool
	guard stepGuard
}

// wrapProgressGuard returns a tool slice with each entry wrapped in a
// progressGuardTool carrying the same step snapshot.
func wrapProgressGuard(tools []fantasy.AgentTool, guard stepGuard) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(tools))
	for i, tool := range tools {
		out[i] = &progressGuardTool{inner: tool, guard: guard}
	}
	return out
}

func (t *progressGuardTool) PolicyLeafTool() fantasy.AgentTool { return t.inner }

func (t *progressGuardTool) Info() fantasy.ToolInfo {
	return t.inner.Info()
}

func (t *progressGuardTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *progressGuardTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

// RestrictedRunAction delegates through the inner tool, exactly as hookedTool
// does, so a tool that exposes it keeps exposing it.
func (t *progressGuardTool) RestrictedRunAction() string {
	provider, ok := t.inner.(interface{ RestrictedRunAction() string })
	if !ok {
		return ""
	}
	return provider.RestrictedRunAction()
}

func (t *progressGuardTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	input := fantasy.ToolCallContent{ToolName: call.Name, Input: call.Input}
	if isWaitOnlyCall(input) && t.guard.refuseWaitCall() {
		return t.refuse("wait", refuseWaitText(t.guard, call))
	}
	if spans := readSpans(input); t.guard.refuseRereadCall(spans) {
		return t.refuse("reread", refuseRereadText(t.guard, spans[0]))
	}
	return t.inner.Run(ctx, call)
}

// refuse builds the error result of a blocked call. fantasy copies
// ToolResponse.Metadata straight into ToolResultContent.ClientMetadata
// (agent.go's executeSingleTool, error and success path alike), which is where
// classifyStepCall looks for the tag.
func (t *progressGuardTool) refuse(refused, text string) (fantasy.ToolResponse, error) {
	resp := fantasy.NewTextErrorResponse(text)
	resp.Metadata = progressGuardMetadataJSON(refused)
	return resp, nil
}

// refuseWaitText delegates the wait refusal text to the leaf
// (turnstep.RefuseWaitText).
func refuseWaitText(g stepGuard, call fantasy.ToolCall) string {
	return turnstep.RefuseWaitText(leafGuard(g), call)
}

// refuseRereadText delegates the re-read refusal text to the leaf
// (turnstep.RefuseRereadText).
func refuseRereadText(g stepGuard, first namedSpan) string {
	return turnstep.RefuseRereadText(leafGuard(g), turnstep.NamedSpan{Path: first.path, Span: toLeafSpan(first.span)})
}

// guardStopDetails delegates the finish details to the leaf
// (turnstep.GuardStopDetails).
func guardStopDetails(p turnProgress) string {
	return turnstep.GuardStopDetails(leafTurnProgress(p))
}
