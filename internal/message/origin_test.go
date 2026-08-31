// Round-trip tests for the messages.origin column: CreateMessageParams
// carries the entry-channel origin and it must persist, while params
// without an Origin read back as OriginUnspecified via the column's
// DEFAULT ''.
package message

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateMessageOriginRoundTrip(t *testing.T) {
	t.Parallel()

	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	sessionID := "test-origin-session"

	msg, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:   User,
		Parts:  []ContentPart{TextContent{Text: "hi"}},
		Origin: OriginSDK,
	})
	require.NoError(t, err)
	assert.Equal(t, OriginSDK, msg.Origin)

	got, err := svc.Get(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, OriginSDK, got.Origin)

	list, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, OriginSDK, list[0].Origin)
}

func TestCreateMessageOriginDefaultsUnspecified(t *testing.T) {
	t.Parallel()

	_, q := newTestMessageDB(t)
	svc := NewService(q)
	ctx := t.Context()
	sessionID := "test-origin-default-session"

	msg, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  User,
		Parts: []ContentPart{TextContent{Text: "no origin"}},
	})
	require.NoError(t, err)
	assert.Equal(t, OriginUnspecified, msg.Origin)

	got, err := svc.Get(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, OriginUnspecified, got.Origin)
}
