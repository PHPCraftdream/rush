// Supervision: wake the root session after chat silence while its scope
// still has open work (design doc docs/plans/2026-09-27-async-structured-
// concurrency.md §7, wake-tools-contract.md §5.1/§5.2/§6/§7, operator
// decision 2026-09-27 item 5).
//
// Deliberately NOT a workLedger job: a supervision timer never appears in
// bySession[x].jobs, so it can never itself keep l.running(x)/next() from
// reporting "no open work" -- rush run still exits the instant its OTHER
// work drains, and the timer is simply abandoned when the process exits
// (Go does not wait for stray goroutines). This sidesteps the jobKind/
// HoldsScope question docs/plans/2026-09-28-async-phase3-spec.md §0.2 left
// open for phase 5 entirely: supervision was never a counted unit of work
// to begin with.
//
// The single per-process timer goroutine is shared with asyncJob deadlines
// (work_ledger_timeout.go's timeoutService/timeoutEntry) -- this file never
// starts a second worker or a goroutine per session.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// noticeKindSupervision tags a supervision tick's persisted message
// (Message.NoticeKind) -- both the ordinary check-in and the terminal
// "paused" notice use it, so agent_prompt.go's history filter treats them
// uniformly (only the newest one is ever shown to the model).
const noticeKindSupervision = "supervision"

// Defaults for SupervisionConfig. supervisionMaxInterval/
// supervisionMaxNoProgress are implementation constants, not configurable
// (only enable/disable and the initial interval are, per the task's
// requirements) -- the design doc's own example numbers (5 -> 10 -> 20 min,
// cap 60; a bound on consecutive no-progress ticks) rather than a value with
// an established operational history worth exposing.
const (
	supervisionDefaultInterval  = 5 * time.Minute
	supervisionMaxInterval      = 60 * time.Minute
	supervisionMaxNoProgress    = 6
	supervisionMinIntervalFloor = time.Second // guards against a pathological zero/negative override
)

// recheckDebtCheckBudget bounds ONLY the DB read in recheckDebtOnRelease
// (B1 fix): a var, not a const, so a test can shrink it to reproduce "the
// debt check's own budget must never bound the Drain turn it triggers" at
// test timescale instead of a real 30s wait.
var recheckDebtCheckBudget = 30 * time.Second

// SupervisionConfig is one root session's effective supervision policy,
// resolved once per noteWorkStarted/recordProgress call from config +
// per-call CallOptions (coordinator.resolveSupervisionConfig).
type SupervisionConfig struct {
	Enabled       bool
	Interval      time.Duration
	MaxInterval   time.Duration
	MaxNoProgress int
}

// DefaultSupervisionConfig returns the built-in policy: on, 5-minute
// interval, doubling to a 60-minute cap, pausing after 6 consecutive
// no-progress ticks.
func DefaultSupervisionConfig() SupervisionConfig {
	return SupervisionConfig{
		Enabled:       true,
		Interval:      supervisionDefaultInterval,
		MaxInterval:   supervisionMaxInterval,
		MaxNoProgress: supervisionMaxNoProgress,
	}
}

// resolveSupervisionConfig merges config.Options (process-wide default) with
// ctx's CallOptions (this run's override, `rush run --no-supervision`/
// `--supervision-interval`). CallOptions wins when present, matching every
// other per-call override in this file (AllowPeakHours, DisableSubAgents, …).
func (c *coordinator) resolveSupervisionConfig(ctx context.Context) SupervisionConfig {
	cfg := DefaultSupervisionConfig()
	if c.cfg != nil {
		if opts := c.cfg.Config().Options; opts != nil {
			if opts.SupervisionEnabled != nil {
				cfg.Enabled = *opts.SupervisionEnabled
			}
			if opts.SupervisionIntervalMinutes > 0 {
				cfg.Interval = time.Duration(opts.SupervisionIntervalMinutes) * time.Minute
			}
		}
	}
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		if callOpts.SupervisionDisabled {
			cfg.Enabled = false
		}
		if callOpts.SupervisionInterval > supervisionMinIntervalFloor {
			cfg.Interval = callOpts.SupervisionInterval
		}
	}
	if cfg.Interval < supervisionMinIntervalFloor {
		cfg.Interval = supervisionDefaultInterval
	}
	return cfg
}

