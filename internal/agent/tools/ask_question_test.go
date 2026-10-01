package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestAskQuestionTool_ReturnsAwaitingAnswerError(t *testing.T) {
	t.Parallel()

	tool := NewAskQuestionTool()
	require.Equal(t, AskQuestionToolName, tool.Info().Name)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-123")

	input, err := json.Marshal(AskQuestionParams{
		Question: "Which environment should I deploy to?",
		Options:  []string{"staging", "production"},
	})
	require.NoError(t, err)

	resp, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  AskQuestionToolName,
		Input: string(input),
	})

	// The tool must signal via the returned Go error, not a normal
	// (successful) ToolResponse — that's what makes fantasy treat this as
	// a critical, turn-stopping error instead of a regular tool result the
	// model can keep going after.
	require.Error(t, runErr)
	require.Equal(t, fantasy.ToolResponse{}, resp)

	var askErr *AskQuestionError
	require.True(t, errors.As(runErr, &askErr), "expected *AskQuestionError, got %T: %v", runErr, runErr)
	require.Equal(t, "Which environment should I deploy to?", askErr.Question)
	require.Equal(t, []string{"staging", "production"}, askErr.Options)
	require.Equal(t, "sess-123", askErr.SessionID)
}

func TestAskQuestionTool_NoOptions(t *testing.T) {
	t.Parallel()

	tool := NewAskQuestionTool()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-456")

	input, err := json.Marshal(AskQuestionParams{Question: "Proceed with the migration?"})
	require.NoError(t, err)

	_, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  AskQuestionToolName,
		Input: string(input),
	})

	var askErr *AskQuestionError
	require.True(t, errors.As(runErr, &askErr))
	require.Equal(t, "Proceed with the migration?", askErr.Question)
	require.Empty(t, askErr.Options)
	require.Equal(t, "sess-456", askErr.SessionID)
}

func TestAskQuestionTool_EmptyQuestionDoesNotStopTurn(t *testing.T) {
	t.Parallel()

	tool := NewAskQuestionTool()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-789")

	input, err := json.Marshal(AskQuestionParams{Question: "   "})
	require.NoError(t, err)

	resp, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  AskQuestionToolName,
		Input: string(input),
	})

	// A malformed call (blank question) is a normal, retryable tool error
	// — it must come back as an in-band ToolResponse.IsError, with a nil
	// Go error, so the turn does NOT stop and the model can retry.
	require.NoError(t, runErr)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "question must not be empty")
}

// A15: a question asked while the session's own background work is running
// must NOT stop the turn — the tool hands back a hint and the model keeps
// going, so the run does not end with the work orphaned.
//
// Revert-check: removing the OwnRunningJobsFromContext branch in
// ask_question.go turns this back into a force-finish and the test goes red
// (runErr becomes *AskQuestionError, resp is the zero response).
func TestAskQuestionTool_RunningOwnJobsReturnsHintAndContinues(t *testing.T) {
	t.Parallel()

	tool := NewAskQuestionTool()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-bg")
	ctx = WithOwnRunningJobsReader(ctx, func(_ context.Context, sessionID string) (int, bool) {
		require.Equal(t, "sess-bg", sessionID, "the reader must be asked about THIS session")
		return 2, true
	})

	input, err := json.Marshal(AskQuestionParams{Question: "Continue while the heavy runs finish?"})
	require.NoError(t, err)

	resp, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  AskQuestionToolName,
		Input: string(input),
	})

	require.NoError(t, runErr, "a question over running work must not force-finish the turn")
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "2 running background task(s)")
	require.Contains(t, resp.Content, "WITHOUT any tool call",
		"the hint must tell the model how to actually wait (end the turn)")
	var askErr *AskQuestionError
	require.False(t, errors.As(runErr, &askErr))
}

// The zero-running case keeps the legacy force-finish even when a reader is
// wired: the carve-out is for live work, not for "a reader exists".
func TestAskQuestionTool_ZeroRunningJobsStillEndsTurn(t *testing.T) {
	t.Parallel()

	tool := NewAskQuestionTool()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-idle")
	ctx = WithOwnRunningJobsReader(ctx, func(context.Context, string) (int, bool) {
		return 0, true
	})

	input, err := json.Marshal(AskQuestionParams{Question: "Which environment?"})
	require.NoError(t, err)

	_, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  AskQuestionToolName,
		Input: string(input),
	})

	var askErr *AskQuestionError
	require.True(t, errors.As(runErr, &askErr), "no running work: the legacy awaiting-answer path applies")
}

func TestAskQuestionTool_MissingSessionID(t *testing.T) {
	t.Parallel()

	tool := NewAskQuestionTool()

	input, err := json.Marshal(AskQuestionParams{Question: "What now?"})
	require.NoError(t, err)

	_, runErr := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "test-call",
		Name:  AskQuestionToolName,
		Input: string(input),
	})

	var askErr *AskQuestionError
	require.True(t, errors.As(runErr, &askErr))
	require.Equal(t, "What now?", askErr.Question)
	require.Empty(t, askErr.SessionID)
}
