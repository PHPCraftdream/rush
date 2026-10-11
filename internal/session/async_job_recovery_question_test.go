package session

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Revert-check: async_job_recovery_question.go's guards and cuts must match the agent question-finish parser.
func TestRecoveredQuestionText_Recognition(t *testing.T) {
	const title = "Stopped: agent asked a question and is awaiting an answer"
	const block = "ship?\n\nSuggested options: ship | hold"
	const details = "error context\n\nQUESTION: " + block + "\n\nThis is not a crash — guidance"
	for _, tc := range []struct {
		name    string
		finish  *message.Finish
		want    string
		matched bool
	}{
		{"question", &message.Finish{Reason: message.FinishReasonError, Message: title, Details: details}, block, true},
		{"empty_block", &message.Finish{Reason: message.FinishReasonError, Message: title, Details: "QUESTION: \n\nThis is not a crash"}, "", true},
		{"nil", nil, "", false},
		{"normal_finish", &message.Finish{Reason: message.FinishReasonEndTurn, Message: title, Details: details}, "", false},
		{"other_error", &message.Finish{Reason: message.FinishReasonError, Message: "other error", Details: details}, "", false},
		{"missing_prefix", &message.Finish{Reason: message.FinishReasonError, Message: title, Details: "ship?\n\nThis is not a crash"}, "", false},
		{"missing_guidance", &message.Finish{Reason: message.FinishReasonError, Message: title, Details: "QUESTION: ship?"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, matched := recoveredQuestionText("child", "  preamble  ", tc.finish)
			require.Equal(t, tc.matched, matched)
			if matched {
				require.Equal(t, "preamble\n\nSUB-AGENT QUESTION (session child): "+tc.want, text)
			} else {
				require.Empty(t, text)
			}
		})
	}
}

// Revert-check: async_job_recovery.go's FullText/no-text fallback must survive unrecognized question finishes.
func TestRecoveredDelegationText_QuestionFallback(t *testing.T) {
	for _, preamble := range []string{"", "ordinary error text"} {
		t.Run("text="+preamble, func(t *testing.T) {
			_, q, ctx := newTestStore(t)
			require.NoError(t, seedSession(ctx, q, "child"))
			messages := message.NewService(q)
			_, err := messages.Create(ctx, "child", message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: preamble},
					message.Finish{
						Reason:  message.FinishReasonError,
						Message: "Stopped: agent asked a question and is awaiting an answer",
						Details: "QUESTION: malformed without guidance marker",
					},
				},
			})
			require.NoError(t, err)
			text, err := recoveredDelegationText(ctx, messages, "child")
			require.NoError(t, err)
			want := preamble
			if want == "" {
				want = interruptedNoChildTextText
			}
			require.Equal(t, want, text)
		})
	}
}