// supervisionState is one root session's live countdown/backoff state.
// generation guards every armed heap entry (timeoutEntry) the same way
// subAgentDriverRegistry.generation guards a driver record: a stale fire for
// a generation this state has since moved past is a silent no-op, never a
// second, duplicate tick.
type supervisionState struct {
	rootSessionID string
	cfg           SupervisionConfig
	generation    uint64
	interval      time.Duration // current (grows on no-progress ticks, resets to cfg.Interval on progress)
	tickCount     int           // consecutive no-progress ticks delivered since the last progress reset
	paused        bool          // true once tickCount reaches cfg.MaxNoProgress; cleared only by recordProgress
}

// supervisionRegistry maps root session id -> its live supervision state.
// Bounded by construction, not by an eviction policy: an entry exists only
// while its session has open work (created by noteWorkStarted, removed by
// clearSupervisionIfPresent the instant the scope drains, or by cancelSession
// below) -- there is no per-process cap to enforce because the set can never
// grow past "sessions with open async work right now", already bounded by
// maxAsyncJobsPerSession-style limits elsewhere.
type supervisionRegistry struct {
	mu      sync.Mutex
	byRoot  map[string]*supervisionState
	nextGen uint64
}

func newSupervisionRegistry() *supervisionRegistry {
	return &supervisionRegistry{byRoot: make(map[string]*supervisionState)}
}

// noteWorkStarted arms (or, if paused, resumes) sessionID's supervision timer
// when new open work appears. A session that is CURRENTLY a registered
// delegation driver's child (c.subAgentDrivers) is never supervised here --
// only the root is (design doc §7: "будится только корень"); a delegated
// child cannot itself be running a bash/run_command call before its driver
// is registered (runSubAgent registers before dispatching the child's first
// turn), so this check is never racing the child's own first Start call.
// Called from asyncTool.Run AFTER workLedger.Start has released l.mu, so
// this never nests l.mu under supervisionRegistry.mu (see that struct's own
// lock-ordering note: the two mutexes are always taken sequentially, never
// nested, in either direction).
func (l *workLedger) noteWorkStarted(ctx context.Context, sessionID string) {
	if l == nil || l.supervision == nil || l.coord == nil || sessionID == "" {
		return
	}
	if _, isDelegatedChild := l.coord.subAgentDrivers.get(sessionID); isDelegatedChild {
		return
	}
	cfg := l.coord.resolveSupervisionConfig(ctx)
	if !cfg.Enabled {
		return
	}
	sr := l.supervision
	sr.mu.Lock()
	st, ok := sr.byRoot[sessionID]
	switch {
	case !ok:
		st = &supervisionState{rootSessionID: sessionID, cfg: cfg, interval: cfg.Interval}
		sr.byRoot[sessionID] = st
	case st.paused:
		// New work while paused counts as progress: resume fresh instead of
		// leaving the session silently unsupervised forever.
		st.cfg = cfg
		st.interval = cfg.Interval
		st.tickCount = 0
		st.paused = false
	default:
		// Already counting down; a second job starting mid-countdown does
		// not need a fresh deadline of its own.
		sr.mu.Unlock()
		return
	}
	sr.nextGen++
	st.generation = sr.nextGen
	gen := st.generation
	deadline := time.Now().Add(st.interval)
	sr.mu.Unlock()
	l.timeouts.armFunc(deadline, func() { l.handleSupervisionDeadline(sessionID, gen) })
}

// recordProgress resets sessionID's backoff to its base interval and clears
// the no-progress/paused counters: a real completion (job/sub-agent/timeout
// notice) is "progress" regardless of how many no-progress ticks preceded
// it. Called from wakeSession for every notice EXCEPT supervision's own
// (noticeKindSupervision) -- see that call site's comment for why the tick's
// own resulting turn must not reset the very backoff it just grew.
func (l *workLedger) recordProgress(sessionID string) {
	if l == nil || l.supervision == nil || sessionID == "" {
		return
	}
	sr := l.supervision
	sr.mu.Lock()
	st, ok := sr.byRoot[sessionID]
	if !ok {
		sr.mu.Unlock()
		return
	}
	st.interval = st.cfg.Interval
	st.tickCount = 0
	st.paused = false
	sr.nextGen++
	st.generation = sr.nextGen
	gen := st.generation
	deadline := time.Now().Add(st.interval)
	sr.mu.Unlock()
	l.timeouts.armFunc(deadline, func() { l.handleSupervisionDeadline(sessionID, gen) })
}

