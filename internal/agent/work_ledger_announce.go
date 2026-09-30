// The ack gate's fused-transaction path (DUR-7, docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.3.8, step 6): the "started" tool-result
// message and announced=1 commit in ONE transaction instead of two separate
// writes, closing the crash window where the message lands in history but
// announced never flips -- permanently blocking that row's eventual notice
// from ever being pulled (an unannounced job never produces a notice, but
// this window would leave it BOTH announced-in-history and unannounced-in-
// the-row forever, which is worse: the model already saw "started").
//
// Which tool result is a job's "started" result is decided by a claim tag
// (ackTag), not by tool_call_id: providers that number calls per response
// reuse ids, so an ordinary result can collide with a running job's key.
package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// ackTag is the metadata asyncTool puts on the one tool result that
// acknowledges a job: its "started" response, or the "failed to start" error
// of a job whose launch panicked. claim_id is the job's own claim, unique per
// job incarnation.
type ackTag struct {
	ClaimID string `json:"claim_id,omitempty"`
}

// claimAck returns the job toolResult acknowledges, or nil when it is an
// ordinary result (including one whose tool_call_id collides with a running
// job's). A non-nil job carries the right to acknowledge or abort: it is
// handed to exactly one caller (job.acking), only while the job is still
// unannounced in memory AND in the durable row, so a colliding or repeated
// result can neither overwrite announce_message_id nor abort a job that is
// already announced.
func (l *workLedger) claimAck(ctx context.Context, sessionID string, result message.ToolResult) *asyncJob {
	if l == nil || result.Metadata == "" {
		return nil
	}
	var tag ackTag
	if json.Unmarshal([]byte(result.Metadata), &tag) != nil || tag.ClaimID == "" {
		return nil
	}
	l.mu.Lock()
	var job *asyncJob
	if s := l.bySession[sessionID]; s != nil {
		job = s.jobs[result.ToolCallID]
	}
	if job == nil || job.sync || job.claimID != tag.ClaimID || job.announced || job.acking {
		l.mu.Unlock()
		return nil
	}
	job.acking = true
	store := l.store
	l.mu.Unlock()

	if store != nil {
		// A row that exists but is already announced, or belongs to another
		// claim, is not ours to announce. A missing row is left to the fused
		// path's ErrAsyncJobGone handling.
		if row, err := store.Get(ctx, sessionID, result.ToolCallID); err == nil && (row.Announced != 0 || row.ClaimID != job.claimID) {
			l.mu.Lock()
			job.acking = false
			l.mu.Unlock()
			return nil
		}
	}
	return job
}

// persistToolResult writes one tool-result message and settles whatever the
// ledger owes it. It is agent_turn_stream.go's onToolResult entry point:
//   - a job's own tagged "started" result: fused with announced=1
//     (acknowledgeWithMessageTx), or plain Create + acknowledged;
//   - a successful job_kill result that acted on a tracked job: fused into
//     that row's notice_message_id (jobKillResultMessageTx);
//   - anything else: a plain Create, touching no job.
func (l *workLedger) persistToolResult(ctx context.Context, sessionID string, result message.ToolResult, messages message.Service, params message.CreateMessageParams) error {
	job := l.claimAck(ctx, sessionID, result)
	if job != nil {
		if _, handled, err := l.acknowledgeWithMessageTx(ctx, job, messages, params); handled {
			return err
		}
	} else if result.Name == tools.JobKillToolName && !result.IsError {
		var meta tools.JobKillResponseMetadata
		if json.Unmarshal([]byte(result.Metadata), &meta) == nil && meta.JobID != "" {
			if _, handled, err := l.jobKillResultMessageTx(ctx, sessionID, meta.JobID, messages, params); handled {
				return err
			}
		}
	}
	// The caller passes the turn's outer ctx, which survives genCtx's
	// cancellation, so the message lands even mid-cancel.
	_, err := messages.Create(ctx, sessionID, params)
	if job != nil {
		if err != nil {
			l.abort(job)
		} else {
			l.acknowledged(job)
		}
	}
	return err
}

// acknowledgeWithMessageTx is persistToolResult's fused ack. handled=false
// means the caller must fall back to its own plain messages.Create followed
// by acknowledged/abort: a sync job (announced=true already, no DB row to
// fuse with, doc sec.3.1) or no store.
//
// handled=true, err!=nil mirrors the plain path's createMsgErr!=nil branch:
// the row is already deleted (ASYNC-05, via abort) by the time this returns,
// so the caller only needs to propagate err -- it must NOT also call
// abort/acknowledged itself.
//
// B15: ErrAsyncJobGone (the row was deleted out from under the job, e.g. by the owner session's cascade delete; Rerun only voids rows, it never deletes them) means
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
func (l *workLedger) acknowledgeWithMessageTx(ctx context.Context, job *asyncJob, messages message.Service, params message.CreateMessageParams) (msg message.Message, handled bool, err error) {
	if job == nil {
		return message.Message{}, false, nil
	}
	l.mu.Lock()
	store := l.store
	l.mu.Unlock()
	if job.sync || store == nil {
		return message.Message{}, false, nil
	}

	msg, err = store.AnnounceStarted(ctx, messages, job.owner, job.toolCallID, params)
	if err != nil {
		if errors.Is(err, session.ErrAsyncJobGone) {
			return message.Message{}, false, nil
		}
		// The fused transaction failed (message insert or the announce
		// write) -- abort's existing DB-delete + in-memory drop + executor
		// cancel is the correct recovery, same as a plain Create failure
		// used before this method existed. abort leaves a job that is
		// announced by now alone.
		l.abort(job)
		return message.Message{}, true, err
	}
	// Doc sec.3.8's Ack gate paragraph: "if the row was already terminal,
	// nudge the session" IS this call -- finishAcknowledgeLocally flips
	// job.announced and, if the job's terminal transition already committed
	// while unannounced, delivers it right here via the existing
	// deliverLocked/onWebDone wake-hint machinery (work_ledger.go), the same
	// path a fast job finishing before its own ack always used.
	l.finishAcknowledgeLocally(job)
	return msg, true, nil
}

// jobKillResultMessageTx is persistToolResult's job_kill entry point for A3
// (doc sec.3.2's law "delivery='done' => the row names the message that
// carries its result"): a job_kill CALL's own tool-result message, keyed by
// targetToolCallID -- the async_jobs row job_kill just acted on
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
