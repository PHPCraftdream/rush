// The Drain call kind (phase-4 step 3, docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.4): a SessionAgentCall whose only job is the
// driver's notice pull (agent_notice_pull.go) at turn start, followed by an
// empty-prompt reaction turn if (and only if) debt that pull made visible is
// still owed and the ONE launch predicate (drainPermitted, see
// decideDrainTurn in agent_drain_decision.go) allows a turn. Submitted
// through the ordinary Run/submit path: idle -> becomes the owner like any
// call; busy -> queued, merging with whatever is already there
// (mailbox_ownership.go's submit).
package agent

import (
	"context"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/permission"
)

// newDrainCall is the SINGLE constructor for a Drain call: the only place
// IsDrain is ever set to true. base is either a fresh
// SessionAgentCall{SessionID: sessionID} (a root/non-driver session) or a
// delegated child's driver.callFor("") template (doc sec.3.4: "the child's
// drain uses the driver's template via the single constructor"). Either
// way, every field a call kind or notice-flag could otherwise carry forward
// from an active call is reset HERE, not inherited: a Drain never carries
// another call's prompt, notice kind, existing-message reference, or
// queue/durable-persistence markers.
func newDrainCall(base SessionAgentCall) SessionAgentCall {
	base.IsDrain = true
	base.drainTurnCommitted = false
	base.Prompt = ""
	base.AutoResumed = true
	base.BackgroundJobNotice = true
	base.NoticeKind = ""
	base.ExistingMessageID = ""
	base.OnUserMessageCreated = nil
	base.OnAssistantMessageCreated = nil
	base.FromDurableQueue = false
	base.FailIfSessionBusy = false
	base.onQueueResolved = nil
	return base
}

// callFromActive is the single constructor for a call built from an active
// call or a template (the `sessions inject --interrupt` replacement,
// subAgentDriver.callFor): it keeps the source's model/sampling/policy shape
// but resets the call kind and notice flags, which are never inherited (doc
// sec.3.4). Without it, an operator's interrupt landing on a Drain turn
// would itself run as a Drain -- and, with nothing wake-worthy to pull,
// never reach the provider.
func callFromActive(active SessionAgentCall) SessionAgentCall {
	call := active
	call.IsDrain = false
	call.drainTurnCommitted = false
	call.AutoResumed = false
	call.BackgroundJobNotice = false
	call.NoticeKind = ""
	return call
}

// drainCallFor builds sessionID's Drain call: a delegated child uses its
// driver's own frozen template (same model/provider/sampling shape the
// delegation launched with, doc sec.3.4), a root/non-driver session
// resolves its own pinned models exactly like an ordinary wake turn does
// today (wakeNoticeCall's non-driver branch, which this replaces).
func (c *coordinator) drainCallFor(ctx context.Context, sessionID string) (SessionAgentCall, error) {
	if driver, ok := c.subAgentDrivers.get(sessionID); ok {
		// §6.2 (preserved from the old wakeNoticeCall driver branch): every
		// wake re-inherits the driver's parentSessionID's allowlist baseline
		// onto the child BEFORE waking it -- the child's own turn no longer
		// clears its allowlist entry on return, so re-arming here keeps a
		// woken turn judged by the delegation's policy instead of falling
		// back to the process-wide gate.
		if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok && driver.parentSessionID != "" {
			mgr.InheritSessionRunAllowlistForGeneration(driver.parentSessionID, sessionID, driver.generation)
		}
		return newDrainCall(driver.callFor("")), nil
	}
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return SessionAgentCall{}, err
	}
	call, err := c.buildCall(ctx, sessionID, "", pinned, nil)
	if err != nil {
		return SessionAgentCall{}, err
	}
	return newDrainCall(call), nil
}

// reminderBeforeTail moves preparePrompt's trailing todo reminder in front
// of the last k messages -- the notices a Drain turn's own turn-start pull
// appended -- so they stay the final user messages the provider sees (a
// Drain has no prompt of its own to follow the reminder). No-op unless the
// last message is that reminder and the k before it are user-role.
func reminderBeforeTail(history []fantasy.Message, k int) []fantasy.Message {
	n := len(history)
	if k <= 0 || n < k+1 || !isTodoReminder(history[n-1]) {
		return history
	}
	for _, m := range history[n-1-k : n-1] {
		if m.Role != fantasy.MessageRoleUser {
			return history
		}
	}
	out := make([]fantasy.Message, 0, n)
	out = append(out, history[:n-1-k]...)
	out = append(out, history[n-1])
	return append(out, history[n-1-k:n-1]...)
}

// isTodoReminder reports whether m is preparePrompt's <system_reminder>
// todo message.
func isTodoReminder(m fantasy.Message) bool {
	if m.Role != fantasy.MessageRoleUser || len(m.Content) != 1 {
		return false
	}
	part, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0])
	return ok && strings.HasPrefix(part.Text, "<system_reminder>")
}
