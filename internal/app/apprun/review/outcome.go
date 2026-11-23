package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// agentTurnResponse carries the outcome of one asynchronous
// AgentCoordinator.Run call back to RunNonInteractive's event loop, via the
// done channel below runAgentTurnRecovered writes into.
type AgentTurnResponse struct {
	Result *fantasy.AgentResult
	Err    error
	Queued bool
}

// runAgentTurnRecovered runs runFn (normally app.AgentCoordinator.Run) on the
// calling goroutine and always sends exactly one agentTurnResponse into done,
// including when runFn panics.
//
// Without this, a panic anywhere inside AgentCoordinator.Run — which includes
// every tool call it executes synchronously, e.g. bash, edit, grep, or a
// sub-agent delegation via the `agent` tool (runSubAgent calls
// params.Agent.Run synchronously on this same call stack, see
// coordinator.go's runSubAgent) — would crash the entire rush.exe process
// via Go's default panic handler, which writes only to os.Stderr and never
// through slog. For `rush run` invocations whose stderr isn't captured by
// whatever launched them, that's a silent death with zero log output. It
// also left RunNonInteractive's `case result := <-done` select blocked
// forever, since nothing would ever send on the channel.
//
// This mirrors the panic-isolation pattern already used for WebSocket
// handlers in internal/server/hub.go (runRecovered / Client.dispatch): log
// via slog.Error with a full stack trace, then report a normal error result
// instead of letting the panic propagate.
func RunAgentTurnRecovered(
	ctx context.Context,
	sessionID, prompt string,
	runFn func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error),
	done chan<- AgentTurnResponse,
) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("agent turn panic",
				"session_id", sessionID,
				"panic", r,
				"stack", string(debug.Stack()))
			done <- AgentTurnResponse{
				Err: fmt.Errorf("agent turn panicked (recovered, see rush.log for stack trace): %v", r),
			}
		}
	}()

	result, err := runFn(ctx, sessionID, prompt)
	if err != nil {
		done <- AgentTurnResponse{
			Err: fmt.Errorf("failed to start agent processing stream: %w", agent.LocalizeResetError(err)),
		}
		return
	}
	if result == nil {
		// sessionAgent.Run's ONLY (nil, nil) return is the legacy
		// queueing path (mailbox.submit's "caller queues and returns
		// nil" branch, agent_run.go): every fail-fast busy rejection
		// — the FailIfSessionBusy branch right next to it, and every
		// earlier fast-path check in ExecuteRun (IsSessionBusy,
		// ReserveExclusive) — already wraps agent.ErrSessionBusy in a
		// non-nil err, caught by the branch above. So reaching here
		// with a nil err means this call queued behind the current
		// owner and returned immediately, exactly as the R1-4 legacy
		// queueing contract intends: nothing to report YET (the
		// eventual queued turn runs under the owner's own dispatcher
		// loop and its messages/results land in the SAME session,
		// picked up by messageEvents below) — not a failure. Treating
		// it as agent.ErrSessionBusy here (as this used to) broke
		// exactly the callers this contract exists for: two
		// legacy (non-fail-fast) ExecuteRun calls on one busy session
		// would have the loser's call fail hard instead of queueing,
		// silently regressing R1-4 the instant a caller's timing
		// landed it in the genuine mid-turn queueing window rather
		// than racing in fresh after the owner released (round-3
		// review, R3 follow-up: CI's macOS runner hit the queueing
		// window; a fast idle dev machine almost never does, which is
		// why this went unnoticed until load-sensitive CI exposed it).
		done <- AgentTurnResponse{Queued: true}
		return
	}
	done <- AgentTurnResponse{
		Result: result,
	}
}

