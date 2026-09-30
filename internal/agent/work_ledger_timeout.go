// Explicit per-call timeouts (#1037, docs/plans/2026-09-27-async-phase2-spec.md
// §5): a single per-process timer service (never one goroutine per job),
// the timeout event handler, and best-effort partial-output capture.
package agent

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// timeoutEntry is one heap item: a deadline plus the action to run when it
// elapses. Generalizes the heap beyond asyncJob (phase 2) so a later
// consumer of the same single per-process timer -- supervision.go's
// root-session check-in -- shares it instead of starting a second worker
// goroutine (design doc §7's "one service, not a second timer"); arm keeps
// wrapping this for the asyncJob case so every existing call site is
// unchanged.
type timeoutEntry struct {
	deadline time.Time
	fire     func()
}

// jobDeadlineHeap is a container/heap over armed deadlines, ordered
// soonest-first.
type jobDeadlineHeap []timeoutEntry

func (h jobDeadlineHeap) Len() int           { return len(h) }
func (h jobDeadlineHeap) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }
func (h jobDeadlineHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *jobDeadlineHeap) Push(x any)        { *h = append(*h, x.(timeoutEntry)) }
func (h *jobDeadlineHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = timeoutEntry{}
	*h = old[:n-1]
	return item
}

// timeoutService is the single per-process timer for every asyncJob with a
// non-zero deadline: ONE goroutine sleeping until the nearest deadline,
// never one goroutine per job. Lazy deletion: an entry popped from the heap
// is re-checked against the job's CURRENT state under l.mu before acting --
// a job that already reached a terminal state via finish/cancelSession/
// recheckChild (the ordinary CAS race, ASYNC-03) makes the popped entry a
// silent no-op, exactly like every other transitionToTerminal race.
type timeoutService struct {
	ledger    *workLedger
	mu        sync.Mutex
	heap      jobDeadlineHeap
	wake      chan struct{} // buffered 1: "recompute the nearest deadline"
	stop      chan struct{}
	closeOnce sync.Once // workLedger.close() is not itself guaranteed single-call (e.g. CancelAll during shutdown); close() below must survive a repeat
}

func newTimeoutService(l *workLedger) *timeoutService {
	s := &timeoutService{ledger: l, wake: make(chan struct{}, 1), stop: make(chan struct{})}
	go s.run()
	return s
}

// arm adds job to the heap. l.mu need not be held by the caller (arm takes
// its own s.mu, a DIFFERENT lock than l.mu, to avoid making the ledger's hot
// path -- every Start/finish/cancelSession -- contend on the same mutex the
// timer goroutine polls). s.mu and l.mu are never held nested in either
// order (arm/fire release s.mu before acquiring l.mu, see fireDue/
// workLedger.handleTimeout), so this cannot deadlock.
func (s *timeoutService) arm(job *asyncJob) {
	if s == nil || job == nil {
		return
	}
	s.armFunc(job.deadline, func() { s.ledger.handleTimeout(job) })
}

// armFunc adds an arbitrary (deadline, fire) pair to the single per-process
// heap -- arm above is the asyncJob-specific wrapper every existing call
// site uses; supervision.go's root-session check-in is the second, more
// general caller this generalization exists for (see timeoutEntry's doc).
// Same locking contract as arm: s.mu is a DIFFERENT lock than l.mu, taken
// and released here without ever calling into the ledger, so this cannot
// nest under or deadlock against anything holding l.mu.
func (s *timeoutService) armFunc(deadline time.Time, fire func()) {
	if s == nil || fire == nil {
		return
	}
	s.mu.Lock()
	heap.Push(&s.heap, timeoutEntry{deadline: deadline, fire: fire})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *timeoutService) run() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.mu.Lock()
		d := time.Hour
		if s.heap.Len() > 0 {
			d = time.Until(s.heap[0].deadline)
			if d < 0 {
				d = 0
			}
		}
		s.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
		select {
		case <-s.stop:
			return
		case <-s.wake:
			continue
		case <-timer.C:
			s.fireDue()
		}
	}
}

func (s *timeoutService) fireDue() {
	now := time.Now()
	for {
		s.mu.Lock()
		if s.heap.Len() == 0 || s.heap[0].deadline.After(now) {
			s.mu.Unlock()
			return
		}
		entry := heap.Pop(&s.heap).(timeoutEntry)
		s.mu.Unlock()
		entry.fire()
	}
}

