package agent

import (
	"context"
	"errors"
	"sort"
	"strings"
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
// includes those children's jobs and completion-persistence holds.
func (c *coordinator) CLIActiveWork(ctx context.Context, root string) (bool, error) {
	// Flag ON: the live scope comes from batch_nodes (the node-backed fact
	// source bound at install), never from the ledger. An unbound flag-ON
	// coordinator keeps the legacy path below — no batch runtime means no
	// batch work can be pending for it. A failed read fails closed with the
	// error (the quota loop retries it under its bounded window, never
	// silently reads "idle").
	if facts := coordinatorBatchCutoverFacts(c); facts != nil {
		if _, unbound := facts.(unknownBatchCutoverFacts); !unbound {
			return c.cliActiveWorkBatch(ctx, facts, root)
		}
	}
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return false, errors.New("active work store unavailable")
	}
	c.asyncJobs.store.RecoverOwnerScope(ctx, root, c.messages)
	owners := map[string]bool{root: true}
	tree, treeIncomplete := c.asyncJobs.store.JobsInTree(ctx, root)
	if treeIncomplete {
		return false, errors.New("work tree read incomplete")
	}
	for _, row := range tree {
		owners[row.OwnerSessionID] = true
		if row.ChildSessionID.Valid {
			owners[row.ChildSessionID.String] = true
		}
	}
	active := false
	for owner := range owners {
		if c.background != nil && (c.background.ActiveOwned(owner) > 0 || c.background.PendingCompletionsOwned(owner) > 0) {
			active = true
		}
	}
	jobs, incomplete := c.asyncJobs.store.LiveJobs(ctx, root)
	if incomplete {
		return false, errors.New("active work read incomplete")
	}
	for _, job := range jobs {
		owners[job.SessionID] = true
		if job.ChildSessionID == "" {
			active = true
			continue
		}
		owners[job.ChildSessionID] = true
		// Read durable finish metadata even if the question's delegation is no
		// longer in this process's ledger. A busy child is never paused.
		ag := c.agentFor(job.ChildSessionID)
		if ag != nil && ag.IsSessionBusy(job.ChildSessionID) {
			active = true
			continue
		}
		msgs, err := c.messages.List(ctx, job.ChildSessionID)
		if err != nil {
			return false, err
		}
		paused := false
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != message.Assistant || !msgs[i].IsFinished() {
				continue
			}
			_, paused = subAgentQuestionFromFinish(job.ChildSessionID, msgs[i].FinishPart())
			break
		}
		if !paused {
			active = true
		} else {
			// A held question is announced asynchronously by the existing ledger
			// recheck. Do not release the CLI before that parent notice commits.
			c.asyncJobs.recheckChild(job.ChildSessionID)
			if c.asyncJobs.heldQuestionJob(job.ChildSessionID) != nil {
				notices, err := c.asyncJobs.store.ListSessionNotices(ctx, job.SessionID)
				if err != nil {
					return false, err
				}
				persisted := false
				for _, notice := range notices {
					if notice.Kind == "child_question" && strings.Contains(notice.Text, "SUB-AGENT QUESTION (session "+job.ChildSessionID+")") && strings.Contains(notice.Text, "job "+job.ToolCallID+" stays open") {
						persisted = true
						break
					}
				}
				if !persisted {
					active = true
				}
			}
		}
	}
	return active, nil
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
		} else {
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
		}
		states[dc.child] = st
		order = append(order, dc.child)
	}
	paused := make(map[string]batchPausedChild, len(states))
	for child, st := range states {
		if st.questionPaused {
			paused[child] = batchPausedChild{toolCallID: st.node.ToolCallID, owner: st.node.SessionID}
		}
	}
	committed, err := c.batchCommittedQuestionChildren(ctx, facts, paused)
	if err != nil {
		return false, err
	}
	dormant := make(map[string]bool, len(states))
	for _, child := range order {
		st := states[child]
		if st.busy {
			continue // a busy child is never dormant
		}
		if st.questionPaused && committed[child] {
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
	unknownOwner := make(map[string]bool)
	byOwner := make(map[string][]string)
	for child, p := range paused {
		byOwner[p.owner] = append(byOwner[p.owner], child)
	}
	for owner, children := range byOwner {
		held, herr := facts.LiveHeldQuestions(ctx, owner)
		if herr != nil {
			// Unknown debt for this owner: its children stay active, and the
			// legacy fallback below must not decide for them either.
			unknownOwner[owner] = true
			continue
		}
		heldChildren := make(map[string]bool, len(held))
		for _, row := range held {
			if row.ChildSessionID != "" {
				heldChildren[row.ChildSessionID] = true
			}
		}
		for _, child := range children {
			if heldChildren[child] {
				committed[child] = true
			}
		}
	}
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return committed, nil
	}
	for child, p := range paused {
		if committed[child] || unknownOwner[p.owner] {
			continue
		}
		notices, err := c.asyncJobs.store.ListSessionNotices(ctx, p.owner)
		if err != nil {
			return nil, err
		}
		for _, notice := range notices {
			if notice.Kind != session.NoticeKindChildQuestion {
				continue
			}
			// A batch-marker notice commits only through the fenced
			// LiveHeldQuestions rows above; its body may quote the legacy
			// frame (the raw question), so never match it textually.
			if _, isMarker := session.ParseBatchChildQuestionNotice(notice.Text); isMarker {
				continue
			}
			if !strings.Contains(notice.Text, "SUB-AGENT QUESTION (session "+child+")") {
				continue
			}
			if p.toolCallID != "" && !strings.Contains(notice.Text, "job "+p.toolCallID+" stays open") {
				continue
			}
			committed[child] = true
			break
		}
	}
	return committed, nil
}
