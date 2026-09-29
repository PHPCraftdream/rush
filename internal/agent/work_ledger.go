// workLedger is the single in-memory work registry (phase 1 of the async
// structured-concurrency rewrite). It replaces asyncJobRegistry (former
// async_job_registry.go, 353 lines) and subAgentOutcomeRegistry (former
// subagent_outcome.go, 740 lines) with one type: the delegation-parking half
// lives in work_ledger_delegation.go, the plain-job half here.
//
// Delivery routing is unchanged from today (docs/plans/2026-09-27-async-
// phase1-spec.md §1.6): a job's completion lands on its owner's ready queue
// when the owner is a drained CLI loop (or there is no web callback at all),
// otherwise it is handed to onWebDone. See deliverLocked.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
)

const maxAsyncJobsPerSession = 50

// AsyncCompletion is the result of a tool that returned before its work ended.
type AsyncCompletion struct {
	SessionID  string
	ToolCallID string
	ToolName   string
	Content    string
	IsError    bool
	// cli is the origin of the job that produced this completion. Set only
	// on the wake-callback path, so the woken turn keeps the job's origin.
	cli bool
	// TimedOut is true when the job's terminal state was phaseTimedOut (set
	// by deliverLocked). Selects the contract's timeout wording and
	// NoticeKind instead of the generic finished/failed one.
	TimedOut bool
	// TimeoutSeconds is job.timeoutSeconds, quoted verbatim in the timeout
	// text; meaningless when !TimedOut.
	TimeoutSeconds int
	// Stopped is true when the job's terminal state was phaseCancelled via a
	// PLAIN job (bash/run_command) job_kill call (task #1023 §2.2) -- never
	// true for a delegation's cancelSession-triggered phaseCancelled, which
	// keeps its own pre-existing "sub-agent canceled" wording. Selects the
	// contract's "stopped (job_kill)" wording and NoticeKind.
	Stopped bool
	// Cancelled is true when the job's terminal state is 'cancelled' via
	// Stop (causeSessionCancel), NOT job_kill (Stopped above). Step 3: a
	// plain job cancelled by Stop used to never reach FormatAsyncCompletion
	// at all (the old memory path silently dropped it, doc sec.3.2's
	// "stoppedBySession" branch) -- now the durable pull (sec.3.3) surfaces
	// its 'pending' row regardless, so it needs its own wording instead of
	// falling into the generic finished/failed branch.
	Cancelled bool
	// Interrupted is true for a row a dead-host recovery sweep transitioned
	// to 'interrupted' (doc sec.3.7, step 5): the row's own host process
	// ended before the job finished naturally. Never produced by the
	// in-memory delivery path (deliverLocked) -- only by
	// buildJobNoticeMessageParams reading a pulled row's NoticeKind, since
	// recovery writes the DB directly and never touches workLedger's memory.
	Interrupted bool
	// Metadata carries the inner tool's raw ToolResponse.Metadata through to
	// a SYNC job's jobResult (§4.4) so awaitAndFinish can reconstruct a
	// byte-for-byte response. Unused by every async (CLI/web) delivery,
	// which only ever formats Content/IsError into a human notice.
	Metadata string
	// Wake is the DB row's own committed wake bit (phase-4 step 3, doc
	// sec.3.4's wake-policy table) for a non-sync job: true for a natural
	// finish/timeout-terminated/delegation-release, false for Stop/job_kill/
	// a failed wake-up marker. notifyAsyncCompletion reads this to decide
	// whether the completion's non-blocking Drain HINT is worth submitting
	// at all -- the pull itself (driver-owned, turn start/PrepareStep)
	// always happens regardless of Wake, this field only gates the HINT.
	// Always false for a sync job (no DB row, no wake concept).
	Wake bool
}