// close stops the timer goroutine. Nil-safe so an isolated ledger test that
// never wires a timeoutService can still call workLedger.close() unconditionally.
func (s *timeoutService) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() { close(s.stop) })
}

// handleTimeout re-checks job under l.mu (the ONLY writer of state/deadline
// fields) before acting -- the same "lazy deletion" contract every other
// terminal-race caller already follows (ASYNC-03): a job the timer popped
// stale (already terminal via finish/cancelSession/recheckChild) is silently
// skipped, not double-processed.
func (l *workLedger) handleTimeout(job *asyncJob) {
	l.mu.Lock()
	if job.state != phaseRunning || job.deadline.IsZero() {
		l.mu.Unlock()
		return
	}
	switch job.timeoutKind {
	case timeoutTerminateAndWake:
		owner, toolCallID, toolName, childSession, shellID, outputBuf := job.owner, job.toolCallID, job.toolName, job.childSession, job.shellID, job.outputBuf
		sync, timeoutSeconds := job.sync, job.timeoutSeconds
		cancel := job.cancel
		l.mu.Unlock()

		// Phase-4 step 2 (doc sec.3.1's external-cause order): snapshot ->
		// transition -> stop executor. Capturing partial output BEFORE
		// cancelling (this used to cancel first) matters once transition is
		// DB-durable: cancelling first risks the executor's own
		// ctx-cancellation return racing ahead of and being captured as
		// THIS transition's own result, instead of the intended timeout
		// summary. capturePartial does its own (possibly DB-backed, for a
		// delegation) I/O OUTSIDE l.mu, mirroring recheckChild's own
		// established snapshot-then-refresh-then-relock pattern.
		partial := l.capturePartial(owner, toolCallID, toolName, childSession, shellID, outputBuf)

		if sync {
			// A sync job has no row to commit (doc sec.3.1): reach the
			// timed-out outcome in memory, like job_kill's transitionSync, so
			// the blocked awaitSync caller gets it with the partial output
			// instead of the executor's "context canceled" once cancel below
			// fires.
			l.transitionSync(job, phaseTimedOut, jobResult{
				content: FormatAsyncCompletion(AsyncCompletion{
					ToolCallID: toolCallID, ToolName: toolName, Content: partial.content,
					TimedOut: true, TimeoutSeconds: timeoutSeconds,
				}),
				isError: true,
			})
		} else {
			l.transition(job, causeTimeoutTerminated, partial)
		}
		if cancel != nil {
			cancel() // best-effort: ask the executor to stop, now that the cause is durably recorded
		}
		// Delivery reaches wakeSession through the ORDINARY path
		// (deliverLocked -> notifyAsyncCompletion), exactly like finish/
		// recheckChild -- no separate wake call here. The timeout-specific
		// text and NoticeKind are selected by notifyAsyncCompletion/
		// FormatAsyncCompletion, keyed off completion.TimedOut.
	case timeoutWakeOnly:
		if job.timeoutNotified || job.sync {
			// B-dev9: a sync job (SDK/library caller blocked in awaitSync,
			// doc sec.3.1) has no async_jobs row and no session_notices
			// concept -- a wake_only check-in has nothing to attach to
			// (job_tool_call_id would name a row that was never claimed) and
			// nobody to wake (there is no session-driven turn for it, only
			// the ONE blocked caller). Only terminate_and_wake, via the
			// ordinary ctx cancellation above, applies to a sync call at
			// all.
			l.mu.Unlock()
			return
		}
		job.timeoutNotified = true
		owner, toolCallID, toolName, childSession, shellID, outputBuf := job.owner, job.toolCallID, job.toolName, job.childSession, job.shellID, job.outputBuf
		deadline := job.deadline
		l.mu.Unlock()

		summary := l.capturePartial(owner, toolCallID, toolName, childSession, shellID, outputBuf)
		if l.coord != nil && l.store != nil {
			text := fmt.Sprintf(
				"Timeout reached for async job %s (%s) — it is still running (elapsed %s). Latest output:\n\n%s\n\nThis was a one-time check-in; it will not repeat automatically. %s",
				toolCallID, toolName, time.Since(deadline).Round(time.Second), summary.content, stopGuidanceFor(toolName),
			)
			store, coord := l.store, l.coord
			go func() {
				// Phase-4 step 3 (doc sec.3.2/3.4): a session_notices row,
				// keyed to THIS job (job_tool_call_id) so the pull's void
				// condition (agent_notice_pull.go/sessionNoticeVoidCondition)
				// can drop it if the job is no longer running by the time it
				// is pulled. wake=1 per the wake-policy table.
				if err := store.InsertSessionNotice(context.Background(), owner, session.NoticeKindWakeOnly, text, true, toolCallID); err != nil {
					slog.Error("timeout: failed to persist wake_only check-in notice",
						"session_id", owner, "tool_call_id", toolCallID, "err", err)
					return
				}
				_ = coord.wakeSession(context.Background(), owner, true)
			}()
		}
	default:
		l.mu.Unlock()
	}
}

