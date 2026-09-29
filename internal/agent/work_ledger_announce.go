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
// abort/acknowledged itself. ErrAsyncJobGone (a Rerun truncation raced this
// call) is swallowed here exactly like acknowledged's own MarkAnnounced
// handling: nothing to announce, nothing to abort, no error surfaced.
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
			return message.Message{}, true, nil
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
