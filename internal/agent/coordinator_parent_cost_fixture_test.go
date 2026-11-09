package agent

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/session"
)

// parentCostFailureSessions keeps lineage real while refusing parent charges.
type parentCostFailureSessions struct {
	session.Service
	session.AgentDepthStore
	parentID string
	attempts atomic.Int32
}

func (s *parentCostFailureSessions) IncrementCost(ctx context.Context, id string, delta float64) (session.Session, error) {
	if id == s.parentID {
		s.attempts.Add(1)
		return session.Session{}, errors.New("injected parent cost write failure")
	}
	return s.Service.IncrementCost(ctx, id, delta)
}
