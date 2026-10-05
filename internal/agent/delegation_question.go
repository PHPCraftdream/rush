// The "delegation waits for an answer" state (#1157, A30): a delegated child
// that called ask_question while its own async work (background jobs, timers)
// is still running. Its delegation job X stays armed (the child's scope is
// open, ASYNC-02), the captured turn result is the question -- and nothing
// ever tells the parent: the release that would deliver it waits for the
// child's own work to drain, which never happens, and a resume names a child
// session ASYNC-01 still refuses. Deadlock. The mechanism here derives the
// state from existing ledger state plus history (no new column, no schema
// change), announces it to the parent as ONE child_question session notice
// bound to X (voided at pull time exactly like timeout_wake_only once X is
// terminal), and -- step 2 -- accepts the parent's answer into the HELD
// delegation instead of a new claim (answerHold).
//
// One predicate, childAwaitingQuestion, is read by all three consumers: the
// notice trigger (recheckChild's not-drained branch), the answer path
// (asyncTool's resume intercept) and the supervision/inspect surfaces.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

// childQuestionMaxLen caps the quoted question in the supervision line.
const childQuestionMaxLen = 300

// heldQuestionJob returns the oldest armed, still-running, announced
// delegation parked for childID -- the X a question notice would bind to.
// nil when there is none (never delegated, already released, or not yet
// announced -- ASYNC-05: the 60s pass will catch an unannounced X later).
func (l *workLedger) heldQuestionJob(childID string) *asyncJob {
	if l == nil || childID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	job := oldestArmedLocked(l.byChild[childID])
	if job == nil || !job.announced {
		return nil
	}
	return job
}

