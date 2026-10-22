package agent

import (
	"context"
	"sort"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// IsHardQuotaLimit identifies a provider's hard usage wall, not a retryable 429.
func IsHardQuotaLimit(err error) bool {
	return classifyHardProviderLimit(err, "", time.Now()) == providerLimitHard
}

// CLIActiveWorkSource reads real work without pulling notices or admitting turns.
type CLIActiveWorkSource interface {
	CLIActiveWork(context.Context, string) (bool, error)
}

// CLIActiveWork excludes schedules, debt, and question-paused delegations, but
// includes those children's jobs and completion-persistence holds. Batch-only:
// the live scope comes from batch_nodes (the node-backed fact source bound at
// install). An UNBOUND coordinator (no bound facts) answers false: no batch
// runtime means no batch work can be pending for it. A failed read fails
// closed with the error (the quota loop retries it under its bounded window,
// never silently reads "idle").
func (c *coordinator) CLIActiveWork(ctx context.Context, root string) (bool, error) {
	facts := coordinatorBatchCutoverFacts(c)
	if _, unbound := facts.(unknownBatchCutoverFacts); unbound {
		return false, nil
	}
	return c.cliActiveWorkBatch(ctx, facts, root)
}

// IsAwaitingAnswerFinish identifies durable question-stop metadata.
func IsAwaitingAnswerFinish(fp *message.Finish) bool {
	_, ok := subAgentQuestionBlockFromFinish(fp)
	return ok
}

var _ CLIActiveWorkSource = (*coordinator)(nil)

// cliActiveWorkBatch is CLIActiveWork's flag-ON body: the live scope comes
// from the bound node-backed facts, and a question-paused delegation whose
// parent child_question notice is durably committed no longer keeps the CLI
// active (the OFF branch's exclusion, mirrored through batch facts). A
// failed read fails closed with the error.
func (c *coordinator) cliActiveWorkBatch(ctx context.Context, facts BatchCutoverFacts, root string) (bool, error) {
	// Scope-budgeted recovery (one
	// 10ms budget, one candidate root attempt): recover the dead hosts this
	// read is about to count as live, then answer from fresh reads below.
	// Best-effort; exhaustion keeps the conservative active answer.
	recoverBatchScopeDeadCandidates(ctx, c, root)
	own, descendants, err := facts.LiveWorkSummary(ctx, root)
	if err != nil {
		return false, err
	}
	if own+descendants > 0 {
		active, err := c.batchActiveAfterQuestionPause(ctx, facts, root)
		if err != nil {
			return false, err
		}
		if active {
			return true, nil
		}
	}
	if c.background != nil && (c.background.ActiveOwned(root) > 0 || c.background.PendingCompletionsOwned(root) > 0) {
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

// batchActiveAfterQuestionPause answers the live-scope question with the
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
func (c *coordinator) batchActiveAfterQuestionPause(ctx context.Context, facts BatchCutoverFacts, root string) (bool, error) {
	tree, err := facts.LiveNodes(ctx, root)
	if err != nil {
		return false, err
	}
	delegationNodes, err := facts.LiveDelegationNodes(ctx, root)
	if err != nil {
		return false, err
	}
	if c.messages == nil {
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
		if ag := c.agentFor(dc.child); ag != nil && ag.IsSessionBusy(dc.child) {
			st.busy = true
		}
		msgs, err := c.messages.List(ctx, dc.child)
		if err != nil {
			return false, err
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != message.Assistant || !msgs[i].IsFinished() {
				continue
			}
			_, st.questionPaused = subAgentQuestionFromFinish(dc.child, msgs[i].FinishPart())
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
	committed, err := c.batchCommittedQuestionChildren(ctx, facts, paused)
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
			if dormant[otherChild] && batchNodeStateLive(otherState.node.State) && otherState.node.Kind == "task" {
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
		if dormant[child] && batchNodeStateLive(st.node.State) && st.node.Kind == "task" {
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

// batchCommittedQuestionChildren reports which question-paused children have
// their question durably committed under their IMMEDIATE owner (paused's
// owner field, not the CLI root). Marker-format notices commit ONLY through
// LiveHeldQuestions' fenced rows -- each row now carries its marker's child
// session, so a stale historical marker for the same child can never commit a
// current incarnation (LiveHeldQuestions filters dead node states and token
// mismatches). Legacy-frame notices carry no marker and are text-matched over
// the shared retained session_notices table under the child's own owner,
// exactly as the OFF branch matches them; a failed per-owner read keeps that
// owner's children active (conservative).
func (c *coordinator) batchCommittedQuestionChildren(ctx context.Context, facts BatchCutoverFacts, paused map[string]batchPausedChild) (map[string]bool, error) {
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
