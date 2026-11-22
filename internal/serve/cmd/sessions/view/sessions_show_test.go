package sessview

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func TestSessionsShow_TextOutput(t *testing.T) {
	t.Parallel()

	// Create test database
	conn, q := cmdtest.NewTestDB(t)

	// Create a session
	s := session.NewService(q, conn)
	sess, err := s.Create(context.Background(), "test session")
	require.NoError(t, err)
	require.NoError(t, s.UpdateModels(context.Background(), sess.ID, &session.ModelSlotUpdate{Provider: "anthropic", Model: "claude-3-5-sonnet"}, &session.ModelSlotUpdate{Provider: "anthropic", Model: "claude-3-5-haiku"}))

	// Verify session was created
	retrieved, err := s.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "test session", retrieved.Title)
	require.Equal(t, "claude-3-5-sonnet", retrieved.SmartModelID)
}

func TestSessionsShow_WithMessages(t *testing.T) {
	t.Parallel()

	conn, q := cmdtest.NewTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "messages test")
	require.NoError(t, err)

	// Add messages
	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Hello"}},
	})
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "Hi there"}},
	})
	require.NoError(t, err)

	// Verify messages were created
	msgs, err := m.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, message.Assistant, msgs[1].Role)
}

func TestSessionsShow_NotFound(t *testing.T) {
	t.Parallel()

	conn, q := cmdtest.NewTestDB(t)
	s := session.NewService(q, conn)

	// Try to get nonexistent session
	_, err := s.Get(context.Background(), "nonexistent-id")
	require.Error(t, err)
}

// NOTE (R-ACT-2): the three TestReclassifyCrashedAsDone_* tests were deleted
// with the function they exercised. The rules they pinned moved into the one
// classifier and CHANGED by architect decision: ended_reason is the only end
// signal (a clean end_turn finish without it reads idle, not done -- D4), and
// a recorded dead PID stays crashed whatever the last finish (D3). The
// replacement rows are TestClassifySessionActivity/"D3 dead PID with
// end_turn finish stays crashed" and /"D4 end_turn without ended_reason is
// idle" (internal/session/session_activity_test.go).
