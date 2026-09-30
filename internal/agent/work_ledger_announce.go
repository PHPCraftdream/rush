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
	"log/slog"
	"time"

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
//   - a job_kill result that stopped a tracked job: fused into that row's
//     notice_message_id, with the row re-pended when that write does not
//     happen (persistJobKillResult);
//   - anything else: a plain Create, touching no job.
func (l *workLedger) persistToolResult(ctx context.Context, sessionID string, result message.ToolResult, messages message.Service, params message.CreateMessageParams) error {
	job := l.claimAck(ctx, sessionID, result)
	if job != nil {
		if _, handled, err := l.acknowledgeWithMessageTx(ctx, job, messages, params); handled {
			return err
		}
	} else if result.Name == tools.JobKillToolName {
		if handled, err := l.persistJobKillResult(ctx, sessionID, result, messages, params); handled {
			return err
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

// jobKillRepender is the store method that puts a job_kill row whose fused
// result write did not happen back into the ordinary pull path (R2A-8):
// delivery='pending', reacted=0, wake=0, only while the row still matches
// (owner, tool_call_id, claim_id), is 'done' by job_kill and has no
// notice_message_id. Satisfied by *session.AsyncJobStore (compile-time
// assertion below); tests inject a fake through workLedger.jobKillRepend.
type jobKillRepender interface {
	RependJobKillRowWithoutNotice(ctx context.Context, owner, toolCallID, claimID string) (bool, error)
}

// The real store must satisfy it: a signature drift here would silently
// disable the re-pend (the runtime lookup below is a type assertion).
var _ jobKillRepender = (*session.AsyncJobStore)(nil)

// jobKillRependBudget bounds rependJobKill's retries.
const jobKillRependBudget = 30 * time.Second

// rependJobKill re-pends the job_kill row named by meta (see
// jobKillRepender). Retried like every other store write on this path; the
// caller's ctx may already be cancelled (that is one of the reasons the
// fused write did not happen), so the write is detached from it.
func (l *workLedger) rependJobKill(ctx context.Context, owner string, meta tools.JobKillResponseMetadata) {
	repender := l.jobKillRepend
	if repender == nil {
		var r jobKillRepender
		ok := false
		if l.store != nil {
			r, ok = any(l.store).(jobKillRepender)
		}
		if !ok {
			slog.Error("job_kill result was not recorded and the store cannot re-pend its row; the output stays undelivered",
				"session_id", owner, "tool_call_id", meta.JobID)
			return
		}
		repender = r
	}
	// Detached from ctx (it may already be cancelled -- one reason the fused
	// write did not happen) but bounded, so a store that keeps failing cannot
	// hold the turn's tool-result callback forever.
	detached, stop := context.WithTimeout(context.WithoutCancel(ctx), jobKillRependBudget)
	defer stop()
	if err := l.retryAsyncStoreOp(detached, func() error {
		_, err := repender.RependJobKillRowWithoutNotice(detached, owner, meta.JobID, meta.KilledClaimID)
		if errors.Is(err, session.ErrAsyncJobGone) {
			return nil
		}
		return err
	}); err != nil {
		slog.Error("job_kill: failed to re-pend the row whose result was not recorded",
			"session_id", owner, "tool_call_id", meta.JobID, "err", err)
	}
}

// persistJobKillResult is persistToolResult's job_kill branch (A3 + R2A-8):
// doc sec.3.2's law "delivery='done' => the row names the message that
// carries its result". job_kill's Transition already set delivery='done' /
// reacted=1, so nothing else ever writes notice_message_id -- this fuses
// job_kill's own tool-result message id onto the TARGET row (named by the
// result's metadata, NOT job_kill's own tool_call_id) in the SAME
// transaction as the message insert. Whenever that fused write does not
// happen -- the result is an error, the transaction fails, ctx is cancelled
// -- the row is re-pended so the captured output is delivered by the
// ordinary pull instead of being lost.
//
// handled=false (fall back to the caller's own plain Create) when the
// result names no job it stopped: an ordinary result, a raw shell_id kill,
// or a refusal (B11), none of which carry a killed claim.
func (l *workLedger) persistJobKillResult(ctx context.Context, sessionID string, result message.ToolResult, messages message.Service, params message.CreateMessageParams) (handled bool, err error) {
	var meta tools.JobKillResponseMetadata
	if json.Unmarshal([]byte(result.Metadata), &meta) != nil || meta.JobID == "" || meta.KilledClaimID == "" {
		return false, nil
	}
	l.mu.Lock()
	store := l.store
	l.mu.Unlock()
	if store == nil {
		return false, nil
	}
	if !result.IsError {
		if _, err = store.AnnounceJobKillResult(ctx, messages, sessionID, meta.JobID, params); err == nil {
			return true, nil
		}
		l.rependJobKill(ctx, sessionID, meta)
		return true, err
	}
	// An error result that still names a stopped job: persist the message
	// the ordinary way, and never leave the row done without a notice.
	_, err = messages.Create(ctx, sessionID, params)
	l.rependJobKill(ctx, sessionID, meta)
	return true, err
}
