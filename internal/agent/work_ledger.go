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

// maxNoticedBefore bounds workLedger.noticedBefore (§2.4): one entry per
// wake notice persisted, evicted oldest-first once the bound is hit. Sized
// generously for a long-lived web process without letting it grow
// unbounded (orchestrator decision 2026-09-28 item 2).
const maxNoticedBefore = 1000

// noticedBeforeKey is the noticedBefore map key for (owner, toolCallID).
func noticedBeforeKey(owner, toolCallID string) string {
	return owner + "\x00" + toolCallID
}

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
	// Metadata carries the inner tool's raw ToolResponse.Metadata through to
	// a SYNC job's jobResult (§4.4) so awaitAndFinish can reconstruct a
	// byte-for-byte response. Unused by every async (CLI/web) delivery,
	// which only ever formats Content/IsError into a human notice.
	Metadata string
}

// AsyncCompletionSource exposes session completion events to the CLI runner.
// The runner claims its root session first; completions of every other
// session wake that session instead of queueing where nobody reads them.
type AsyncCompletionSource interface {
	ClaimAsyncCompletions(string)
	NextAsyncCompletion(context.Context, string) (AsyncCompletion, bool, error)
	HasPendingAsyncJobs(string) bool
}

// sessionJobs is one owner's view of the ledger: jobs it still owns (present
// == "not yet delivered"), plus its drained ready queue.
type sessionJobs struct {
	jobs    map[string]*asyncJob
	ready   []AsyncCompletion
	changed chan struct{}
	// drained marks a session whose ready queue a CLI loop consumes (the
	// root of a `rush run`). Sticky: completions arriving after that loop
	// returned still queue instead of starting a turn in a finished run.
	drained bool
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

	// noticedBefore maps "owner\x00toolCallID" -> the persisted notice
	// message id, so a repeated wakeSession call for a job already
	// delivered (§1.3) skips re-persisting and goes straight to step 2.
	// Written when wakeSession's step 1 succeeds; removed once the notice
	// has been consumed by the owner's turn (wakeSession's own Run call
	// returning without error, including a queued admission -- either way
	// the owner's mailbox now owns delivering it). A failed Run's entry is
	// kept (so a hypothetical retry still finds ExistingMessageID) until
	// evicted by size (maxNoticedBefore, FIFO via noticedBeforeKeys) --
	// orchestrator decision 2026-09-28 item 2: no unbounded growth.
	noticedBefore     map[string]string
	noticedBeforeKeys []string

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
	if !sync {
		if store == nil {
			return nil, false, errors.New("async job store is unavailable; cannot start an async job")
		}
		claim, err := store.Claim(context.Background(), session.ClaimParams{
			Owner: owner, ToolCallID: toolCallID, Kind: asyncJobKindFor(toolName),
			Input: input, ChildSessionID: childSession, OriginCLI: cli,
			Deadline: timeoutDeadlinePtr(timeout), TimeoutKind: timeoutKindString(timeout),
		})
		if err != nil {
			return nil, false, err // fail-closed (DUR-8)
		}
		claimExisting = claim.Existing
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
		// A durable row exists for this key with no matching in-memory job
		// -- e.g. a previous process's job this one never started an
		// executor for. Refuse rather than silently starting a second
		// executor for a row another host may still own; recovery/adoption
		// of such a row is a later step (doc sec.5 step 5/6).
		return nil, false, fmt.Errorf("async job %s already started (tracked by a previous process)", toolCallID)
	}
	job := &asyncJob{
		owner: owner, toolCallID: toolCallID, input: input, toolName: toolName,
		childSession: childSession, cli: cli, cancel: cancel,
		sync: sync, announced: sync, startedAt: time.Now(),
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

// noticeFor returns the persisted notice message id previously recorded for
// job (§1.3's idempotency key), if any.
func (l *workLedger) noticeFor(job jobIdentity) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id, ok := l.noticedBefore[noticedBeforeKey(job.owner, job.toolCallID)]
	return id, ok
}

// recordNotice records msgID as job's persisted notice message id, evicting
// the oldest entry first once maxNoticedBefore is exceeded (orchestrator
// decision 2026-09-28 item 2: bounded, not unbounded, growth).
func (l *workLedger) recordNotice(job jobIdentity, msgID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := noticedBeforeKey(job.owner, job.toolCallID)
	if l.noticedBefore == nil {
		l.noticedBefore = make(map[string]string)
	}
	if _, exists := l.noticedBefore[key]; !exists {
		l.noticedBeforeKeys = append(l.noticedBeforeKeys, key)
	}
	l.noticedBefore[key] = msgID
	for len(l.noticedBefore) > maxNoticedBefore && len(l.noticedBeforeKeys) > 0 {
		oldest := l.noticedBeforeKeys[0]
		l.noticedBeforeKeys = l.noticedBeforeKeys[1:]
		delete(l.noticedBefore, oldest)
	}
}

// consumeNotice removes job's noticedBefore entry -- from BOTH the map and
// noticedBeforeKeys -- once its notice has been consumed by the owner's turn
// (wakeSession's own Run call returning without error, including a queued
// admission). Orchestrator decision 2026-09-28 item 2: cleared on
// consumption, not left to accumulate until eviction. Removing only from the
// map (an earlier version of this function) left two bugs: noticedBeforeKeys
// grew forever in a long-lived web process (eviction in recordNotice only
// runs once the MAP exceeds maxNoticedBefore, never observing the orphaned
// slice entries), and a key consumed then re-recorded got a SECOND entry in
// the slice (recordNotice's `!exists` check passes again once the map entry
// is gone), so an eviction could pop the stale occurrence and delete the
// fresh map entry it now collides with by key -- deleting a notice that had
// just been re-recorded.
func (l *workLedger) consumeNotice(job jobIdentity) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := noticedBeforeKey(job.owner, job.toolCallID)
	if _, ok := l.noticedBefore[key]; !ok {
		return
	}
	delete(l.noticedBefore, key)
	for i, k := range l.noticedBeforeKeys {
		if k == key {
			l.noticedBeforeKeys = append(l.noticedBeforeKeys[:i], l.noticedBeforeKeys[i+1:]...)
			break
		}
	}
}