// pushDeadlineOnTurnEnd re-arms sessionID's CURRENT (possibly already grown)
// interval from now, without touching interval size, tickCount or paused.
// Wired onto sessionAgent.onSessionIdle (coordinator_tools.go's
// onSessionIdleHook), the universal "a turn on this session just ended"
// trigger fired for every SessionAgent this coordinator builds -- this is
// what implements "any new message in the root's chat resets the countdown"
// for a plain user/model turn that involves no async-job notice at all,
// without letting a supervision TICK's own resulting turn silently reset the
// backoff it just grew (that would defeat consecutive-no-progress growth
// entirely, since every tick that wakes an idle root produces exactly such a
// turn). A no-op while paused: only recordProgress resumes a paused session.
func (l *workLedger) pushDeadlineOnTurnEnd(sessionID string) {
	if l == nil || l.supervision == nil || sessionID == "" {
		return
	}
	sr := l.supervision
	sr.mu.Lock()
	st, ok := sr.byRoot[sessionID]
	if !ok || st.paused {
		sr.mu.Unlock()
		return
	}
	sr.nextGen++
	st.generation = sr.nextGen
	gen := st.generation
	deadline := time.Now().Add(st.interval)
	sr.mu.Unlock()
	l.timeouts.armFunc(deadline, func() { l.handleSupervisionDeadline(sessionID, gen) })
}

// clearSupervisionIfPresent removes sessionID's supervision state entirely.
// Called from deliverLocked the instant a session's job map empties (scope
// closed, §2.3) and from cancelSession (session-level cancel, the closest
// existing proxy this codebase has for "give up on this session's work" --
// see this function's own doc for the known gap around a bare session
// delete with no prior cancel). A stale heap entry for the removed
// generation simply finds byRoot[sessionID] absent when it fires and no-ops.
func (l *workLedger) clearSupervisionIfPresent(sessionID string) {
	if l == nil || l.supervision == nil {
		return
	}
	sr := l.supervision
	sr.mu.Lock()
	delete(sr.byRoot, sessionID)
	sr.mu.Unlock()
}

