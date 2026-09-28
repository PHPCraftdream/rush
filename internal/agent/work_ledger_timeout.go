// Explicit per-call timeouts (#1037, docs/plans/2026-09-27-async-phase2-spec.md
// §5): a single per-process timer service (never one goroutine per job),
// the timeout event handler, and best-effort partial-output capture.
package agent

import (
	"container/heap"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
)

// jobDeadlineHeap is a container/heap over jobs with a non-zero deadline,
// ordered soonest-first.
type jobDeadlineHeap []*asyncJob

func (h jobDeadlineHeap) Len() int           { return len(h) }
func (h jobDeadlineHeap) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }
func (h jobDeadlineHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *jobDeadlineHeap) Push(x any)        { *h = append(*h, x.(*asyncJob)) }
func (h *jobDeadlineHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
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
	s.mu.Lock()
	heap.Push(&s.heap, job)
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
		job := heap.Pop(&s.heap).(*asyncJob)
		s.mu.Unlock()
		s.ledger.handleTimeout(job)
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
		if job.cancel != nil {
			job.cancel() // best-effort: ask the executor to stop; partial output captured below regardless of whether it stops in time
		}
		owner, toolCallID, toolName, childSession, shellID := job.owner, job.toolCallID, job.toolName, job.childSession, job.shellID
		l.mu.Unlock()

		// capturePartial does its own (possibly DB-backed, for a delegation)
		// I/O OUTSIDE l.mu, mirroring recheckChild's own established
		// snapshot-then-refresh-then-relock pattern (work_ledger_delegation.go)
		// rather than holding the ledger lock across a round trip.
		partial := l.capturePartial(owner, toolCallID, toolName, childSession, shellID)

		l.mu.Lock()
		job.transitionToTerminal(phaseTimedOut, partial) // CAS: a concurrent finish/cancel may already have won; deliverLocked below is safe to call unconditionally either way
		completion, callback := l.deliverLocked(owner, job)
		l.mu.Unlock()
		if callback {
			l.onWebDone(completion)
		}
		// Delivery reaches wakeSession through the ORDINARY path
		// (deliverLocked -> notifyAsyncCompletion), exactly like finish/
		// recheckChild -- no separate wake call here. The timeout-specific
		// text and NoticeKind are selected by notifyAsyncCompletion/
		// FormatAsyncCompletion, keyed off completion.TimedOut.
	case timeoutWakeOnly:
		if job.timeoutNotified {
			l.mu.Unlock()
			return
		}
		job.timeoutNotified = true
		owner, toolCallID, toolName, childSession, shellID := job.owner, job.toolCallID, job.toolName, job.childSession, job.shellID
		deadline := job.deadline
		l.mu.Unlock()

		summary := l.capturePartial(owner, toolCallID, toolName, childSession, shellID)
		if l.coord != nil {
			text := fmt.Sprintf(
				"Timeout reached for async job %s (%s) — it is still running (elapsed %s). Latest output:\n\n%s\n\nThis was a one-time check-in; it will not repeat automatically. %s",
				toolCallID, toolName, time.Since(deadline).Round(time.Second), summary.content, stopGuidanceFor(toolName),
			)
			id := jobIdentity{owner: owner, toolCallID: toolCallID}
			go func() {
				_ = l.coord.wakeSession(context.Background(), id, text, "timeout_wake_only", true)
			}()
		}
	default:
		l.mu.Unlock()
	}
}

// stopGuidanceFor returns the accurate, tool-specific closing sentence for
// the wake_only check-in text. Orchestrator review finding: the original
// text unconditionally offered both job_kill (bash/run_command) and
// stop_agent (agent) regardless of toolName -- job_kill only actually
// controls a running bash job (ResolveJobShellID explicitly refuses
// run_command, see capturePartial's own comment below), run_command has no
// live control at all, and stop_agent does not exist as a tool yet
// (wake-tools-contract plan, later phase). Telling the model to use a tool
// that will refuse, or does not exist, is worse than saying nothing.
func stopGuidanceFor(toolName string) string {
	if toolName == tools.BashToolName {
		return "Decide whether to keep waiting, check again later, or stop it with job_kill."
	}
	// run_command (job_kill refuses it) and agent (no stop tool exists yet)
	// both reduce to the same honest answer: nothing here can stop it yet.
	return "Decide whether to keep waiting or check again later -- it cannot be stopped from here yet."
}

// capturePartial produces a best-effort snapshot of a still-running (or
// just-cancelled) job's output for a timeout event (§5.6). Called WITHOUT
// l.mu held.
func (l *workLedger) capturePartial(owner, toolCallID, toolName, childSession, shellID string) jobResult {
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
	// run_command has no background-process handle to read from (unlike
	// bash's BackgroundShellManager -- ResolveJobShellID refuses run_command
	// jobs for the same reason), and a bash job whose shell id has not been
	// recorded yet (the narrow start-up race before setShellID runs) falls
	// back to the same placeholder.
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
