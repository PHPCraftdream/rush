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
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
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

// recheckDebtCheckBudget bounds ONLY the launch-decision reads in wakeSession
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
		Interval:      defaultSupervisionInterval(),
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
		cfg.Interval = defaultSupervisionInterval()
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
// the cap on non-terminal async jobs elsewhere (work_ledger_cap.go, ASYNC-12).
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
// it. Called from pullPendingNotices (agent_notice_pull.go) when a notice
// moves into history, for every notice EXCEPT supervision's own
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
// Called from sessionAgent.afterTurn (drain_attempt.go) after every turn
// that reached the provider (a Drain that never did -- opening a tab -- must
// not reset the root's silence timer): the universal "a turn on this session
// just ended" trigger -- this is
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
	// pushDeadlineOnTurnEnd (called from afterTurn) reschedules once that turn ends,
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
		_ = coord.wakeSession(context.Background(), rootSessionID, true)
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

	guidance := "Keep waiting, inspect with job_output, or stop with job_kill (commands); for a sub-agent use inspect_agent, inject_agent or stop_agent."
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
// (coordinator_tools.go): the universal "the mailbox of this session was
// just released" trigger. It does no work on the releasing goroutine (no DB,
// no lock): everything runs in one goroutine (afterRelease).
func (c *coordinator) onSessionIdleHook(sessionID string) {
	go c.afterRelease(sessionID)
}

// afterRelease is the release-time work: the phase-3 delegation re-check
// (noteSubAgentChildRunEnded, which returns at once for a session that is
// not a delegated child) and doc sec.3.4 item 3 -- the debt check that
// submits a Drain if the session owes a reaction. Which launches are allowed
// is decided by wakeSession's ONE predicate (policy, then the launch gate the
// finished leg's accounting already wrote): a failed, refused or no-turn Drain
// does not relaunch itself here, and an unreadable debt check goes to the
// re-check set.
func (c *coordinator) afterRelease(sessionID string) {
	if c.asyncJobs == nil || sessionID == "" {
		return
	}
	// A delegated child's turn just ended (its first turn, a Drain turn, a
	// turn a Stop cut off). #1130: no parent charge — the child paid its own
	// cost_self as it ran, and readers sum the subtree on demand.
	c.noteSubAgentChildRunEnded(sessionID)
	if err := c.wakeSession(context.Background(), sessionID, false); err != nil {
		slog.Debug("onSessionIdle: release-triggered drain attempt did not complete", "session_id", sessionID, "err", err)
	}
}

// supervisionIntervalNS shrinks the built-in initial interval for tests
// (SetSupervisionDefaultIntervalForTest); a value below supervisionMinIntervalFloor
// (0 included) keeps supervisionDefaultInterval.
var supervisionIntervalNS atomic.Int64

func defaultSupervisionInterval() time.Duration {
	if ns := supervisionIntervalNS.Load(); time.Duration(ns) >= supervisionMinIntervalFloor {
		return time.Duration(ns)
	}
	return supervisionDefaultInterval
}