// markDrained records that a CLI loop consumes owner's ready queue. CLI
// completions of any other session wake that session through the callback
// instead, like web-origin ones: nobody drains its queue.
func (l *workLedger) markDrained(owner string) {
	if owner == "" {
		return
	}
	l.mu.Lock()
	l.sessionLocked(owner).drained = true
	l.mu.Unlock()
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
// Routing (ready queue vs onWebDone) is byte-for-byte the same condition as
// today's queuesLocked (async_job_registry.go): job.cli && (s.drained ||
// onWebDone == nil).
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
	completion := AsyncCompletion{
		SessionID:      owner,
		ToolCallID:     job.toolCallID,
		ToolName:       job.toolName,
		Content:        job.result.content,
		IsError:        job.result.isError,
		TimedOut:       job.state == phaseTimedOut,
		TimeoutSeconds: job.timeoutSeconds,
		Stopped:        job.state == phaseCancelled && job.childSession == "",
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
	queued := job.cli && (s.drained || l.onWebDone == nil)
	if queued {
		s.ready = append(s.ready, completion)
	}
	signalWorkSession(s)
	completion.cli = job.cli
	return completion, !queued && l.onWebDone != nil
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
		_ = retryAsyncStoreOp(context.Background(), func() error {
			if err := store.MarkAnnounced(context.Background(), sessionID, toolCallID); err != nil {
				if errors.Is(err, session.ErrAsyncJobGone) {
					return nil
				}
				return err
			}
			return nil
		})
	}

	l.mu.Lock()
	s = l.bySession[sessionID]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job = s.jobs[toolCallID]
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
		_ = retryAsyncStoreOp(context.Background(), func() error {
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

// MarkJobStopped implements tools.JobShellResolver. See that interface's doc.
//
// Phase-4 step 2 (doc sec.3.1's external-cause order, "snapshot output ->
// transition with the cause -> stop the executor"): this call itself
// snapshots the job's current partial output and runs the causeJobKill
// transition BEFORE the caller (job_kill's tool wrapper) actually kills the
// background shell. There is no more stopRequested flag for finish() to
// read -- finish's own later call is a no-op once transition has already
// made the job terminal (its CAS loses/skips). Best-effort, matching the
// interface's doc: a jobID that does not resolve, or is already terminal,
// is silently ignored.
func (l *workLedger) MarkJobStopped(owner, toolCallID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	s := l.bySession[owner]
	var job *asyncJob
	if s != nil {
		job = s.jobs[toolCallID]
	}
	if job == nil || job.state.terminal() || job.transitioning {
		l.mu.Unlock()
		return
	}
	toolName, shellID := job.toolName, job.shellID
	l.mu.Unlock()

	partial := l.capturePartial(owner, toolCallID, toolName, "", shellID, nil)
	l.transition(owner, toolCallID, causeJobKill, partial)
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
func (l *workLedger) StopRunCommandJob(owner, jobID string) error {
	if l == nil {
		return fmt.Errorf("job %s not found (async job tracking is unavailable)", jobID)
	}
	l.mu.Lock()
	var job *asyncJob
	if s := l.bySession[owner]; s != nil {
		job = s.jobs[jobID]
	}
	if job == nil || job.toolName != tools.RunCommandToolName {
		l.mu.Unlock()
		return fmt.Errorf("job %s not found (not owned by this session, already delivered, or not a run_command job)", jobID)
	}
	if job.state.terminal() || job.transitioning {
		l.mu.Unlock()
		// Idempotency rule (contract §1.5): a repeat stop on an already-
		// stopping/stopped target is safe but reports the same "not found"
		// shape, not a disguised second success.
		return fmt.Errorf("job %s not found (not owned by this session, already delivered, or already stopped)", jobID)
	}
	var partial string
	if job.outputBuf != nil {
		partial = job.outputBuf.String()
	}
	cancel := job.cancel
	l.mu.Unlock()

	l.transition(owner, jobID, causeJobKill, jobResult{content: partial})
	if cancel != nil {
		cancel() // triggers run_command's cmd.Cancel tree-kill (configureRunCommandProcess)
	}
	return nil
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

func (l *workLedger) next(ctx context.Context, sessionID string) (AsyncCompletion, bool, error) {
	for {
		l.mu.Lock()
		s := l.bySession[sessionID]
		if s == nil {
			l.mu.Unlock()
			return AsyncCompletion{}, false, nil
		}
		if len(s.ready) > 0 {
			completion := s.ready[0]
			s.ready[0] = AsyncCompletion{}
			s.ready = s.ready[1:]
			l.mu.Unlock()
			return completion, true, nil
		}
		if len(s.jobs) == 0 {
			l.mu.Unlock()
			return AsyncCompletion{}, false, nil
		}
		changed := s.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return AsyncCompletion{}, false, ctx.Err()
		case <-changed:
		}
	}
}

func (l *workLedger) pending(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.bySession[sessionID]
	return s != nil && (len(s.jobs) > 0 || len(s.ready) > 0)
}

// running reports whether sessionID still has a job that has NOT reached
// delivery, i.e. one still present in the jobs map (running OR terminal but
// not yet announced/delivered). Deliberately NOT pending(): a ready-queue
// entry is already-delivered work waiting to be drained by the CLI root
// loop, and nobody drains a child session's queue, so counting it would make
// a child look like it owns live work forever.
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
func (l *workLedger) close() {
	l.mu.Lock()
	l.closed = true
	var jobs []*asyncJob
	for _, s := range l.bySession {
		for _, job := range s.jobs {
			jobs = append(jobs, job)
		}
		signalWorkSession(s)
	}
	for _, job := range jobs {
		job.shutdownCancelled = true
	}
	l.mu.Unlock()
	l.timeouts.close()
	for _, job := range jobs {
		if job.cancel != nil {
			job.cancel()
		}
	}
}
