package app

import (
	"bytes"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Тестовый барьер после scope read, перед ожиданием hint; production оставляет nil.
var cliLoopInterruptWaitSeam func()

// Queue остаётся единственным владельцем исполнения; CLI забирает его результат.
func (l *cliLoop) runWaitingInterrupt(messageID string) error {
	if l.env.Pump() == nil {
		return fmt.Errorf("rush run: interrupt requires the durable run queue")
	}
	before := l.sessionUsage()
	if err := l.host.drainPendingBeforeRun(l.ctx, l.sessionID); err != nil {
		return err
	}
	if err := l.host.waitLocalOwnerReleased(l.ctx, l.sessionID); err != nil {
		return err
	}
	msgs, err := l.env.Messages().List(l.ctx, l.sessionID)
	if err != nil {
		return fmt.Errorf("rush run: read interrupt outcome: %w", err)
	}
	anchor := -1
	for i := range msgs {
		if msgs[i].ID == messageID {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return fmt.Errorf("rush run: accepted interrupt message is unavailable")
	}
	var final *message.Message
	counts := make(map[string]int)
	for i := anchor + 1; i < len(msgs); i++ {
		if msgs[i].Role != message.Assistant {
			continue
		}
		for _, call := range msgs[i].ToolCalls() {
			counts[call.Name]++
		}
		if msgs[i].IsFinished() {
			final = &msgs[i]
		}
	}
	if final == nil {
		return fmt.Errorf("rush run: interrupt finished without a completed assistant message")
	}
	text, notes := final.FullText(), ""
	stripped, stripError, stripReason := 0, "", ""
	if l.overrides.StripJSONFences {
		var stripErr error
		text, notes, stripErr = stripAndExtractJSON(text)
		stripped = max(len(final.FullText())-len(text), 0)
		if stripErr != nil {
			stripError, stripReason = stripErr.Error(), "invalid_json"
		}
	}
	after := l.sessionUsage()
	tokens, cost := int64(0), float64(0)
	if before.ok && after.ok {
		tokens = max(after.tokens-before.tokens, 0)
		cost = max(after.cost-before.cost, 0)
	}
	var subOutputs []SubAgentOutput
	if l.overrides.AggregationMode == "attach" {
		subOutputs = l.host.collectSubAgentOutputs(l.ctx, l.sessionID)
	}
	result := buildRunResult(l.sessionID, text, notes, string(final.FinishReason()), nil, false, counts, tokens, cost, time.Since(l.started), "", "", stripped, stripError, stripReason, subOutputs, "")
	if usage, err := l.env.Messages().UsageBySession(l.ctx, l.sessionID); err == nil {
		result.Usage.Session = buildSessionUsageInfo(usage)
	}
	l.flushQueuedUsage(before)
	l.tot.add(&result)
	l.final, l.lastBuffered = &result, bytes.NewBufferString(text)
	if l.mode == RunModeStream {
		if _, err := fmt.Fprint(l.output, text); err != nil {
			return err
		}
	}
	l.runErr, l.lastFailed = nil, nil
	l.streak = drainStreak{}
	l.reviewerDone = false
	l.reviewBasis = l.host.captureReviewBasis(l.ctx, l.overrides.ModelRole, msgs[anchor].FullText(), l.started)
	return nil
}
