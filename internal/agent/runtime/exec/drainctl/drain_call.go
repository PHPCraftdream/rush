package drainctl

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// NewDrainCall is the SINGLE constructor for a Drain call: the only place
// IsDrain is ever set to true. base is either a fresh
// agentcall.SessionAgentCall{SessionID: sessionID} (a root/non-driver session) or a
// delegated child's driver.callFor("") template (doc sec.3.4: "the child's
// drain uses the driver's template via the single constructor"). Either
// way, every field a call kind or notice-flag could otherwise carry forward
// from an active call is reset HERE, not inherited: a Drain never carries
// another call's prompt, notice kind, existing-message reference, or
// queue/durable-persistence markers.
func NewDrainCall(base agentcall.SessionAgentCall) agentcall.SessionAgentCall {
	base.IsDrain = true
	base.DrainTurnCommitted = false
	base.Prompt = ""
	base.AutoResumed = true
	base.BackgroundJobNotice = true
	base.NoticeKind = ""
	base.ExistingMessageID = ""
	base.OnUserMessageCreated = nil
	base.OnAssistantMessageCreated = nil
	base.FromDurableQueue = false
	base.FailIfSessionBusy = false
	base.OnQueueResolved = nil
	return base
}

// LastUserMessageOrigin returns the origin of the chronologically latest user
// message. Read failures and empty history fail closed (no web preference).
func LastUserMessageOrigin(ctx context.Context, messages message.Service, sessionID string) (message.Origin, bool) {
	if messages == nil {
		return message.OriginUnspecified, false
	}
	rows, err := messages.ListUserMessages(ctx, sessionID)
	if err != nil {
		slog.Warn("Drain: could not read last user-message origin; using configured models", "session_id", sessionID, "error", err)
		return message.OriginUnspecified, false
	}
	if len(rows) == 0 {
		return message.OriginUnspecified, false
	}
	return rows[0].Origin, true
}

func ApplyDrainModelOptions(call agentcall.SessionAgentCall, model agentcall.Model) agentcall.SessionAgentCall {
	call.SmartModel = &model
	call.MaxOutputTokens = model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		call.MaxOutputTokens = model.ModelCfg.MaxTokens
	}
	call.Temperature = model.ModelCfg.Temperature
	call.TopP = model.ModelCfg.TopP
	call.TopK = model.ModelCfg.TopK
	call.FrequencyPenalty = model.ModelCfg.FrequencyPenalty
	call.PresencePenalty = model.ModelCfg.PresencePenalty
	return call
}

// ReminderBeforeTail moves preparePrompt's trailing todo reminder in front
// of the last k messages -- the notices a Drain turn's own turn-start pull
// appended -- so they stay the final user messages the provider sees (a
// Drain has no prompt of its own to follow the reminder). No-op unless the
// last message is that reminder and the k before it are user-role.
func ReminderBeforeTail(history []fantasy.Message, k int) []fantasy.Message {
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

// BuildSessionNoticeMessageParams converts a pulled session_notices row into
// the history message's params. The row's own text is already the final,
// formatted wording (built once, at insert time, by supervision.go/
// work_ledger_timeout.go/coordinator_background.go) -- no second formatter.
func BuildSessionNoticeMessageParams(row sessionnotice.SessionNoticeRow) message.CreateMessageParams {
	kind := row.Kind
	if kind == session.NoticeKindBGShellDone {
		// Preserve today's exact behavior for this one kind: BackgroundJobNotice
		// alone carried it before step 3, with no distinct message-level
		// NoticeKind. session_notices.kind still records "bg_shell_done" for
		// bookkeeping/display (`sessions why`), just not as the message's
		// own NoticeKind.
		kind = ""
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: row.Text}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		NoticeKind:          kind,
	}
}

func BuildBatchNoticeMessageParams(row session.BatchNoticeRow) message.CreateMessageParams {
	return batchmeta.BuildBatchNoticeMessageParams(row)
}

func ClearCancelRequest(ctx context.Context, sessions coresession.Service, sessionID string) {
	if sessions == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := sessions.ClearCancelRequest(ctx, sessionID); err != nil {
		slog.Warn("could not clear the cancel-requested flag", "session_id", sessionID, "err", err)
	}
}
