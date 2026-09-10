// The non-interactive run path: run modes and per-invocation overrides,
// session resolution, the panic-isolated agent-turn wrapper, and the
// ExecuteRun event loop that computes a run and streams output; the thin
// RunNonInteractive wrapper renders the final envelope.

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/charmbracelet/x/ansi"
)

// cleanupTimeout bounds the best-effort DB writes in RunNonInteractive's
// post-run defers (A-2, task #779). Chosen as a small multiple of ordinary
// SQLite lock contention (a single writer holding the DB for a query or two,
// typically single-digit milliseconds) while staying far short of the 30s
// busy_timeout configured in internal/db/connect.go -- long enough that a
// momentarily busy DB still gets the write, short enough that a genuinely
// stuck writer can't reproduce the "looks like a hang after output already
// printed" symptom this fixes.
const cleanupTimeout = 2 * time.Second

// messageEventsClosedSeam is a test-only hook fired once, the first time
// RunNonInteractive's event loop observes messageEvents closed (H-2, task
// #779). nil in production. Lets tests assert the closed-channel branch was
// actually taken exactly once (not spun on) rather than only inferring it
// from wall-clock loop termination.
var messageEventsClosedSeam func()

// executeRunBeforeTurnLaunchSeam is a test-only hook called after the live
// message subscription is installed and immediately before the turn starts.
// nil in production.
var executeRunBeforeTurnLaunchSeam func()

// executeRunDoneCaseSeam is a test-only hook invoked after the done result is
// selected and before queued message events are drained. nil in production.
var executeRunDoneCaseSeam func()