// childAwaitingQuestion reports whether childID is a delegated child paused
// on a question it asked while its delegation is still held: X armed,
// running and announced; the child not running a turn; and its last FINISHED
// assistant message ending on a question-tool stop (subAgentQuestionBlock).
// Deliberately in-memory + one history read, no DB scope check -- the notice
// trigger runs on every recheckChild of a busy child and must stay cheap.
func (l *workLedger) childAwaitingQuestion(childID string) (*asyncJob, string, bool) {
	job := l.heldQuestionJob(childID)
	if job == nil {
		return nil, "", false
	}
	if l.coord == nil || l.coord.messages == nil {
		return nil, "", false
	}
	if ag := l.coord.agentFor(childID); ag != nil && ag.IsSessionBusy(childID) {
		return nil, "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := l.coord.messages.List(ctx, childID)
	if err != nil {
		return nil, "", false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || !msg.IsFinished() {
			continue
		}
		block, ok := subAgentQuestionBlockFromFinish(msg.FinishPart())
		if !ok {
			return nil, "", false
		}
		return job, block, true
	}
	return nil, "", false
}

// childOwnRunningJobs counts childID's own running ledger jobs (its bash/
// run_command work), the N quoted in the notice and supervision texts.
func (l *workLedger) childOwnRunningJobs(childID string) int {
	if l == nil || childID == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	if s := l.bySession[childID]; s != nil {
		for _, job := range s.jobs {
			if job.state == phaseRunning {
				n++
			}
		}
	}
	return n
}

// childQuestionNoticeText builds the child_question notice body: the same
// "SUB-AGENT QUESTION (session ...)" frame the parent model already knows,
// followed by the paused-state paragraph with the exact answer and give-up
// calls. ownJobs is the child's own still-running job count; jobID is the
// held delegation X the answer's result will arrive with.
func childQuestionNoticeText(preamble, questionBlock, childID, jobID string, ownJobs int) string {
	frame := fmt.Sprintf("SUB-AGENT QUESTION (session %s): %s", childID, questionBlock)
	body := subAgentQuestionWithPreamble(preamble, frame)
	return fmt.Sprintf(
		"%s\n\nThe sub-agent is paused on this question. Its own %d background job(s) are still running, so job %s stays open -- waiting will not produce a result.\n"+
			"Answer: call `agent` with resume_session_id=%q and your answer as prompt; the result then arrives with job %s.\n"+
			"Give up: stop_agent(child_session_id=%q) -- this also stops its running jobs.",
		body, ownJobs, jobID, childID, jobID, childID,
	)
}

// noteChildQuestion fires the child_question notice for childID's held
// delegation, if it is indeed awaiting an answer and has not been announced
// yet. Called from recheckChild's not-drained branch, so every existing
// re-check trigger (arm, child turn end, its own job completions, the 60s
// pass) reaches it. All gating happens in memory before any DB write; the
// dedup flag questionNoticed is set under l.mu before the goroutine runs, so
// concurrent triggers produce exactly one row; a failed insert resets it for
// the next trigger.
func (l *workLedger) noteChildQuestion(childID string) {
	if l == nil || l.coord == nil || l.store == nil || childID == "" {
		return
	}
	job, question, ok := l.childAwaitingQuestion(childID)
	if !ok {
		return
	}
	if !l.coord.autoResumeSuspended(childID) {
		return
	}
	l.mu.Lock()
	if job.questionNoticed || job.state != phaseRunning || !job.announced {
		l.mu.Unlock()
		return
	}
	job.questionNoticed = true
	owner, jobID := job.owner, job.toolCallID
	l.mu.Unlock()

	text := childQuestionNoticeText(l.coord.childQuestionPreamble(childID), question, childID, jobID, l.childOwnRunningJobs(childID))
	store, coord := l.store, l.coord
	go func() {
		if err := store.InsertSessionNotice(context.Background(), owner, session.NoticeKindChildQuestion, text, true, jobID); err != nil {
			slog.Error("delegation question: failed to persist child_question notice",
				"owner", owner, "child_session_id", childID, "job", jobID, "err", err)
			l.mu.Lock()
			job.questionNoticed = false
			l.mu.Unlock()
			return
		}
		// The hint is bumped unconditionally: a CLI root is hint-driven and
		// must see the fact even where no Drain can be built (fixtures
		// without config).
		coord.asyncJobs.bumpHint(owner)
		if coord.cfg != nil {
			_ = coord.wakeSession(context.Background(), owner, false)
		}
	}()
}

// answerHeldDelegation accepts the parent's answer into the HELD delegation
// on childID (step 2): instead of a new store.Claim -- which ASYNC-01 refuses
// while the child's row runs -- it replays the answer as the child's next
// turn on its own driver, exactly as runSubAgent resumes it, and leaves X in
// place so the child's final result still arrives under it. answerHold gates
// recheckChild for the whole admission, closing the race where the child's
// own work drains between the decision and the answer turn's registration
// and releases X with the question still in it. Returns false (and touches
// nothing) when this is not a held question: the caller falls through to the
// ordinary Start/Claim path, keeping the phase-4 refusal for a plain resume.
func (l *workLedger) answerHeldDelegation(ctx context.Context, owner, childID, prompt string) (fantasy.ToolResponse, bool) {
	if l == nil || l.coord == nil || owner == "" || childID == "" {
		return fantasy.ToolResponse{}, false
	}
	job := l.heldQuestionJob(childID)
	// A sync X (#1212) has no durable row to hold and its consumer is blocked
	// in awaitSync: it is never answered here.
	if job == nil || job.owner != owner || job.sync {
		return fantasy.ToolResponse{}, false
	}
	// Not a held question: ordinary path (Claim; the phase-4 refusal stands).
	if _, _, ok := l.childAwaitingQuestion(childID); !ok {
		return fantasy.ToolResponse{}, false
	}
	l.mu.Lock()
	if job.state != phaseRunning {
		// X reached terminal between the predicate and here: the ordinary
		// Claim path is correct for the caller.
		l.mu.Unlock()
		return fantasy.ToolResponse{}, false
	}
	job.answerHold = true
	job.questionNoticed = false
	jobID := job.toolCallID
	l.mu.Unlock()

	driver, ok := l.coord.subAgentDrivers.get(childID)
	if !ok || driver.agent == nil {
		l.clearAnswerHold(job)
		return fantasy.ToolResponse{}, false
	}
	l.coord.resetConsecutiveResume(childID)
	clearCancelRequest(ctx, l.coord.sessions, childID)
	if l.coord.permissions != nil {
		if mgr, ok := l.coord.permissions.(permission.SessionRunAllowlistManager); ok && driver.parentSessionID != "" {
			mgr.InheritSessionRunAllowlistForGeneration(driver.parentSessionID, childID, driver.generation)
		}
	}
	call := driver.callFor(prompt)
	agent := l.coord.agentFor(childID)
	go func() {
		// Detached from the caller's turn like any delegation executor; the
		// replayed turn carries X's own origin (#1212) so the child's async
		// tools register as jobs instead of blocking the turn.
		runCtx := context.WithoutCancel(ctx)
		if job.cli {
			runCtx = WithCallOrigin(runCtx, message.OriginCLI)
		} else {
			runCtx = WithCallOrigin(runCtx, message.OriginWeb)
		}
		_, _, _ = l.coord.runAwaitingAdmission(runCtx, agent, call)
		l.clearAnswerHold(job)
		l.recheckChild(childID)
	}()
	return answerAcceptedResponse(childID, jobID), true
}

// clearAnswerHold drops job's answerHold latch once the answer turn returned.
// Terminal X needs no latch (recheckChild's guard is moot on it).
func (l *workLedger) clearAnswerHold(job *asyncJob) {
	l.mu.Lock()
	job.answerHold = false
	l.mu.Unlock()
}

// answerAcceptedResponse is the immediate tool result for an accepted answer:
// no new job id, no pending row (ASYNC-01 untouched) -- the child's result
// still arrives under the original job.
func answerAcceptedResponse(childID, jobID string) fantasy.ToolResponse {
	text := fmt.Sprintf(
		"Answer delivered to sub-agent %s (paused under job %s); it is running again and its result arrives with job %s. End your turn if nothing else is left.",
		childID, jobID, jobID,
	)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), asyncToolMetadata{
		Async: false, JobID: jobID, ChildSessionID: childID, Status: "running",
	})
}

// childAwaitingQuestionSummary is supervision's per-delegation line for an
// X whose child is paused on a question. ok=false when it is not.
func (l *workLedger) childAwaitingQuestionSummary(childID string, elapsed time.Duration) (line string, ok bool) {
	job, question, is := l.childAwaitingQuestion(childID)
	if !is {
		return "", false
	}
	if len(question) > childQuestionMaxLen {
		question = strings.TrimSpace(question[:childQuestionMaxLen]) + "..."
	}
	return fmt.Sprintf(
		"- %s (sub-agent, child session %s): AWAITING YOUR ANSWER for %s -- it asked: %s. Its own %d job(s) still run. Answer: agent(resume_session_id=%q, prompt=\"...\"); give up: stop_agent(child_session_id=%q).",
		job.toolCallID, childID, elapsed.Round(time.Second), question, l.childOwnRunningJobs(childID), childID, childID,
	), true
}
