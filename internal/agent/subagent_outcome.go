// Parked sub-agent delegation outcomes.
//
// The `agent` and `agentic_fetch` tools are dispatched asynchronously for
// CLI and web origins (see asyncTool.Run and wrapAsyncTools). The
// completion delivered to the PARENT session used to be produced the
// instant the CHILD's Run() returned, which is only the end of one model
// TURN: a child that started its own async tools or background shells has
// yielded, not finished. That produced a parent notice reading "Async job
// ... finished." sitting on top of content that said the child was still
// waiting on its own work, and the child's later self-directed work then
// only ever reached the child session — so no final result ever reached
// the parent.
//
// This registry parks the delegation outcome instead of finishing it
// immediately, and releases it exactly once, only once the child's own
// async work is terminal. Every release goes through the single emission
// point asyncJobRegistry.finishParked, so the CLI-ready queue and the web
// auto-resume path stay byte-for-byte what they were.
//
// ACCEPTED, PRE-EXISTING VISIBILITY GAP (out of scope this cycle): if the
// child auto-resumes itself on its own background jobs AFTER the
// delegation's own async work has drained, the single delegation notice is
// emitted at that point and the self-directed follow-on work is not tracked
// for the parent. That is the same behavior as before this registry
// existed; closing it would require attributing every child-session turn
// back to a specific parent delegation, which is a different change.
package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
)

// subAgentOutcomeTickInterval is the fallback re-check period. It only ever
// runs while at least one entry is parked, and it is a SAFETY NET: the
// ordinary re-check triggers are (i) the park site, (ii) a child async-job
// completion, (iii) a child background-job completion, and (iv) the end of
// a child run (coordinator_run.go's runInternal exit). A var, not a const,
// so a test can shrink it instead of sleeping through the real delay.
var subAgentOutcomeTickInterval = 2 * time.Second

// subAgentOutcomeCancelledText is the body delivered to the parent when a
// parked delegation is released by Cancel/CancelAll rather than by the
// child finishing its work.
const subAgentOutcomeCancelledText = "sub-agent canceled"

// subAgentOutcomeEntry is ONE delegated sub-agent call whose completion is
// waiting for the child's own async work to become terminal.
type subAgentOutcomeEntry struct {
	// childSessionID is the sub-session the delegation ran in. The park
	// registry is keyed by it, because that is the session whose async
	// jobs and background shells must drain.
	childSessionID string
	// parentSessionID is the session whose tool call spawned the
	// delegation. The notice is delivered there, never to the child.
	parentSessionID string
	toolCallID      string
	toolName        string
	// cli mirrors the flag the originating asyncTool.Run passed to
	// asyncJobs.start: it decides whether the released completion lands on
	// the CLI ready queue or on the web auto-resume callback.
	cli bool
	// completion holds the child's result as captured when its run
	// returned. IsError distinguishes failure from success; a cancel
	// release overwrites it with subAgentOutcomeCancelledText.
	completion AsyncCompletion
	// released is the one-shot latch. It is claimed under the registry
	// mutex BEFORE the completion is refreshed or emitted, so no trigger
	// can deliver the same entry twice.
	released bool
}

// subAgentOutcomeRegistry holds the parked entries, keyed by child session
// id. Each value is a FIFO: a child that is resumed (resume_session_id) and
// yields again parks a SECOND entry, and both must be delivered in order.
type subAgentOutcomeRegistry struct {
	coord *coordinator

	mu       sync.Mutex
	byChild  map[string][]*subAgentOutcomeEntry
	tickStop chan struct{}
	closed   bool
}

func newSubAgentOutcomeRegistry(coord *coordinator) *subAgentOutcomeRegistry {
	return &subAgentOutcomeRegistry{
		coord:   coord,
		byChild: make(map[string][]*subAgentOutcomeEntry),
	}
}

