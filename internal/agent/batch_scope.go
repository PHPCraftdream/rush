package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
)

// batchAgentScope preserves the root call surface; childagent owns all state.
type batchAgentScope struct {
	state *childagent.Scope
}

type batchAgentInjectMessage = batchinst.AgentInjectMessage

var (
	ErrBatchAgentNotHeld                 = batchinst.ErrBatchAgentNotHeld
	ErrBatchAgentStopped                 = batchinst.ErrBatchAgentStopped
	ErrBatchAgentStaleGeneration         = batchinst.ErrBatchAgentStaleGeneration
	ErrBatchAgentQuestionHeld            = batchinst.ErrBatchAgentQuestionHeld
	ErrBatchQuestionNoRoute              = batchinst.ErrBatchQuestionNoRoute
	ErrBatchQuestionCapabilityNotDurable = batchinst.ErrBatchQuestionCapabilityNotDurable
)

func newBatchAgentScope() *batchAgentScope {
	return &batchAgentScope{state: childagent.NewScope()}
}

func (s *batchAgentScope) HeldDepth() (int, bool) { return s.state.HeldDepth() }
func (s *batchAgentScope) PublishQuestion(publish func(sequence uint64) error) error {
	return s.state.PublishQuestion(publish)
}

func (s *batchAgentScope) beginStop() { s.state.BeginStop() }
func (s *batchAgentScope) stop() bool { return s.state.Stop() }
func (s *batchAgentScope) hold(depth int) (*childagent.HeldQuestion, error) {
	return s.state.Hold(depth)
}
func (s *batchAgentScope) discardHold(h *childagent.HeldQuestion) { s.state.DiscardHold(h) }
func (s *batchAgentScope) deliverWithCommit(msg batchAgentInjectMessage, commit func() error) error {
	return s.state.DeliverWithCommit(msg, commit)
}