// drainOutcomeError translates DrainSessionNow's (result, drainErr) pair into the
// final error that RunNonInteractive should return. It enforces the contract
// that DrainPartial and DrainFailed must always carry a non-nil drainErr,
// converting a violation into a wrapped ErrDrainFailureUnspecified so that the
// run exits non-zero rather than silently treating a nil drain as success.
//
// Task #616/P2-2 (2026-08-20 read-only release review): DrainNoWork now has
// its OWN case rather than falling into default alongside a genuinely
// invalid DrainResult. Before this, an unrecognized future DrainResult value
// (a bug: e.g. a fifth enum member added to runq.DrainResult without a
// matching case here) would silently inherit DrainNoWork's "return
// originalErr unchanged" behavior instead of being caught — exactly the
// kind of contract-drift this file's other two cases already guard against
// for DrainComplete/DrainPartial/DrainFailed. See runq.DrainNoWork's own
// doc for why originalErr (not drainErr) is the right thing to return here:
// DrainSessionNow's own doc records that a few of its early-exit paths pair
// DrainNoWork with a non-nil, call-scoped drainErr (this call's own ctx
// already done, its own lease attempt failing, etc.) that describes why
// DrainSessionNow itself did not run anything — not a row's outcome — so
// surfacing THAT would be reporting information about DrainSessionNow's
// internal retry loop instead of the run's actual outcome. originalErr is
// guaranteed non-nil on this function's sole production call site
// (app_run.go's isCanceled gate below requires runErr != nil before
// DrainSessionNow is ever invoked), so this is traced, not live-tested, the
// same way task #607/commit 732155ad already recorded for this exact path.
func DrainOutcomeError(sessID string, result runq.DrainResult, drainErr, originalErr error) error {
	switch result {
	case runq.DrainComplete:
		// DrainComplete is the ONLY DrainResult DrainSessionNow ever pairs
		// with a nil error (see its own doc), so drainErr is guaranteed nil
		// here; asserted defensively rather than trusted blindly.
		if drainErr != nil {
			slog.Error("run: DrainSessionNow reported DrainComplete with a non-nil error -- contract violation, treating as failure", "session_id", sessID, "err", drainErr)
			return drainErr
		}
		return nil
	case runq.DrainPartial, runq.DrainFailed:
		// At least one row genuinely executed, but the call did not end in a
		// full, confirmed success -- surface that as the run's outcome. This
		// guard closes the consumer-side hole where a nil drainErr would be
		// forwarded as-is, causing finish(nil) to exit 0 despite a
		// Partial/Failed result.
		if drainErr == nil {
			slog.Error("run: DrainSessionNow reported a partial/failed drain with a nil error -- contract violation, treating as failure", "session_id", sessID, "result", result.String())
			return fmt.Errorf("%w (session=%s)", runq.ErrDrainFailureUnspecified, sessID)
		}
		return agent.LocalizeResetError(drainErr)
	case runq.DrainNoWork:
		// Nothing ran here. This case covers BOTH the shapes task
		// #624/F-5 distinguishes: a call that never observed a same-pump
		// admission, AND a call that DID observe one — the latter keeps
		// its DrainNoWork even when an outstanding leased row exists,
		// because that row is plausibly the local admission holder's
		// in-flight work. Either nothing was pending, or something was
		// but a live owner WITHIN THIS PUMP's admission held the session,
		// or this call stopped for a call-scoped reason
		// of its own (ctx already done, its own lease attempt failing
		// at the DB layer, etc -- see runq.DrainNoWork's own doc;
		// drainErr may be non-nil in that case, but it describes why
		// DrainSessionNow itself did not run anything, not this run's
		// outcome). All of those mean the same thing to this command: no
		// continuation completed in this process, so the original
		// cancellation stands. It was real.
		//
		// NOTE (task #624/F-5): a DIFFERENT kind of "different live
		// owner" -- another PROCESS's pump holding a live DB lease on
		// a row of this session, with no admission this call could ever
		// observe -- no longer reaches this case. Leasing a row never
		// consults the OS session lock, so such a row is indistinguishable
		// from an orphaned one by the only query available, and
		// DrainSessionNow now reports it as DrainFailed/
		// ErrOutstandingRunQueueEntry instead (mirroring task #610's
		// already-accepted reporting for calls that DID execute
		// something). That lands in the DrainPartial/DrainFailed case
		// above and returns drainErr instead of originalErr -- both are
		// non-nil here, so the run exits non-zero either way; what changes
		// is WHICH error the operator sees, and the outstanding-row one
		// names the undrained durable work rather than repeating the
		// original cancellation.
		return originalErr
	default:
		// An unrecognized DrainResult -- a programming error (a new
		// runq.DrainResult value added without a matching case here), not
		// a reachable production outcome today. Silently falling through to
		// originalErr (the pre-fix behavior) would let a future enum
		// addition inherit DrainNoWork's semantics by accident, hiding a
		// contract violation exactly like the DrainComplete/DrainPartial/
		// DrainFailed guards above already refuse to do for THEIR contracts.
		// Log it and return a non-nil sentinel so the run exits non-zero
		// rather than risking a false success.
		slog.Error("run: DrainSessionNow reported an unrecognized DrainResult -- contract violation, treating as failure", "session_id", sessID, "result", result.String(), "err", drainErr)
		return fmt.Errorf("%w (session=%s, result=%s)", runq.ErrDrainFailureUnspecified, sessID, result.String())
	}
}