// park appends entry and makes sure the fallback re-check ticker is
// running. Nil-receiver and nil-coordinator safe so bare test fixtures
// keep working.
func (r *subAgentOutcomeRegistry) park(entry *subAgentOutcomeEntry) {
	if r == nil || r.coord == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.byChild[entry.childSessionID] = append(r.byChild[entry.childSessionID], entry)
	r.startTickerLocked()
	r.mu.Unlock()
	// The park site is re-check trigger (i). It is called AFTER park
	// returns so the entry is already visible.
	r.tryRelease(entry.childSessionID)
}

// tryRelease walks childSessionID's parked entries in FIFO order and
// releases every one whose child has become terminal. Entries whose child
// is still working stay parked and are re-checked by another trigger.
func (r *subAgentOutcomeRegistry) tryRelease(childSessionID string) {
	if r == nil || r.coord == nil || childSessionID == "" {
		return
	}
	for {
		entry := r.nextReleasable(childSessionID)
		if entry == nil {
			return
		}
		r.release(entry)
	}
}

// nextReleasable returns the oldest unreleased entry for childSessionID
// when the child's work is terminal, else nil.
func (r *subAgentOutcomeRegistry) nextReleasable(childSessionID string) *subAgentOutcomeEntry {
	if !r.coord.subAgentWorkTerminal(childSessionID) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.byChild[childSessionID] {
		if !entry.released {
			return entry
		}
	}
	return nil
}

// release claims the one-shot latch under the mutex, then refreshes the
// completion from the child's newest finished assistant message and emits
// it exactly once. Claiming the latch FIRST is what makes the emission
// one-shot: the refresh performs a DB read OUTSIDE the mutex, so the latch
// cannot be flipped after it without leaving a window in which two
// concurrent re-checks (the 2s fallback ticker and the job-completed hook,
// say) both hold the same entry and both hand it to finishParked.
//
// The completion is refreshed from the child session's newest finished
// assistant message before it is emitted, so what the parent receives is
// the child's FINAL state rather than the snapshot taken when its turn
// yielded.
func (r *subAgentOutcomeRegistry) release(entry *subAgentOutcomeEntry) {
	if !r.claim(entry) {
		return
	}
	entry.completion = r.coord.refreshSubAgentCompletion(entry.childSessionID, entry.completion)
	r.emit(entry)
}

// claim flips entry's one-shot latch under the mutex. It reports false when
// the entry was already claimed by a concurrent releaser, so exactly one
// caller ever refreshes or emits a given entry.
func (r *subAgentOutcomeRegistry) claim(entry *subAgentOutcomeEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry.released {
		return false
	}
	entry.released = true
	r.gcLocked(entry.childSessionID)
	return true
}

// emit is the single delivery point: it hands the entry's completion to
// asyncJobRegistry.finishParked, which owns exactly-once delivery from
// there — the CLI ready queue or the web auto-resume callback.
func (r *subAgentOutcomeRegistry) emit(entry *subAgentOutcomeEntry) {
	if r.coord.asyncJobs != nil {
		r.coord.asyncJobs.finishParked(entry.completion, entry.cli)
	}
}

// installSubAgentOutcomeHooks wires the origin-independent async-job
// completion hook (re-check trigger (ii)). Called by NewCoordinator once
// both registries exist, and by tests that build a coordinator by hand.
//
// onWebDone is deliberately NOT wrapped: it only fires for web-origin jobs,
// while a CLI-origin job merely lands on the session's ready queue — which
// nobody drains for a child session. The hook below is what makes a CLI
// child's job completion able to release a parked delegation at all.
//
// The cli flag matters for ordering. A CLI-origin job wakes nobody, so
// re-checking immediately is safe. A web-origin job DOES wake the session
// through notifyAsyncCompletion's own auto-resume run, and re-checking
// immediately would race that run's claim on the session's mailbox — the
// re-check could see the session idle and release the notice in the window
// between "job done" and "child claimed its next turn". So for web-origin
// jobs the re-check is deliberately deferred to the end of that run (see
// the defer inside notifyAsyncCompletion's run closure).
func (c *coordinator) installSubAgentOutcomeHooks() {
	if c.asyncJobs == nil || c.subAgentOutcomes == nil {
		return
	}
	c.asyncJobs.setJobCompletedHook(func(completion AsyncCompletion, cli bool) {
		if cli {
			c.subAgentOutcomes.noteChildRunEnded(completion.SessionID)
			return
		}
		// Web origin: notifyAsyncCompletion already re-checks after its
		// auto-resume run returns. Nothing to do here.
	})
}

