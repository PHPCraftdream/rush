package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
)

type batchChildQuestionNotice struct {
	Owner, ChildSessionID string
	NodeID                string
	Token                 uint64
	Depth                 int
	Sequence              uint64
	Question              string
}
type batchChildQuestionPublisher interface {
	PublishChildQuestion(context.Context, batchChildQuestionNotice) error
}
type batchCoordinatorQuestionPublisher struct {
	store *session.BatchStore
	wake  func(context.Context, string)
}

func (p batchCoordinatorQuestionPublisher) PublishChildQuestion(ctx context.Context, n batchChildQuestionNotice) error {
	if p.store == nil {
		return errors.New("batch question: publisher unavailable")
	}
	if n.Owner == "" || n.ChildSessionID == "" || n.NodeID == "" || n.Token == 0 || n.Depth <= 0 || n.Sequence == 0 {
		return errors.New("batch question: incomplete identity")
	}
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n", n.NodeID, n.Token, n.Sequence, n.Depth, n.ChildSessionID)
	text := marker + "SUB-AGENT QUESTION: " + n.Question + "\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	if err := p.store.BatchPublishQuestion(ctx, n.Owner, n.NodeID, int64(n.Token), marker, text); err != nil {
		return err
	}
	if p.wake != nil {
		p.wake(ctx, n.Owner)
	}
	return nil
}

func buildBatchQuestionTools(list []fantasy.AgentTool, ctx context.Context) ([]fantasy.AgentTool, error) {
	if _, ok := batchAgentQuestionBindingFrom(ctx); !ok {
		return list, nil
	}
	wrapped := append([]fantasy.AgentTool(nil), list...)
	for i, tool := range wrapped {
		if tool.Info().Name == tools.AskQuestionToolName {
			wrapped[i] = batchAgentAskQuestionTool{AgentTool: tool}
		}
	}
	return wrapped, nil
}

type batchAgentAskQuestionTool struct{ fantasy.AgentTool }

func (t batchAgentAskQuestionTool) Info() fantasy.ToolInfo { return t.AgentTool.Info() }
func (t batchAgentAskQuestionTool) ProviderOptions() fantasy.ProviderOptions {
	return t.AgentTool.ProviderOptions()
}

func (t batchAgentAskQuestionTool) SetProviderOptions(p fantasy.ProviderOptions) {
	t.AgentTool.SetProviderOptions(p)
}

func (t batchAgentAskQuestionTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	binding, ok := batchAgentQuestionBindingFrom(ctx)
	if !ok {
		return t.AgentTool.Run(ctx, call)
	}
	var params tools.AskQuestionParams
	if err := json.Unmarshal([]byte(call.Input), &params); err != nil {
		return fantasy.NewTextErrorResponse("question must be valid JSON"), nil
	}
	question := strings.TrimSpace(params.Question)
	if question == "" {
		return fantasy.NewTextErrorResponse("question must not be empty"), nil
	}
	depth := batchAgentQuestionDepth(ctx)
	if depth <= 0 {
		depth = 1
	}
	answer, err := binding.Ask(ctx, depth, question)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	var text string
	if json.Unmarshal(answer, &text) == nil {
		return fantasy.NewTextResponse(text), nil
	}
	return fantasy.NewTextResponse(string(answer)), nil
}
