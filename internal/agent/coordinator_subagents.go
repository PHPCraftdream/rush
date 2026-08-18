// Sub-agent delegation (session creation/resume, cost transfer) and skill
// discovery/logging. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
)

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
	// ResumeSessionID, when non-empty, continues an existing sub-agent
	// session instead of creating a new one — see AgentParams.ResumeSessionID
	// for the model-facing contract. Must name a session whose
	// ParentSessionID equals SessionID; runSubAgent verifies this before
	// touching it (see the ownership check in runSubAgent).
	ResumeSessionID string
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session (or resumes an existing one, see ResumeSessionID),
// runs the agent with the given prompt, and propagates the cost to the parent
// session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	var session session.Session
	if params.ResumeSessionID != "" {
		// Resume path (Gap 2 / phase 3.2): reuse the existing sub-session so
		// the sub-agent's prior context (files read, work already done) is
		// preserved instead of thrown away. SECURITY: verify the session
		// being resumed is genuinely a child of the CURRENT parent session
		// before touching it — otherwise a model could pass an arbitrary
		// session id (someone else's session, or an unrelated top-level
		// session) and read/append to it. A bad id here is a model mistake,
		// not a crash: return a normal tool error (nil Go error) so the
		// caller can retry rather than aborting the parent's turn.
		existing, err := c.sessions.Get(ctx, params.ResumeSessionID)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf(
				"resume_session_id %q not found: %s", params.ResumeSessionID, err,
			)), nil
		}
		if existing.ParentSessionID != params.SessionID {
			return fantasy.NewTextErrorResponse(fmt.Sprintf(
				"resume_session_id %q is not a child of the current session; refusing to resume a session that does not belong to this agent call",
				params.ResumeSessionID,
			)), nil
		}
		session = existing
	} else {
		// Create sub-session
		agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		created, err := c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
		}
		session = created
	}

	// Propagate the PARENT session's auto-approve status to this child.
	//
	// A sub-agent runs under its own child session id
	// (parentMessageID$$toolCallID), which is a completely different key
	// from the parent's. A non-interactive `crush run` auto-approves ONLY
	// the root session id it was handed (app.go's
	// Permissions.AutoApproveSession(sess.ID)), so before this call every
	// delegated sub-agent was unapproved: its first non-safe tool call
	// (anything outside tools.safeCommands — e.g. a bare `wc -l`) reached
	// permission.Request's final select, which blocks on a UI response
	// that does not exist in that mode, and hung until the 45-minute tool
	// watchdog cap. Observed in the wild as "the agent spawns a sub-agent
	// and then nothing happens at all" — the parent sat in `delegating`
	// with a healthy heartbeat while the child was stuck on a trivial
	// command.
	//
	// Inheritance (not a blanket auto-approve) is deliberate: an
	// INTERACTIVE parent's sub-agent must still go through the normal
	// prompt path, and the restricted-run allowlist gate still applies on
	// top, since it is consulted inside the auto-approve branch itself.
	// Applied BEFORE SessionSetup so an explicit setup callback can still
	// override (agentic_fetch_tool.go auto-approves unconditionally).
	if c.permissions != nil {
		c.permissions.InheritSessionAutoApprove(params.SessionID, session.ID)
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(session.ID)
	}

	// Get model configuration. Defaults to the dispatch agent's own shared
	// model (built once from the merged system/folder config); a session
	// override on the PARENT session — params.SessionID, not the freshly
	// created sub-agent session.ID above — replaces it for THIS call only,
	// via the same immutable per-call pin SessionAgentCall.LargeModel
	// already uses for top-level turns (task #341/P0-1). Never mutates
	// params.Agent's own shared fields, so a session-scoped worker override
	// cannot leak into a concurrent sub-agent dispatch from a different
	// session sharing the same underlying agent object (task #466).
	model := params.Agent.Model()
	if override, err := c.resolveSubAgentModelOverride(ctx, params.SessionID); err != nil {
		slog.Error("runSubAgent: failed to resolve session worker override, falling back to default", "sessionID", params.SessionID, "err", err)
	} else if override != nil {
		model = *override
	}
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return fantasy.ToolResponse{}, err
	}

	// Run the agent
	pinnedModel := model
	run := func() (*fantasy.AgentResult, error) {
		return params.Agent.Run(ctx, SessionAgentCall{
			SessionID:        session.ID,
			Prompt:           params.Prompt,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  getProviderOptions(model, providerCfg),
			Temperature:      model.ModelCfg.Temperature,
			TopP:             model.ModelCfg.TopP,
			TopK:             model.ModelCfg.TopK,
			FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
			PresencePenalty:  model.ModelCfg.PresencePenalty,
			NonInteractive:   true,
			SmartModel:       &pinnedModel,
		})
	}
	var result *fantasy.AgentResult
	err := c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var runErr error
		result, runErr = run()
		return runErr
	}, nil)
	// Notify only if still unauthorized after retry.
	if err != nil && c.isUnauthorized(err) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}
	// Charge the parent for whatever the child spent on this run, on EVERY
	// outcome (success, ask_question pause, or genuine error). This replaces
	// the old in-memory baselineCost scheme: TransferChildCostToParent reads
	// the child's persisted parent_cost_accounted ledger inside one
	// transaction and charges only the delta since the last transfer, so the
	// spent amount is never lost. A failed charge only logs a warning — the
	// uncharged delta persists in (child.cost - child.parent_cost_accounted)
	// and is recovered on the next successful call. The previous code skipped
	// the charge on the generic-error path, permanently losing that cost.
	//
	// Detached timeout, same pattern as agent.go's error-path flush (see
	// agent.go's flushCtx uses): ctx may already be cancelled here — by the
	// parent's stream watchdog or a user Ctrl-C — since this runs after the
	// child's own Run() returns. Charging the parent is a single short DB
	// transaction, not tied to the run that just ended, so it must survive
	// that cancellation rather than fail immediately with "begin
	// transaction: context canceled" and silently drop the child's spend
	// (recovery only happens if this same child is ever charged again,
	// which is not guaranteed for a one-shot sub-agent).
	costCtx, costCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	costErr := c.updateParentSessionCost(costCtx, session.ID, params.SessionID)
	costCancel()
	if costErr != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", session.ID,
			"parent_session", params.SessionID,
			"error", costErr,
		)
	}

	// A sub-agent that called ask_question stops its turn via
	// AwaitingAnswerError (see question_stop.go), not a genuine failure —
	// letting it fall into the generic branch below would tell the parent
	// model "the sub-agent FAILED", and it would likely redo the work itself
	// instead of answering. Recognize this case and return a normal
	// SUCCESSFUL tool response shaped as a question. Cost was already charged
	// above regardless of outcome.
	var awaitingAnswer *AwaitingAnswerError
	if errors.As(err, &awaitingAnswer) {
		return fantasy.NewTextResponse(subAgentQuestionText(session.ID, awaitingAnswer)), nil
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", err)), nil
	}

	output := subAgentOutput(result)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output."), nil
	}
	return fantasy.NewTextResponse(output), nil
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}