// refreshSubAgentCompletion re-reads childSessionID's newest finished
// assistant message and folds its state into the completion about to be
// handed to the parent. Content and IsError are both taken from that
// message when one exists, so:
//
//   - a child whose last turn ended cleanly delivers "finished";
//   - a child whose last turn errored (FinishReasonError — e.g. its own
//     background job's failure surfacing through the auto-resume turn)
//     delivers "failed";
//   - a child that never produced a finished assistant message keeps the
//     completion captured at park time (an error return from the child's
//     run, or a panic in the tool).
//
// A DB failure keeps the captured completion rather than dropping the
// notice: losing the notice is the worse outcome, and the captured value is
// still the child's own last turn.
func (c *coordinator) refreshSubAgentCompletion(childSessionID string, completion AsyncCompletion) AsyncCompletion {
	if c.messages == nil || childSessionID == "" {
		return completion
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := c.messages.List(ctx, childSessionID)
	if err != nil {
		slog.Debug("sub-agent outcome refresh failed, using captured completion",
			"child_session", childSessionID, "err", err)
		return completion
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || !msg.IsFinished() {
			continue
		}
		if text := strings.TrimSpace(msg.FullText()); text != "" {
			completion.Content = tools.TruncateOutput(text)
		}
		completion.IsError = msg.FinishReason() == message.FinishReasonError
		return completion
	}
	return completion
}

// DescendantWorkPending reports whether sessionID, or ANY session below it in
// the parent→child session tree, still owns work that has not reached a
// terminal state. This is the transitive form root-terminality needs: an
// intermediate end_turn/yield after delegation is NOT completion, so the
// root session must be held running/waiting until every depth of its
// delegation tree is terminal.
//
// "Still owns work" means any of:
//
//   - an async job registered to that session that has not finished
//     (asyncJobRegistry.running), or
//   - a background shell owned by that session that is still running
//     (BackgroundShellManager.ActiveOwned), or
//   - a delegation parked for that session that has not been released yet
//     (subAgentOutcomeRegistry), either as the parent that has not been
//     told, or as the child whose run has not come back.
//
// The walk is deliberately guarded by a fast in-memory pre-check: when
// nothing is pending anywhere, there is no DB access at all, so the common
// "everything finished" case stays free. Only when the pre-check says
// something IS pending does this touch the sessions table, and then it
// walks breadth-first from the root with a visited set so a corrupt or
// cyclic linkage cannot loop.
//
// Deliberately NOT consulted: the child's mailbox ownership (IsSessionBusy).
// That is the resume path's state, not work; gating on it would wedge
// `rush run --session <id>`.
func (c *coordinator) DescendantWorkPending(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	// Fast path: nothing pending process-wide, so nothing can be pending
	// below this session either. No DB read.
	if !c.anyPendingWorkInMemory() {
		return false
	}
	if c.sessions == nil {
		// Without the session table we cannot walk the tree, so the safest
		// answer is the one that does not strand a running workflow: report
		// pending only for the session we were asked about.
		return c.sessionOwnsPendingWork(sessionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	visited := make(map[string]struct{})
	queue := []string{sessionID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, seen := visited[current]; seen || current == "" {
			continue
		}
		visited[current] = struct{}{}

		if c.sessionOwnsPendingWork(current) {
			return true
		}
		children, err := c.sessions.ListSubSessions(ctx, current)
		if err != nil {
			// A failed read must not silently clear the gate. Fall back to
			// the conservative answer for the sessions already visited.
			slog.Debug("DescendantWorkPending: child listing failed", "session", current, "err", err)
			return true
		}
		for _, child := range children {
			if _, seen := visited[child.ID]; !seen {
				queue = append(queue, child.ID)
			}
		}
	}
	return false
}

// anyPendingWorkInMemory is the cheap pre-check: is there ANY pending async
// job, live background shell, or unreleased parked delegation anywhere in
// this process? Touches only in-memory registries.
func (c *coordinator) anyPendingWorkInMemory() bool {
	if c.asyncJobs != nil && c.asyncJobs.anyRunning() {
		return true
	}
	if c.background != nil && c.background.ActiveJobs() > 0 {
		return true
	}
	if c.subAgentOutcomes != nil && c.subAgentOutcomes.hasParked() {
		return true
	}
	return false
}

// sessionOwnsPendingWork is the single-session form of
// DescendantWorkPending: does THIS session own pending work?
func (c *coordinator) sessionOwnsPendingWork(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if c.asyncJobs != nil && c.asyncJobs.running(sessionID) {
		return true
	}
	if c.background != nil && c.background.ActiveOwned(sessionID) > 0 {
		return true
	}
	if c.subAgentOutcomes != nil && c.subAgentOutcomes.hasParkedFor(sessionID) {
		return true
	}
	return false
}

// noteChildRunEnded is re-check trigger (iv): a run on childSessionID has
// just returned, so a gate that was deferred by the busy check in
// subAgentWorkTerminal can now be re-evaluated.
func (r *subAgentOutcomeRegistry) noteChildRunEnded(sessionID string) {
	if r == nil {
		return
	}
	r.tryRelease(sessionID)
}

// parkedParentSessions returns the distinct parent session ids that still
// have at least one unreleased delegation parked. Used by the session
// status classifier (see ParkedSubAgentWorkReporter) so an at-rest parent
// that is still mid-workflow is not reported as done.
func (r *subAgentOutcomeRegistry) parkedParentSessions() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{})
	parents := make([]string, 0, len(r.byChild))
	for _, entries := range r.byChild {
		for _, entry := range entries {
			if entry.released {
				continue
			}
			if _, dup := seen[entry.parentSessionID]; dup {
				continue
			}
			seen[entry.parentSessionID] = struct{}{}
			parents = append(parents, entry.parentSessionID)
		}
	}
	return parents
}

// ParkedSubAgentWorkReporter is the OPTIONAL interface *coordinator exposes
// so a status classifier can tell "session at rest, nothing outstanding"
// apart from "session at rest, but a delegated sub-agent outcome is still
// parked".
//
// It is deliberately NOT on the Coordinator interface: every mock
// implementing that interface in internal/server and elsewhere would have to
// grow a method. Callers type-assert instead, and a value that does not
// implement it simply means "no parked-delegation signal available".
type ParkedSubAgentWorkReporter interface {
	// ParkedSubAgentParents returns the parent session ids that currently
	// have at least one delegated sub-agent outcome parked. Empty (or a
	// value not implementing this interface) means nothing is parked.
	ParkedSubAgentParents() []string
}

// ParkedSubAgentParents implements ParkedSubAgentWorkReporter.
func (c *coordinator) ParkedSubAgentParents() []string {
	if c.subAgentOutcomes == nil {
		return nil
	}
	return c.subAgentOutcomes.parkedParentSessions()
}

// releaseCanceledForParent releases every parked delegation SPAWNED BY
// parentSessionID as a cancellation. It must run before
// asyncJobRegistry.cancelSession drops that session's job rows, or the
// final notice would have nowhere to land.
func (r *subAgentOutcomeRegistry) releaseCanceledForParent(parentSessionID string) {
	if r == nil || parentSessionID == "" {
		return
	}
	r.mu.Lock()
	pending := make([]*subAgentOutcomeEntry, 0)
	for _, entries := range r.byChild {
		for _, entry := range entries {
			if entry.released || entry.parentSessionID != parentSessionID {
				continue
			}
			pending = append(pending, entry)
		}
	}
	r.mu.Unlock()

	for _, entry := range pending {
		r.releaseCanceled(entry)
	}
}

// releaseCanceledForChild releases every parked delegation that RAN IN
// childSessionID as a cancellation.
func (r *subAgentOutcomeRegistry) releaseCanceledForChild(childSessionID string) {
	if r == nil || childSessionID == "" {
		return
	}
	r.mu.Lock()
	pending := make([]*subAgentOutcomeEntry, 0)
	for _, entry := range r.byChild[childSessionID] {
		if !entry.released {
			pending = append(pending, entry)
		}
	}
	r.mu.Unlock()

	for _, entry := range pending {
		r.releaseCanceled(entry)
	}
}

// releaseAllCanceled releases every parked entry as a cancellation. Used by
// CancelAll, which also stops the fallback ticker.
func (r *subAgentOutcomeRegistry) releaseAllCanceled() {
	if r == nil {
		return
	}
	r.mu.Lock()
	pending := make([]*subAgentOutcomeEntry, 0)
	for _, entries := range r.byChild {
		for _, entry := range entries {
			if !entry.released {
				pending = append(pending, entry)
			}
		}
	}
	r.mu.Unlock()

	for _, entry := range pending {
		r.releaseCanceled(entry)
	}
}

// releaseCanceled marks entry as a cancellation and releases it. Success,
// failure and cancel stay distinguishable for the parent through
// AsyncCompletion.IsError plus this distinct body text. The cancel path
// deliberately skips refreshSubAgentCompletion so the child's last text can
// never masquerade as a successful outcome.
func (r *subAgentOutcomeRegistry) releaseCanceled(entry *subAgentOutcomeEntry) {
	entry.completion.IsError = true
	entry.completion.Content = subAgentOutcomeCancelledText
	if !r.claim(entry) {
		return
	}
	r.emit(entry)
}

// hasParked reports whether any entry is still waiting. Used by tests and
// by the fallback ticker's own bookkeeping.
func (r *subAgentOutcomeRegistry) hasParked() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entries := range r.byChild {
		for _, entry := range entries {
			if !entry.released {
				return true
			}
		}
	}
	return false
}