// stopGuidanceFor returns the accurate, tool-specific closing sentence for
// the wake_only check-in text. Orchestrator review finding (pre-#1023): the
// original text unconditionally offered both job_kill (bash/run_command) and
// stop_agent (agent) regardless of toolName, but job_kill only controlled a
// running bash job then (ResolveJobShellID refused run_command) and
// stop_agent does not exist as a tool yet (wake-tools-contract plan, stage
// 3). Telling the model to use a tool that will refuse, or does not exist,
// is worse than saying nothing. Task #1023 §3 made run_command controllable
// too (job_kill now tree-kills it via its live output buffer/ctx
// cancellation), so it joins bash below; agent/stop_agent is unchanged.
func stopGuidanceFor(toolName string) string {
	if toolName == tools.BashToolName || toolName == tools.RunCommandToolName {
		return "Decide whether to keep waiting, check again later, or stop it with job_kill."
	}
	// agent: no stop tool exists yet (stage 3, stop_agent).
	return "Decide whether to keep waiting or check again later -- it cannot be stopped from here yet."
}

// capturePartial produces a best-effort snapshot of a still-running (or
// just-cancelled) job's output for a timeout event (§5.6). Called WITHOUT
// l.mu held. outputBuf is job.outputBuf's snapshot taken under l.mu by the
// caller (task #1023 §3): run_command has no BackgroundShellManager entry,
// so this is its only source of partial output.
func (l *workLedger) capturePartial(owner, toolCallID, toolName, childSession, shellID string, outputBuf tools.LiveOutputBuffer) jobResult {
	if childSession != "" {
		return l.capturePartialDelegation(childSession)
	}
	if toolName == tools.BashToolName && shellID != "" && l.coord != nil && l.coord.background != nil {
		if sh, ok := l.coord.background.GetOwned(owner, shellID); ok {
			// GetOutput is safe to call on a still-running shell: its
			// underlying boundedBuffer is guarded by its own RWMutex
			// (internal/shell/background.go), independent of whether the
			// process has exited yet.
			stdout, stderr, _, runErr := sh.GetOutput()
			out := strings.TrimSpace(strings.Join(filterNonEmpty(stdout, stderr), "\n"))
			out = tools.TruncateOutput(out)
			if out == "" {
				out = "(no output yet)"
			}
			return jobResult{content: out, isError: runErr != nil}
		}
	}
	if toolName == tools.RunCommandToolName && outputBuf != nil {
		out := tools.TruncateOutput(strings.TrimSpace(outputBuf.String()))
		if out == "" {
			out = "(no output yet)"
		}
		return jobResult{content: out}
	}
	// A bash job whose shell id has not been recorded yet (the narrow
	// start-up race before setShellID runs), or a run_command job whose
	// output sink has not registered yet (same race, setRunCommandBuffer),
	// falls back to the same placeholder.
	return jobResult{content: fmt.Sprintf("job %s (%s) is still running; no partial output is available yet", toolCallID, toolName)}
}

// capturePartialDelegation reads childSessionID's newest assistant message
// as a best-effort progress indicator -- explicitly NOT requiring
// IsFinished() (unlike refreshSubAgentCompletion, coordinator_work_scope.go):
// a timeoutWakeOnly/timeoutTerminateAndWake event fires while the delegation
// may still be mid-turn, so "latest text so far" is the right signal, not
// "last completed turn".
func (l *workLedger) capturePartialDelegation(childSessionID string) jobResult {
	const placeholder = "sub-agent is still working; no partial output is available yet"
	if l.coord == nil || l.coord.messages == nil {
		return jobResult{content: placeholder}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := l.coord.messages.List(ctx, childSessionID)
	if err != nil {
		return jobResult{content: placeholder}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant {
			continue
		}
		if text := strings.TrimSpace(msg.FullText()); text != "" {
			return jobResult{content: "(sub-agent has not finished; latest progress, not a final result)\n\n" + tools.TruncateOutput(text)}
		}
	}
	return jobResult{content: placeholder}
}
