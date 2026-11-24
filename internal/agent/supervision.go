// Supervision: wake the root session after chat silence while its scope
// still has open work (design doc docs/plans/2026-09-27-async-structured-
// concurrency.md §7, wake-tools-contract.md §5.1/§5.2/§6/§7, operator
// decision 2026-09-27 item 5).
//
// Deliberately NOT a counted unit of work: a supervision timer never appears
// in any live-work read, so it can never keep a scope predicate from
// reporting "no open work" -- rush run still exits the instant its OTHER
// work drains, and the timer is simply abandoned when the process exits
// (Go does not wait for stray goroutines).
//
// The single per-process timer goroutine is the shared timeoutService
// (timeout_service.go) -- this file never starts a second worker or a
// goroutine per session. The registry and the service hang off the
// coordinator (the deleted workLedger's retained halves, rehomed).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

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
func (sr *supervisionRegistry) resolveSupervisionConfig(ctx context.Context, host supervisionHost) SupervisionConfig {
	cfg := DefaultSupervisionConfig()
	if cfgStore := host.configStore(); cfgStore != nil {
		if opts := cfgStore.Config().Options; opts != nil {
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
	// flaggedOnce is retained state shape (the legacy silence escalation's
	// memory); nothing writes it in the batch world -- the escalation is
	// gone with the ledger output buffers. Cleared by recordProgress (and
	// noteWorkStarted's resume branch).
	flaggedOnce map[string]bool
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

type supervisionHost interface {
	configStore() *config.ConfigStore
	lookupDriver(string) (subAgentDriver, bool)
	armSupervisionDeadline(time.Time, func())
	handleSupervisionDeadline(string, uint64)
	batchCutoverFacts() BatchCutoverFacts
	agentFor(string) SessionAgent
	sessionNoticeStore() *session.SessionNoticeStore
	admitCallback(*sync.WaitGroup) (context.Context, bool)
	afterReleaseContext(context.Context, string)
	runReleaseCallbackHook(context.Context)
	noteSubAgentChildRunEnded(string)
	sessionService() coresession.Service
	autoResumeSuspended(string) bool
	releaseDriverGeneration(string, uint64)
}

type coordinatorSupervisionHost struct{ coordinator *coordinator }

func (h coordinatorSupervisionHost) configStore() *config.ConfigStore {
	return h.coordinator.cfg
}

func (h coordinatorSupervisionHost) lookupDriver(sessionID string) (subAgentDriver, bool) {
	return h.coordinator.subAgentDrivers.get(sessionID)
}

func (h coordinatorSupervisionHost) armSupervisionDeadline(deadline time.Time, fire func()) {
	h.coordinator.timeouts.ArmFunc(deadline, fire)
}

func (h coordinatorSupervisionHost) handleSupervisionDeadline(sessionID string, generation uint64) {
	h.coordinator.handleSupervisionDeadline(sessionID, generation)
}

func (h coordinatorSupervisionHost) batchCutoverFacts() BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(h.coordinator)
}

func (h coordinatorSupervisionHost) agentFor(sessionID string) SessionAgent {
	return h.coordinator.agentFor(sessionID)
}

func (h coordinatorSupervisionHost) sessionNoticeStore() *session.SessionNoticeStore {
	return h.coordinator.sessionNoticeStore()
}

func (h coordinatorSupervisionHost) admitCallback(wg *sync.WaitGroup) (context.Context, bool) {
	return h.coordinator.admitCallback(wg)
}

func (h coordinatorSupervisionHost) afterReleaseContext(ctx context.Context, sessionID string) {
	h.coordinator.afterReleaseContext(ctx, sessionID)
}

func (h coordinatorSupervisionHost) runReleaseCallbackHook(ctx context.Context) {
	c := h.coordinator
	if c.releaseCallbackHook != nil {
		c.releaseCallbackHook(ctx)
	}
}

func (h coordinatorSupervisionHost) noteSubAgentChildRunEnded(sessionID string) {
	h.coordinator.noteSubAgentChildRunEnded(sessionID)
}

func (h coordinatorSupervisionHost) sessionService() coresession.Service {
	if h.coordinator == nil {
		return nil
	}
	return h.coordinator.sessions
}

func (h coordinatorSupervisionHost) autoResumeSuspended(sessionID string) bool {
	return h.coordinator.autoResumeSuspended(sessionID)
}

func (h coordinatorSupervisionHost) releaseDriverGeneration(sessionID string, generation uint64) {
	h.coordinator.releaseDriverGeneration(sessionID, generation)
}

type supervisionCallbackState struct {
	mu      *sync.Mutex
	closing *bool
	ctx     *context.Context
	cancel  *context.CancelFunc
}

func (c *coordinator) supervisionCallbackState() supervisionCallbackState {
	return supervisionCallbackState{&c.callbackMu, &c.callbackClosing, &c.callbackCtx, &c.callbackCancel}
}

func (c *coordinator) supervisionForCall() *supervisionRegistry {
	if c == nil {
		return nil
	}
	return c.supervision
}

func (c *coordinator) resolveSupervisionConfig(ctx context.Context) SupervisionConfig {
	return c.supervisionForCall().resolveSupervisionConfig(ctx, coordinatorSupervisionHost{c})
}

func (c *coordinator) noteWorkStarted(ctx context.Context, sessionID string) {
	c.supervisionForCall().noteWorkStarted(ctx, sessionID, coordinatorSupervisionHost{c})
}

func (c *coordinator) recordProgress(sessionID string) {
	c.supervisionForCall().recordProgress(sessionID, coordinatorSupervisionHost{c})
}

func (c *coordinator) pushDeadlineOnTurnEnd(sessionID string) {
	c.supervisionForCall().pushDeadlineOnTurnEnd(sessionID, coordinatorSupervisionHost{c})
}

func (c *coordinator) clearSupervisionIfPresent(sessionID string) {
	c.supervisionForCall().clearSupervisionIfPresent(sessionID)
}

func (c *coordinator) supervisionWorkOpen(sessionID string) bool {
	return c.supervisionForCall().supervisionWorkOpen(sessionID, coordinatorSupervisionHost{c})
}

func (c *coordinator) handleSupervisionDeadline(rootSessionID string, generation uint64) {
	c.supervisionForCall().handleSupervisionDeadline(rootSessionID, generation, coordinatorSupervisionHost{c}, c.wakeSession)
}

func (c *coordinator) buildSupervisionSummaryAt(now time.Time, rootSessionID string, tickNum int, interval time.Duration, paused bool) string {
	return c.supervisionForCall().buildSupervisionSummaryAt(now, rootSessionID, tickNum, interval, paused, coordinatorSupervisionHost{c})
}

func (c *coordinator) onSessionIdleHook(sessionID string) {
	c.supervisionForCall().onSessionIdleHook(sessionID, coordinatorSupervisionHost{c}, &c.releaseWG)
}

func (c *coordinator) admitCallback(wg *sync.WaitGroup) (context.Context, bool) {
	return c.supervisionForCall().admitCallback(wg, c.supervisionCallbackState())
}

func (c *coordinator) callbackContextLocked() context.Context {
	return c.supervisionForCall().callbackContextLocked(c.supervisionCallbackState())
}

func (c *coordinator) afterRelease(sessionID string) {
	c.supervisionForCall().afterRelease(sessionID, coordinatorSupervisionHost{c}, &c.releaseWG)
}

func (c *coordinator) afterReleaseContext(ctx context.Context, sessionID string) {
	c.supervisionForCall().afterReleaseContext(ctx, sessionID, coordinatorSupervisionHost{c}, c.wakeSession)
}

func (c *coordinator) releaseChildDriverIfScopeClosed(ctx context.Context, sessionID string) {
	c.supervisionForCall().releaseChildDriverIfScopeClosed(ctx, sessionID, coordinatorSupervisionHost{c})
}

// noteWorkStarted arms (or, if paused, resumes) sessionID's supervision timer
// when new open work appears. A session that is CURRENTLY a registered
// delegation driver's child (c.subAgentDrivers) is never supervised here --
// only the root is (design doc §7: "будится только корень"); a delegated
// child cannot itself be running a bash/run_command call before its driver
// is registered (runSubAgent registers before dispatching the child's first
// turn), so this check is never racing the child's own first Start call.
// Called from the batch delivery's started boundary (the deleted asyncTool
// the arming never holds any other owner lock (the registry's own
// lock-ordering note: its mutex is always taken sequentially, never nested,
// with any delivery lock).
func (sr *supervisionRegistry) noteWorkStarted(ctx context.Context, sessionID string, host supervisionHost) {
	if sr == nil || sessionID == "" {
		return
	}
	if _, isDelegatedChild := host.lookupDriver(sessionID); isDelegatedChild {
		return
	}
	cfg := sr.resolveSupervisionConfig(ctx, host)
	if !cfg.Enabled {
		return
	}
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
		st.flaggedOnce = nil
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
	host.armSupervisionDeadline(deadline, func() { host.handleSupervisionDeadline(sessionID, gen) })
}

// recordProgress resets sessionID's backoff to its base interval and clears
// the no-progress/paused counters: a real completion (job/sub-agent/timeout
// notice) is "progress" regardless of how many no-progress ticks preceded
// it. Called from pullPendingNotices (drain_call_component.go) when a notice
// moves into history, for every notice EXCEPT supervision's own
// (sessionprompt.NoticeKindSupervision) -- see that call site's comment for why the tick's
// own resulting turn must not reset the very backoff it just grew.
func (sr *supervisionRegistry) recordProgress(sessionID string, host supervisionHost) {
	if sr == nil || sessionID == "" {
		return
	}
	sr.mu.Lock()
	st, ok := sr.byRoot[sessionID]
	if !ok {
		sr.mu.Unlock()
		return
	}
	st.interval = st.cfg.Interval
	st.tickCount = 0
	st.paused = false
	st.flaggedOnce = nil
	sr.nextGen++
	st.generation = sr.nextGen
	gen := st.generation
	deadline := time.Now().Add(st.interval)
	sr.mu.Unlock()
	host.armSupervisionDeadline(deadline, func() { host.handleSupervisionDeadline(sessionID, gen) })
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
func (sr *supervisionRegistry) pushDeadlineOnTurnEnd(sessionID string, host supervisionHost) {
	if sr == nil || sessionID == "" {
		return
	}
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
	host.armSupervisionDeadline(deadline, func() { host.handleSupervisionDeadline(sessionID, gen) })
}

// clearSupervisionIfPresent removes sessionID's supervision state entirely.
// Called from deliverLocked the instant a session's job map empties (scope
// closed, §2.3) and from cancelSession (session-level cancel, the closest
// existing proxy this codebase has for "give up on this session's work" --
// see this function's own doc for the known gap around a bare session
// delete with no prior cancel). A stale heap entry for the removed
// generation simply finds byRoot[sessionID] absent when it fires and no-ops.
func (sr *supervisionRegistry) clearSupervisionIfPresent(sessionID string) {
	if sr == nil {
		return
	}
	sr.mu.Lock()
	delete(sr.byRoot, sessionID)
	sr.mu.Unlock()
}

// supervisionWorkOpen is the scope-open input of the supervision deadline:
// batch live work (own roots plus delegation descendants). Unknown reads
// fail open (work treated as open).
func (sr *supervisionRegistry) supervisionWorkOpen(sessionID string, host supervisionHost) bool {
	own, desc, err := host.batchCutoverFacts().LiveWorkSummary(context.Background(), sessionID)
	if err != nil {
		return true
	}
	return own > 0 || desc > 0
}

// handleSupervisionDeadline is the timeoutService fire callback for
// sessionID's armed generation. It re-checks everything under
// supervisionRegistry.mu / the agent mailbox before acting
// (the same "lazy recheck, stale fire is a no-op" discipline
// work_ledger_timeout.go's handleTimeout already documents for asyncJob
// deadlines): a generation mismatch, no open work, or a live turn all make
// this a no-op rather than a wrong tick.
func (sr *supervisionRegistry) handleSupervisionDeadline(rootSessionID string, generation uint64, host supervisionHost, wake func(context.Context, string, bool) error) {
	if sr == nil {
		return
	}

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
	if !sr.supervisionWorkOpen(rootSessionID, host) {
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
	//
	// agentFor can be nil without any race: it falls back to the
	// coordinator's currentAgent, and a coordinator built as a bare struct
	// literal (every ledger-level fixture, anything that arms a deadline
	// before an agent is wired) has none. IsSessionBusy on a nil interface
	// panics, and this callback runs on the timeout service's goroutine --
	// a panic here kills the whole process (observed: the full internal/
	// agent run died here from a fire armed by an earlier test). Nobody can
	// answer the busy question, so this fire is a no-op exactly like a
	// stale generation: the countdown stops until something re-arms it.
	agent := host.agentFor(rootSessionID)
	if agent == nil {
		return
	}
	if agent.IsSessionBusy(rootSessionID) {
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
		st.interval = watchdog.GrowInterval(st.interval, st.cfg.MaxInterval)
	}
	interval := st.interval
	var nextGen uint64
	var nextDeadline time.Time
	if !lastTick {
		sr.nextGen++
		st.generation = sr.nextGen
		nextGen = st.generation
		// This tick moved the state's generation, so the fired one is now
		// stale by definition.
		generation = nextGen
		nextDeadline = time.Now().Add(interval)
	}
	sr.mu.Unlock()

	text := sr.buildSupervisionSummaryAt(time.Now(), rootSessionID, tickNum, interval, lastTick, host)
	if !lastTick {
		host.armSupervisionDeadline(nextDeadline, func() { host.handleSupervisionDeadline(rootSessionID, nextGen) })
	}
	store := host.sessionNoticeStore()
	if store == nil {
		slog.Error("supervision: no session notice store bound; check-in not persisted", "session_id", rootSessionID)
		return
	}
	go func() {
		// Phase-4 step 3 (doc sec.3.2/3.3): persist the check-in as a
		// session_notices row FIRST (no job_tool_call_id -- the void
		// condition for kind=supervision checks "any running row for
		// owner", not one specific job), then submit the wake hint. wake=1
		// per the wake-policy table; the pull (agent_notice_pull.go) voids
		// it instead of delivering it if the scope has since closed.
		if err := store.InsertSessionNotice(context.Background(), rootSessionID, session.NoticeKindSupervision, text, true, ""); err != nil {
			slog.Error("supervision: failed to persist check-in notice", "session_id", rootSessionID, "err", err)
			return
		}
		_ = wake(context.Background(), rootSessionID, true)
	}()
}

// buildSupervisionSummaryAt is the supervision summary: one line per live
// batch node, from the facts' live read, with the delegation count and the
// #1157 awaiting-question line sourced from batch FACTS
// (LiveDelegationNodes' descendants and LiveHeldQuestions' pending
// child_question rows). There is NO output-buffer/last-activity seam on
// BatchCutoverFacts, so no silence line and no flagged job exists here (the
// legacy output-buffer silence escalation is gone with the ledger job map;
// the pause line and the awaiting-question callout are retained).
func (sr *supervisionRegistry) buildSupervisionSummaryAt(now time.Time, rootSessionID string, tickNum int, interval time.Duration, paused bool, host supervisionHost) string {
	facts := host.batchCutoverFacts()
	nodes, err := facts.LiveNodes(context.Background(), rootSessionID)
	lines := make([]string, 0, len(nodes))
	for _, node := range nodes {
		name := node.ToolCallID
		if name == "" {
			name = node.NodeID
		}
		kind := node.TaskKind
		if kind == "" {
			kind = node.Kind
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %s, running %s",
			name, kind, node.State, now.Sub(node.StartedAt).Round(time.Second)))
	}
	// The delegation count is the DESCENDANT facts' count (every live child
	// node naming a delegation child session), not a recount of the tree
	// lines: a descendant whose root line is present still counts once.
	delegations := 0
	if delegationNodes, derr := facts.LiveDelegationNodes(context.Background(), rootSessionID); derr == nil {
		for _, node := range delegationNodes {
			if node.ChildSessionID != "" {
				delegations++
			}
		}
	}
	// #1157 on batch facts: LiveHeldQuestions is the pending child_question
	// debt bound to a LIVE node incarnation (conservative fail-safe: 1 +
	// err). Its rows carry only id+kind (sessionnotice.PendingNoticeDebt), no
	// question text, so the count clause stands alone -- the question
	// itself reaches the parent through the pulled notice. A settled or
	// stale-incarnation question is NOT counted (it can no longer be
	// answered through inject_agent).
	awaitingCount := 0
	if held, herr := facts.LiveHeldQuestions(context.Background(), rootSessionID); herr == nil {
		awaitingCount = len(held)
	}
	body := strings.Join(lines, "\n")
	if body == "" {
		if err != nil {
			body = "(live scope unreadable: " + err.Error() + ")"
		} else {
			body = "(no details available)"
		}
	}
	guidance := "Inspect with batch_output, or stop with batch_stop."
	if awaitingCount > 0 {
		guidance = "A sub-agent is blocked on your answer; waiting will not progress it.\n" + guidance
	}
	if paused {
		guidance = fmt.Sprintf(
			"%s\nSupervision is now paused after %d consecutive check-ins with no progress; it resumes automatically once something changes (a job or sub-agent completes).",
			guidance, tickNum)
	}
	awaitingClause := ""
	if awaitingCount > 0 {
		awaitingClause = fmt.Sprintf(" (%d awaiting your answer)", awaitingCount)
	}
	text := fmt.Sprintf(
		"Supervision check-in (tick %d, interval %gm): %d background job(s) running, %d sub-agent delegation(s) in progress%s.\n\n%s\n\n%s",
		tickNum, interval.Minutes(), len(nodes), delegations, awaitingClause, body, guidance)
	return text
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
// just released" trigger. Only admission runs on the releasing goroutine;
// bookkeeping and DB work run in the registered callback.
func (sr *supervisionRegistry) onSessionIdleHook(sessionID string, host supervisionHost, wg *sync.WaitGroup) {
	ctx, ok := host.admitCallback(wg)
	if !ok {
		return
	}
	go func() {
		defer wg.Done()
		host.afterReleaseContext(ctx, sessionID)
	}()
}

// admitCallback registers BEFORE spawn, under the same mutex that closes
// admission. No positive Add can race shutdown's Wait, including a natural
// ledger completion whose onWebDone invocation was delayed past ledger.close.
func (sr *supervisionRegistry) admitCallback(wg *sync.WaitGroup, state supervisionCallbackState) (context.Context, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if *state.closing {
		return nil, false
	}
	ctx := sr.callbackContextLocked(state)
	wg.Add(1)
	return ctx, true
}

func (sr *supervisionRegistry) callbackContextLocked(state supervisionCallbackState) context.Context {
	if *state.ctx == nil {
		*state.ctx, *state.cancel = context.WithCancel(context.Background())
	}
	return *state.ctx
}

// afterRelease is the release-time work: the phase-3 delegation re-check
// (noteSubAgentChildRunEnded, which returns at once for a session that is
// not a delegated child) and doc sec.3.4 item 3 -- the debt check that
// submits a Drain if the session owes a reaction. Which launches are allowed
// is decided by wakeSession's ONE predicate (policy, then the launch gate the
// finished leg's accounting already wrote): a failed, refused or no-turn Drain
// does not relaunch itself here, and an unreadable debt check goes to the
// re-check set.
func (sr *supervisionRegistry) afterRelease(sessionID string, host supervisionHost, wg *sync.WaitGroup) {
	ctx, ok := host.admitCallback(wg)
	if !ok {
		return
	}
	defer wg.Done()
	host.afterReleaseContext(ctx, sessionID)
}

func (sr *supervisionRegistry) afterReleaseContext(ctx context.Context, sessionID string, host supervisionHost, wake func(context.Context, string, bool) error) {
	host.runReleaseCallbackHook(ctx)
	if sessionID == "" {
		return
	}
	// A delegated child's turn just ended (its first turn, a Drain turn, a
	// turn a Stop cut off). #1130: no parent charge — the child paid its own
	// cost_self as it ran, and readers sum the subtree on demand.
	host.noteSubAgentChildRunEnded(sessionID)
	if err := wake(ctx, sessionID, false); err != nil {
		slog.Debug("onSessionIdle: release-triggered drain attempt did not complete", "session_id", sessionID, "err", err)
	}
	// The wake above routes through agentFor, so it must run BEFORE the
	// driver record can be torn down. After it, close the record's lifecycle:
	// in the batch world nothing else ever unregisters a child's
	// subAgentDriver (the ledger's recheckChild tail that used to call
	// releaseDriverIfScopeClosed is deleted), so agentFor would route every
	// later wake/cancel to a stale task-agent forever, contradicting
	// coordinator_work_scope.go's "a delivered delegation is unregistered by
	// design".
	//
	// REVERT CHECK (batch port, repair round 11 item 3): making this call a
	// no-op leaves TestChildRunEnd_ReleaseChargesNothing red at "and its
	// driver is released" (coordinator_child_cost_test.go) -- the driver
	// record survives Kill + afterRelease forever.
	sr.releaseChildDriverIfScopeClosed(ctx, sessionID, host)
}

// releaseChildDriverIfScopeClosed is afterReleaseContext's driver-lifecycle
// tail: it removes sessionID's subAgentDriver record (and its allowlist
// entry) once the child's scope is closed. Conservative by construction:
//
//   - only a delegated child (a resolvable ParentSessionID) is considered;
//   - a session mid-turn (its driver's mailbox busy) keeps its record -- a
//     live turn means the scope is not closing (the BGShell wake contract:
//     TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays);
//   - a delegation whose batch row still runs AND whose tree was not stopped
//     keeps its record: the delegation still drives the child;
//   - any unreadable read (session row, delegation row) keeps the record.
//
// A stopped tree is autoResumeSuspended (stopTree suspends every member),
// which is the in-process fact that the operator closed the scope even while
// the durable stop request still pends on the executor (in fixtures there is
// no runner at all, so the row can never go terminal on its own). Releasing
// cannot strand debt: the debt stays in the DB, and decide()/drainPolicy
// re-derives every later launch through the policy (a released child's Drain
// is refused there, and a resume_session_id re-registers the driver via
// runSubAgent before its next turn).
func (sr *supervisionRegistry) releaseChildDriverIfScopeClosed(ctx context.Context, sessionID string, host supervisionHost) {
	if host.sessionService() == nil || sessionID == "" {
		return
	}
	observed, registered := host.lookupDriver(sessionID)
	if !registered {
		return
	}
	if ag := host.agentFor(sessionID); ag != nil && ag.IsSessionBusy(sessionID) {
		return
	}
	sess, err := host.sessionService().Get(ctx, sessionID)
	if err != nil {
		return
	}
	if sess.ParentSessionID == "" {
		return
	}
	live, err := host.batchCutoverFacts().LiveDelegationChildOf(ctx, sess.ParentSessionID, sessionID)
	if err != nil {
		return
	}
	if live && !host.autoResumeSuspended(sessionID) {
		return
	}
	// Release exactly the registration the reads above were made about: a
	// resume that registered a newer driver meanwhile keeps its record.
	host.releaseDriverGeneration(sessionID, observed.generation)
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