// credentialsRunner is the slice of the coordinator that supports
// per-call credential isolation (agent's RunWithCredentials). A
// consuming-package interface on purpose, and type-asserted rather than
// added to agent.Coordinator, so that interface's many existing test
// fakes stay untouched.
type CredentialsRunner interface {
	RunWithCredentials(ctx context.Context, sessionID, prompt string, creds *agent.CredentialSet, attachments ...message.Attachment) (*fantasy.AgentResult, error)
}

// REVIEWER PASS stall handling: the turn-stall policy (internal/agent)
// aborts a stalled review turn with agent.ErrTurnStalled, but the reviewer
// pass itself owns the retry decision. This file restarts a stalled review
// turn up to twice (3 attempts total); if the reviewer is still stalling,
// the outcome is ErrReviewerUnresponsive and the EXISTING review-failed
// path keeps the executor's result so the process exits instead of hanging.

// ErrReviewerUnresponsive is joined into the review error when every
// reviewer attempt stalled. The review gate maps it onto the existing
// review-failed path: the executor's result is kept.
var ErrReviewerUnresponsive = errors.New("review failed: reviewer unresponsive")

// reviewerStallMaxAttempts is the total number of review turn attempts:
// the first run plus two restarts of the reviewer pass.
const ReviewerStallMaxAttempts = 3

// retryStalledReviewerPass runs the review turn, restarting it whenever an
// attempt was aborted by the turn-stall policy (agent.ErrTurnStalled), at
// most reviewerStallMaxAttempts-1 restarts, each announced with a WARN.
// Any other failure returns immediately. When the final attempt also
// stalled, ErrReviewerUnresponsive is joined into the returned error; the
// returned result is never the executor's and is discarded by the gate.
func RetryStalledReviewerPass(attempt func() (*RunResult, error)) (*RunResult, error) {
	result, err := attempt()
	for restart := 1; err != nil && errors.Is(err, agent.ErrTurnStalled) && restart < ReviewerStallMaxAttempts; restart++ {
		slog.Warn("reviewer pass stalled; restarting the review turn",
			"attempt", restart+1, "max_attempts", ReviewerStallMaxAttempts)
		result, err = attempt()
	}
	if err != nil && errors.Is(err, agent.ErrTurnStalled) {
		slog.Warn("reviewer pass unresponsive after stall restarts", "max_attempts", ReviewerStallMaxAttempts)
		err = errors.Join(err, ErrReviewerUnresponsive)
	}
	return result, err
}

// reviewerPassMarker is the literal prefix of the automatic reviewer pass's
// user prompt. Tests, the audit trail and the reviewer tool-call counter all
// find the review turn by this substring, so the first line of
// reviewerPassPrompt must stay byte-identical to it.
const ReviewerPassMarker = "You are now acting as the independent reviewer"

