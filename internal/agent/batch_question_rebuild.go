package agent

import (
	"context"
	"fmt"
)

// A rebuilt delegation has no live answer binding.
func (c *coordinator) rebuiltChildQuestionState(ctx context.Context, sessionID string) (*batchChildQuestionState, error) {
	return (&batchQuestionRebuildComponent{host: c}).rebuiltChildQuestionState(ctx, sessionID)
}

type batchQuestionRebuildHost interface {
	isDurableDelegationChild(ctx context.Context, sessionID string) (bool, error)
}

type batchQuestionRebuildComponent struct{ host batchQuestionRebuildHost }

func (w *batchQuestionRebuildComponent) rebuiltChildQuestionState(ctx context.Context, sessionID string) (*batchChildQuestionState, error) {
	delegated, err := w.host.isDurableDelegationChild(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("resolve rebuilt question policy: %w", err)
	}
	if !delegated {
		return nil, nil
	}
	return &batchChildQuestionState{Route: false}, nil
}
