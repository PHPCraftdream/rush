package session

import (
	"strings"

	"github.com/PHPCraftdream/rush/internal/message"
)

// Match agent.subAgentQuestionBlockFromFinish without a session -> agent import.
func recoveredQuestionText(childSessionID, preamble string, fp *message.Finish) (string, bool) {
	if fp == nil || fp.Reason != message.FinishReasonError || fp.Message != "Stopped: agent asked a question and is awaiting an answer" {
		return "", false
	}
	_, rest, ok := strings.Cut(fp.Details, "QUESTION: ")
	if !ok {
		return "", false
	}
	block, _, ok := strings.Cut(rest, "\n\nThis is not a crash")
	if !ok {
		return "", false
	}
	text := childQuestionFramePrefix + childSessionID + "): " + block
	if preamble = strings.TrimSpace(preamble); preamble != "" {
		text = preamble + "\n\n" + text
	}
	return text, true
}
