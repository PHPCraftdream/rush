package agent

import (
	"context"
	"encoding/json"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/session"
)

type (
	batchChildQuestionNotice    = childagent.QuestionNotice
	batchChildQuestionPublisher = childagent.QuestionPublisher
	batchAgentAskQuestionTool   = childagent.AskQuestionTool[batchQuestionContextPort]
)

// The store literals remain compatible with root callers and fixtures. The
// runtime adapters own question publication and resolution.
type batchCoordinatorQuestionPublisher struct {
	store *session.BatchStore
	wake  func(context.Context, string)
}

func (p batchCoordinatorQuestionPublisher) PublishChildQuestion(ctx context.Context, n batchChildQuestionNotice) error {
	return newBatchQuestionPublisher(p.store, p.wake).PublishChildQuestion(ctx, n)
}

func newBatchQuestionPublisher(store *session.BatchStore, wake func(context.Context, string)) batchChildQuestionPublisher {
	if store == nil {
		return childagent.NewQuestionPublisher(nil, wake)
	}
	return childagent.NewQuestionPublisher(store, wake)
}

type batchStoreQuestionResolver struct{ store *session.BatchStore }

func (r batchStoreQuestionResolver) ResolveChildQuestions(ctx context.Context, owner, node, child string, token, sequence int64) error {
	return childagent.NewQuestionResolver(r.store).ResolveChildQuestions(ctx, owner, node, child, token, sequence)
}

func buildBatchQuestionTools(list []fantasy.AgentTool, ctx context.Context) ([]fantasy.AgentTool, error) {
	return childagent.BuildQuestionTools[batchQuestionContextPort](list, ctx)
}

type batchQuestionContextPort struct{}

func (batchQuestionContextPort) AskFrom(ctx context.Context) (func(context.Context, int, string) (json.RawMessage, error), bool) {
	binding, ok := batchAgentQuestionBindingFrom(ctx)
	return binding.Ask, ok
}

func (batchQuestionContextPort) Depth(ctx context.Context) int { return batchAgentQuestionDepth(ctx) }
func (batchQuestionContextPort) NoRouteError() error           { return ErrBatchQuestionNoRoute }
