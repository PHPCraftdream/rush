package childagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// QuestionNotice identifies a held question in a specific child incarnation.
type QuestionNotice struct {
	Owner, ChildSessionID string
	NodeID                string
	Token                 uint64
	Depth                 int
	Sequence              uint64
	Question              string
}

type QuestionPublisher interface {
	PublishChildQuestion(context.Context, QuestionNotice) error
}

type questionPublishStore interface {
	BatchPublishQuestion(context.Context, string, string, int64, string, string) error
}

type coordinatorQuestionPublisher struct {
	store questionPublishStore
	wake  func(context.Context, string)
}

// NewQuestionPublisher installs notice persistence and the optional owner wake.
func NewQuestionPublisher(store questionPublishStore, wake func(context.Context, string)) QuestionPublisher {
	return coordinatorQuestionPublisher{store: store, wake: wake}
}

// PublishChildQuestion persists the notice before waking its owner.
func (p coordinatorQuestionPublisher) PublishChildQuestion(ctx context.Context, n QuestionNotice) error {
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

type questionResolveStore interface {
	BatchResolveChildQuestions(context.Context, string, string, string, int64, int64) (int64, error)
}

type storeQuestionResolver struct{ store questionResolveStore }

// NewQuestionResolver adapts the installed store to the execution-side resolver.
func NewQuestionResolver(store questionResolveStore) storeQuestionResolver {
	return storeQuestionResolver{store: store}
}

func (r storeQuestionResolver) ResolveChildQuestions(ctx context.Context, owner, node, child string, token, sequence int64) error {
	_, err := r.store.BatchResolveChildQuestions(ctx, owner, node, child, token, sequence)
	return err
}

// QuestionContextPort reads the installed held-answer binding from the call
// context. Its zero value must be usable so direct wrappers share the same reads.
type QuestionContextPort interface {
	AskFrom(context.Context) (func(context.Context, int, string) (json.RawMessage, error), bool)
	Depth(context.Context) int
	NoRouteError() error
}

// BuildQuestionTools strips asks when no answer route exists, or wraps them
// when a held-answer binding is present.
func BuildQuestionTools[P QuestionContextPort](list []fantasy.AgentTool, ctx context.Context) ([]fantasy.AgentTool, error) {
	if route, ok := batchmeta.BatchQuestionRouteFrom(ctx); ok && !route {
		return batchmeta.StripAskQuestionTools(list), nil
	}
	var port P
	if _, ok := port.AskFrom(ctx); !ok {
		return list, nil
	}
	wrapped := append([]fantasy.AgentTool(nil), list...)
	for i, tool := range wrapped {
		if tool.Info().Name == tools.AskQuestionToolName {
			wrapped[i] = AskQuestionTool[P]{AgentTool: tool}
		}
	}
	return wrapped, nil
}

// AskQuestionTool routes ask_question through the held-answer bridge.
type AskQuestionTool[P QuestionContextPort] struct{ fantasy.AgentTool }

func (t AskQuestionTool[P]) Info() fantasy.ToolInfo { return t.AgentTool.Info() }
func (t AskQuestionTool[P]) ProviderOptions() fantasy.ProviderOptions {
	return t.AgentTool.ProviderOptions()
}

func (t AskQuestionTool[P]) SetProviderOptions(p fantasy.ProviderOptions) {
	t.AgentTool.SetProviderOptions(p)
}

const noQuestionRoute = "ask_question is not available: this batch incarnation has no answer route (unattended batch); finish the work without asking"

func (t AskQuestionTool[P]) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// Deny stale wrappers before looking up a binding or invoking the raw tool.
	if allowed, ok := batchmeta.BatchQuestionRouteFrom(ctx); ok && !allowed {
		return fantasy.NewTextErrorResponse(noQuestionRoute), nil
	}
	var port P
	ask, ok := port.AskFrom(ctx)
	if !ok {
		return t.AgentTool.Run(ctx, call)
	}
	// Validate the full original schema, including fields the bridge doesn't use.
	var params tools.AskQuestionParams
	if err := json.Unmarshal([]byte(call.Input), &params); err != nil {
		return fantasy.NewTextErrorResponse("question must be valid JSON"), nil
	}
	question := strings.TrimSpace(params.Question)
	if question == "" {
		return fantasy.NewTextErrorResponse("question must not be empty"), nil
	}
	depth := port.Depth(ctx)
	if depth <= 0 {
		depth = 1
	}
	answer, err := ask(ctx, depth, question)
	if err != nil {
		if errors.Is(err, port.NoRouteError()) {
			return fantasy.NewTextErrorResponse(noQuestionRoute), nil
		}
		// A released hold cannot continue as an ordinary tool error.
		return fantasy.NewTextErrorResponse(err.Error()), err
	}
	var text string
	if json.Unmarshal(answer, &text) == nil {
		return fantasy.NewTextResponse(text), nil
	}
	return fantasy.NewTextResponse(string(answer)), nil
}