// updateParentSessionCost transfers the cost a child session accrued since
// the last transfer to its parent. It is a thin delegate to the
// transactional session.Service.TransferChildCostToParent, which reads the
// child's persisted parent_cost_accounted ledger, charges only the delta
// (cost - accounted, clamped >= 0) to the parent via an atomic additive
// UPDATE, and advances the child's accounted marker — all inside one DB
// transaction.
//
// Because the baseline now lives in the database (parent_cost_accounted),
// there is no in-memory baselineCost parameter: the function is safe across
// process restarts, concurrent resumes of the same child, and failed
// charges (an uncharged delta persists and is recovered on the next call).
// Callers should invoke it on EVERY sub-agent outcome (success,
// ask_question, error) so no spent cost is ever silently dropped.
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	return c.sessions.TransferChildCostToParent(ctx, childSessionID, parentSessionID)
}

// discoverSkills runs skill discovery for this coordinator at session
// start. Fork note: upstream threads a pre-built skills.Manager through
// from app.New; we rejected that abstraction (see CHANGELOG.fork.md
// Section 2) and keep the simple inline discovery here.
func discoverSkills(cfg *config.ConfigStore) (allSkills, activeSkills []*skills.Skill) {
	builtin, builtinStates := skills.DiscoverBuiltinWithStates()
	discovered := append([]*skills.Skill(nil), builtin...)

	var userStates []*skills.SkillState
	var userPaths []string

	opts := cfg.Config().Options
	if opts != nil && len(opts.SkillsPaths) > 0 {
		userPaths = make([]string, 0, len(opts.SkillsPaths))
		for _, pth := range opts.SkillsPaths {
			expanded := home.Long(pth)
			if strings.HasPrefix(expanded, "$") {
				if resolved, err := cfg.Resolver().ResolveValue(expanded); err == nil {
					expanded = resolved
				}
			}
			userPaths = append(userPaths, expanded)
		}
		var userSkills []*skills.Skill
		userSkills, userStates = skills.DiscoverWithStates(userPaths)
		discovered = append(discovered, userSkills...)
	}

	allSkills = skills.Deduplicate(discovered)
	var disabledSkills []string
	if opts != nil {
		disabledSkills = opts.DisabledSkills
	}
	activeSkills = skills.Filter(allSkills, disabledSkills)

	allStates := append([]*skills.SkillState(nil), builtinStates...)
	allStates = append(allStates, userStates...)

	allStates = skills.DeduplicateStates(allStates)

	slices.SortStableFunc(allStates, func(a, b *skills.SkillState) int {
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})
	skills.SetLatestStates(allStates)
	skills.PublishStates(allStates)

	logDiscoveryStats(builtin, builtinStates, userStates, userPaths, allSkills, activeSkills, disabledSkills)
	return allSkills, activeSkills
}

// logTurnSkillUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func logTurnSkillUsage(
	sessionID string,
	prompt string,
	activeSkills []*skills.Skill,
	tracker *skills.Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start.
func logDiscoveryStats(
	builtin []*skills.Skill,
	builtinStates, userStates []*skills.SkillState,
	userPaths []string,
	allSkills, activeSkills []*skills.Skill,
	disabled []string,
) {
	countErrors := func(states []*skills.SkillState) int {
		n := 0
		for _, s := range states {
			if s.State == skills.StateError {
				n++
			}
		}
		return n
	}

	userOK := 0
	for _, s := range userStates {
		if s.State == skills.StateNormal {
			userOK++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := skills.ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", len(builtin),
		"builtin_errors", countErrors(builtinStates),
		"user_ok", userOK,
		"user_errors", countErrors(userStates),
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", skills.ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}
