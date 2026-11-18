// Background-job completion notices, Phase-4 autonomous idle-resume, busy
// queries, and the web bg-shell auto-turn cap. Bodies moved from
// coordinator_background.go, coordinator_bgshell_cap.go and bgshell_claim.go
// (the drainComponent/wakeComponent host pattern: the component never holds
// the raw *coordinator, only the backgroundHost port); those files keep the
// thin coordinator delegates.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

// backgroundHost is the narrow coordinator surface the moved background /
// bg-shell-cap bodies need (the wakeComponent pattern).
type backgroundHost interface {
	coordinatorIsNil() bool
	cfg() *config.ConfigStore
	sessions() coresession.Service
	subAgentDrivers() *subAgentDriverRegistry
	currentAgent() SessionAgent
	notifyWG() *sync.WaitGroup
	notifyCallbackHook() func(context.Context)
	recheckWakes() *sync.WaitGroup
	admitCallback(wg *sync.WaitGroup) (context.Context, bool)
	wakeSession(ctx context.Context, sessionID string, fact bool) error
	noteSubAgentChildRunEnded(sessionID string)
	bgShellRowCancelled(sessionID, shellID string) bool
	addToRecheckSet(sessionID string)
	consecutiveResume(sessionID string) int
	coordinatorBatchCutoverFacts() BatchCutoverFacts
	bgArrivalLock(ctx context.Context) error
	bgArrivalUnlock()
	sessionNoticeStore() *session.SessionNoticeStore
	persistentMode() bool
	autonomyEnabled() bool
	claimAutoResumeSlot(sessionID string, rowID int64, eligible bool) bool
	arb() *arbiter
}

type (
	backgroundComponent       struct{ host backgroundHost }
	coordinatorBackgroundHost struct{ coordinator *coordinator }
)

func newBackgroundComponent(c *coordinator) *backgroundComponent {
	return &backgroundComponent{host: coordinatorBackgroundHost{coordinator: c}}
}

func (h coordinatorBackgroundHost) coordinatorIsNil() bool { return h.coordinator == nil }
func (h coordinatorBackgroundHost) cfg() *config.ConfigStore {
	return h.coordinator.cfg
}

func (h coordinatorBackgroundHost) sessions() coresession.Service {
	return h.coordinator.sessions
}

func (h coordinatorBackgroundHost) subAgentDrivers() *subAgentDriverRegistry {
	return h.coordinator.subAgentDrivers
}

func (h coordinatorBackgroundHost) currentAgent() SessionAgent {
	return h.coordinator.currentAgent
}
func (h coordinatorBackgroundHost) notifyWG() *sync.WaitGroup { return &h.coordinator.notifyWG }
func (h coordinatorBackgroundHost) notifyCallbackHook() func(context.Context) {
	return h.coordinator.notifyCallbackHook
}

func (h coordinatorBackgroundHost) recheckWakes() *sync.WaitGroup {
	return &h.coordinator.recheckWakes
}

func (h coordinatorBackgroundHost) admitCallback(wg *sync.WaitGroup) (context.Context, bool) {
	return h.coordinator.admitCallback(wg)
}

func (h coordinatorBackgroundHost) wakeSession(ctx context.Context, sessionID string, fact bool) error {
	return h.coordinator.wakeSession(ctx, sessionID, fact)
}

func (h coordinatorBackgroundHost) noteSubAgentChildRunEnded(sessionID string) {
	h.coordinator.noteSubAgentChildRunEnded(sessionID)
}

func (h coordinatorBackgroundHost) bgShellRowCancelled(sessionID, shellID string) bool {
	return h.coordinator.bgShellRowCancelled(sessionID, shellID)
}

func (h coordinatorBackgroundHost) addToRecheckSet(sessionID string) {
	h.coordinator.addToRecheckSet(sessionID)
}

func (h coordinatorBackgroundHost) consecutiveResume(sessionID string) int {
	return h.coordinator.consecutiveResume(sessionID)
}

func (h coordinatorBackgroundHost) coordinatorBatchCutoverFacts() BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(h.coordinator)
}

func (h coordinatorBackgroundHost) bgArrivalLock(ctx context.Context) error {
	return h.coordinator.bgArrival.lock(ctx)
}
func (h coordinatorBackgroundHost) bgArrivalUnlock() { h.coordinator.bgArrival.unlock() }
func (h coordinatorBackgroundHost) sessionNoticeStore() *session.SessionNoticeStore {
	return h.coordinator.sessionNoticeStore()
}

func (h coordinatorBackgroundHost) persistentMode() bool {
	return h.coordinator.persistentMode.Load()
}

