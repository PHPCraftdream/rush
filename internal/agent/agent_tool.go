package agent

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/clibridge"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Role   RequestedModelRole `json:"role,omitempty" description:"Optional model profile: smart, fast, worker, or reviewer. Changes only the model and inference settings, not tools or permissions. Omitted preserves automatic legacy selection; explicit smart never selects worker."`
	Prompt string             `json:"prompt" description:"The task for the agent to perform. When resume_session_id is set, this is instead the answer/instruction to send to the paused sub-agent — it is appended to that sub-agent's existing conversation, not a new task."`
	// ResumeSessionID, when set, continues an existing sub-agent session
	// instead of starting a new one. Use this to answer a sub-agent that
	// stopped with a "SUB-AGENT QUESTION" tool result: the sub-agent's full
	// prior context (files it already read, work already done) is preserved,
	// so the task does not have to be restarted from scratch. Must be the
	// exact session id from that tool result, and must be a child of the
	// current session — resuming any other session id is rejected.
	ResumeSessionID string `json:"resume_session_id,omitempty" description:"Optional: the child session id of a paused sub-agent to resume (from a prior \"SUB-AGENT QUESTION (session ...)\" result), instead of starting a new sub-agent. The sub-agent's context is preserved."`
	// Timeout is an optional explicit deadline for this delegation (#1037).
	// See tools.TimeoutParams's doc for the shape.
	Timeout *tools.TimeoutParams `json:"timeout,omitempty" description:"Optional explicit deadline: {\"seconds\": N, \"kind\": \"wake_only\"|\"terminate_and_wake\"}. \"wake_only\" lets the delegated work keep running and just notifies you it's taking a while; \"terminate_and_wake\" stops it and reports partial output. No default -- set it only when you actually want a deadline. A bare number of seconds is also accepted and means {\"seconds\": N, \"kind\": \"wake_only\"}."`
}

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	prompt, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agentCtx, childScope := toolwrap.ChildToolBuildContext(ctx) // pct: isolate this child's captured tool build scope.
	agent, err := c.buildAgent(agentCtx, prompt, agentCfg, true)
	if err != nil {
		if childScope != nil {
			childScope.Release()
		}
		return nil, err
	}
	tool := fantasy.NewParallelAgentTool(
		modelkit.AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			remaining, depthErr := MaxAgentDepthFrom(ctx)
			if depthErr != nil {
				return fantasy.ToolResponse{}, depthErr
			}
			if params.ResumeSessionID == "" && remaining == 0 {
				return fantasy.NewTextErrorResponse(ErrAgentDepthExceeded.Error()), nil
			}
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			childCtx, depthErr := WithAgentDepth(ctx, remaining)
			if depthErr != nil {
				return fantasy.ToolResponse{}, depthErr
			}
			handoff := toolwrap.HandoffFrom(childCtx)
			return c.runSubAgent(childCtx, subAgentParams{
				Agent:              agent,
				SessionID:          sessionID,
				AgentMessageID:     agentMessageID,
				ToolCallID:         call.ID,
				Prompt:             params.Prompt,
				SessionTitle:       "New Agent Session",
				ResumeSessionID:    params.ResumeSessionID,
				RequestedModelRole: params.Role,
				// slotModel: this agent's model came from the buildAgent
				// cascade (config role slots) — the only dispatch path the
				// dispatch-time slot refresh may rebuild.
				slotModel:  true,
				pctHandoff: handoff,
			})
		},
	)
	if childScope != nil {
		return childagent.NewPctCapturedAgentTool(tool, childScope), nil
	}
	return tool, nil
}

// ownWorkBatch is the in-turn guard's own-work predicate on batch facts. The
// turn never touches a legacy job store (agent_turn.go), so the guard answers
// the same "does this owner have live work" question CLIActiveWork answers,
// through the same bound cutover-facts seam.
//
// The predicate is CLIActiveWork's flag-ON body WITHOUT
// recoverBatchScopeDeadCandidates: that recovery already ran at this turn's
// preamble, and armProgressGuard runs once per step, so repeating the
// dead-host sweep here would multiply bounded recovery writes per turn. A
// dead host the sweep would have recovered only keeps the conservative
// (active) answer until the next turn.
//
// A failed read fails closed exactly like CLIActiveWork's batch branch: the
// session is treated as having live work (ownWork=true), never as silently
// idle.
func (a *sessionAgent) ownWorkBatch(ctx context.Context, sessionID string) bool {
	if a.coord == nil {
		return false
	}
	coord := a.coord
	if !coord.BatchFactsInstalled() {
		// No batch runtime installed for this coordinator (cli_active_work.go's
		// unbound rule): the guard never answers from batch rows and the
		// caller treats the session as idle -- an unbound coordinator has no
		// durable live-work source to consult.
		return false
	}
	facts := coordinatorBatchCutoverFacts(coord)
	bctx, cancel := context.WithTimeout(ctx, drainAccountBudget())
	defer cancel()
	own, descendants, err := facts.LiveWorkSummary(bctx, sessionID)
	if err != nil {
		slog.Warn("Batch own-work read failed; treating the session as active",
			"session_id", sessionID, "err", err)
		return true
	}
	if own+descendants > 0 {
		active, err := clibridge.NewCliActiveWorkComponent(cliActiveWorkHostAdapter{coord: coord}).DoBatchActiveAfterQuestionPause(bctx, facts, sessionID)
		if err != nil {
			slog.Warn("Batch own-work question-pause read failed; treating the session as active",
				"session_id", sessionID, "err", err)
			return true
		}
		if active {
			return true
		}
	}
	if bg := coord.Background(); bg != nil && (bg.ActiveOwned(sessionID) > 0 || bg.PendingCompletionsOwned(sessionID) > 0) {
		return true
	}
	return false
}