// ExecuteRun computes a single agent turn and returns the result envelope
// without rendering it (see RunMode for the output shapes that shape the
// streaming behaviour). Streaming output goes to req.Stdout, diagnostics to
// req.Stderr; nil falls back to io.Discard. For RunModeJSON the caller is
// responsible for encoding the returned *RunResult.
func (app *App) ExecuteRun(ctx context.Context, req RunRequest) (*RunResult, error) {
	mode := req.Mode
	continueSessionID := req.ContinueSessionID
	useLast := req.UseLast
	overrides := req.Overrides
	systemPrompt := overrides.SystemPrompt
	prompt := req.Prompt
	slog.Info("Running in non-interactive mode")

	ctx, cancel, setup, err := app.prepareExecuteRun(ctx, req)
	if err != nil {
		return nil, err
	}
	defer cancel()
	stdout := setup.stdout
	stderr := setup.stderr
	credsRunner := setup.credsRunner
	smartOverride := setup.smartOverride
	fastOverride := setup.fastOverride
	modelOverrideRequested := setup.modelOverrideRequested
	stopSpinner := setup.stopSpinner
	stderrTTY := setup.stderrTTY
	progress := setup.progress
	runSpec := setup.runSpec
	folderScopeSpec := setup.folderScopeSpec
	failIfSessionBusy := setup.failIfSessionBusy

	defer stopSpinner()

	sess, err := app.resolveSession(ctx, continueSessionID, useLast)
	if err != nil {
		return nil, fmt.Errorf("failed to create session for non-interactive mode: %w", err)
	}

	// FailIfSessionBusy (sdk.Client.Run/RunWithCredentials): reject the
	// request when the session already has an in-process owner, instead
	// of silently queueing behind it. Opt-in on purpose: `rush run` and
	// the web server keep their intentional queueing behaviour.
	//
	// This pre-check is only the FAST path — it avoids spawning a turn
	// goroutine in the common already-busy case. It cannot close the
	// check-then-act window by itself: for FailIfSessionBusy callers the
	// atomic mailbox reservation immediately below both decides admission
	// and carries the ownership era into the turn; for queueing callers the
	// contract is still enforced at the session's mailbox reservation inside
	// sessionAgent.Run (mailbox.submit), whose single check-and-set returns
	// without queueing for such a call, so sessionAgent.Run reports
	// ErrSessionBusy (R1-4).
	if failIfSessionBusy && app.AgentCoordinator.IsSessionBusy(sess.ID) {
		slog.Warn("Run rejected: session already has an in-process owner", "session_id", sess.ID)
		return nil, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
	}

	// R2-1/R2-3 (round-2 SDK review): for fail-fast callers claim the
	// session's mailbox ownership ATOMICALLY right here — before ANY
	// per-run mutation below (system prompt, reasoning effort, auto-approve,
	// the per-session permission policy, cancel flag, budget, ended_reason,
	// title). The old shape mutated shared session state first and let the
	// real admission decision happen much later, deep inside mailbox.submit
	// (sessionAgent.Run): two simultaneous FailIfSessionBusy callers on one
	// idle session both passed the fast-path check above, both mutated
	// shared state, and the eventual loser's deferred
	// ClearSessionRunAllowlist then deleted the WINNER's armed policy
	// (R2-1), while a caller that was about to be rejected could rewrite
	// the running session's prompt, budget or title before learning it had
	// lost (R2-3). ReserveExclusive is the same atomic mbIdle->mbOwned
	// check-and-set mailbox.submit performs, reused per the review's
	// recommendation ("Move atomic run admission ahead of side effects.
	// This can also provide the owner token needed to solve R2-1
	// cleanly").
	//
	// FailIfSessionBusy == false (`rush run`, the web server) keeps the
	// intentional queueing contract untouched (R1-4): no reservation is
	// attempted, the mutations below run at queue time exactly as before,
	// and mailbox.submit decides ownership/queueing when the turn
	// goroutine reaches it.
	var (
		reservedEpoch   uint64
		reservedCancel  context.CancelFunc
		reservedHandoff atomic.Bool
		// reservedHold is the holdCtx returned by ReserveExclusive
		// (context.WithCancel-derived); Cancel(sessionID)/CancelAll landing
		// during the hold cancels it via the session mailbox.
		reservedHold context.Context
	)
	if failIfSessionBusy {
		holdCtx, epoch, cancel, ok := app.AgentCoordinator.ReserveExclusive(ctx, sess.ID)
		if !ok {
			slog.Warn("Run rejected: session already has an in-process owner (atomic reservation)", "session_id", sess.ID)
			return nil, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
		}
		reservedEpoch = epoch
		reservedCancel = cancel
		reservedHold = holdCtx
		// Ownership continues into the turn: the token makes sessionAgent.Run
		// CONTINUE this era (the same rebindDispatcher mechanism /compact and
		// rerun already use) instead of racing a fresh submit() that would
		// find the mailbox owned by this very caller. onHandoff disarms the
		// bail-out release below exactly when the turn takes over:
		// ExecuteRun can return on cancellation while the turn goroutine is
		// still running, so a blind release defer would end a live era under
		// a running turn. Every error return BETWEEN here and the handoff
		// (UpdateSystemPrompt and friends) releases via this defer instead.
		ctx = agent.WithReservedOwnership(ctx, sess.ID, reservedEpoch, reservedCancel, func() {
			reservedHandoff.Store(true)
		})
		defer func() {
			if !reservedHandoff.Load() {
				app.AgentCoordinator.ReleaseExclusive(sess.ID, reservedEpoch, reservedCancel)
			}
		}()
	}

	// R3-3: mutCtx governs every pre-handoff session mutation below. On
	// the reserved path it is reservedHold, the context ReserveExclusive
	// derives via context.WithCancel and wires into the mailbox as the
	// era's cancel target, so a Cancel(sessionID)/CancelAll landing during
	// the hold window cancels it while caller-ctx cancellation still
	// propagates (it derives from ctx). Previously ExecuteRun discarded
	// holdCtx, so mailbox-directed cancellation in this window fired a
	// placeholder nobody observed and the mutations and turn proceeded
	// anyway. After the handoff holdCtx is intentionally dead
	// (RunWithReservedOwnership / the pre-reserved-ownership path fires it
	// once the turn's own cancel is live), so the turn goroutine and
	// everything downstream keeps the original ctx.
	mutCtx := ctx
	if reservedHold != nil {
		mutCtx = reservedHold
	}

	// checkHoldCanceled is checked before every pre-handoff mutation block
	// and immediately before the handoff, so a canceled hold bails out
	// through the armed ReleaseExclusive defer without mutating session
	// state or starting a turn.
	//
	// F6 (2026-09-01 SDK review): admissionAborted records that a
	// bail-out actually fired. The ended_reason and on-finish-hook
	// defers below are registered before the LAST gate and write
	// through Background-derived contexts on purpose (a started run's
	// cleanup must complete even when its own ctx is already gone), so
	// ctx cancellation cannot stop them in the window where the whole
	// run never started; they check this flag instead. A plain bool is
	// enough: every checkHoldCanceled call site and both defers run on
	// ExecuteRun's goroutine.
	admissionAborted := false
	checkHoldCanceled := func() error {
		if reservedHold == nil {
			return nil
		}
		err := reservedHold.Err()
		if err == nil {
			return nil
		}
		admissionAborted = true
		slog.Warn("Run abandoned: cancellation landed during admission hold", "session_id", sess.ID, "err", err)
		return fmt.Errorf("session %q canceled during run admission: %w", sess.ID, err)
	}

	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	// Persist the requested system prompt for this session. Coordinator's
	// resolveSessionSystemPrompt will pick it up on the next Run(); leaving
	// systemPrompt empty preserves whatever was previously stored (or causes
	// the default prompt to be built and stored on first run).
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	if systemPrompt != "" {
		if err := app.Sessions.UpdateSystemPrompt(mutCtx, sess.ID, systemPrompt); err != nil {
			return nil, fmt.Errorf("failed to set system prompt for session: %w", err)
		}
	}

	// Persist reasoning effort onto the active slot. We pass the current
	// stored value for the *other* slot through so we don't clobber it —
	// UpdateReasoningEffort takes both fields as a single transaction.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	if overrides.ReasoningEffort != "" {
		smart := sess.SmartModelReasoningEffort
		fast := sess.FastModelReasoningEffort
		if overrides.RoleSmart {
			smart = overrides.ReasoningEffort
		} else {
			fast = overrides.ReasoningEffort
		}
		if err := app.Sessions.UpdateReasoningEffort(mutCtx, sess.ID, smart, fast); err != nil {
			return nil, fmt.Errorf("failed to set reasoning effort: %w", err)
		}
	}

	// Automatically approve all permission requests for this non-interactive
	// session.
	// checkHoldCanceled guards both calls below: they have no ctx parameter
	// to carry cancellation, so the hold check is the only gate.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	app.Permissions.AutoApproveSession(sess.ID)

	// Fork patch (run allowlist): the spec merge moved above the
	// per-call CallOptions (T10 — the folder scope's KeepCommandTools
	// decision and the fs_* AllowTools append derive from it). What
	// remains here is the compile and the session/ctx wiring below.
	// Only affects the auto-approve path exercised above; interactive
	// sessions never run this code. R2-1's epoch binding has been
	// superseded by the per-call binding (R3-4), which covers the
	// reserved path too: the reserved call also carries its policy and
	// arms it at turn start.
	compiled, allowErr := permission.BuildRunAllowlist(runSpec)
	if allowErr != nil {
		slog.Warn("Restricted-run allowlist has invalid patterns (skipping them)", "err", allowErr)
	}
	// F2: pin THIS run's compiled policy as the session baseline. It was
	// once the mechanism that kept a durably restarted turn restricted, but
	// it is now a DEMOTED legacy-row fallback: the durable run queue
	// persists each call's own policy spec (WithRunAllowlistSpec below) and
	// the pump recompiles and re-arms it per call, keyed by LogicalCallID —
	// Request consults the per-call entry first — so the baseline no longer
	// governs any NEW row. It now only judges turns rebuilt from rows
	// persisted before the spec field existed (no spec in their JSON), for
	// which nothing better can be reconstructed in-process. Residual: after
	// a real process restart even the in-memory baseline is gone, and a
	// legacy row then falls to the unrestricted process-wide gate — an
	// accepted, documented migration-window behavior that any re-run
	// through ExecuteRun heals. Never cleared on run end — a later run on
	// the same session re-arms it.
	if mgr, ok := app.Permissions.(permission.SessionRunAllowlistBaselineManager); ok {
		mgr.SetSessionRunAllowlistBaseline(sess.ID, compiled)
	}
	// R2-1: the unconditional process-wide SetRunAllowlist write is GONE
	// from this path.
	//
	// R3-4: the compiled policy is no longer armed here at call time —
	// that write raced the mailbox: a queued (FailIfSessionBusy=false)
	// call overwrote the ACTIVE owner's entry at queue time and its
	// front-end defer cleared the only entry before the queued turn ever
	// ran, and even the reserved path's entry armed before the turn
	// goroutine had been admitted by the mailbox. The policy now travels
	// with the call (agent.WithRunAllowlist below, stamped onto
	// SessionAgentCall by buildCall/runInternal) and the turn loop arms it
	// — bound to the call's LogicalCallID — only when the call actually
	// becomes the active turn, for BOTH the reserved and the legacy
	// queueing path, and clears it with that same call id at loop end.
	// FailIfSessionBusy==false keeps the queueing contract (R1-4)
	// untouched: queueing no longer has ANY global side effect at call
	// time.
	ctx = agent.WithRunAllowlist(ctx, &compiled)
	// R4-1/R4-2/R4-3: the UNCOMPILED spec travels with the call too, so it
	// is serialized onto any durable run-queue row this call produces
	// (ToSessionAgentCallData). The pump rebuilds the call, recompiles the
	// spec, and arms the restarted turn's OWN policy keyed by its
	// LogicalCallID — replacing the per-session baseline below as the
	// mechanism that keeps a durable restart restricted.
	ctx = agent.WithRunAllowlistSpec(ctx, &runSpec)

	// T12: the folder-scope spec travels with the call too, so it is
	// serialized onto any durable run-queue row this call produces
	// (ToSessionAgentCallData). The pump rebuilds the call, recompiles
	// the spec, and rebinds the restarted turn's scoped filesystem
	// toolset — without this, a durably-restarted scoped call would
	// silently come back with the shared unscoped toolset.
	if folderScopeSpec != nil {
		ctx = agent.WithFolderScopeSpec(ctx, folderScopeSpec)
	}

	// Fork patch: batch 8/30 + peak-hours bypass (R1-1). This run's
	// timeout-extension policy, cost/token caps and peak-hours bypass now
	// travel in callOpts (WithCallOptions above) and are consumed per call
	// by runInternal/agent_turn — the former SetAgentTimeoutOptions /
	// SetRunLimits / SetAllowPeakHours calls wrote coordinator-wide state
	// that a concurrent run could overwrite before this run's turn read
	// it.

	// Fork patch: batch 30 — clear stale cancel flag.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	if err := app.Sessions.ClearCancelRequest(mutCtx, sess.ID); err != nil {
		slog.Warn("Failed to clear cancel request flag", "session_id", sess.ID, "err", err)
	}

	// Fork patch (operator UX): persist budget at run start so
	// `sessions show` / `sessions locks` can display "cost vs limit".
	// Also clear ended_reason since the session is being (re)started.
	if err := app.Sessions.SetBudget(mutCtx, sess.ID, overrides.MaxCost, overrides.MaxTokens, int64(overrides.Timeout.Seconds())); err != nil {
		slog.Warn("Failed to persist budget", "session_id", sess.ID, "err", err)
	}
	if err := app.Sessions.SetEndedReason(mutCtx, sess.ID, ""); err != nil {
		slog.Warn("Failed to clear ended_reason", "session_id", sess.ID, "err", err)
	}

	// Fork patch (operator UX): auto-title from first user prompt. If the
	// session title is empty or "Untitled Session", set it to the first 60
	// chars of the user prompt. Makes `sessions list` immediately useful
	// without requiring the orchestrator to pass a title.
	if prompt != "" && (sess.Title == "" || sess.Title == "Untitled Session" || sess.Title == sess.ID) {
		autoTitle := prompt
		if len(autoTitle) > 60 {
			autoTitle = autoTitle[:60] + "…"
		}
		// Strip newlines so it fits in one line in `sessions list`.
		autoTitle = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, autoTitle)
		if err := app.Sessions.Rename(mutCtx, sess.ID, autoTitle); err != nil {
			slog.Warn("Failed to auto-title session", "session_id", sess.ID, "err", err)
		}
	}

	// Fork patch: batch 24 — on-finish hook support. Captures run
	// metadata as it becomes available and executes the hook on return.
	var (
		hookExitReason string
		hookCost       float64
		hookTokens     int64
	)
	runStart := time.Now()
	tokensBefore := sess.PromptTokens + sess.CompletionTokens
	costBefore := sess.Cost

	// Fork patch (operator UX): persist ended_reason when the run finishes.
	// hookExitReason is always set before return, so this defer fires after it.
	//
	// A-2 (task #779): these defers deliberately use context.Background()
	// rather than the run's own ctx -- by the time they fire, ctx is
	// already cancelled (cancel() above, or a --timeout/interrupt), so
	// reusing it would make the write fail instantly every time, silently
	// dropping ended_reason/usage bookkeeping on every cancelled run. But
	// an unbounded Background() context can block for the full SQLite
	// busy_timeout (30s, see internal/db/connect.go's pragma map) waiting
	// on a lock -- to the user, who already has their result printed, that
	// looks like a hang. cleanupTimeout gives these best-effort writes a
	// short budget of their own: long enough to clear an ordinary,
	// momentary busy_timeout contention window, short enough that a
	// genuinely stuck writer can't reproduce the 30s freeze this fixes.
	defer func() {
		if admissionAborted {
			slog.Info("Skipping ended_reason write: the run never started (canceled during admission)", "session_id", sess.ID)
			return
		}
		reason := hookExitReason
		if reason == "" {
			reason = "done"
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cleanupCancel()
		if setErr := app.Sessions.SetEndedReason(cleanupCtx, sess.ID, reason); setErr != nil {
			slog.Warn("Failed to persist ended_reason", "session_id", sess.ID, "reason", reason, "err", setErr)
		}
	}()
	if overrides.OnFinishHook != "" {
		defer func() {
			if admissionAborted {
				slog.Info("Skipping on-finish hook: the run never started (canceled during admission)", "session_id", sess.ID)
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cleanupCancel()
			if freshSess, err := app.Sessions.Get(cleanupCtx, sess.ID); err == nil {
				hookTokens = freshSess.PromptTokens + freshSess.CompletionTokens - tokensBefore
				hookCost = freshSess.Cost - costBefore
			} else {
				slog.Warn("Failed to refresh session for on-finish hook usage", "session_id", sess.ID, "err", err)
			}
			duration := time.Since(runStart)
			runOnFinishHook(overrides.OnFinishHook, sess.ID, hookExitReason, hookCost, hookTokens, duration)
		}()
	}

	done := make(chan agentTurnResponse, 1)

	runFn := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		if reservedHold != nil && modelOverrideRequested {
			return app.AgentCoordinator.RunWithReservedOwnership(ctx, sessionID, prompt,
				reservedEpoch, reservedCancel, func() { reservedHandoff.Store(true) }, smartOverride, fastOverride)
		}
		if modelOverrideRequested {
			return app.AgentCoordinator.RunWithOverrides(ctx, sessionID, prompt, smartOverride, fastOverride)
		}
		return app.AgentCoordinator.Run(ctx, sessionID, prompt)
	}
	if credsRunner != nil {
		creds := req.Credentials
		runFn = func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
			return credsRunner.RunWithCredentials(ctx, sessionID, prompt, creds)
		}
	}
	// Last gate before the turn goroutine launches. A cancel landing in
	// the irreducible window between this check and the turn's mailbox
	// rebind is the accepted design race (identical to the rerun
	// handler's): the turn must not run under holdCtx, since the handoff
	// deliberately retires it.
	if err := checkHoldCanceled(); err != nil {
		hookExitReason = "cancelled"
		return nil, err
	}
	startTurn := func() {
		if executeRunBeforeTurnLaunchSeam != nil {
			executeRunBeforeTurnLaunchSeam()
		}
		go runAgentTurnRecovered(ctx, sess.ID, prompt, runFn, done)
	}
	// Subscribe before launching the turn. The message broker is live-only;
	// a fast provider can publish and finish before a later subscriber exists.
	baselineIDs := make(map[string]struct{})
	baselineKnown := true
	if existing, listErr := app.Messages.List(ctx, sess.ID); listErr != nil {
		baselineKnown = false
		slog.Warn("run: failed to snapshot pre-run messages; terminal reconciliation will use live events", "session", sess.ID, "err", listErr)
	} else {
		for _, msg := range existing {
			baselineIDs[msg.ID] = struct{}{}
		}
	}
	messageEvents := app.Messages.Subscribe(ctx)
	startTurn()
	messageReadBytes := make(map[string]int)
	seenToolCalls := make(map[string]bool)
	toolCallCounts := make(map[string]int)    // name → count, for JSON output
	printedFinal := make(map[string]bool)     // for terse mode: print once per finished assistant msg
	var finalText string                      // last assistant FullText seen, for JSON output
	var finalReason string                    // last assistant Finish.Reason seen, for JSON output
	var finalErrTitle, finalErrDetails string // Finish.Message + Finish.Details, surfaced into envelope.Error when reason=error
	var printed bool
	var reconciliationDiagnostic string

	handleMessageEvent := func(event pubsub.Event[message.Message]) error {
		msg := event.Payload
		if msg.SessionID != sess.ID || msg.Role != message.Assistant || len(msg.Parts) == 0 {
			return nil
		}
		stopSpinner()

		// Tool-call names always go to stderr - one short line per new call.
		for _, p := range msg.Parts {
			if tc, ok := p.(message.ToolCall); ok && tc.Name != "" && !seenToolCalls[tc.ID] {
				seenToolCalls[tc.ID] = true
				toolCallCounts[tc.Name]++
				prefix := ""
				if stderrTTY {
					prefix = "\r" + ansi.EraseEntireLine
				}
				fmt.Fprintf(stderr, prefix+"▶ %s\n", tc.Name)
			}
		}

		// Live events drive progress and streaming. The persisted row below
		// is authoritative for the terminal envelope.
		if msg.IsFinished() {
			finalText = msg.FullText()
			for _, p := range msg.Parts {
				if f, ok := p.(message.Finish); ok {
					finalReason = string(f.Reason)
					finalErrTitle = f.Message
					finalErrDetails = f.Details
					break
				}
			}
		}

		switch mode {
		case RunModeJSON:
			// Suppress per-message stdout; the summary is printed below.
		case RunModeTerse:
			if !msg.IsFinished() || printedFinal[msg.ID] {
				return nil
			}
			text := strings.TrimLeft(msg.FullText(), " \t\n")
			if text != "" {
				printedFinal[msg.ID] = true
				printed = true
				fmt.Fprint(stdout, text)
			}
		case RunModeStream:
			content := msg.FullText()
			readBytes := messageReadBytes[msg.ID]
			if len(content) < readBytes {
				slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
				return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
			}
			part := content[readBytes:]
			if readBytes == 0 {
				part = strings.TrimLeft(part, " \t")
			}
			if printed || strings.TrimSpace(part) != "" {
				printed = true
				fmt.Fprint(stdout, part)
			}
			messageReadBytes[msg.ID] = len(content)
		}
		return nil
	}

	drainMessageEvents := func() error {
		for messageEvents != nil {
			select {
			case event, ok := <-messageEvents:
				if !ok {
					messageEvents = nil
					if messageEventsClosedSeam != nil {
						messageEventsClosedSeam()
					}
					continue
				}
				if err := handleMessageEvent(event); err != nil {
					return err
				}
			default:
				return nil
			}
		}
		return nil
	}

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(stderr, ansi.ResetProgressBar)
		}

		// JSON mode emits its own trailing newline via json.Encoder; the
		// terse/stream modes need a bare \n so a follow-up shell prompt
		// doesn't overwrite the last token.
		if mode != RunModeJSON {
			_, _ = fmt.Fprintln(stdout)
		}
	}()

	// finish builds the final envelope/error from runErr plus whatever
	// finalText/finalReason/toolCallCounts have accumulated via messageEvents
	// so far, and is the sole return point for a completed run. Extracted
	// (task #421/P0-1) from the body of `case result := <-done:` below so
	// BOTH that case AND drainDone's case (a durable continuation's outcome,
	// possibly arriving well after the original done fired) can reach it —
	// see the select loop's own doc for why this split exists.
	var (
		cachedTerminal       *terminalReconciliation
		cachedTerminalCtx    context.Context
		cachedTerminalCancel context.CancelFunc
	)
	finish := func(runErr error) (*RunResult, error) {
		stopSpinner()
		if errors.Is(runErr, ErrRunQueued) {
			// The shared session stream may have delivered the active owner's
			// messages before the mailbox reported this call as queued. Do not
			// attribute that output or its tool calls to the queued prompt.
			finalText = ""
			finalReason = ""
			finalErrTitle = ""
			finalErrDetails = ""
			toolCallCounts = make(map[string]int)
		}
		isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, agent.ErrRequestCancelled))
		finalCtx := cachedTerminalCtx
		finalCancel := cachedTerminalCancel
		if finalCtx == nil {
			finalCtx, finalCancel = context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		}
		defer finalCancel()
		if !errors.Is(runErr, ErrRunQueued) {
			var reconciled terminalReconciliation
			var reconcileErr error
			authoritativeTerminal := false
			if cachedTerminal != nil {
				reconciled = *cachedTerminal
				authoritativeTerminal = true
			} else {
				reconciled, reconcileErr = app.reconcileTerminalMessage(finalCtx, sess.ID, baselineIDs, baselineKnown, runStart)
			}
			if reconcileErr != nil {
				reconciliationDiagnostic = "authoritative terminal message reconciliation failed: " + reconcileErr.Error() + "; using live run events"
				slog.Warn("run: failed to reconcile authoritative terminal message", "session", sess.ID, "err", reconcileErr)
			} else {
				// Replay the committed row through the normal output handler.
				// It emits only unread content, so a dropped terminal event cannot
				// lose output or duplicate it.
				if outputErr := handleMessageEvent(pubsub.Event[message.Message]{Payload: reconciled.message}); outputErr != nil {
					return nil, outputErr
				}
				toolCallCounts = reconciled.toolCalls
				authoritativeTerminal = true
			}
			if authoritativeTerminal && isCanceled && !runFailed(finalReason, nil, false) {
				runErr = nil
				isCanceled = false
			}
		}

		if mode == RunModeJSON {
			// Re-fetch the session row so the usage delta reflects
			// the writes the agent made during the run.
			freshSess, usageErr := app.Sessions.Get(finalCtx, sess.ID)
			deltaTokens := int64(0)
			deltaCost := float64(0)
			if usageErr != nil {
				slog.Warn("run: failed to read session usage for the JSON envelope; reporting zero deltas", "session", sess.ID, "err", usageErr)
			} else {
				deltaTokens = freshSess.PromptTokens + freshSess.CompletionTokens - tokensBefore
				deltaCost = freshSess.Cost - costBefore
				if deltaTokens < 0 {
					slog.Warn("run: session token usage moved backwards; reporting zero delta", "session", sess.ID, "before", tokensBefore, "after", freshSess.PromptTokens+freshSess.CompletionTokens)
					deltaTokens = 0
				}
				if deltaCost < 0 {
					slog.Warn("run: session cost moved backwards; reporting zero delta", "session", sess.ID, "before", costBefore, "after", freshSess.Cost)
					deltaCost = 0
				}
			}
			// Fork patch (orchestrator UX): when the caller asked
			// for JSON, defang the persistent "model wrapped its
			// final JSON in a ```json fence and added prose" case
			// here so wrappers can pipe final_text straight into
			// jq. The original is preserved in assistant_notes.
			//
			// Fork patch (orchestrator UX): stripAndExtractJSON handles
			// the common fast-model failure mode: prose preamble + JSON,
			// or even multiple JSON values separated by prose (observed
			// with GLM-5-turbo). Returns a wrapped JSON array when N≥2
			// valid values are found, a single value for N=1, and
			// ErrInvalidStripJSON for N=0 (original text preserved in
			// final_text so the orchestrator can inspect what the model
			// actually said).
			finalTextOut := finalText
			assistantNotes := ""
			strippedBytes := 0
			stripErr := ""
			stripErrReason := ""
			if overrides.StripJSONFences && finalReason != "error" && finalReason != "canceled" {
				cleaned, notes, vErr := stripAndExtractJSON(finalText)
				finalTextOut = cleaned
				assistantNotes = notes
				strippedBytes = len(finalText) - len(cleaned)
				if strippedBytes < 0 {
					strippedBytes = 0
				}
				if vErr != nil {
					stripErr = vErr.Error()
					stripErrReason = "invalid_json"
				}
			}
			// Fork patch (orchestrator UX): sub-agent aggregation.
			// session-#3 (2026-05-17) feedback measured a 7×
			// reduction where parent collapsed sub-agent outputs
			// into a one-paragraph wrap-up. Two responses:
			//
			// 1. ALWAYS-ON warning when reduction ratio is bad
			//    (≥3 sub-agents emitted output AND final_text is
			//    <40% of their combined chars). Operator sees it
			//    in envelope.warnings without flipping a flag.
			// 2. OPT-IN --aggregation=attach: collect each
			//    sub-agent's last assistant text into
			//    envelope.SubAgentOutputs so the orchestrator
			//    recovers the lost detail.
			var subOutputs []SubAgentOutput
			var reductionWarning string
			subAgentCalls := toolCallCounts["agent"] + toolCallCounts["agentic_fetch"]
			if subAgentCalls > 0 {
				count, totalChars := app.subAgentSummaryStats(finalCtx, sess.ID)
				if count >= 2 && totalChars > 0 {
					parentChars := len(finalTextOut)
					ratio := float64(parentChars) / float64(totalChars)
					if ratio < 0.4 {
						reductionWarning = fmt.Sprintf(
							"reduction-loss: final_text is %d chars (%.0f%% of %d combined sub-agent chars across %d sub-session(s)). The parent likely summarised away detail. Re-run with --aggregation=attach or --aggregation=concat to recover; or query the sub-sessions directly.",
							parentChars, ratio*100, totalChars, count,
						)
					}
				}
			}
			if overrides.AggregationMode == "attach" {
				subOutputs = app.collectSubAgentOutputs(finalCtx, sess.ID)
			}
			summary := buildRunResult(
				sess.ID, finalTextOut, assistantNotes, finalReason, runErr, isCanceled,
				toolCallCounts,
				deltaTokens,
				deltaCost,
				time.Since(runStart),
				finalErrTitle, finalErrDetails,
				strippedBytes, stripErr, stripErrReason,
				subOutputs, reductionWarning,
			)
			if reconciliationDiagnostic != "" {
				summary.Warnings = append(summary.Warnings, reconciliationDiagnostic)
			}
			// Per-message token/cache accounting for the session (task
			// #480). Best-effort: an orchestrator losing statistics must
			// never turn a successful run into a failed one.
			if report, uErr := app.Messages.UsageBySession(finalCtx, sess.ID); uErr != nil {
				slog.Warn("run: failed to read per-message usage for the JSON envelope", "session", sess.ID, "err", uErr)
			} else {
				summary.Usage.Session = buildSessionUsageInfo(report)
			}
			// Fork patch: batch 8 — surface orphan partial text.
			if partial := app.findOrphanPartial(finalCtx, sess.ID); partial != nil {
				summary.RecoveredPartial = partial
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"recovered %d chars of partial assistant text from session %s — model run was interrupted",
					partial.Chars, sess.ID,
				))
			}
			hookExitReason = summary.ExitReason
			if runFailed(finalReason, runErr, isCanceled) {
				return &summary, &runIncompleteError{reason: summary.ExitReason, detail: summary.Error, cause: runErr}
			}
			return &summary, nil
		}

		if runErr != nil {
			if guidance := sessionBusyGuidance(sess.ID, runErr); guidance != "" {
				slog.Warn("Non-interactive run rejected because session is already locked",
					"session_id", sess.ID,
					"guidance", guidance,
					"err", runErr)
				fmt.Fprintf(stderr, "\n%s\n\n", guidance)
			}
			// Peak-hours refusal carries multiline orchestrator
			// guidance (RESUME AT + don't-retry instructions) that
			// fang's ERROR box truncates at the first newline. Print
			// the guidance to stderr separately BEFORE the ERROR box
			// so the operator / orchestrator actually sees it.
			// Reuses agent.PeakHoursGuidance so the stderr text stays
			// identical to the DB finish-message details recorded by
			// peakHoursStoppedFinishText (sessions why / diff, etc.).
			var peakErr *agent.PeakHoursError
			if errors.As(runErr, &peakErr) {
				fmt.Fprintf(stderr, "\n%s\n\n", agent.PeakHoursGuidance(peakErr))
			}
			if isCanceled {
				slog.Debug("Non-interactive: agent processing cancelled", "session_id", sess.ID)
				hookExitReason = "cancelled"
				return nil, cancelledRunError(runErr, finalReason, finalErrTitle, finalErrDetails)
			}
			hookExitReason = "error"
			return nil, fmt.Errorf("agent processing failed: %w", runErr)
		}
		// runErr == nil, but the turn may still have ended in-band on an
		// error / canceled / max_tokens finish — not a clean completion,
		// so exit non-zero (the final text is already on stdout).
		if runFailed(finalReason, runErr, isCanceled) {
			reason := finalReason
			if reason == "" {
				reason = "error"
			}
			hookExitReason = reason
			detail := finalErrTitle
			if finalErrDetails != "" {
				if detail != "" {
					detail += ": "
				}
				detail += finalErrDetails
			}
			return nil, &runIncompleteError{reason: reason, detail: detail}
		}
		hookExitReason = "stop"
		return nil, nil
	}

	// drainDone carries the outcome of a P0-1 durable-continuation drain
	// (see the `case result := <-done` branch below) back into this same
	// select loop, on its OWN turn through the loop rather than synchronously
	// inside done's case body. This matters: DrainSessionNow can take
	// seconds (a real second provider round-trip) and, while it runs, the
	// continuation's OWN assistant messages are published to the same
	// message broker messageEvents is subscribed to — those messages MUST
	// still be read by `case event := <-messageEvents` (that's what updates
	// finalText/finalReason/toolCallCounts, and what streams live output in
	// RunModeStream/Terse) while the drain is in flight. Calling
	// DrainSessionNow synchronously inside done's own case body would block
	// this entire select for the drain's whole duration, starving
	// messageEvents and leaving finalText/finalReason stuck at whatever the
	// CANCELLED first generation had produced — confirmed directly: an
	// earlier, synchronous-in-place version of this fix passed a superficial
	// smoke test but failed a stricter end-to-end regression test
	// (TestRunNonInteractive_P0_1_LiveContinuation) with the continuation's
	// own content never reaching the envelope.
	drainDone := make(chan error, 1)

	for {
		if progress && stderrTTY {
			// HACK: Reinitialize the terminal progress bar on every iteration
			// so it doesn't get hidden by the terminal due to inactivity.
			_, _ = fmt.Fprintf(stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			if executeRunDoneCaseSeam != nil {
				executeRunDoneCaseSeam()
			}
			if err := drainMessageEvents(); err != nil {
				return nil, err
			}
			if result.queued {
				return finish(&runQueuedError{sessionID: sess.ID})
			}
			runErr := result.err
			isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, agent.ErrRequestCancelled))

			// P0-1 fix (task #421): a cross-process interrupt landing on a
			// busy session (rush sessions inject --interrupt) cancels the
			// in-flight generation and durably enqueues its replacement
			// (handleInterruptTick), deliberately WITHOUT a live mailbox
			// handoff — the durable run_queue row is the only remaining
			// owner (see mailbox.go's FromDurableQueue guard). Without this,
			// that row sits pending until the background RunQueuePump's
			// next tick (3s in production) happens to fire before this
			// process exits — a race this short-lived process routinely
			// loses, since the rest of this select fires within
			// milliseconds of the cancellation. DrainSessionNow runs any
			// such pending continuation to completion, in THIS process,
			// before the envelope is built from what would otherwise be a
			// stale, cancelled-generation result.
			//
			// isCanceled gates this deliberately: DrainSessionNow itself is
			// a no-op (DrainNoWork) when nothing is pending, so a plain
			// user/--timeout cancellation with no durable continuation is
			// unaffected — the drainDone case below restores the ORIGINAL
			// runErr in that case, rather than fabricating a success.
			//
			// Runs in its OWN goroutine (see drainDone's doc above for why
			// synchronous-in-place doesn't work) — this select loop keeps
			// servicing messageEvents (and ctx.Done()) the whole time.
			if isCanceled && app.RunQueuePump != nil {
				go func(originalErr error) {
					result, drainErr := app.RunQueuePump.DrainSessionNow(ctx, sess.ID)
					drainDone <- drainOutcomeError(sess.ID, result, drainErr, originalErr)
				}(runErr)
				continue
			}

			return finish(runErr)

		case drainErr := <-drainDone:
			return finish(drainErr)

		case event, ok := <-messageEvents:
			if !ok {
				messageEvents = nil
				if messageEventsClosedSeam != nil {
					messageEventsClosedSeam()
				}
				continue
			}
			if err := handleMessageEvent(event); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			reconciled, reconcileErr := app.reconcileTerminalMessage(probeCtx, sess.ID, baselineIDs, baselineKnown, runStart)
			if reconcileErr == nil {
				cachedTerminal = &reconciled
				cachedTerminalCtx = probeCtx
				cachedTerminalCancel = probeCancel
				return finish(ctx.Err())
			}
			probeCancel()
			// Cancellation and the buffered turn result can become ready in
			// either order. Prefer the committed result when it is already
			// available so final reconciliation still runs.
			select {
			case result := <-done:
				if err := drainMessageEvents(); err != nil {
					return nil, err
				}
				if result.queued {
					return finish(&runQueuedError{sessionID: sess.ID})
				}
				if result.err != nil && (errors.Is(result.err, context.Canceled) || errors.Is(result.err, agent.ErrRequestCancelled)) && app.RunQueuePump != nil {
					go func(originalErr error) {
						queuedResult, drainErr := app.RunQueuePump.DrainSessionNow(ctx, sess.ID)
						drainDone <- drainOutcomeError(sess.ID, queuedResult, drainErr, originalErr)
					}(result.err)
					continue
				}
				return finish(result.err)
			default:
				stopSpinner()
				hookExitReason = "cancelled"
				return nil, ctx.Err()
			}
		}
	}
}

// RunNonInteractive runs a single agent turn and writes its result to
// `output`. See RunMode for the available output shapes. It is a thin
// wrapper over ExecuteRun: it supplies the process streams as defaults
// and renders the JSON envelope for RunModeJSON.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	summary, err := app.ExecuteRun(ctx, RunRequest{
		Prompt:            prompt,
		Overrides:         overrides,
		Mode:              mode,
		ContinueSessionID: continueSessionID,
		UseLast:           useLast,
		Origin:            overrides.Origin,
		Stdout:            output,
		Stderr:            os.Stderr,
		HideSpinner:       hideSpinner,
	})
	if mode == RunModeJSON && summary != nil {
		enc := json.NewEncoder(output)
		if encErr := enc.Encode(summary); encErr != nil {
			return fmt.Errorf("failed to encode JSON result: %w", encErr)
		}
	}
	return err
}