// sessionJobs is one owner's view of the ledger: jobs it still owns (present
// == "not yet delivered"), plus the phase-4 step 4 hint/policy bookkeeping
// the in-memory ready queue used to carry (doc sec.3.4/3.5): the queue
// itself is gone -- every non-sync completion goes through onWebDone now,
// and a `rush run` loop re-derives its next turn from the DB debt/scope
// predicate instead of draining a memory queue.
type sessionJobs struct {
	jobs    map[string]*asyncJob
	changed chan struct{}
	// hintSeq is a monotonic counter bumped every time something about
	// owner's reaction debt may have changed (doc sec.3.4 "у сессии --
	// счётчик подсказок"): a committed transition/notice with wake=1, and
	// every wakeSession call regardless of outcome. Compared, not waited on
	// directly -- see hintSeqLocked/waitForHint.
	hintSeq uint64
	// noTurnDrainRelease is set the instant a Drain call ends WITHOUT a
	// provider turn (no debt, or policy forbade one) and this mailbox
	// release is that Drain's own -- doc sec.3.4's anti-idle-loop rule (a):
	// the NEXT release-triggered debt re-check is skipped when the hint
	// counter is unchanged since the Drain's own check, but every other
	// onSessionIdle side effect (recheckChild, supervision) still runs.
	// Consumed (read-and-cleared) by consumeNoTurnDrainRelease.
	noTurnDrainRelease bool
	// noTurnDrainHintSeq is hintSeq's value at the moment the no-turn Drain
	// made its OWN debt check, so the release-time re-check can tell "hint
	// counter unchanged since then" apart from "something hinted again in
	// between" (doc sec.3.4 rule (a)).
	noTurnDrainHintSeq uint64
	// externalDriver marks a session whose turns are driven by an external
	// loop (the CLI root of a live `rush run` process, doc sec.3.4 "session
	// with an external driver"): wakeSession must send it only a hint, never
	// submit a Drain turn -- the loop re-evaluates its own scope/debt from
	// the DB. Claimed/released by the loop itself (app_run_async.go).
	externalDriver bool
	// admissionRefusedRelease is set the instant runOwned refuses a call
	// because another process already holds the session's OS lock (B2/C2
	// fix, doc sec.3.4 rule (b)): the in-process mailbox reservation was
	// claimed and immediately abandoned WITHOUT any turn (or even an
	// attempt to run one) ever starting. Unlike noTurnDrainRelease this is
	// never hint-gated: the refusal has nothing to do with reaction-debt
	// hints, and retrying immediately is certain to fail identically (the
	// foreign holder does not release just because our hint counter moved).
	// Consumed (read-and-cleared) by consumeAdmissionRefusedRelease.
	admissionRefusedRelease bool
}

// workLedger is the single owner of in-memory work state: plain async jobs
// (bash/run_command) and delegation jobs (agent/agentic_fetch), sharing one
// mutex and one per-job state machine (asyncJob.transitionToTerminal).
type workLedger struct {
	mu        sync.Mutex
	bySession map[string]*sessionJobs // owner -> its jobs
	// byChild indexes delegation jobs (childSession != "") by the child
	// session they ran in, FIFO per child. See work_ledger_delegation.go.
	byChild   map[string][]*asyncJob
	onWebDone func(AsyncCompletion)

	// store is the durable job store (phase-4 step 2, DUR-1/DUR-8): the DB
	// row decides a non-sync async job's terminal state, not memory.
	// Production always wires one (see coordinator.go); nil ONLY in an
	// isolated ledger test that constructs a workLedger by hand without
	// also calling newTestAsyncJobStore, which must not start a non-sync
	// job either (Start fails closed -- see Start's doc).
	store *session.AsyncJobStore

	// coord backs childScopeDrained's background/mailbox-busy checks (see
	// work_ledger_delegation.go) and recheckChild's DB refresh. Nil-safe:
	// isolated ledger tests never set it. Same pattern as
	// subAgentOutcomeRegistry.coord before it.
	coord *coordinator

	closed bool
	// closedCh is closed exactly once, by close(), so every store-retry
	// backoff wait (commitTransition, retryAsyncStoreOp) can select on it
	// instead of blocking in a plain time.Sleep -- a retry loop must return
	// promptly once the ledger is closed (review finding P1), not leak a
	// goroutine sleeping out a full backoff schedule against a DB the
	// process may already be tearing down. closeOnce guards the close(
	// closedCh) call itself: workLedger.close() is not guaranteed single-
	// call (see closeOnce's twin on timeoutService).
	closedCh  chan struct{}
	closeOnce sync.Once

	// timeouts is the single per-process timer service for every asyncJob
	// with a non-zero deadline (§5.4). Nil-safe throughout (timeoutService's
	// methods all check for a nil receiver): isolated ledger tests that
	// never call newTimeoutService simply never arm/fire a timeout.
	timeouts *timeoutService

	// supervision is the per-root-session supervision registry (see
	// supervision.go). Nil-safe throughout: isolated ledger tests that never
	// call newSupervisionRegistry simply never arm a check-in. Deliberately
	// NOT a workLedger job/asyncJob -- it never appears in bySession[x].jobs,
	// so it can never itself keep a session's scope "open" (l.running/
	// next() are computed purely from the jobs map, unaffected by this
	// field) -- see supervision.go's file doc for why that sidesteps the
	// jobKind/HoldsScope question docs/plans/2026-09-28-async-phase3-spec.md
	// §0.2 left for phase 5.
	supervision *supervisionRegistry
}

func newWorkLedger(onWebDone func(AsyncCompletion)) *workLedger {
	return &workLedger{
		bySession: make(map[string]*sessionJobs),
		byChild:   make(map[string][]*asyncJob),
		onWebDone: onWebDone,
		closedCh:  make(chan struct{}),
	}
}

func (l *workLedger) sessionLocked(owner string) *sessionJobs {
	s := l.bySession[owner]
	if s == nil {
		s = &sessionJobs{jobs: make(map[string]*asyncJob), changed: make(chan struct{}, 1)}
		l.bySession[owner] = s
	}
	return s
}