// growInterval doubles cur, capped at max.
func growInterval(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// handleSupervisionDeadline is the timeoutService fire callback for
// sessionID's armed generation. It re-checks everything under
// supervisionRegistry.mu / workLedger.mu / the agent mailbox before acting
// (the same "lazy recheck, stale fire is a no-op" discipline
// work_ledger_timeout.go's handleTimeout already documents for asyncJob
// deadlines): a generation mismatch, no open work, or a live turn all make
// this a no-op rather than a wrong tick.
func (l *workLedger) handleSupervisionDeadline(rootSessionID string, generation uint64) {
	if l == nil || l.supervision == nil || l.coord == nil {
		return
	}
	sr := l.supervision

	sr.mu.Lock()
	st, ok := sr.byRoot[rootSessionID]
	stale := !ok || st.generation != generation || st.paused
	sr.mu.Unlock()
	if stale {
		return
	}

	// Only while the root's scope has open work (§2.2's open(S), already
	// computed by l.running -- a supervision entry is never itself part of
	// that computation, see this file's doc). No open work means the scope
	// closed since this deadline was armed; drop the state instead of
	// waiting for deliverLocked to have caught it first (belt and braces --
	// the two can race harmlessly, both converge on "state gone").
	if !l.running(rootSessionID) {
		sr.mu.Lock()
		if st2, ok := sr.byRoot[rootSessionID]; ok && st2.generation == generation {
			delete(sr.byRoot, rootSessionID)
		}
		sr.mu.Unlock()
		return
	}

	// Only while the root is not running a turn. If busy, do nothing here:
	// pushDeadlineOnTurnEnd (onSessionIdle) reschedules once that turn ends,
	// using the SAME (unchanged) interval -- this fire is simply skipped,
	// not counted as a no-progress tick.
	if l.coord.agentFor(rootSessionID).IsSessionBusy(rootSessionID) {
		return
	}

	sr.mu.Lock()
	st, ok = sr.byRoot[rootSessionID]
	if !ok || st.generation != generation || st.paused {
		sr.mu.Unlock()
		return
	}
	st.tickCount++
	tickNum := st.tickCount
	lastTick := tickNum >= st.cfg.MaxNoProgress
	if lastTick {
		st.paused = true
	} else {
		st.interval = growInterval(st.interval, st.cfg.MaxInterval)
	}
	interval := st.interval
	var nextGen uint64
	var nextDeadline time.Time
	if !lastTick {
		sr.nextGen++
		st.generation = sr.nextGen
		nextGen = st.generation
		nextDeadline = time.Now().Add(interval)
	}
	sr.mu.Unlock()

	text := l.buildSupervisionSummary(rootSessionID, tickNum, interval, lastTick)
	if !lastTick {
		l.timeouts.armFunc(nextDeadline, func() { l.handleSupervisionDeadline(rootSessionID, nextGen) })
	}

	coord := l.coord
	id := jobIdentity{owner: rootSessionID, toolCallID: "supervision-" + uuid.NewString()}
	go func() {
		// Phase-4 step 3 (doc sec.3.2/3.3): persist the check-in as a
		// session_notices row FIRST (no job_tool_call_id -- the void
		// condition for kind=supervision checks "any running row for
		// owner", not one specific job), then submit the wake hint. wake=1
		// per the wake-policy table; the pull (agent_notice_pull.go) voids
		// it instead of delivering it if the scope has since closed.
		if l.store == nil {
			return
		}
		if err := l.store.InsertSessionNotice(context.Background(), rootSessionID, session.NoticeKindSupervision, text, true, ""); err != nil {
			slog.Error("supervision: failed to persist check-in notice", "session_id", rootSessionID, "err", err)
			return
		}
		_ = coord.wakeSession(context.Background(), id, true)
	}()
}

// jobSnapshot is a copy of the fields buildSupervisionSummary needs from one
// open job, taken under l.mu and used afterward without it (mirroring
// capturePartial's own "snapshot under lock, I/O outside it" discipline).
type jobSnapshot struct {
	toolCallID, toolName, childSession, shellID string
	outputBuf                                   tools.LiveOutputBuffer
	elapsed                                     time.Duration
}

// buildSupervisionSummary composes the check-in text from rootSessionID's
// currently open jobs, reusing capturePartial/capturePartialDelegation
// (work_ledger_timeout.go) for each one's last-output-line -- the same
// cheap, best-effort helpers §5.6's timeout notices already use, per the
// task's explicit instruction to reuse them rather than add a second
// output-reading path.
func (l *workLedger) buildSupervisionSummary(rootSessionID string, tickNum int, interval time.Duration, paused bool) string {
	l.mu.Lock()
	var snaps []jobSnapshot
	if s := l.bySession[rootSessionID]; s != nil {
		now := time.Now()
		for _, job := range s.jobs {
			snaps = append(snaps, jobSnapshot{
				toolCallID: job.toolCallID, toolName: job.toolName,
				childSession: job.childSession, shellID: job.shellID,
				outputBuf: job.outputBuf, elapsed: now.Sub(job.startedAt),
			})
		}
	}
	l.mu.Unlock()

	commands, delegations := 0, 0
	lines := make([]string, 0, len(snaps))
	for _, j := range snaps {
		partial := l.capturePartial(rootSessionID, j.toolCallID, j.toolName, j.childSession, j.shellID, j.outputBuf)
		last := lastNonEmptyLine(tools.TruncateOutput(strings.TrimSpace(partial.content)))
		if last == "" {
			last = "(no output yet)"
		}
		if j.childSession != "" {
			delegations++
			lines = append(lines, fmt.Sprintf("- %s (sub-agent, child session %s): running %s, last activity: %s",
				j.toolCallID, j.childSession, j.elapsed.Round(time.Second), last))
		} else {
			commands++
			lines = append(lines, fmt.Sprintf("- %s (%s): running %s, last output: %s",
				j.toolCallID, j.toolName, j.elapsed.Round(time.Second), last))
		}
	}
	body := strings.Join(lines, "\n")
	if body == "" {
		body = "(no details available)"
	}

	guidance := "Keep waiting, inspect with job_output, or stop with job_kill (commands only; sub-agent control tools are not available yet)."
	if paused {
		guidance = fmt.Sprintf(
			"Supervision is now paused after %d consecutive check-ins with no progress; it resumes automatically once something changes (a job or sub-agent completes). %s",
			tickNum, guidance)
	}
	return fmt.Sprintf(
		"Supervision check-in (tick %d, interval %gm): %d background job(s) running, %d sub-agent delegation(s) in progress.\n\n%s\n\n%s",
		tickNum, interval.Minutes(), commands, delegations, body, guidance)
}

// lastNonEmptyLine returns the last non-blank line of s, or "".
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// onSessionIdleHook is wired as every SessionAgent's OnSessionIdle
// (coordinator_tools.go): the universal "a turn on this session just ended"
// trigger. It fires the phase-3 delegation re-check (noteSubAgentChildRunEnded),
// pushes back the caller's supervision deadline UNLESS this release is a
// Drain that ended without ever reaching the provider (doc sec.3.4 last
// paragraph: "opening a tab must not reset the root's silence timer"), and
// implements doc sec.3.4 item 3 -- EVERY mailbox release checks the
// session's reaction debt in the DB and submits a Drain if there is one, in
// a SEPARATE goroutine, never from this (or any) defer. Rule (a)'s
// anti-idle-loop exception: a no-turn Drain's OWN release skips this
// re-launch check, but only if the hint counter is unchanged since that
// Drain's own check -- every other side effect here (noteSubAgentChildRunEnded,
// pushDeadlineOnTurnEnd) is unaffected and always runs.
func (c *coordinator) onSessionIdleHook(sessionID string) {
	c.noteSubAgentChildRunEnded(sessionID)
	// B2/C2 fix (doc sec.3.4 rule (b)): a release caused by an admission
	// refusal (runOwned could not acquire the session's OS lock -- another
	// process already holds it) ran no turn at all and must never trigger an
	// immediate relaunch: the foreign holder does not release just because
	// this process re-checks, so an unconditional recheckDebtOnRelease here
	// would hot-loop (claim, refuse, release, re-check, claim, ...) with no
	// pause. Route to the 60s recheck pass instead, exactly like a session-
	// lock-busy Drain submission already does via recordDrainOutcome's own
	// turnAttemptRefused branch -- this closes the SAME gap for every other
	// caller of Run() that hits the same refusal, not only wakeSession's own.
	if c.asyncJobs != nil && c.asyncJobs.consumeAdmissionRefusedRelease(sessionID) {
		c.addToRecheckSet(sessionID)
		return
	}
	wasNoTurnDrain, hintUnchanged := false, false
	if c.asyncJobs != nil {
		wasNoTurnDrain, hintUnchanged = c.asyncJobs.consumeNoTurnDrainRelease(sessionID)
		if !wasNoTurnDrain {
			c.asyncJobs.pushDeadlineOnTurnEnd(sessionID)
		}
	}
	if wasNoTurnDrain && hintUnchanged {
		return
	}
	go c.recheckDebtOnRelease(sessionID)
}

// recheckDebtOnRelease is onSessionIdleHook's separate-goroutine debt check
// (doc sec.3.4 item 3): reads sessionID's CURRENT reaction debt from the DB
// and, if any, either hints an external-driver session (its own loop
// re-checks) or submits a Drain the same way wakeSession would for a
// completion's own hint -- this is what catches an "orphaned" Drain (a row
// that arrived inside the release window) and a debt row left by a prior
// pass that skipped its own re-launch under rule (a).
//
// B1 fix: the 30s budget bounds ONLY the debt-existence read. The Drain
// itself is submitted/run on a context detached from that deadline
// (context.Background(), not context.WithoutCancel(checkCtx) -- checkCtx is
// about to be cancelled by this function's own return, which would cancel
// a still-running Drain's whole turn the instant this function returns).
// Before this fix, wakeSession/agent.Run/runOwned's whole turn loop ran
// under the SAME 30s-deadline ctx as the debt check: a Drain turn longer
// than 30s hit DeadlineExceeded, classified as a terminal provider error,
// and settled the debt by failure (wake_failed marker) even though the
// provider may have still been working. Shutdown is observed independently
// inside runOwned/transition via the coordinator's own admission gate and
// shutdown latch, not via this ctx's cancellation.
func (c *coordinator) recheckDebtOnRelease(sessionID string) {
	if c.asyncJobs == nil || sessionID == "" {
		return
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), recheckDebtCheckBudget)
	debt, err := c.asyncJobs.reactionDebtExists(checkCtx, sessionID)
	cancel()
	if err != nil {
		slog.Warn("onSessionIdle: reaction debt check failed", "session_id", sessionID, "err", err)
		return
	}
	if !debt {
		return
	}
	if c.asyncJobs.isExternalDriver(sessionID) {
		c.asyncJobs.bumpHint(sessionID)
		return
	}
	id := jobIdentity{owner: sessionID, toolCallID: "release-recheck"}
	if err := c.wakeSession(context.Background(), id, true); err != nil {
		slog.Debug("onSessionIdle: release-triggered drain attempt did not complete", "session_id", sessionID, "err", err)
	}
}
