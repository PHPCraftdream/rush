package cmd

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDetectCacheInvalidations_FlagsWarmToColdTransition covers the P2
// warm->cold detector: a turn that read a well-warmed cache followed by one
// that reads nothing but writes fresh tokens is the signature of a broken
// prompt prefix.
func TestDetectCacheInvalidations_FlagsWarmToColdTransition(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	ctx := context.Background()

	sess, err := s.Create(ctx, "warm to cold")
	require.NoError(t, err)

	warm, err := m.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "warm"}},
	})
	require.NoError(t, err)
	require.NoError(t, m.SetUsage(ctx, warm.ID, message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 5000, TotalTokens: 5010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	}))

	cold, err := m.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "cold"}},
	})
	require.NoError(t, err)
	require.NoError(t, m.SetUsage(ctx, cold.ID, message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 4000, TotalTokens: 4010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	}))

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Len(t, got, 1)
	require.Equal(t, cold.ID, got[0].MessageID)
	require.Equal(t, int64(4000), got[0].CacheCreationTokens)
}

// TestDetectCacheInvalidations_IgnoresNonNativeCacheSupport proves the gate:
// implicit-cache providers report noisy CacheReadTokens and must never
// false-positive just because CacheSupport isn't native.
func TestDetectCacheInvalidations_IgnoresNonNativeCacheSupport(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	ctx := context.Background()

	sess, err := s.Create(ctx, "not native")
	require.NoError(t, err)

	warm, err := m.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "warm"}},
	})
	require.NoError(t, err)
	require.NoError(t, m.SetUsage(ctx, warm.ID, message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 5000, TotalTokens: 5010,
		Provider: "some-implicit", Model: "m", CacheSupport: message.CacheSupportNone,
	}))

	cold, err := m.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "cold"}},
	})
	require.NoError(t, err)
	require.NoError(t, m.SetUsage(ctx, cold.ID, message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 4000, TotalTokens: 4010,
		Provider: "some-implicit", Model: "m", CacheSupport: message.CacheSupportNone,
	}))

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Empty(t, got, "non-native cache support must never be flagged, even with the same token pattern")
}

// TestDetectCacheInvalidations_BelowFloorIsNotFlagged confirms the 2048
// floor: a session that was never meaningfully warm must not be reported as
// an invalidation.
func TestDetectCacheInvalidations_BelowFloorIsNotFlagged(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	ctx := context.Background()

	sess, err := s.Create(ctx, "never warm")
	require.NoError(t, err)

	first, err := m.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "first"}},
	})
	require.NoError(t, err)
	require.NoError(t, m.SetUsage(ctx, first.ID, message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 100, TotalTokens: 110,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	}))

	second, err := m.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "second"}},
	})
	require.NoError(t, err)
	require.NoError(t, m.SetUsage(ctx, second.ID, message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 200, TotalTokens: 210,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	}))

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Empty(t, got, "a prior read below the warm floor must not count as an invalidation")
}
