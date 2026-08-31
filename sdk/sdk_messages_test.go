package sdk_test

// Tests for the sdk.Client read facade methods Messages and Session:
// thin pass-throughs to the already-existing internal message/session
// services, exposed so embedders can read history and metadata without
// touching internal packages.
//
//   - Messages returns the full chronological history of a session
//     (every role), with or without a prior SubscribeMessages
//     subscription (the subscription channel carries no backlog);
//   - for an ephemeral in-memory session (ModeLibrary with no
//     WorkingDir) it is the ONLY way to see history once Run has
//     returned, and it dies permanently with Close;
//   - Session returns the session's current metadata (ID, title,
//     counters), not its history.

import (
	"context"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

func TestClientMessages_PersistedSessionHistory(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	server := newCredentialServer(t, "MESSAGES_PERSISTED_OK")
	workDir := t.TempDir()

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		WorkingDir:    workDir,
		LibraryConfig: libraryConfigFor(server.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	// No SubscribeMessages anywhere: history must still be retrievable
	// after the Run has returned, from the persisted store.
	res := runLibraryPrompt(t, client, "")

	msgs, err := client.Messages(context.Background(), res.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)

	// First message is the user prompt, chronologically before the
	// assistant's reply.
	require.Equal(t, message.User, msgs[0].Role)
	require.Contains(t, msgs[0].FullText(), "marker text")

	lastAssistant := -1
	for i, msg := range msgs {
		if msg.Role == message.Assistant {
			lastAssistant = i
		}
	}
	require.GreaterOrEqual(t, lastAssistant, 0, "no assistant message in history")
	require.Contains(t, msgs[lastAssistant].FullText(), "MESSAGES_PERSISTED_OK")
	require.Less(t, 0, lastAssistant, "user prompt must precede the assistant reply")

	// CreatedAt must be non-decreasing across the slice.
	for i := 1; i < len(msgs); i++ {
		require.GreaterOrEqual(t, msgs[i].CreatedAt, msgs[i-1].CreatedAt,
			"CreatedAt must be non-decreasing at index %d", i)
	}
}

func TestClientMessages_EphemeralSessionHistoryWithoutSubscription(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	server := newCredentialServer(t, "MESSAGES_EPHEMERAL_OK")

	// No WorkingDir at all: fully in-memory session. Once Run returns,
	// client.Messages is the ONLY way to see this session's history,
	// and it becomes permanently unavailable the moment Close is
	// called -- nothing survives on disk.
	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(server.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	res := runLibraryPrompt(t, client, "")
	require.NotEmpty(t, res.SessionID)

	// No prior SubscribeMessages subscription: still retrievable.
	msgs, err := client.Messages(context.Background(), res.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)

	require.Equal(t, message.User, msgs[0].Role)
	require.Contains(t, msgs[0].FullText(), "marker text")

	lastAssistant := -1
	for i, msg := range msgs {
		if msg.Role == message.Assistant {
			lastAssistant = i
		}
	}
	require.GreaterOrEqual(t, lastAssistant, 0, "no assistant message in history")
	require.Contains(t, msgs[lastAssistant].FullText(), "MESSAGES_EPHEMERAL_OK")
	require.Less(t, 0, lastAssistant, "user prompt must precede the assistant reply")
}

func TestClientSession_ReturnsSessionMetadata(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	server := newCredentialServer(t, "SESSION_META_OK")

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(server.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	res := runLibraryPrompt(t, client, "")

	got, err := client.Session(context.Background(), res.SessionID)
	require.NoError(t, err)
	require.Equal(t, res.SessionID, got.ID)
	// Sessions are created with agent.DefaultSessionName as title and
	// may be retitled asynchronously: assert non-empty, not an exact
	// value.
	require.NotEmpty(t, got.Title)
	require.True(t, strings.TrimSpace(got.Title) != "")
}
