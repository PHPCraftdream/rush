package clibridge

import (
	"context"
	"sort"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// IsHardQuotaLimit identifies a provider's hard usage wall, not a retryable 429.
func IsHardQuotaLimit(err error) bool {
	return usage.IsHardQuotaLimit(err)
}

// CLIActiveWorkSource reads real work without pulling notices or admitting turns.
type CLIActiveWorkSource interface {
	CLIActiveWork(context.Context, string) (bool, error)
}

// IsAwaitingAnswerFinish identifies durable question-stop metadata.
func IsAwaitingAnswerFinish(fp *message.Finish) bool {
	_, ok := agentcall.SubAgentQuestionBlockFromFinish(fp)
	return ok
}

type cliActiveWorkHost interface {
	BatchCutoverFacts() agentcall.BatchCutoverFacts
	RecoverBatchScopeDeadCandidates(ctx context.Context, owner string)
	BackgroundOwnsWork(root string) bool
	CLIActiveWorkMessages() message.Service
	ChildSessionBusy(child string) bool
	UnboundBatchFacts(facts agentcall.BatchCutoverFacts) bool
	SubAgentQuestionBlockFromFinish(fp *message.Finish) (string, bool)
	SubAgentQuestionFromFinish(childSessionID string, fp *message.Finish) (string, bool)
	BatchNodeStateLive(state string) bool
}

type CliActiveWorkComponent struct{ host cliActiveWorkHost }

func NewCliActiveWorkComponent(host cliActiveWorkHost) *CliActiveWorkComponent {
	return &CliActiveWorkComponent{host: host}
}

func (w *CliActiveWorkComponent) DoCLIActiveWork(ctx context.Context, root string) (bool, error) {
	facts := w.host.BatchCutoverFacts()
	if w.host.UnboundBatchFacts(facts) {
		return false, nil
	}
	return w.DoCLIActiveWorkBatch(ctx, facts, root)
}

func (w *CliActiveWorkComponent) DoCLIActiveWorkBatch(ctx context.Context, facts agentcall.BatchCutoverFacts, root string) (bool, error) {
	// Scope-budgeted recovery (one
	// 500ms budget, one candidate root attempt): recover the dead hosts this
	// read is about to count as live, then answer from fresh reads below.
	// Best-effort; exhaustion keeps the conservative active answer.
	w.host.RecoverBatchScopeDeadCandidates(ctx, root)
	own, descendants, err := facts.LiveWorkSummary(ctx, root)
	if err != nil {
		return false, err
	}
	if own+descendants > 0 {
		active, err := w.DoBatchActiveAfterQuestionPause(ctx, facts, root)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
	}
	if w.host.BackgroundOwnsWork(root) {
		return true, nil
	}
	return false, nil
}

// batchPausedChild carries one question-paused delegation child's commit
// identity: the delegation root's tool call, and the IMMEDIATE owner session
// whose scope contains the delegation -- the publisher stores the question
// under that session (batch_exec_agent.go), never under the CLI root.
type batchPausedChild struct {
	toolCallID string
	owner      string
	nodeID     string
	busy       bool
}

// BatchActiveAfterQuestionPause answers the live-scope question with the
// question-pause exclusion applied at NODE granularity. A delegation child is
// dormant when its question is paused AND its committed notice is fenced to
// the CURRENT incarnation (the seam's LiveHeldQuestions rows), or when it is
// a pure conduit whose entire own live task scope consists of dormant nested
// delegations. A batch root is excluded only when EVERY one of its live task
// nodes (root counts, batch_read.go) is a dormant live delegation -- a
// running command sibling always keeps it, and the whole answer, active.
// Dormancy resolves deepest-first: batchLiveReplay appends Descendants in
// depth order (batch_read_live.go), so a conduit is classified after its
// nested children.
func (w *CliActiveWorkComponent) DoBatchActiveAfterQuestionPause(ctx context.Context, facts agentcall.BatchCutoverFacts, root string) (bool, error) {
	tree, err := facts.LiveNodes(ctx, root)
	if err != nil {
		return false, err
	}
	delegationNodes, err := facts.LiveDelegationNodes(ctx, root)
	if err != nil {
		return false, err
	}
	if w.host.CLIActiveWorkMessages() == nil {
		// No history reader: a question-paused child cannot be proven, so
		// every live node stays active (the fail-closed answer).
		return true, nil
	}
	liveByRoot := make(map[string]int, len(tree))
	for _, node := range tree {
		// Pending tasks are scheduler state (sequential / concurrency
		// limited), not yet Active: they are live work all the same.
		liveByRoot[node.NodeID] = node.Counts.Active + node.Counts.Pending
	}
	type delegationChild struct {
		node  session.BatchLiveNode
		child string
	}
	children := make([]delegationChild, 0, len(delegationNodes))
	for _, node := range delegationNodes {
		if node.ChildSessionID != "" {
			children = append(children, delegationChild{node: node, child: node.ChildSessionID})
		}
	}
	ordered := make([]delegationChild, len(children))
	copy(ordered, children)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].node.Depth > ordered[j].node.Depth })
	type childState struct {
		node           session.BatchLiveNode
		busy           bool
		questionPaused bool
	}
	states := make(map[string]*childState, len(ordered))
	order := make([]string, 0, len(ordered)) // deepest-first, each child once
	for _, dc := range ordered {
		if _, done := states[dc.child]; done {
			continue
		}
		st := &childState{node: dc.node}
		if w.host.ChildSessionBusy(dc.child) {
			st.busy = true
		}
		msgs, err := w.host.CLIActiveWorkMessages().List(ctx, dc.child)
		if err != nil {
			return false, err
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != message.Assistant || !msgs[i].IsFinished() {
				continue
			}
			_, st.questionPaused = w.host.SubAgentQuestionFromFinish(dc.child, msgs[i].FinishPart())
			break
		}
		if st.busy {
			st.questionPaused = true
		}
		states[dc.child] = st
		order = append(order, dc.child)
	}
	paused := make(map[string]batchPausedChild, len(states))
	for child, st := range states {
		if st.questionPaused {
			paused[child] = batchPausedChild{toolCallID: st.node.ToolCallID, owner: st.node.SessionID, nodeID: st.node.NodeID, busy: st.busy}
		}
	}
	committed, err := w.batchCommittedQuestionChildren(ctx, facts, paused)
	if err != nil {
		return false, err
	}
	dormant := make(map[string]bool, len(states))
	for _, child := range order {
		st := states[child]
		if st.busy && !committed[child] {
			continue
		}
		if committed[child] {
			dormant[child] = true
			continue
		}
		nestedNodes, nestedDormant := 0, 0
		for otherChild, otherState := range states {
			if otherState.node.SessionID != child {
				continue
			}
			nestedNodes++
			if dormant[otherChild] && w.host.BatchNodeStateLive(otherState.node.State) && otherState.node.Kind == "task" {
				nestedDormant++
			}
		}
		ownLive := 0
		for _, rootNode := range tree {
			if rootNode.SessionID == child {
				ownLive += rootNode.Counts.Active + rootNode.Counts.Pending
			}
		}
		if nestedNodes > 0 && ownLive > 0 && ownLive == nestedDormant {
			// Pure conduit: the child's whole live task scope is dormant
			// nested delegations, so it holds no work of its own.
			dormant[child] = true
		}
	}
	dormantByRoot := make(map[string]int)
	for child, st := range states {
		if dormant[child] && w.host.BatchNodeStateLive(st.node.State) && st.node.Kind == "task" {
			dormantByRoot[st.node.Ref.RootID]++
		}
	}
	excludedRoots := map[string]bool{}
	for rootID, live := range liveByRoot {
		if live > 0 && live == dormantByRoot[rootID] {
			excludedRoots[rootID] = true
		}
	}
	for _, node := range tree {
		if !excludedRoots[node.NodeID] {
			return true, nil
		}
	}
	return false, nil
}

// BatchCommittedQuestionChildren reports which question-paused children have
// their question durably committed under their IMMEDIATE owner (paused's
// owner field, not the CLI root). Marker-format notices commit ONLY through
// LiveHeldQuestions' fenced rows -- each row now carries its marker's child
// session, so a stale historical marker for the same child can never commit a
// current incarnation (LiveHeldQuestions filters dead node states and token
// mismatches).
func (w *CliActiveWorkComponent) batchCommittedQuestionChildren(ctx context.Context, facts agentcall.BatchCutoverFacts, paused map[string]batchPausedChild) (map[string]bool, error) {
	committed := make(map[string]bool, len(paused))
	byOwner := make(map[string][]string)
	for child, p := range paused {
		byOwner[p.owner] = append(byOwner[p.owner], child)
	}
	for owner := range byOwner {
		held, herr := facts.LiveHeldQuestions(ctx, owner)
		if herr != nil {
			// Unknown debt for this owner: its children stay active.
			continue
		}
		for _, row := range held {
			for child, p := range paused {
				if p.owner == owner && row.ChildSessionID == child && row.NodeID == p.nodeID {
					committed[child] = true
				}
			}
		}
	}
	return committed, nil
}
