package app

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// agentTurnResponse carries the outcome of one asynchronous
// AgentCoordinator.Run call back to RunNonInteractive's event loop, via the
// done channel below runAgentTurnRecovered writes into.
type agentTurnResponse struct {
	result *fantasy.AgentResult
	err    error
	queued bool
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
func runAgentTurnRecovered(
	ctx context.Context,
	sessionID, prompt string,
	runFn func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error),
	done chan<- agentTurnResponse,
) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("agent turn panic",
				"session_id", sessionID,
				"panic", r,
				"stack", string(debug.Stack()))
			done <- agentTurnResponse{
				err: fmt.Errorf("agent turn panicked (recovered, see rush.log for stack trace): %v", r),
			}
		}
	}()

	result, err := runFn(ctx, sessionID, prompt)
	if err != nil {
		done <- agentTurnResponse{
			err: fmt.Errorf("failed to start agent processing stream: %w", err),
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
		done <- agentTurnResponse{queued: true}
		return
	}
	done <- agentTurnResponse{
		result: result,
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
// (a bug: e.g. a fifth enum member added to session.DrainResult without a
// matching case here) would silently inherit DrainNoWork's "return
// originalErr unchanged" behavior instead of being caught — exactly the
// kind of contract-drift this file's other two cases already guard against
// for DrainComplete/DrainPartial/DrainFailed. See session.DrainNoWork's own
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
func drainOutcomeError(sessID string, result session.DrainResult, drainErr, originalErr error) error {
	switch result {
	case session.DrainComplete:
		// DrainComplete is the ONLY DrainResult DrainSessionNow ever pairs
		// with a nil error (see its own doc), so drainErr is guaranteed nil
		// here; asserted defensively rather than trusted blindly.
		if drainErr != nil {
			slog.Error("run: DrainSessionNow reported DrainComplete with a non-nil error -- contract violation, treating as failure", "session_id", sessID, "err", drainErr)
			return drainErr
		}
		return nil
	case session.DrainPartial, session.DrainFailed:
		// At least one row genuinely executed, but the call did not end in a
		// full, confirmed success -- surface that as the run's outcome. This
		// guard closes the consumer-side hole where a nil drainErr would be
		// forwarded as-is, causing finish(nil) to exit 0 despite a
		// Partial/Failed result.
		if drainErr == nil {
			slog.Error("run: DrainSessionNow reported a partial/failed drain with a nil error -- contract violation, treating as failure", "session_id", sessID, "result", result.String())
			return fmt.Errorf("%w (session=%s)", session.ErrDrainFailureUnspecified, sessID)
		}
		return drainErr
	case session.DrainNoWork:
		// Nothing ran here. This case covers BOTH the shapes task
		// #624/F-5 distinguishes: a call that never observed a same-pump
		// admission, AND a call that DID observe one — the latter keeps
		// its DrainNoWork even when an outstanding leased row exists,
		// because that row is plausibly the local admission holder's
		// in-flight work. Either nothing was pending, or something was
		// but a live owner WITHIN THIS PUMP's admission held the session,
		// or this call stopped for a call-scoped reason
		// of its own (ctx already done, its own lease attempt failing
		// at the DB layer, etc -- see session.DrainNoWork's own doc;
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
		// session.DrainResult value added without a matching case here), not
		// a reachable production outcome today. Silently falling through to
		// originalErr (the pre-fix behavior) would let a future enum
		// addition inherit DrainNoWork's semantics by accident, hiding a
		// contract violation exactly like the DrainComplete/DrainPartial/
		// DrainFailed guards above already refuse to do for THEIR contracts.
		// Log it and return a non-nil sentinel so the run exits non-zero
		// rather than risking a false success.
		slog.Error("run: DrainSessionNow reported an unrecognized DrainResult -- contract violation, treating as failure", "session_id", sessID, "result", result.String(), "err", drainErr)
		return fmt.Errorf("%w (session=%s, result=%s)", session.ErrDrainFailureUnspecified, sessID, result.String())
	}
}

// credentialsRunner is the slice of the coordinator that supports
// per-call credential isolation (agent's RunWithCredentials). A
// consuming-package interface on purpose, and type-asserted rather than
// added to agent.Coordinator, so that interface's many existing test
// fakes stay untouched.
type credentialsRunner interface {
	RunWithCredentials(ctx context.Context, sessionID, prompt string, creds *agent.CredentialSet, attachments ...message.Attachment) (*fantasy.AgentResult, error)
}