// reviewerPassPrompt is the fixed user prompt of the automatic reviewer
// pass. It tells the model that the orchestrator's run is over, that it is a
// different agent now, what it may check with which read-only tools, what it
// must check, and how the verdict line and the report are spelled out.
//
// It must always steer the reviewer toward ending with a conclusion and
// never toward asking a question or proposing more work: an ask_question
// tool call here would force-finish the turn with awaiting_answer, which
// nothing is watching to answer.
const ReviewerPassPrompt = `You are now acting as the independent reviewer for this session.

ROLE SWITCH. The orchestrator agent has FINISHED its work: its final report
is the last assistant message above, and its run is over. You are NOT that
agent. From this message on you are a different agent with a different job:
the independent reviewer who audits what the orchestrator did and claimed.
You received its conversation only as evidence to examine; do not continue
its task, do not fix anything, do not speak as "I" about its actions.

WHO WROTE WHAT. Every assistant message above was written by the orchestrator
and by the workers it delegated to - not by you. When those
messages say "I read", "I ran", "verified", "green", "closed", they are CLAIMS
to check, not facts you observed. Until you call a tool in this turn, you have
verified nothing. Never write "I ran" or "I verified" about anything you did
not do in this turn.

YOUR TOOLS are read-only: view, grep, glob, ls, git_read (status, diff, log,
show, blame) and read_delegation_transcript. You cannot edit files, run shell
commands or tests, or ask questions - do not try. There is no limit on tool
calls: check as much as you need to be confident in the verdict. Issue
independent reads in parallel in one step, and use offset/limit and path
filters instead of reading whole large files (the context is already large).

EVIDENCE. The <review_evidence> block at the end of this message was computed
by rush itself from the git working tree and the session database, not by the
orchestrator. Where it disagrees with the transcript, trust the evidence and
say so.

MANDATORY CHECKS - do all of them, in this order:
1. Request. State in 1-3 lines what was asked (original_request in the
   evidence; if it points to a spec or task file, view that file). Judge
   everything below against the request, not against the orchestrator's own
   summary of it.
2. Changes. Use the evidence's changed-file list and git_read diff on the
   files that matter. Flag every change the request did not ask for: unrelated
   files, deleted or weakened tests, config or version edits, generated files.
3. Claims. Take at least 3 concrete claims from the orchestrator's final
   report (all of them if there are fewer) - "function X does Y", "test Z
   fails without the fix", "file is N lines", "item M is closed" - and check
   each against the files with view/grep. Mark each one: confirmed (file:line),
   refuted (file:line), or unverifiable with these tools.
4. Tests. Read each new or changed test and judge whether it is vacuous: would
   it still pass if the change were reverted? Does it assert the behaviour, or
   only that code runs? Does it mock the very thing under test, skip, or assert
   constants? Take what actually ran, and its exit code, from the evidence's
   command results: code that changed with no passing test/build run after it
   is a finding.
5. Stubs. grep the changed files for TODO, FIXME, XXX, "not implemented",
   placeholder panics, empty bodies and commented-out code added by this run.
6. Final answer. Judge the orchestrator's final answer itself (final_text in
   the evidence): is it coherent, in the request's language, and an actual
   report of the work - or is it empty, truncated, repetitive, garbled or
   mixed-language, or a short reply to a background notice that buried the
   real report? Address every flag in final_text_checks explicitly. A
   degenerate, empty or incoherent final answer is a finding on its own, even
   when the work itself is good.
7. Process. Note waiting loops (sleep/echo/polling), repeated failing
   attempts, open todos from the evidence, and work the orchestrator said it
   would do but did not do.

VERDICT RULES.
- FAIL if any of: the request is not fulfilled; a claim is refuted; a P0/P1
  defect; the last relevant test/build run failed, or code changed with no
  run after it; out-of-scope changes that matter; stubs left in; the final
  answer is degenerate or is not a report; or you could not verify anything
  yourself.
- PASS_WITH_NOTES if the work meets the request but you found P2/P3 issues or
  some claims stayed unverifiable.
- PASS only if you confirmed at least 3 claims yourself with tools in this
  turn and found nothing listed above.
A PASS or PASS_WITH_NOTES without your own tool checks in this turn is
forbidden.

OUTPUT. Write in the language of the original request. The first line must be
exactly one of:
VERDICT: PASS
VERDICT: PASS_WITH_NOTES
VERDICT: FAIL
For a Russian request append the label after the token: "Зачёт",
"Зачёт с замечаниями" or "Отказ". Then these sections:
## Request vs result - what was asked, what was done, what is missing.
## Checked myself - each check: tool, file:line, result.
## Only from the orchestrator's words - claims you did not or could not check.
## Findings - each: severity P0-P3, file:line, what is wrong, why it matters.
## Final answer and process - checks 6 and 7.
This is the session's final message: do not ask questions, do not offer
further work, do not address the orchestrator. Conclude.`

// reviewerPassTimeout bounds one automatic reviewer pass on its own wall
// clock. It is a hang-guard, NOT a cap on how much the reviewer may check:
// the review turn reads as much as it needs to be confident in its verdict,
// and it already runs under the run's own context (--timeout, the 6h
// default cap). This ceiling only stops a reviewer that never finishes from
// holding the process open forever; a reviewer that legitimately needs the
// full hour is not interrupted.
const ReviewerPassTimeout = 60 * time.Minute
