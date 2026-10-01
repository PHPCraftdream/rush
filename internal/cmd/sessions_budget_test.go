// T13, list part (#1130): `sessions list` orders by delegation-subtree
// activity — a busy child lifts its root to the top without the root's own
// row ever changing.

package cmd

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type stubActivitySvc struct {
	session.Service
	active map[string]int64
}

func (s *stubActivitySvc) SubtreeUpdatedAt(_ context.Context, id string) (int64, error) {
	return s.active[id], nil
}

// Revert-check: flipping orderBySubtreeActivity's comparison (oldest-first)
// turns this test red.
func TestOrderBySubtreeActivity_BusyChildLiftsItsRoot(t *testing.T) {
	svc := &stubActivitySvc{active: map[string]int64{
		"quiet":  1_000_100, // the freshest OWN row — old ordering would put it first
		"active": 1_000_200, // max over its subtree: the child sits at 1_000_200
	}}
	sessions := []session.Session{
		{ID: "quiet", UpdatedAt: 1_000_100},
		{ID: "active", UpdatedAt: 1_000_000},
	}

	orderBySubtreeActivity(context.Background(), svc, sessions)

	require.Equal(t, "active", sessions[0].ID, "the root with the busy child is listed first")
	require.Equal(t, "quiet", sessions[1].ID)
}
