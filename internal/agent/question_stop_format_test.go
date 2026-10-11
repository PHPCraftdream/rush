package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: shared framing drift or producer/recovery divergence must fail.
func TestQuestionStopFormatRecoverDeadHost(t *testing.T) {
	t.Parallel()
	for _, preamble := range []string{"", "Checked the output; one detail is unclear."} {
		name := "empty_preamble"
		if preamble != "" {
			name = "with_preamble"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
			q := db.New(conn)
			sessions := session.NewService(q, conn)
			parent, err := sessions.Create(ctx, "question parent")
			require.NoError(t, err)
			child, err := sessions.Create(ctx, "question child")
			require.NoError(t, err)
			messages := message.NewService(q)
			const question = "Should I ship or hold?"
			title, details := awaitingAnswerStoppedFinishText(&AwaitingAnswerError{
				Question: question, Options: []string{"ship", "hold"}, SessionID: child.ID,
			})
			require.Equal(t, "Stopped: agent asked a question and is awaiting an answer", title)
			require.Contains(t, details, "QUESTION: "+question)
			require.Contains(t, details, "\n\nThis is not a crash")
			_, err = messages.Create(ctx, child.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: preamble},
					message.Finish{Reason: message.FinishReasonError, Message: title, Details: details},
				},
			})
			require.NoError(t, err)

			const host, job = "question-format-dead-host", "question-format-job"
			lock, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, host))
			require.NoError(t, err)
			require.NoError(t, lock.Release())
			_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
				OwnerSessionID: parent.ID, ToolCallID: job, Kind: "agent", ToolName: "agent",
				InputHash: "question-format", HostID: host, ClaimID: "question-format-claim",
				ChildSessionID: sql.NullString{String: child.ID, Valid: true},
				CreatedAt:      1700000000, UpdatedAt: 1700000000,
			})
			require.NoError(t, err)
			_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
				OwnerSessionID: parent.ID, ToolCallID: job, UpdatedAt: 1700000001,
			})
			require.NoError(t, err)

			out, err := store.RecoverDeadHost(ctx, host, messages)
			require.NoError(t, err)
			require.Equal(t, 1, out.Interrupted)
			row, err := store.Get(ctx, parent.ID, job)
			require.NoError(t, err)
			require.Equal(t, "interrupted", row.State)
			want := "SUB-AGENT QUESTION (session " + child.ID + "): " + question + "\n\nSuggested options: ship | hold"
			if preamble != "" {
				want = preamble + "\n\n" + want
			}
			require.True(t, row.ResultSummary.Valid)
			require.Equal(t, want, row.ResultSummary.String)
		})
	}
}
