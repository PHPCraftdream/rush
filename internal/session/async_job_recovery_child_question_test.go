package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Revert-check: async_job_recovery.go:262 must not let FullText bypass a question finish before notice_pull.go:281 voids its notice.
func TestRecoverDeadHost_ChildQuestionSurvivesNoticePull(t *testing.T) {
	for _, entrypoint := range []string{"RecoverDeadHost", "SweepDeadHosts", "RecoverOwnerScope"} {
		for _, preamble := range []string{"", "Checked the output; one detail is unclear."} {
			name := "empty_preamble"
			if preamble != "" {
				name = "with_preamble"
			}
			t.Run(entrypoint+"/"+name, func(t *testing.T) {
				store, q, ctx := newTestStore(t)
				const parent, child, job, host = "question-parent", "question-child", "question-delegation", "question-dead-host"
				const question = "Should I ship or hold?"
				const options = "Suggested options: ship | hold"
				require.NoError(t, seedSession(ctx, q, parent))
				require.NoError(t, seedSession(ctx, q, child))
				messages := message.NewService(db.New(store.sqlDB))

				// Match agent/question_stop.go's encoding; question/options live
				// only in Finish.Details, avoiding a session -> agent import.
				details := fmt.Sprintf(
					"agent asked a question and is awaiting an answer\n\nQUESTION: %s\n\n%s\n\n"+
						"This is not a crash — rush is intentionally stopping this turn because "+
						"the agent asked a question and needs an answer before it can continue. "+
						"rush is exiting now; it will not retry or re-ask on its own.\n\n"+
						"If an orchestrating agent is driving this session: decide the answer "+
						"yourself (or ask the human operator if it's genuinely their call), then "+
						"resume with:\n\n  rush run --session %s \"<your answer>\"\n\n"+
						"This is a normal continuation, not a retry — do not treat exit_reason "+
						"\"awaiting_answer\" as a failure.", question, options, child,
				)
				parts := []message.ContentPart{}
				if preamble != "" {
					parts = append(parts, message.TextContent{Text: preamble})
				}
				parts = append(parts, message.Finish{
					Reason:  message.FinishReasonError,
					Message: "Stopped: agent asked a question and is awaiting an answer",
					Details: details,
				})
				_, err := messages.Create(ctx, child, message.CreateMessageParams{Role: message.Assistant, Parts: parts})
				require.NoError(t, err)
				// The literal last message is a tool result, as in a real question stop.
				_, err = messages.Create(ctx, child, message.CreateMessageParams{
					Role: message.Tool,
					Parts: []message.ContentPart{message.ToolResult{
						ToolCallID: "ask-question", Name: "ask_question", Content: "turn ended",
					}},
				})
				require.NoError(t, err)

				fabricateDeadHost(t, ctx, q, store.dataDir, host)
				status, err := ProbeHostShared(store.dataDir, host)
				require.NoError(t, err)
				require.Equal(t, HostStatusDead, status)
				seedRunningJob(t, ctx, q, parent, job, host, child, true)
				frame := "SUB-AGENT QUESTION (session " + child + "): "
				require.NoError(t, store.InsertSessionNotice(ctx, parent, NoticeKindChildQuestion,
					frame+question+"\n"+options+"\n\nThe sub-agent is paused; its delegation stays open.", true, job))

				switch entrypoint {
				case "RecoverDeadHost":
					out, err := store.RecoverDeadHost(ctx, host, messages)
					require.NoError(t, err)
					require.Equal(t, 1, out.Interrupted)
				case "SweepDeadHosts":
					out, err := store.SweepDeadHosts(ctx, messages)
					require.NoError(t, err)
					require.Equal(t, 1, out[host].Interrupted)
				case "RecoverOwnerScope":
					store.RecoverOwnerScope(ctx, parent, messages)
				}
				row, err := store.Get(ctx, parent, job)
				require.NoError(t, err)
				require.Equal(t, "interrupted", row.State)
				require.Equal(t, "pending", row.Delivery)
				before, err := messages.List(ctx, parent)
				require.NoError(t, err)
				require.Empty(t, before, "recovery must not write parent history")

				var delivered JobNoticeRow
				pulled, err := store.PullJobNotices(ctx, messages, parent, func(row JobNoticeRow) message.CreateMessageParams {
					delivered = row
					return message.CreateMessageParams{
						Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: row.ResultContent}},
						BackgroundJobNotice: true, NoticeKind: row.NoticeKind,
					}
				})
				require.NoError(t, err)
				require.Len(t, pulled, 1)
				assert.Equal(t, "interrupted", delivered.State)
				assert.Equal(t, "interrupted", delivered.NoticeKind)
				assert.True(t, delivered.ResultIsError)
				assert.False(t, pulled[0].Wake)
				text := pulled[0].Message.FullText()
				assert.Contains(t, text, frame)
				assert.Contains(t, text, question)
				assert.Contains(t, text, options)
				if preamble != "" {
					assert.Contains(t, text, preamble)
					assert.True(t, strings.HasPrefix(text, preamble), "preamble must precede the question")
				}

				notices, err := store.PullSessionNotices(ctx, messages, parent, sessionNoticeParams)
				require.NoError(t, err)
				require.Empty(t, notices, "the original held-question notice must not duplicate the interrupted result")
				var delivery string
				require.NoError(t, store.sqlDB.QueryRowContext(ctx,
					"SELECT delivery FROM session_notices WHERE owner = ? AND kind = ?", parent, NoticeKindChildQuestion).Scan(&delivery))
				assert.Equal(t, "void", delivery)
				pulled, err = store.PullJobNotices(ctx, messages, parent, jobNoticeParams)
				require.NoError(t, err)
				require.Empty(t, pulled)
				notices, err = store.PullSessionNotices(ctx, messages, parent, sessionNoticeParams)
				require.NoError(t, err)
				require.Empty(t, notices)
				history, err := messages.List(ctx, parent)
				require.NoError(t, err)
				require.Len(t, history, 1, "both pulls together must create exactly one parent message")
			})
		}
	}
}
