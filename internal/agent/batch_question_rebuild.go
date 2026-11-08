package agent

import (
	"context"
	"fmt"
)

// A rebuilt delegation has no live answer binding.
func (c *coordinator) rebuiltChildQuestionState(ctx context.Context, sessionID string) (*batchChildQuestionState, error) {
	delegated, err := c.isDurableDelegationChild(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("resolve rebuilt question policy: %w", err)
	}
	if !delegated {
		return nil, nil
	}
	return &batchChildQuestionState{Route: false}, nil
}