func (h coordinatorBackgroundHost) autonomyEnabled() bool {
	return h.coordinator.autonomyEnabled()
}

func (h coordinatorBackgroundHost) claimAutoResumeSlot(sessionID string, rowID int64, eligible bool) bool {
	return h.coordinator.claimAutoResumeSlot(sessionID, rowID, eligible)
}
func (h coordinatorBackgroundHost) arb() *arbiter { return &h.coordinator.arb }

// filterNonEmpty returns the subset of inputs that are non-empty after
// trimming surrounding whitespace. Used to join stdout/stderr cleanly.
func filterNonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// FormatAsyncCompletion makes a completed job visible to the model and UI.
// The TimedOut branch's wording matches
// docs/plans/2026-09-27-wake-tools-contract.md §5.1 verbatim.
func FormatAsyncCompletion(completion AsyncCompletion) string {
	content := tools.TruncateOutput(strings.TrimSpace(completion.Content))
	if content == "" {
		content = "(no output)"
	}
	if completion.TimedOut {
		return fmt.Sprintf("Async job %s (%s) timed out after %ds and was stopped. Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, completion.TimeoutSeconds, content)
	}
	if completion.Stopped {
		return fmt.Sprintf("Async job %s (%s) was stopped (batch_stop). Partial output before the stop:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	if completion.Cancelled {
		return fmt.Sprintf("Async job %s (%s) was cancelled (session stopped). Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	if completion.Interrupted {
		return fmt.Sprintf("Async job %s (%s) was interrupted: its host process ended before the job finished.\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	status := "finished"
	if completion.IsError {
		status = "failed"
	}
	return fmt.Sprintf("Async job %s (%s) %s.\n\n%s",
		completion.ToolCallID, completion.ToolName, status, content)
}

// notifyAsyncCompletion is the completion wake callback the batch delivery
// invokes: the delivery already committed its durable pending notice
// (a batch row or a session_notices row) --
// there is no text/NoticeKind left to build here (doc sec.3.3: the driver's
// pull reconstructs both from the row at pull time, agent_notice_pull.go).
// This is now just the non-blocking wake HINT (doc sec.3.4): submit a Drain
// call to the session's owning driver iff the committed row wants a wake.
// See wakeSession's own doc for why a driver-owned child session's wake must
// never run on c.host.currentAgent() (task #1049).
func (c *backgroundComponent) notifyAsyncCompletion(completion AsyncCompletion) {
	// No ctx tagging needed anymore: a Drain call's own constructor
	// (newDrainCall) unconditionally stamps AutoResumed/BackgroundJobNotice/
	// NoticeKind itself, and a Drain never creates a user message (empty
	// Prompt), so there is no Origin left for a caller-supplied ctx to
	// influence either.
	// Supervision progress is recorded when the notice moves into history
	// (agent_notice_pull.go), not here.
	ctx, ok := c.host.admitCallback(c.host.notifyWG())
	if !ok {
		// The terminal fact is already durable; shutdown owes no new wake.
		return
	}
	go func() {
		defer c.host.notifyWG().Done()
		if c.host.notifyCallbackHook() != nil {
			c.host.notifyCallbackHook()(ctx)
		}
		// Re-check trigger (ii)/(iv): this goroutine IS the delivery that
		// the completed job woke -- either directly (mailbox was idle) or
		// merged into an already-live/queued generation (mailbox was busy).
		// Re-evaluate any delegation parked for this session once wakeSession
		// returns, so a release can never land in the window before the
		// wake's effect is visible. wakeSession itself already logs+persists
		// a visible marker on failure; there is nothing else to do with its
		// error here.
		defer c.host.noteSubAgentChildRunEnded(completion.SessionID)
		if completion.Wake {
			_ = c.host.wakeSession(ctx, completion.SessionID, true)
		}
	}()
}

// Phase-4 step 4 (doc sec.3.5): ClaimAsyncCompletions/NextAsyncCompletion/
// HasPendingAsyncJobs and the in-memory ready queue they read are gone.
// `rush run`'s loop (internal/app/app_run_async.go) now claims/releases the
// external-driver marker directly via ClaimExternalDriver/ReleaseExternalDriver
// (coordinator_reaction_source.go) and re-derives its next turn from
// CLIScope against the DB, waiting on WaitForHint (with
// its bounded 5s fallback) instead of draining a memory queue.

// backgroundJobSummary formats a finished background command for injection
// into the owning session. Pure and deterministic so it can be unit-tested
// without a running shell.
func backgroundJobSummary(id, command string, stdout, stderr string, exitCode int, elapsed time.Duration) string {
	out := strings.TrimSpace(strings.Join(filterNonEmpty(stdout, stderr), "\n"))
	out = tools.TruncateOutput(out)
	if out == "" {
		out = "(no output)"
	}
	return fmt.Sprintf("Background job %s (`%s`) finished: exit %d, ran %s.\n\n%s",
		id, command, exitCode, elapsed.Round(time.Second), out)
}

// notifyBackgroundJobDone handles an SDK background shell's completion
// (doc sec.2: a completion with no batch root of its own). Step 3: the fact is
// persisted FIRST, durably, as a session_notices row (kind
// NoticeKindBGShellDone, wake=1 per the wake-policy table -- the row's own
// wake bit does not depend on session policy, only whether a TURN is forced
// right now does). AutoResumeOnJobDone decides ONLY that immediate-turn
// question for a session that is not a delegated child, exactly as today: on,
// a Drain call is submitted right away; off, the notice simply waits for the
// next natural turn/Drain to pull it (its wake=1 still counts then). A child
// whose delegation row runs is woken regardless (delegatedChildDriven).
func (c *backgroundComponent) notifyBackgroundJobDone(sessionID string, sh *shell.BackgroundShell) {
	// A shell batch_stop already cancelled (its row is terminal, the tool result
	// is its answer) produces no wake notice and takes no auto-resume slot: its
	// exit would otherwise read as "finished: exit 1" and wake the session a
	// second time. The completion hold still ends and the scope is re-checked.
	if c.host.bgShellRowCancelled(sessionID, sh.ID) {
		sh.MarkCompletionRecorded()
		c.host.noteSubAgentChildRunEnded(sessionID)
		return
	}
	stdout, stderr, _, runErr := sh.GetOutput()
	summary := backgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed())

	// The wake notice and the slot decision are one step (persistBGShellCompletion);
	// the shell's own row is closed by the observer registered at claim time.
	claimed := c.persistBGShellCompletion(sessionID, sh.ID, summary)
	// The notice is durable (or its insert failed and nothing more will be
	// written): the shell's completion hold ends here so the session's own
	// debt answers "is work owed", and only now do this callback's re-checks
	// run (R7B-1). The shell manager releases it on a panic or return too.
	sh.MarkCompletionRecorded()
	// A delegated child is driven by its delegation, not by the auto-resume
	// policy (R6B-1): the claim (web only, AutoResumeOnJobDone on, a free slot)
	// bounds a session's OWN chain of automatic turns. Refusing a child's wake
	// for it left the child's debt owed with nothing to launch it while the
	// parent's delegation waited. drainPermitted stays the single launch gate
	// (a running row allows the turn, a released child is refused there); a root
	// keeps the cap and the policy.
	driven := false
	if !claimed {
		var err error
		if driven, err = c.delegatedChildDriven(context.Background(), sessionID); err != nil {
			slog.Warn("bg-shell completion: delegation state unreadable", "session_id", sessionID, "err", err)
			if !driven {
				c.host.addToRecheckSet(sessionID) // the 60s pass decides it
			}
		}
	}
	if claimed || driven {
		// Autonomous idle-resume: start (or, if busy, queue) a Drain call
		// over the just-persisted notice. The bound was spent by
		// claimAutoResumeSlot (reset by any human message); a driven child spends
		// no slot.
		slog.Info("Phase 4: resuming session on background job completion",
			"session_id", sessionID, "shell_id", sh.ID, "delegated_child", driven,
			"consecutive", c.host.consecutiveResume(sessionID))
		ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
		ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
		// The slot was spent ONCE, synchronously, at admission (before the
		// wake's goroutine spawns), so a burst of near-simultaneous
		// completions is bounded deterministically: at most
		// maxConsecutiveAutoResumes completions per human message launch a
		// turn of their own. The launch decision of this wake does not
		// re-check the counter (R2B-16); a paced or refused launch keeps its
		// slot and submits nothing.
		c.host.recheckWakes().Add(1) // waitRecheckWakes covers this detached wake too
		go func() {
			defer c.host.recheckWakes().Done()
			// Re-check trigger (iii): a job owned by this session just
			// became terminal and this goroutine is the delivery it woke.
			// Re-evaluate any delegation parked for this session once
			// wakeSession returns, so the release cannot fire in the window
			// between "job done" and "child claimed its next turn".
			defer c.host.noteSubAgentChildRunEnded(sessionID)
			_ = c.host.wakeSession(ctx, sessionID, true)
		}()
		return
	}

	// AutoResumeOnJobDone off (doc sec.3.4's session-policy table): the
	// notice is already durable and web-visible on the next pull; no turn
	// is forced here, matching today's behavior.
	c.host.noteSubAgentChildRunEnded(sessionID)
}

func (c *backgroundComponent) isBusy() bool {
	return c.host.currentAgent().IsBusy()
}

func (c *backgroundComponent) isSessionBusy(sessionID string) bool {
	return c.host.currentAgent().IsSessionBusy(sessionID)
}

// delegatedChildDriven reports whether sessionID is a delegation's child that
// its delegation currently drives: a RUNNING async_jobs row names it as the
// child. The durable row decides (another process may own the delegation; this
// process's ledger and driver registry can be empty), so a released child, whose
// row is terminal, is not driven even while its driver is still registered.
// An unreadable row falls back to the driver registration, which only ever
// exists for a child, and reports the error: the launch decision that follows
// (wakeSession) is drainPermitted's, which refuses whatever the policy refuses.
func (c *backgroundComponent) delegatedChildDriven(ctx context.Context, sessionID string) (bool, error) {
	parent := ""
	if c.host.sessions() != nil {
		sess, err := c.host.sessions().Get(ctx, sessionID)
		switch {
		case err == nil:
			parent = sess.ParentSessionID
		case !errors.Is(err, sql.ErrNoRows):
			// The session row is unreadable (not merely absent): the parent
			// cannot be resolved, so fall back to the driver registration
			// and surface the read error, exactly like an unreadable
			// delegation read.
			_, registered := c.host.subAgentDrivers().get(sessionID)
			return registered, err
		}
	}
	if parent != "" {
		running, err := c.host.coordinatorBatchCutoverFacts().LiveDelegationChildOf(ctx, parent, sessionID)
		if err == nil {
			return running, nil
		}
		_, registered := c.host.subAgentDrivers().get(sessionID)
		return registered, err
	}
	_, registered := c.host.subAgentDrivers().get(sessionID)
	return registered, nil
}

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
// This function writes only the wake channel -- a kind=bg_shell_done
// session_notices row, whose id is what the arbiter's over-cap set is keyed
// by -- and takes the slot decision on it. Fixtures without a bound notice
// store get no row (only the log); the pull drains whatever exists.
func (c *backgroundComponent) persistBGShellCompletion(sessionID, shellID, summary string) (claimed bool) {
	if seam := bgArrivalEnteredSeam.Load(); seam != nil {
		(*seam)(sessionID)
	}
	_ = c.host.bgArrivalLock(context.Background()) // cannot fail: Background never ends
	defer c.host.bgArrivalUnlock()
	// B9-7: job_kill owns a cancelled shell's answer: under the same gate the
	// kill decided at, a callback that lost the race inserts nothing.
	if c.host.bgShellRowCancelled(sessionID, shellID) {
		return false
	}
	var rowID int64
	if store := c.host.sessionNoticeStore(); store != nil {
		c.pruneBGShellOverCap(sessionID)
		insertCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		id, err := store.InsertSessionNoticeReturningID(insertCtx, sessionID, session.NoticeKindBGShellDone, summary, true, "")
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
	eligible := c.host.persistentMode() && c.host.autonomyEnabled()
	return c.host.claimAutoResumeSlot(sessionID, rowID, eligible)
}

// pruneBGShellOverCap is the best-effort prune at a completion's arrival (under
// bgArrival): an unreadable debt leaves the set as it is.
func (c *backgroundComponent) pruneBGShellOverCap(sessionID string) {
	if c.bgShellOverCapCount(sessionID) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recheckDebtCheckBudget)
	defer cancel()
	rows, err := c.host.coordinatorBatchCutoverFacts().PendingInclusiveDebt(ctx, sessionID)
	if err != nil {
		return
	}
	c.host.arb().pruneOverCap(sessionID, rows.Notices)
}

// bgShellOverCapCount returns how many over-cap completion rows sessionID
// currently remembers (since its last human message).
func (c *backgroundComponent) bgShellOverCapCount(sessionID string) int {
	return c.host.arb().overCapCount(sessionID)
}

// notifyOnBackgroundJobDone mirrors buildTools' kill-switch default
// (coordinator_tools.go): a nil store/options or a nil pointer means enabled.
func (c *backgroundComponent) notifyOnBackgroundJobDone() bool {
	if c.host.coordinatorIsNil() || c.host.cfg() == nil {
		return true
	}
	opts := c.host.cfg().Config().Options
	return opts == nil || opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
}