// hasParkedFor reports whether sessionID still owns an unreleased parked
// delegation — either as the PARENT that has not yet been told, or as the
// CHILD session whose run has not yet come back to release it. This is the
// single-session input to DescendantWorkPending.
func (r *subAgentOutcomeRegistry) hasParkedFor(sessionID string) bool {
	if r == nil || sessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for childID, entries := range r.byChild {
		if childID == sessionID {
			for _, entry := range entries {
				if !entry.released {
					return true
				}
			}
			continue
		}
		for _, entry := range entries {
			if entry.parentSessionID == sessionID && !entry.released {
				return true
			}
		}
	}
	return false
}

// close stops the fallback ticker and refuses further parks. Safe to call
// more than once.
func (r *subAgentOutcomeRegistry) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	stop := r.tickStop
	r.tickStop = nil
	r.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// startTickerLocked arms the fallback ticker unless it is already running.
// Caller must hold r.mu.
func (r *subAgentOutcomeRegistry) startTickerLocked() {
	if r.closed || r.tickStop != nil {
		return
	}
	stop := make(chan struct{})
	r.tickStop = stop
	interval := subAgentOutcomeTickInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.tick()
			}
		}
	}()
}

// tick is the fallback re-check. It never blocks a trigger: it only
// re-evaluates gates, and it stops itself once nothing is parked.
func (r *subAgentOutcomeRegistry) tick() {
	if r == nil || r.coord == nil {
		return
	}
	r.mu.Lock()
	children := make([]string, 0, len(r.byChild))
	for childID, entries := range r.byChild {
		for _, entry := range entries {
			if !entry.released {
				children = append(children, childID)
				break
			}
		}
	}
	r.mu.Unlock()

	for _, childID := range children {
		r.tryRelease(childID)
	}

	if r.hasParked() {
		return
	}
	r.mu.Lock()
	stop := r.tickStop
	r.tickStop = nil
	r.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// gcLocked drops fully-released child buckets so the map cannot grow
// without bound over a long-lived coordinator. Caller must hold r.mu.
func (r *subAgentOutcomeRegistry) gcLocked(childSessionID string) {
	entries := r.byChild[childSessionID]
	kept := entries[:0]
	for _, entry := range entries {
		if !entry.released {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		delete(r.byChild, childSessionID)
		return
	}
	r.byChild[childSessionID] = kept
}

// subAgentWorkTerminal reports whether childID's own asynchronous work has
// reached a terminal state, i.e. whether a parked delegation may be
// released.
//
// It deliberately does NOT consult the child's run-queue rows or its
// mailbox ownership as a POSITIVE requirement. The busy check below is a
// NEGATIVE gate only: it exists to close the window where an auto-resume
// turn has started but has not yet registered its next async job, so a
// release fired in that window would be premature. If the child is
// mid-turn when a re-check fires, the release is deferred and another
// trigger retries it. Nothing here ever holds the child's mailbox or OS
// lock — that would wedges `rush run --session <id>` resume.
//
// The async-job check uses asyncJobRegistry.running, NOT pending: a job that
// already reached a terminal state but is still queued for delivery (the
// CLI ready queue, which nothing drains for a child session) is finished
// work, not outstanding work, and must not hold a delegation parked.
func (c *coordinator) subAgentWorkTerminal(childSessionID string) bool {
	if childSessionID == "" {
		return false
	}
	if c.asyncJobs != nil && c.asyncJobs.running(childSessionID) {
		return false
	}
	if c.background != nil && c.background.ActiveOwned(childSessionID) > 0 {
		return false
	}
	if c.currentAgent != nil && c.currentAgent.IsSessionBusy(childSessionID) {
		return false
	}
	return true
}

// parkSubAgentOutcome records one delegated sub-agent call whose completion
// must wait for childSessionID's own async work to drain. Nil-receiver
// safe so bare test fixtures keep working.
func (c *coordinator) parkSubAgentOutcome(childSessionID, parentSessionID, toolCallID, toolName string, cli bool, completion AsyncCompletion) {
	if c.subAgentOutcomes == nil {
		return
	}
	c.subAgentOutcomes.park(&subAgentOutcomeEntry{
		childSessionID:  childSessionID,
		parentSessionID: parentSessionID,
		toolCallID:      toolCallID,
		toolName:        toolName,
		cli:             cli,
		completion:      completion,
	})
}

// noteSubAgentChildRunEnded is re-check trigger (iv): see
// subAgentOutcomeRegistry.noteChildRunEnded.
func (c *coordinator) noteSubAgentChildRunEnded(sessionID string) {
	if c.subAgentOutcomes == nil {
		return
	}
	c.subAgentOutcomes.noteChildRunEnded(sessionID)
}

// releaseSubAgentOutcomesForParentCancel and the two siblings below wire
// the cancel path. Ordering matters: they run BEFORE
// asyncJobRegistry.cancelSession, which is what drops the originating
// job row and would otherwise leave the notice with nowhere to land.
func (c *coordinator) releaseSubAgentOutcomesForParentCancel(parentSessionID string) {
	if c.subAgentOutcomes == nil {
		return
	}
	c.subAgentOutcomes.releaseCanceledForParent(parentSessionID)
}

func (c *coordinator) releaseSubAgentOutcomesForChildCancel(childSessionID string) {
	if c.subAgentOutcomes == nil {
		return
	}
	c.subAgentOutcomes.releaseCanceledForChild(childSessionID)
}

func (c *coordinator) releaseAllSubAgentOutcomesCanceled() {
	if c.subAgentOutcomes == nil {
		return
	}
	c.subAgentOutcomes.releaseAllCanceled()
	c.subAgentOutcomes.close()
}
