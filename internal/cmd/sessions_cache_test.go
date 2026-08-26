package cmd

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// backdateMessage sets a message's created_at directly in the DB (stored in
// seconds), since the public message.Service API always stamps "now" and
// exposes no way to inject an artificial timestamp for tests.
func backdateMessage(t *testing.T, conn *sql.DB, messageID string, age time.Duration) {
	t.Helper()
	createdAt := time.Now().Add(-age).Unix()
	_, err := conn.ExecContext(t.Context(), "UPDATE messages SET created_at = ? WHERE id = ?", createdAt, messageID)
	require.NoError(t, err)
}

// mkAssistant creates an assistant message with the given usage.
func mkAssistant(t *testing.T, ctx context.Context, m message.Service, sessionID, text string, usage message.TokenUsage) message.Message {
	t.Helper()
	msg, err := m.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	require.NoError(t, m.SetUsage(ctx, msg.ID, usage))
	return msg
}

// mkNoUsage creates a user/tool message that never gets a TokenUsage - this
// is what real interleaving between assistant turns looks like.
func mkNoUsage(t *testing.T, ctx context.Context, m message.Service, sessionID string, role message.MessageRole, text string) message.Message {
	t.Helper()
	msg, err := m.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  role,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	return msg
}

// TestDetectCacheInvalidations_FlagsWarmToColdTransition covers the P2
// warm->cold detector across a REALISTIC message sequence: assistant turns
// are interleaved with user/tool messages that carry no usage. This is the
// regression proof - it fails against a detector that resets prev on every
// nil-usage message instead of skipping them.
func TestDetectCacheInvalidations_FlagsWarmToColdTransition(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	ctx := context.Background()

	sess, err := s.Create(ctx, "warm to cold")
	require.NoError(t, err)

	mkAssistant(t, ctx, m, sess.ID, "warm", message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 5000, TotalTokens: 5010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})
	mkNoUsage(t, ctx, m, sess.ID, message.Tool, "tool_result")
	mkNoUsage(t, ctx, m, sess.ID, message.User, "next question")
	cold := mkAssistant(t, ctx, m, sess.ID, "cold", message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 4000, TotalTokens: 4010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Len(t, got, 1)
	require.Equal(t, cold.ID, got[0].MessageID)
	require.Equal(t, int64(4000), got[0].CacheCreationTokens)
}

// TestDetectCacheInvalidations_ModelSwitchNotFlagged proves a mid-session
// model/provider switch is never flagged: switching legitimately cold-starts
// a different cache and is not an invalidation.
func TestDetectCacheInvalidations_ModelSwitchNotFlagged(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	ctx := context.Background()

	sess, err := s.Create(ctx, "model switch")
	require.NoError(t, err)

	mkAssistant(t, ctx, m, sess.ID, "warm", message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 5000, TotalTokens: 5010,
		Provider: "anthropic", Model: "claude-a", CacheSupport: message.CacheSupportNative,
	})
	mkNoUsage(t, ctx, m, sess.ID, message.Tool, "tool_result")
	mkNoUsage(t, ctx, m, sess.ID, message.User, "next question")
	mkAssistant(t, ctx, m, sess.ID, "cold", message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 4000, TotalTokens: 4010,
		Provider: "anthropic", Model: "claude-b", CacheSupport: message.CacheSupportNative,
	})

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Empty(t, got, "a model switch must never be flagged as a cache invalidation")
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

	mkAssistant(t, ctx, m, sess.ID, "warm", message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 5000, TotalTokens: 5010,
		Provider: "some-implicit", Model: "m", CacheSupport: message.CacheSupportNone,
	})
	mkNoUsage(t, ctx, m, sess.ID, message.Tool, "tool_result")
	mkNoUsage(t, ctx, m, sess.ID, message.User, "next question")
	mkAssistant(t, ctx, m, sess.ID, "cold", message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 4000, TotalTokens: 4010,
		Provider: "some-implicit", Model: "m", CacheSupport: message.CacheSupportNone,
	})

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Empty(t, got, "non-native cache support must never be flagged, even with the same token pattern")
}

// TestDetectCacheInvalidations_ShortGapIsNotLikelyTTLExpiry proves the
// default classification: a warm->cold transition with only a few seconds
// between the two turns (the shape every other test in this file already
// produces, back-to-back message creation) is NOT flagged as likely TTL
// expiry — it stays a genuine invalidation candidate.
func TestDetectCacheInvalidations_ShortGapIsNotLikelyTTLExpiry(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	ctx := context.Background()

	sess, err := s.Create(ctx, "short gap")
	require.NoError(t, err)

	mkAssistant(t, ctx, m, sess.ID, "warm", message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 5000, TotalTokens: 5010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})
	mkNoUsage(t, ctx, m, sess.ID, message.Tool, "tool_result")
	cold := mkAssistant(t, ctx, m, sess.ID, "cold", message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 4000, TotalTokens: 4010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Len(t, got, 1)
	require.Equal(t, cold.ID, got[0].MessageID)
	require.False(t, got[0].LikelyTTLExpiry,
		"a short gap between turns must not be classified as TTL expiry")
}

// TestDetectCacheInvalidations_LongGapIsLikelyTTLExpiry proves the new
// classification: a warm->cold transition preceded by a gap at or beyond
// likelyTTLExpiryThreshold (5 minutes, the real Anthropic-style cache TTL)
// is STILL reported (never silently dropped) but flagged as likely TTL
// expiry rather than a genuine prefix-change invalidation.
func TestDetectCacheInvalidations_LongGapIsLikelyTTLExpiry(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	ctx := context.Background()

	sess, err := s.Create(ctx, "long gap")
	require.NoError(t, err)

	warm := mkAssistant(t, ctx, m, sess.ID, "warm", message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 5000, TotalTokens: 5010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})
	backdateMessage(t, conn, warm.ID, 10*time.Minute)
	mkNoUsage(t, ctx, m, sess.ID, message.Tool, "tool_result")
	cold := mkAssistant(t, ctx, m, sess.ID, "cold", message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 4000, TotalTokens: 4010,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Len(t, got, 1, "a long-gap transition must still be reported, not silently dropped")
	require.Equal(t, cold.ID, got[0].MessageID)
	require.True(t, got[0].LikelyTTLExpiry,
		"a gap past the TTL threshold must be classified as likely TTL expiry")
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

	mkAssistant(t, ctx, m, sess.ID, "first", message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 100, TotalTokens: 110,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})
	mkNoUsage(t, ctx, m, sess.ID, message.Tool, "tool_result")
	mkNoUsage(t, ctx, m, sess.ID, message.User, "next question")
	mkAssistant(t, ctx, m, sess.ID, "second", message.TokenUsage{
		InputTokens: 10, CacheCreationTokens: 200, TotalTokens: 210,
		Provider: "anthropic", Model: "claude", CacheSupport: message.CacheSupportNative,
	})

	msgs, err := m.List(ctx, sess.ID)
	require.NoError(t, err)

	got := detectCacheInvalidations(msgs)
	require.Empty(t, got, "a prior read below the warm floor must not count as an invalidation")
}
