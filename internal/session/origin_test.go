// Round-trip tests for the sessions.origin column: the explicit-origin
// creators (CreateWithOrigin / CreateWithIDAndOrigin) persist the entry
// channel, while the legacy creators (Create / CreateWithID) leave it as
// OriginUnspecified via the column's empty-string default.
package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/message"
)

func TestCreateWithOriginRoundTrip(t *testing.T) {
	t.Parallel()

	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	oc, ok := svc.(OriginCreator)
	require.True(t, ok, "*service must implement OriginCreator")

	s, err := oc.CreateWithOrigin(ctx, "cli session", message.OriginCLI)
	require.NoError(t, err)
	assert.Equal(t, message.OriginCLI, s.Origin)

	got, err := svc.Get(ctx, s.ID)
	require.NoError(t, err)
	assert.Equal(t, message.OriginCLI, got.Origin)

	list, err := svc.List(ctx)
	require.NoError(t, err)
	var found *Session
	for i := range list {
		if list[i].ID == s.ID {
			found = &list[i]
		}
	}
	require.NotNil(t, found, "session must appear in List")
	assert.Equal(t, message.OriginCLI, found.Origin)
}

func TestCreateWithIDAndOriginRoundTrip(t *testing.T) {
	t.Parallel()

	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	oc, ok := svc.(OriginCreator)
	require.True(t, ok, "*service must implement OriginCreator")

	s, err := oc.CreateWithIDAndOrigin(ctx, "web-fixed-id", "web session", message.OriginWeb)
	require.NoError(t, err)
	assert.Equal(t, "web-fixed-id", s.ID)
	assert.Equal(t, message.OriginWeb, s.Origin)

	got, err := svc.Get(ctx, "web-fixed-id")
	require.NoError(t, err)
	assert.Equal(t, message.OriginWeb, got.Origin)
}

func TestCreateLeavesOriginUnspecified(t *testing.T) {
	t.Parallel()

	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	s, err := svc.Create(ctx, "legacy")
	require.NoError(t, err)
	assert.Equal(t, message.OriginUnspecified, s.Origin)

	got, err := svc.Get(ctx, s.ID)
	require.NoError(t, err)
	assert.Equal(t, message.OriginUnspecified, got.Origin)

	withID, err := svc.CreateWithID(ctx, "legacy-with-id", "legacy via id")
	require.NoError(t, err)
	assert.Equal(t, message.OriginUnspecified, withID.Origin)

	gotWithID, err := svc.Get(ctx, "legacy-with-id")
	require.NoError(t, err)
	assert.Equal(t, message.OriginUnspecified, gotWithID.Origin)
}
