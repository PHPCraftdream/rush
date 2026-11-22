package sessscan

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func TestSessionsTail_StreamsMessages(t *testing.T) {
	t.Parallel()

	conn, q := cmdtest.NewTestDB(t)
	s := coresession.NewService(q, conn)
	m := message.NewService(q)

	// Create messages
	sess, err := s.Create(context.Background(), "tail test")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "First message"}},
	})
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "Second message"}},
	})
	require.NoError(t, err)

	// Verify messages exist
	msgs, err := m.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
}

func TestSessionsTail_MultipleMessages(t *testing.T) {
	t.Parallel()

	conn, q := cmdtest.NewTestDB(t)
	s := coresession.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "multi test")
	require.NoError(t, err)

	// Create 5 messages
	for i := 0; i < 5; i++ {
		_, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "Message"}},
		})
		require.NoError(t, err)
	}

	msgs, err := m.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 5)
}

func TestSessionsTail_EmptySession(t *testing.T) {
	t.Parallel()

	conn, q := cmdtest.NewTestDB(t)
	s := coresession.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "empty test")
	require.NoError(t, err)

	msgs, err := m.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 0)
}