func signalWorkSession(s *sessionJobs) {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Start registers a job for (owner, toolCallID), or -- if that key already
// has a row for the SAME tool and input -- returns the EXISTING job with
// existing=true. That idempotency (closing #1038) means the caller
// (asyncTool.Run) must not start a second executor for a repeated tool call:
// a provider retrying the same call must not repeat the underlying side
// effect (e.g. a second `rm -rf`). A reused id with a DIFFERENT tool or input
// is a distinct call colliding on the id and is refused, so it is never
// silently reported as started without running.
//
// sync is true for a non-CLI/non-Web origin (§4.2): its only consumer is the
// goroutine blocked in awaitSync, so it is never routed to the ready queue
// or onWebDone (see deliverLocked's sync short-circuit). announced=sync
// closes the ack-gate immediately for a sync job, since there is no separate
// "started" tool result to wait for -- the FINAL response IS this call's
// only tool result, exactly like any ordinary synchronous tool.
//
// timeout, when non-nil, arms this job with an explicit per-call deadline
// (§5.4) via l.timeouts (nil-safe: an isolated ledger test with no timer
// service simply never arms).
//
// Phase-4 step 2 (DUR-8): a non-sync job's Start calls store.Claim FIRST --
// if the DB is unavailable or Claim fails, Start returns an error and the
// executor is never started (fail-closed). A DB row that exists for this
// key with NO matching in-memory job (e.g. a previous process's job this
// one never adopted -- recovery/adoption is a later step) is also refused,
// never silently given a second executor. Sync jobs never touch the store
// at all -- they stay on the old memory-only path.
func (l *workLedger) Start(owner, toolCallID, input, toolName, childSession string, cli, sync bool, timeout *TimeoutSpec, cancel context.CancelFunc) (*asyncJob, bool, error) {
	if owner == "" || toolCallID == "" {
		return nil, false, errors.New("async job requires a session and tool call ID")
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, false, errors.New("async job registry is closed")
	}
	s := l.sessionLocked(owner)
	if existing, ok := s.jobs[toolCallID]; ok {
		l.mu.Unlock()
		if existing.toolName != toolName || existing.input != input {
			return nil, false, fmt.Errorf("async job %s is already running", toolCallID)
		}
		return existing, true, nil
	}
	if len(s.jobs) >= maxAsyncJobsPerSession {
		l.mu.Unlock()
		return nil, false, fmt.Errorf("maximum number of async jobs (%d) reached", maxAsyncJobsPerSession)
	}
	store := l.store
	l.mu.Unlock()

	var claimExisting bool
	var claimID string
	if !sync {
		if store == nil {
			return nil, false, errors.New("async job store is unavailable; cannot start an async job")
		}
		timeoutSeconds := 0
		if timeout != nil {
			timeoutSeconds = timeout.Seconds
		}
		claim, err := store.Claim(context.Background(), session.ClaimParams{
			Owner: owner, ToolCallID: toolCallID, Kind: asyncJobKindFor(toolName),
			Input: input, ChildSessionID: childSession, OriginCLI: cli,
			Deadline: timeoutDeadlinePtr(timeout), TimeoutKind: timeoutKindString(timeout),
			ToolName: toolName, TimeoutSeconds: timeoutSeconds,
		})
		if err != nil {
			return nil, false, err // fail-closed (DUR-8)
		}
		claimExisting = claim.Existing
		// A11: this claim's own claim_id, carried by the executor for the
		// job's whole lifetime and threaded into every future commitTransition
		// call (work_ledger_transition.go). Meaningless when claimExisting
		// (Start refuses that case below, never creating a job for it).
		claimID = claim.Row.ClaimID
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, false, errors.New("async job registry is closed")
	}
	s = l.sessionLocked(owner)
	if existing, ok := s.jobs[toolCallID]; ok {
		if existing.toolName != toolName || existing.input != input {
			return nil, false, fmt.Errorf("async job %s is already running", toolCallID)
		}
		return existing, true, nil
	}
	if claimExisting {
		// A durable row exists for this key with no matching in-memory job.
		// The common case is mundane: THIS process already ran and delivered
		// this exact tool call, and the provider is repeating it -- not
		// necessarily "a previous process" (review finding P3). Refuse
		// rather than silently starting a second executor for a row that
		// may still be owned by a live host; recovery/adoption of a row a
		// dead host owns is a later step (doc sec.5 step 5/6).
		return nil, false, fmt.Errorf("async job %s was already started earlier; not starting it again", toolCallID)
	}
	job := &asyncJob{
		owner: owner, toolCallID: toolCallID, input: input, toolName: toolName,
		childSession: childSession, cli: cli, cancel: cancel,
		sync: sync, announced: sync, startedAt: time.Now(), claimID: claimID,
	}
	if sync {
		job.done = make(chan struct{})
	}
	if timeout != nil {
		job.deadline = timeout.Deadline
		job.timeoutKind = timeout.Kind
		job.timeoutSeconds = timeout.Seconds
		l.timeouts.arm(job) // nil-safe: isolated ledger tests never wire a timeoutService
	}
	s.jobs[toolCallID] = job
	signalWorkSession(s)
	return job, false, nil
}

// deliverLocked delivers job if it is still present in bySession[owner].jobs
// (i.e. not already delivered by a concurrent winner of the state race),
// terminal, and announced. It is safe to call unconditionally after ANY
// transitionToTerminal attempt, win or lose: a losing caller's job is either
// already gone (this no-ops) or still mid-race (this reads whatever the
// CURRENT, single, coherent state+result is -- transitionToTerminal sets
// both under the same lock hold, so a reader here never sees a torn mix of
// two different outcomes' fields).
//
// Phase-4 step 4 (doc sec.3.4/3.5): the in-memory ready queue is gone --
// every non-sync completion, CLI-origin or web, now routes through
// onWebDone (notifyAsyncCompletion), which persists the wake hint and lets
// wakeSession decide turn vs. hint-only by session policy (an external-
// driver session, i.e. a live `rush run` loop's own root, gets a hint only).
// A ledger with no onWebDone wired (isolated tests) simply drops the
// completion, same as before for that degenerate case.
//
// Caller must hold l.mu, and must invoke the returned callback (if any)
// AFTER releasing it.
func (l *workLedger) deliverLocked(owner string, job *asyncJob) (AsyncCompletion, bool) {
	s := l.bySession[owner]
	if s == nil {
		return AsyncCompletion{}, false
	}
	current, present := s.jobs[job.toolCallID]
	if !present || current != job || !job.state.terminal() || !job.announced {
		return AsyncCompletion{}, false
	}
	if job.sync {
		// A sync job's only consumer is the goroutine blocked in awaitSync
		// (§4.4): never routed to the ready queue or onWebDone.
		delete(s.jobs, job.toolCallID)
		if job.childSession != "" {
			l.gcChildLocked(job.childSession)
		}
		if len(s.jobs) == 0 {
			// Scope closed (§2.3): no open work left for owner, so its
			// supervision timer (if any) is dropped -- see supervision.go.
			l.clearSupervisionIfPresent(owner)
		}
		close(job.done)
		return AsyncCompletion{}, false
	}
	if job.stoppedBySession && job.childSession == "" {
		// Doc sec.3.4: a plain job cancelled by Stop produces no in-memory
		// notice regardless of which cause actually won the CAS -- only the
		// DB row records the fact (review finding P2). This guard used to
		// live in transition()'s own caller and fire unconditionally the
		// instant the CAS committed, BEFORE this job was ever necessarily
		// announced (B10): Stop landing between Claim and the "started" ack
		// would drop the row from memory right then, so when the ack later
		// arrived there was no job left to mark announced=true on, orphaning
		// the DB row at announced=0/delivery=pending forever. Living HERE
		// instead means the top guard's own `!job.announced` check already
		// withholds this until announced actually flips (finishAcknowledgeLocally
		// calls deliverLocked again once it does) -- so the drop now happens
		// exactly once, whenever announced first becomes true, never before.
		delete(s.jobs, job.toolCallID)
		if len(s.jobs) == 0 {
			l.clearSupervisionIfPresent(owner)
		}
		signalWorkSession(s)
		return AsyncCompletion{}, false
	}
	completion := AsyncCompletion{
		SessionID:      owner,
		ToolCallID:     job.toolCallID,
		ToolName:       job.toolName,
		Content:        job.result.content,
		IsError:        job.result.isError,
		TimedOut:       job.state == phaseTimedOut,
		TimeoutSeconds: job.timeoutSeconds,
		Stopped:        job.state == phaseCancelled && job.childSession == "",
		Wake:           job.wake,
	}
	delete(s.jobs, job.toolCallID)
	if job.childSession != "" {
		l.gcChildLocked(job.childSession)
	}
	if len(s.jobs) == 0 {
		// Scope closed (§2.3): no open work left for owner, so its
		// supervision timer (if any) is dropped -- see supervision.go.
		l.clearSupervisionIfPresent(owner)
	}
	signalWorkSession(s)
	completion.cli = job.cli
	return completion, l.onWebDone != nil
}

// acknowledged is the ack-gate: marks that the "started" tool result for
// (sessionID, toolCallID) is persisted (agent_turn_stream.go's onToolResult,
// called after a successful messages.Create), then attempts delivery. Name
// unchanged from today (async_job_registry.go's acknowledged) -- the caller
// is not touched by phase 1.
//
// Phase-4 step 2 (DUR-7): a non-sync job's "started" tool result is durably
// marked via store.MarkAnnounced BEFORE the in-memory announced flag flips,
// with the same growing-backoff retry rule as transition (no shutdown-latch
// check here -- unlike transition, this is not tied to an executor
// cancelled by close()). A gone row (e.g. a Rerun truncation raced it) is a
// benign no-op, not a retry target.
func (l *workLedger) acknowledged(sessionID, toolCallID string) {
	l.mu.Lock()
	s := l.bySession[sessionID]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	if job == nil {
		l.mu.Unlock()
		return
	}
	store, sync := l.store, job.sync
	l.mu.Unlock()

	if !sync && store != nil {
		_ = l.retryAsyncStoreOp(context.Background(), func() error {
			if err := store.MarkAnnounced(context.Background(), sessionID, toolCallID); err != nil {
				if errors.Is(err, session.ErrAsyncJobGone) {
					return nil
				}
				return err
			}
			return nil
		})
	}

	l.finishAcknowledgeLocally(sessionID, toolCallID)
}

// finishAcknowledgeLocally is the ack gate's in-memory tail (job.announced
// flip + deliverLocked/onWebDone), shared by acknowledged (the plain
// MarkAnnounced path) and acknowledgeWithMessageTx (step 6's fused DB
// transaction, work_ledger_announce.go) -- the durable half differs between
// the two callers, this half does not: a job whose terminal transition
// already committed (job.state.terminal()) while announced was still false
// is delivered HERE, the instant announced flips (deliverLocked's own
// `!job.announced` guard is what withheld it until now) -- this is the
// "fast job that finishes before the ack" case's existing wake-hint path
// (doc sec.3.8's Ack gate paragraph), not a new mechanism.
func (l *workLedger) finishAcknowledgeLocally(sessionID, toolCallID string) {
	l.mu.Lock()
	s := l.bySession[sessionID]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	if job == nil {
		l.mu.Unlock()
		return
	}
	job.announced = true
	completion, callback := l.deliverLocked(sessionID, job)
	l.mu.Unlock()
	if callback {
		l.onWebDone(completion)
	}
}

// abort drops (sessionID, toolCallID) and cancels its executor context.
// Called ONLY when the "started" tool result write itself failed -- strictly
// before Announce could ever be called for this id, so there is nothing to
// preserve (ASYNC-05). Name unchanged from today (async_job_registry.go's
// abort). Phase-4 step 2: a non-sync job's durable row is deleted via
// store.DeleteUnannounced (same growing-backoff retry rule) before the
// in-memory drop.
func (l *workLedger) abort(sessionID, toolCallID string) {
	l.mu.Lock()
	s := l.bySession[sessionID]
	var job *asyncJob
	if s != nil {
		job = s.jobs[toolCallID]
	}
	store, sync := l.store, false
	if job != nil {
		sync = job.sync
	}
	l.mu.Unlock()

	if !sync && store != nil {
		_ = l.retryAsyncStoreOp(context.Background(), func() error {
			return store.DeleteUnannounced(context.Background(), sessionID, toolCallID)
		})
	}

	l.mu.Lock()
	s = l.bySession[sessionID]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job = s.jobs[toolCallID]
	delete(s.jobs, toolCallID)
	signalWorkSession(s)
	l.mu.Unlock()
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

// awaitSync blocks until job's outcome is ready (deliverLocked closed
// job.done) or ctx is done. job.sync must be true. Reads job.result under
// l.mu -- deliverLocked writes it (via transitionToTerminal) under the same
// lock before closing done, so there is no torn read.
func (l *workLedger) awaitSync(ctx context.Context, job *asyncJob) (jobResult, error) {
	select {
	case <-job.done:
		l.mu.Lock()
		res := job.result
		l.mu.Unlock()
		return res, nil
	case <-ctx.Done():
		// The job's own executor context is EITHER ctx itself (sync path,
		// §4.2) or derived from it -- cancelling ctx already cancels the
		// executor, which will reach finalize/deliverLocked on its own and
		// eventually close job.done. This branch does not need to cancel
		// anything itself; it only stops THIS caller from waiting further.
		return jobResult{}, ctx.Err()
	}
}

// setShellID records the background shell id backing a running command job,
// as soon as asyncTool.awaitShell learns it from the inner tool's response
// metadata. No-op if the job already finished/was removed (e.g. a fast
// command whose completion raced this call) -- there is nothing left to
// annotate. Task #1053: this is what makes ResolveJobShellID possible while
// the job is still running.
func (l *workLedger) setShellID(owner, toolCallID, shellID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.bySession[owner]
	if s == nil {
		return
	}
	if job := s.jobs[toolCallID]; job != nil {
		job.shellID = shellID
	}
}

// ResolveJobShellID resolves a model-visible job id (the async job id
// returned in "Async <tool> job <id> started", asyncToolMetadata.JobID) to
// the background shell id backing it, scoped to owner: a job from another
// session never resolves, the same ownership boundary
// BackgroundShellManager.GetOwned/KillOwned already enforce for shell_id.
// Implements tools.JobShellResolver -- see coordinator_tools.go's wiring.
// Task #1053: before this, job_kill/job_output could never be called on a
// still-running CLI/web command, because the model only ever learns the
// job id, never the shell id (that stays inside asyncTool.awaitShell until
// the job is already finished).
func (l *workLedger) ResolveJobShellID(owner, jobID string) (string, error) {
	if l == nil {
		return "", fmt.Errorf("job %s not found (async job tracking is unavailable)", jobID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var job *asyncJob
	if s := l.bySession[owner]; s != nil {
		job = s.jobs[jobID]
	}
	if job == nil {
		return "", fmt.Errorf("job %s not found (not owned by this session, or already delivered)", jobID)
	}
	if job.toolName == tools.RunCommandToolName {
		// No background shell to resolve to; callers route this to
		// RunCommandController instead (task #1023 §3).
		return "", &tools.RunCommandJobError{JobID: jobID}
	}
	if job.toolName != tools.BashToolName {
		// agent/agentic_fetch: a delegation, not a command.
		return "", &tools.DelegationJobError{JobID: jobID, ChildSessionID: job.childSession}
	}
	if job.shellID == "" {
		return "", fmt.Errorf("job %s is still starting; try again in a moment", jobID)
	}
	return job.shellID, nil
}

// completionFromSnapshot builds the AsyncCompletion a caller that LOST its
// own terminal-transition attempt must answer with (B11): snap reflects
// whatever cause actually committed, so the wording must come from IT, not
// from the losing caller's own intent. Cancelled (not Stopped) for a
// phaseCancelled snapshot -- reaching here at all means THIS caller's own
// causeJobKill did not win, so any cancellation already committed came from
// something else (session Stop is the only other source of a plain job's
// 'cancelled' state that still reaches a snapshot here; deliverLocked drops
// a Stop-caused cancellation before commitAndDeliver ever returns a
// found=true snapshot for one of ITS OWN transitions, but this snapshot is
// read directly from the job, before delivery's own drop, so it still
// reflects the truth here).
func completionFromSnapshot(toolCallID, toolName string, snap jobOutcomeSnapshot, timeoutSeconds int) AsyncCompletion {
	c := AsyncCompletion{
		ToolCallID: toolCallID, ToolName: toolName,
		Content: snap.result.content, IsError: snap.result.isError,
	}
	switch snap.state {
	case phaseTimedOut:
		c.TimedOut = true
		c.TimeoutSeconds = timeoutSeconds
	case phaseCancelled:
		c.Cancelled = true
	}
	return c
}

// MarkJobStopped implements tools.JobShellResolver. See that interface's doc.
//
// Phase-4 step 2 (doc sec.3.1's external-cause order, "snapshot output ->
// transition with the cause -> stop the executor"): this call itself
// snapshots the job's current partial output and runs the causeJobKill
// transition BEFORE the caller (job_kill's tool wrapper) actually kills the
// background shell. There is no more stopRequested flag for finish() to
// read -- finish's own later call is a no-op once transition has already
// made the job terminal (its CAS loses/skips).
//
// Task #1063 (step 6): the return value IS the job_kill tool's own final
// answer -- the real output snapshot just captured, worded via
// FormatAsyncCompletion(Stopped: true) exactly like the contract's "stopped
// (job_kill)" notice text, so job_kill never again answers with a content-
// free "terminated successfully" while the row's own notice (now
// delivery='done', never pulled) carries the real output nobody sees.
//
// B11: that "stopped" answer is only correct when THIS call's own
// causeJobKill transition actually won the CAS -- a concurrent natural
// finish/timeout/Stop can commit first in the window between the initial
// guard above and commitAndDeliver's own store write (capturePartial's I/O
// runs in between, unlocked). ok is false when jobID does not resolve to a
// live, ledger-tracked job still running, OR when this call's own attempt
// did not win: job_kill then falls back to its pre-existing bgManager-driven
// flow/wording for the "not found" case, and for the "lost the race" case is
// refused outright rather than answering "stopped" for an outcome this call
// did not cause (job_kill.go, B11) -- the "already stopped"/idempotent shape
// the doc's ASYNC-01 sibling rule (§1.5) requires either way. killRequested
// (set before any of the DB/kill I/O below runs) closes the narrow window
// where a truly concurrent second call could otherwise race ahead of the
// first's own transitioning latch and be handed the same "proceed" verdict.
func (l *workLedger) MarkJobStopped(owner, toolCallID string) (text string, ok bool) {
	if l == nil {
		return "", false
	}
	l.mu.Lock()
	s := l.bySession[owner]
	var job *asyncJob
	if s != nil {
		job = s.jobs[toolCallID]
	}
	if job == nil || job.state.terminal() || job.transitioning || job.killRequested {
		l.mu.Unlock()
		return "", false
	}
	job.killRequested = true
	sync := job.sync
	toolName, shellID, timeoutSeconds := job.toolName, job.shellID, job.timeoutSeconds
	l.mu.Unlock()

	partial := l.capturePartial(owner, toolCallID, toolName, "", shellID, nil)
	if sync {
		// Review finding P2 (regression of #1023 for library/SDK mode): a
		// sync job never touches the store, so it must reach its "stopped
		// (job_kill)" outcome via the OLD memory-only path -- otherwise a
		// blocked awaitSync caller silently loses that outcome. A sync job's
		// own transitionSyncStopped has no CAS to lose (in-memory only,
		// guarded by the SAME top check above under the SAME lock it never
		// releases in between) -- B11's race does not apply to it.
		l.transitionSyncStopped(owner, toolCallID, partial)
		return FormatAsyncCompletion(AsyncCompletion{
			ToolCallID: toolCallID, ToolName: toolName,
			Content: partial.content, IsError: partial.isError, Stopped: true,
		}), true
	}
	outcome, snap := l.commitAndDeliver(owner, toolCallID, causeJobKill, partial)
	if outcome != commitWon {
		if !snap.found {
			return "", false
		}
		return FormatAsyncCompletion(completionFromSnapshot(toolCallID, toolName, snap, timeoutSeconds)), true
	}
	return FormatAsyncCompletion(AsyncCompletion{
		ToolCallID: toolCallID, ToolName: toolName,
		Content: partial.content, IsError: partial.isError, Stopped: true,
	}), true
}

// setRunCommandBuffer records a run_command job's live output sink, as soon
// as run_command.go's process starts (async_tool.go's context sink, task
// #1023 §3). No-op if the job already finished/was removed -- mirrors
// setShellID's own doc.
func (l *workLedger) setRunCommandBuffer(owner, toolCallID string, buf tools.LiveOutputBuffer) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.bySession[owner]
	if s == nil {
		return
	}
	if job := s.jobs[toolCallID]; job != nil {
		job.outputBuf = buf
	}
}

// RunCommandOutput implements tools.RunCommandController. See that
// interface's doc.
func (l *workLedger) RunCommandOutput(owner, jobID string, cursor int64) (string, bool, int64, error) {
	if l == nil {
		return "", false, 0, fmt.Errorf("job %s not found (async job tracking is unavailable)", jobID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var job *asyncJob
	if s := l.bySession[owner]; s != nil {
		job = s.jobs[jobID]
	}
	if job == nil || job.toolName != tools.RunCommandToolName {
		return "", false, 0, fmt.Errorf("job %s not found (not owned by this session, already delivered, or not a run_command job)", jobID)
	}
	if job.outputBuf == nil {
		// Narrow start-up race: Start() registered the job but run_command's
		// process has not called back through the context sink yet.
		return "", false, cursor, nil
	}
	data, next := job.outputBuf.Read(cursor)
	return data, job.state.terminal(), next, nil
}

// StopRunCommandJob implements tools.RunCommandController. See that
// interface's doc.
//
// Phase-4 step 2 (doc sec.3.1's external-cause order): snapshot -> transition
// -> kill. The live output buffer is snapshotted and the causeJobKill
// transition committed BEFORE ctx is cancelled, so a killed run_command's
// own "context canceled" return (its ctx-cancellation branch discards its
// real partial content) can never race ahead of and overwrite the intended
// cause -- finish's later call simply finds the job already terminal.
//
// Task #1063 (step 6): on success, text is job_kill's own final answer --
// the real output snapshot just captured, worded via
// FormatAsyncCompletion(Stopped: true) -- so job_kill returns it directly
// instead of its old "kill requested; result will arrive as a message"
// placeholder. The error return keeps its pre-existing shape/wording
// unchanged (idempotency rule, contract §1.5): a repeat stop answers with
// the same "not found ... or already stopped" text as before this task.
//
// B11: like MarkJobStopped, this call's own causeJobKill transition can lose
// the race to a concurrent natural finish/timeout/Stop in the window between
// the guard above and commitAndDeliver's store write -- the returned text
// must reflect what actually committed, never assume this call caused it.
func (l *workLedger) StopRunCommandJob(owner, jobID string) (text string, err error) {
	if l == nil {
		return "", fmt.Errorf("job %s not found (async job tracking is unavailable)", jobID)
	}
	l.mu.Lock()
	var job *asyncJob
	if s := l.bySession[owner]; s != nil {
		job = s.jobs[jobID]
	}
	if job == nil || job.toolName != tools.RunCommandToolName {
		l.mu.Unlock()
		return "", fmt.Errorf("job %s not found (not owned by this session, already delivered, or not a run_command job)", jobID)
	}
	if job.state.terminal() || job.transitioning || job.killRequested {
		l.mu.Unlock()
		// Idempotency rule (contract §1.5): a repeat stop on an already-
		// stopping/stopped target is safe but reports the same "not found"
		// shape, not a disguised second success.
		return "", fmt.Errorf("job %s not found (not owned by this session, already delivered, or already stopped)", jobID)
	}
	job.killRequested = true
	sync := job.sync
	timeoutSeconds := job.timeoutSeconds
	var partial string
	if job.outputBuf != nil {
		partial = job.outputBuf.String()
	}
	cancel := job.cancel
	l.mu.Unlock()

	result := jobResult{content: partial}
	if sync {
		// Review finding P2: same sync/memory-only path as MarkJobStopped --
		// no CAS to lose (B11 does not apply to a sync job).
		l.transitionSyncStopped(owner, jobID, result)
		if cancel != nil {
			cancel()
		}
		return FormatAsyncCompletion(AsyncCompletion{
			ToolCallID: jobID, ToolName: tools.RunCommandToolName,
			Content: result.content, IsError: result.isError, Stopped: true,
		}), nil
	}
	outcome, snap := l.commitAndDeliver(owner, jobID, causeJobKill, result)
	if cancel != nil {
		cancel() // triggers run_command's cmd.Cancel tree-kill (configureRunCommandProcess); harmless no-op if the process already exited via another cause
	}
	if outcome != commitWon {
		if !snap.found {
			return "", fmt.Errorf("job %s not found (not owned by this session, already delivered, or already stopped)", jobID)
		}
		return FormatAsyncCompletion(completionFromSnapshot(jobID, tools.RunCommandToolName, snap, timeoutSeconds)), nil
	}
	return FormatAsyncCompletion(AsyncCompletion{
		ToolCallID: jobID, ToolName: tools.RunCommandToolName,
		Content: result.content, IsError: result.isError, Stopped: true,
	}), nil
}

// finish is the terminal transition for a PLAIN job's (bash/run_command)
// NATURAL completion (causeNaturalFinish). Not used for a delegation (agent/
// agentic_fetch) -- see armDelegation. For a sync job it still writes
// directly via transitionToTerminal (sync jobs never touch the store, doc
// sec.3.1) -- see the sync branch below.
//
// Phase-4 step 2: this used to special-case job.stopRequested to build the
// "stopped (job_kill)" result itself. That is gone -- MarkJobStopped/
// StopRunCommandJob now transition the job to cancelled BEFORE the caller
// kills the process (snapshot -> transition -> kill), so by the time THIS
// call arrives for a job_kill'd job, the job is already terminal and
// l.transition below is a no-op (ASYNC-03's CAS skip), never overwriting
// the recorded cause with the killed process's own exit content.
func (l *workLedger) finish(owner, toolCallID string, result jobResult) {
	l.mu.Lock()
	s := l.bySession[owner]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	if job == nil {
		l.mu.Unlock()
		return
	}
	// B9: mark this job's executor as having genuinely returned BEFORE
	// releasing l.mu for the first time -- close() consults this so it never
	// latches shutdownCancelled onto a job whose real, natural outcome is
	// already known (or about to be committed below) just because it
	// happens to still be non-terminal in memory at the instant close()
	// scans. See executorReturned's own doc (work_job.go) and close()'s.
	job.executorReturned = true
	if job.sync {
		job.transitionToTerminal(phaseFor(result), result)
		completion, callback := l.deliverLocked(owner, job)
		l.mu.Unlock()
		if callback {
			l.onWebDone(completion)
		}
		return
	}
	l.mu.Unlock()
	l.transition(owner, toolCallID, causeNaturalFinish, result)
}

// phaseFor maps a sync job's raw result to its terminal jobPhase. Sync jobs
// never go through causeStateNoticeKindWake (no DB row at all).
func phaseFor(result jobResult) jobPhase {
	if result.isError {
		return phaseFailed
	}
	return phaseCompleted
}

// running reports whether sessionID still has a job that has NOT reached
// delivery, i.e. one still present in the jobs map (running OR terminal but
// not yet announced/delivered). Phase-4 step 4 deleted the separate
// ready-queue/pending() concept entirely (doc sec.3.5) -- every non-sync
// completion routes through onWebDone now, so this in-memory map is the
// only "does owner still own live work" signal left at this layer.
func (l *workLedger) running(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.bySession[sessionID]
	return s != nil && len(s.jobs) > 0
}

// close is the graceful-exit path (doc sec.3.1/3.7: "graceful exit =
// crash"). It no longer transitions anything -- a natural, non-shutdown-
// caused transition already in flight still commits; every OTHER job's
// executor is cancelled with shutdownCancelled set first, so the shutdown
// latch inside transition (see work_ledger_transition.go) suppresses its
// write and leaves the row 'running' for the next host to recover. Only
// cancels executors and stops the timeout service -- it does NOT call
// cancelSession or otherwise write a terminal state.
//
// B9: the latch is set ONLY for a job close() is actually about to cancel --
// one that is both non-terminal (job.state.terminal() false: a terminal job
// has nothing left for close() to interrupt) and not executorReturned (its
// real, natural outcome is not already known/in flight, see that field's own
// doc). The old code marked EVERY job present in the map unconditionally,
// including ones whose executor had already returned with a real result and
// was merely waiting its turn for l.mu to report it: that natural
// completion's own (unrelated-to-shutdown) commitTransition call would then
// see shuttingDown=true on its very first attempt and abandon it entirely,
// leaving the row 'running' forever for a later recovery sweep to mark
// 'interrupted' -- discarding a result that was never actually lost to
// shutdown at all (violates §3.1).
func (l *workLedger) close() {
	l.mu.Lock()
	l.closed = true
	var toCancel []*asyncJob
	for _, s := range l.bySession {
		for _, job := range s.jobs {
			if !job.state.terminal() && !job.executorReturned {
				job.shutdownCancelled = true
				toCancel = append(toCancel, job)
			}
		}
		signalWorkSession(s)
	}
	l.mu.Unlock()
	l.closeOnce.Do(func() { close(l.closedCh) })
	l.timeouts.close()
	for _, job := range toCancel {
		if job.cancel != nil {
			job.cancel()
		}
	}
}
