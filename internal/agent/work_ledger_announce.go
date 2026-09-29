// The ack gate's fused-transaction path (DUR-7, docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.3.8, step 6): the "started" tool-result
// message and announced=1 commit in ONE transaction instead of two separate
// writes, closing the crash window where the message lands in history but
// announced never flips -- permanently blocking that row's eventual notice
// from ever being pulled (an unannounced job never produces a notice, but
// this window would leave it BOTH announced-in-history and unannounced-in-
// the-row forever, which is worse: the model already saw "started").
package agent

import (
	"context"
	"errors"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// acknowledgeWithMessageTx is agent_turn_stream.go's onToolResult entry
// point for the ack gate. handled=false means this tool result is NOT a
// durable async job's own "started" result -- no ledger entry for
// (sessionID, toolCallID) at all (an ordinary tool call), or a sync job
// (announced=true already, no DB row to fuse with, doc sec.3.1) -- and the
// caller must fall back to its own plain messages.Create followed by
// acknowledged/abort, exactly as before this method existed.
//
// handled=true, err!=nil mirrors onToolResult's pre-existing
// createMsgErr!=nil branch: the row is already deleted (ASYNC-05, via the
// same abort path a failed plain Create used) by the time this returns, so
// the caller only needs to propagate err -- it must NOT also call
// abort/acknowledged itself.
//
// B15: ErrAsyncJobGone (a Rerun truncation raced this call) means
// AnnounceStarted's OWN transaction rolled back -- including the tool-result
// message insert it had staged, since MarkAsyncJobAnnounced's 0-rows return
// happens AFTER messages.CreateTx in the SAME tx (notice_pull.go). Returning
// handled=true here used to swallow that as a total no-op, but the tool_use
// this result answers was already recorded in history: skipping the result
// entirely leaves a tool_use with no matching tool_result, which every
// provider rejects on the NEXT turn. handled=false instead falls through to
// the caller's own plain messages.Create (persisting the result the
// ORDINARY way) followed by acknowledged, which already tolerates a gone row
// (MarkAnnounced's own ErrAsyncJobGone handling) -- the in-memory job, if
// still present, is cleaned up later when its eventual transition attempt
// resolves TransitionGone (work_ledger_transition.go), same as any other
// row deleted out from under a still-running executor.
func (l *workLedger) acknowledgeWithMessageTx(ctx context.Context, sessionID, toolCallID string, messages message.Service, params message.CreateMessageParams) (msg message.Message, handled bool, err error) {
	l.mu.Lock()
	s := l.bySession[sessionID]
	var job *asyncJob
	if s != nil {
		job = s.jobs[toolCallID]
	}
	store := l.store
	l.mu.Unlock()
	if job == nil || job.sync || store == nil {
		return message.Message{}, false, nil
	}

	msg, err = store.AnnounceStarted(ctx, messages, sessionID, toolCallID, params)
	if err != nil {
		if errors.Is(err, session.ErrAsyncJobGone) {
			return message.Message{}, false, nil
		}
		// The fused transaction failed (message insert or the announce
		// write) -- abort's existing DB-delete + in-memory drop + executor
		// cancel is the correct recovery, same as a plain Create failure
		// used before this method existed.
		l.abort(sessionID, toolCallID)
		return message.Message{}, true, err
	}
	// Doc sec.3.8's Ack gate paragraph: "if the row was already terminal,
	// nudge the session" IS this call -- finishAcknowledgeLocally flips
	// job.announced and, if the job's terminal transition already committed
	// while unannounced, delivers it right here via the existing
	// deliverLocked/onWebDone wake-hint machinery (work_ledger.go), the same
	// path a fast job finishing before its own ack always used.
	l.finishAcknowledgeLocally(sessionID, toolCallID)
	return msg, true, nil
}

// jobKillResultMessageTx is agent_turn_stream.go's onToolResult entry point
// for A3 (doc sec.3.2's law "delivery='done' => the row names the message
// that carries its result"): a job_kill CALL's own tool-result message,
// keyed by targetToolCallID -- the async_jobs row job_kill just acted on
// (JobKillResponseMetadata.JobID), NOT job_kill's own tool_call_id -- needs
// that message's id fused into notice_message_id in the SAME transaction as
// its insert, mirroring the ack gate's acknowledgeWithMessageTx.
//
// handled=false (fall back to the caller's own plain Create) when there is
// no store wired or targetToolCallID is empty -- an ordinary tool result, or
// a job_kill call that never resolved to a tracked job (raw shell_id, or the
// B11 refusal path, which builds no JobKillResponseMetadata at all). A
// non-nil err here is a real failure (message insert itself failed): the
// caller must propagate it, not fall back to its own Create (the row's
// notice_message_id write and the message either both happen or neither
// does -- this method's own tx handles that, unlike a caller-side retry
// which would risk a second message for the same tool_use).
func (l *workLedger) jobKillResultMessageTx(ctx context.Context, sessionID, targetToolCallID string, messages message.Service, params message.CreateMessageParams) (msg message.Message, handled bool, err error) {
	l.mu.Lock()
	store := l.store
	l.mu.Unlock()
	if store == nil || targetToolCallID == "" {
		return message.Message{}, false, nil
	}
	msg, err = store.AnnounceJobKillResult(ctx, messages, sessionID, targetToolCallID, params)
	if err != nil {
		return message.Message{}, true, err
	}
	return msg, true, nil
}