// CLIActiveWork excludes schedules, debt, and question-paused delegations, but
// includes those children's jobs and completion-persistence holds. Batch-only:
// the live scope comes from batch_nodes (the node-backed fact source bound at
// install). An UNBOUND coordinator (no bound facts) answers false: no batch
// runtime means no batch work can be pending for it. A failed read fails
// closed with the error (the quota loop retries it under its bounded window,
// never silently reads "idle").
func (c *coordinator) CLIActiveWork(ctx context.Context, root string) (bool, error) {
	return clibridge.NewCliActiveWorkComponent(c).DoCLIActiveWork(ctx, root)
}

var _ clibridge.CLIActiveWorkSource = (*coordinator)(nil)

func (c *coordinator) cliActiveWorkBatch(ctx context.Context, facts BatchCutoverFacts, root string) (bool, error) {
	return clibridge.NewCliActiveWorkComponent(c).DoCLIActiveWorkBatch(ctx, facts, root)
}

func (c *coordinator) BatchCutoverFacts() agentcall.BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(c)
}

func (c *coordinator) SubAgentQuestionBlockFromFinish(fp *message.Finish) (string, bool) {
	return subAgentQuestionBlockFromFinish(fp)
}

func (c *coordinator) SubAgentQuestionFromFinish(childSessionID string, fp *message.Finish) (string, bool) {
	return subAgentQuestionFromFinish(childSessionID, fp)
}

func (c *coordinator) BatchNodeStateLive(state string) bool {
	return batchNodeStateLive(state)
}

// BatchFactsInstalled reports whether a concrete (node-backed) batch cutover
// facts source is bound for the coordinator.
func (c *coordinator) BatchFactsInstalled() bool {
	if c == nil {
		return false
	}
	facts := coordinatorBatchCutoverFacts(c)
	return !c.UnboundBatchFacts(facts)
}

func (c *coordinator) UnboundBatchFacts(facts agentcall.BatchCutoverFacts) bool {
	_, unbound := facts.(unknownBatchCutoverFacts)
	return unbound
}

func (c *coordinator) RecoverBatchScopeDeadCandidates(ctx context.Context, owner string) {
	c.scopeRecoverySource().recoverDeadCandidates(ctx, owner)
}

func (c *coordinator) BackgroundOwnsWork(root string) bool {
	return c.background != nil && (c.background.ActiveOwned(root) > 0 || c.background.PendingCompletionsOwned(root) > 0)
}

func (c *coordinator) CLIActiveWorkMessages() message.Service { return c.messages }

func (c *coordinator) ChildSessionBusy(child string) bool {
	ag := c.agentFor(child)
	return ag != nil && ag.IsSessionBusy(child)
}

type cliActiveWorkHostAdapter struct{ coord agentcall.CoordinatorPort }

func (a cliActiveWorkHostAdapter) BatchCutoverFacts() agentcall.BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(a.coord)
}

func (a cliActiveWorkHostAdapter) RecoverBatchScopeDeadCandidates(ctx context.Context, owner string) {
	recoverBatchScopeDeadCandidates(ctx, a.coord, owner)
}

func (a cliActiveWorkHostAdapter) BackgroundOwnsWork(root string) bool {
	return a.coord.BackgroundOwnsWork(root)
}

func (a cliActiveWorkHostAdapter) CLIActiveWorkMessages() message.Service {
	return a.coord.CLIActiveWorkMessages()
}

func (a cliActiveWorkHostAdapter) ChildSessionBusy(child string) bool {
	return a.coord.ChildSessionBusy(child)
}

func (a cliActiveWorkHostAdapter) UnboundBatchFacts(facts agentcall.BatchCutoverFacts) bool {
	return a.coord.UnboundBatchFacts(facts)
}

func (a cliActiveWorkHostAdapter) SubAgentQuestionBlockFromFinish(fp *message.Finish) (string, bool) {
	return a.coord.SubAgentQuestionBlockFromFinish(fp)
}

func (a cliActiveWorkHostAdapter) SubAgentQuestionFromFinish(child string, fp *message.Finish) (string, bool) {
	return a.coord.SubAgentQuestionFromFinish(child, fp)
}

func (a cliActiveWorkHostAdapter) BatchNodeStateLive(state string) bool {
	return a.coord.BatchNodeStateLive(state)
}
