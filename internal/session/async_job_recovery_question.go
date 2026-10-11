package session

import (
	"strings"

	"github.com/PHPCraftdream/rush/internal/message"
)

// Match agent.subAgentQuestionBlockFromFinish without a session -> agent import.
func recoveredQuestionText(childSessionID, preamble string, fp *message.Finish) (string, bool) {
	if fp == nil || fp.Reason != message.FinishReasonError || fp.Message != AwaitingAnswerStoppedTitle {
		return "", false
	}
	_, rest, ok := strings.Cut(fp.Details, AwaitingQuestionPrefix)
	if !ok {
		return "", false
	}
	block, _, ok := strings.Cut(rest, AwaitingGuidanceMarker)
	if !ok {
		return "", false
	}
	text := childQuestionFramePrefix + childSessionID + "): " + block
	if preamble = strings.TrimSpace(preamble); preamble != "" {
		text = preamble + "\n\n" + text
	}
	return text, true
}
