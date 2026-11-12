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

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/home"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	sessionpkg "github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/skills"
)

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent              SessionAgent
	SessionID          string
	AgentMessageID     string
	ToolCallID         string
	Prompt             string
	SessionTitle       string
	RequestedModelRole RequestedModelRole
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
	// ResumeSessionID, when non-empty, continues an existing sub-agent
	// session instead of creating a new one — see AgentParams.ResumeSessionID
	// for the model-facing contract. Must name a session whose
	// ParentSessionID equals SessionID; runSubAgent verifies this before
	// touching it (see the ownership check in runSubAgent).
	ResumeSessionID string
	// slotModel reports that params.Agent's model was built from a config
	// role slot (the agent tool's buildAgent cascade). Only such dispatches
	// may be refreshed at dispatch time; agentic_fetch's fast-built child
	// and any other caller are exempt.
	slotModel  bool
	pctHandoff *pctDelegationHandoff // pct: admission-held child construction refs.
}

func loadChildRoleContract(ctx context.Context, sessions sessionpkg.Service, childID string) (*sessionpkg.RoleContract, error) {
	store, ok := sessions.(sessionpkg.SessionRoleContractStore)
	if !ok {
		return nil, fmt.Errorf("%w: child role contract store unavailable", ErrAgentRoleUnconfigured)
	}
	contract, found, err := store.GetSessionRoleContract(ctx, childID)
	if err != nil || !found {
		return contract, err
	}
	if err := contract.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid child role contract: %v", ErrAgentRoleUnconfigured, err)
	}
	return contract.Clone(), nil
}

// It creates a sub-session (or resumes an existing one, see ResumeSessionID),
// runs the agent with the given prompt, and propagates the cost to the parent
// session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	var childSelected bool
	var pctReleases []func()
	var pctDriverTransferred bool
	defer func() {
		if !pctDriverTransferred {
			pctReleaseScopeRefs(pctReleases)
		}
	}()
	lifecycle, lifecycleBound := ctx.Value(batchAgentLifecycleContextKey{}).(batchAgentLifecycle)
	defer func() {
		if !childSelected && lifecycleBound && lifecycle.NoChildSelected != nil {
			lifecycle.NoChildSelected()
		}
	}()
	// A batch node starts its child without an enclosing turn, so nothing has
	// drained the asynchronous prompt/tool build of params.Agent yet; a child
	// run before that would go out with an empty system prompt and toolset.
	var remaining int
	var err error
	if err := c.readyWg.Wait(); err != nil {
		return fantasy.ToolResponse{}, err
	}
	if params.ResumeSessionID == "" {
		remaining, err = effectiveParentAgentDepth(ctx, c.sessions, params.SessionID)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if remaining == 0 {
			return fantasy.NewTextErrorResponse(ErrAgentDepthExceeded.Error()), nil
		}
	}
	if params.ResumeSessionID != "" {
		parentRemaining, depthErr := effectiveParentAgentDepth(ctx, c.sessions, params.SessionID)
		if depthErr != nil {
			return fantasy.ToolResponse{}, depthErr
		}
		remaining, err = MaxAgentDepthFrom(ctx)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if remaining > parentRemaining {
			remaining = parentRemaining
		}
	}
	childRemaining := remaining
	if params.ResumeSessionID == "" {
		childRemaining--
	}
	depthStore, depthStoreOK := c.sessions.(sessionpkg.AgentDepthStore)
	if !depthStoreOK {
		return fantasy.ToolResponse{}, errors.New("agent depth persistence unavailable")
	}
	cfg, _ := c.cfg.Snapshot()
	requestedRole := params.RequestedModelRole
	var explicitRoleModel *delegationRoleModel
	var childRoleContract *sessionpkg.RoleContract
	var session sessionpkg.Session
	resolveRole := func(childID string) error {
		var err error
		requestedRole, err = resolveDelegationRequestedRole(ctx, c.sessions, childID, requestedRole)
		if err != nil {
			return err
		}
		if requestedRole == "" {
			if params.ResumeSessionID != "" {
				childRoleContract, err = loadChildRoleContract(ctx, c.sessions, childID)
				return err
			}
			if parent := parentRoleContractFrom(ctx); parent != nil && (parent.Tools != nil || len(parent.Inherited) > 0) {
				childRoleContract, err = copyInheritedRoleContract(parent)
				if err != nil {
					return err
				}
			}
			return nil
		}
		if admitted := selectedRoleContractFrom(ctx); admitted != nil {
			if admitted.Name != string(requestedRole) {
				return ErrAgentRoleMismatch
			}
			childRoleContract = admitted.Clone()
		} else if childID != "" {
			childRoleContract, err = loadChildRoleContract(ctx, c.sessions, childID)
			if err != nil {
				return err
			}
			if childRoleContract != nil && childRoleContract.Name != string(requestedRole) {
				return ErrAgentRoleMismatch
			}
		}
		if !requestedRole.builtin() && childRoleContract == nil {
			childRoleContract, err = ResolveRoleContract(cfg, string(requestedRole), parentRoleContractFrom(ctx))
			if err != nil {
				return err
			}
		}
		if childRoleContract != nil {
			ctx = WithSelectedRoleContract(ctx, childRoleContract)
		}
		selected, err := c.resolveRequestedDelegationModel(ctx, cfg, params.SessionID, requestedRole)
		if err != nil {
			return err
		}
		if requestedRole.builtin() && childRoleContract == nil {
			if parent := parentRoleContractFrom(ctx); parent != nil && (parent.Tools != nil || len(parent.Inherited) > 0) {
				childRoleContract = roleContractWithName(selected.Model.ModelCfg, string(requestedRole), parent)
				if childRoleContract == nil {
					return fmt.Errorf("%w: cannot snapshot inherited role cap", ErrAgentRoleUnconfigured)
				}
			}
		}
		if childRoleContract != nil && childID != "" {
			if err := bindDelegationRoleContract(ctx, c.sessions, childID, childRoleContract); err != nil {
				return err
			}
		}
		explicitRoleModel = &selected
		return nil
	}
	if params.ResumeSessionID != "" {
		existing, getErr := c.sessions.Get(ctx, params.ResumeSessionID)
		if getErr != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("resume_session_id %q not found: %s%s", params.ResumeSessionID, getErr, c.batchHeldQuestionResumeHint(ctx, params.SessionID))), nil
		}
		if existing.ParentSessionID != params.SessionID {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("resume_session_id %q is not a child of the current session; refusing to resume a session that does not belong to this agent call%s", params.ResumeSessionID, c.batchHeldQuestionResumeHint(ctx, params.SessionID))), nil
		}
		if refusal := c.batchHeldQuestionResumeRefusal(ctx, params.SessionID, existing.ID); refusal != "" {
			return fantasy.NewTextErrorResponse(refusal), nil
		}
		if err := resolveRole(existing.ID); err != nil {
			return fantasy.ToolResponse{}, err
		}
		session = existing
		if childRoleContract != nil {
			if err := bindDelegationRoleContract(ctx, c.sessions, session.ID, childRoleContract); err != nil {
				return fantasy.ToolResponse{}, err
			}
		}
		childRemaining, err = depthStore.ResolveAgentDepth(ctx, existing.ID, remaining)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("resolve resumed agent depth: %w", err)
		}
		if err := depthStore.LowerAgentDepth(ctx, existing.ID, childRemaining); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("lower resumed agent depth: %w", err)
		}
	} else {
		if err := resolveRole(""); err != nil {
			return fantasy.ToolResponse{}, err
		}
		agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		created, createErr := depthStore.CreateTaskSessionWithAgentDepth(ctx, agentToolSessionID, params.SessionID, params.SessionTitle, childRemaining)
		if createErr != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create agent child session: %w", createErr)
		}
		session = created
		if childRoleContract != nil {
			if err := bindDelegationRoleContract(ctx, c.sessions, session.ID, childRoleContract); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("bind child role contract: %w", err)
			}
		}
	}
	if err := bindDelegationRequestedRole(ctx, c.sessions, session.ID, requestedRole); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("bind child model role: %w", err)
	}

	// Session selection has been validated; bind this exact ID, including
	// resume. The bind persists the batch delegation child link BEFORE the
	// child runs: a bind failure is fatal to this launch (the child must
	// not start), so the error propagates and NoChildSelected marks the
	// node through the defer above.
	if lifecycleBound && lifecycle.BindChildSession != nil {
		if err := lifecycle.BindChildSession(ctx, session.ID); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("bind batch child session: %w", err)
		}
	}
	childSelected = true

	// A delegation starting on this child lifts any suspension of its
	// automatic turns (a Stop, or a question it asked earlier and the parent
	// now answers by resuming it): from here it is driven by this delegation.
	c.resetConsecutiveResume(session.ID)

	// A resumed child starts with a stale `sessions cancel` request spent: the
	// delegation is a fresh, deliberate turn (R8A-2). A new child has no flag.
	if params.ResumeSessionID != "" {
		clearCancelRequest(ctx, c.sessions, session.ID)
	}

	// Propagate the PARENT session's auto-approve status to this child.
	//
	// A sub-agent runs under its own child session id
	// (parentMessageID$$toolCallID), which is a completely different key
	// from the parent's. A non-interactive `rush run` auto-approves ONLY
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
		// R1-1's restricted-run gate inheritance moved below (§6.2), right
		// after the driver registers: it needs the driver's generation to
		// bind the allowlist entry to, so releaseDriverIfScopeClosed can
		// clear it without the §6.3 resume_session_id race.
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(session.ID)
	}

	// Get model configuration. Defaults to the dispatch agent's own shared
	// model (built once from the merged system/folder config); a session
	// override on the PARENT session — params.SessionID, not the freshly
	// created sub-agent session.ID above — replaces it for THIS call only,
	// via the same immutable per-call pin SessionAgentCall.SmartModel
	// already uses for top-level turns (task #341/P0-1). Never mutates
	// params.Agent's own shared fields, so a session-scoped worker override
	// cannot leak into a concurrent sub-agent dispatch from a different
	// session sharing the same underlying agent object (task #466).
	model := params.Agent.Model()
	if explicitRoleModel == nil && childRoleContract != nil && childRoleContract.Name != "" {
		selected, _, resolveErr := currentOrSnapshotRoleModel(cfg, childRoleContract)
		if resolveErr != nil {
			return fantasy.ToolResponse{}, resolveErr
		}
		model, _, resolveErr = c.buildModelsFromCfg(ctx, cfg, selected, selected, true)
		if resolveErr != nil {
			return fantasy.ToolResponse{}, resolveErr
		}
	}

	// Per-call CredentialSet (RunWithCredentials): the parent call's
	// tenant credentials win for the slots they cover — worker first (the
	// slot sub-agent spawns exist for), then smart — and the session-DB
	// worker override below is skipped entirely for a covered call.
	// Building the child's model from the SAME CredentialSet keeps a
	// tenant's fan-out on the tenant's provider; the shared config would
	// silently route the child to the operator's provider, the exact leak
	// per-call credentials exist to prevent. A build failure is a hard
	// error for the same reason: falling through to `model` would run
	// tenant work on the shared config.
	hbSource := hbSourceSlotStart
	var credProviderCfg *config.ProviderConfig
	var callCreds *CredentialSet
	if explicitRoleModel != nil {
		model = explicitRoleModel.Model
		credProviderCfg = &explicitRoleModel.Provider
		callCreds = explicitRoleModel.Credentials
		hbSource = explicitRoleModel.Source
	}
	if creds := callCredentialsFrom(ctx); explicitRoleModel == nil && creds != nil {
		callCreds = creds
		choice, covered := creds.Models[RoleWorker]
		if !covered {
			choice, covered = creds.Models[RoleSmart]
		}
		if childRoleContract != nil && childRoleContract.Name != "" {
			choice, covered = creds.Models[Role(childRoleContract.Name)]
		}
		if covered {
			credModel, provCfg, err := c.buildCredentialModel(ctx, cfg, creds, choice)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to build sub-agent model from per-call credentials: %w", err)
			}
			model = credModel
			credProviderCfg = &provCfg
			hbSource = hbSourcePerCall
		}
	}

	if explicitRoleModel == nil && childRoleContract != nil && childRoleContract.Name != "" && callCreds == nil {
		selected, source, resolveErr := currentOrSnapshotRoleModel(cfg, childRoleContract)
		if resolveErr != nil {
			return fantasy.ToolResponse{}, resolveErr
		}
		model, _, resolveErr = c.buildModelsFromCfg(ctx, cfg, selected, selected, true)
		if resolveErr != nil {
			return fantasy.ToolResponse{}, resolveErr
		}
		hbSource = source
	}

	if explicitRoleModel == nil && childRoleContract == nil && credProviderCfg == nil && WebModelPolicy(ctx) {
		if override, err := c.resolveSubAgentModelOverride(ctx, params.SessionID); err != nil {
			slog.Error("runSubAgent: failed to resolve session worker override, falling back to default", "sessionID", params.SessionID, "err", err)
		} else if override != nil {
			model = *override
			hbSource = hbSourceSessionOverride
		}
	}
	// The refresh is best-effort; preserve the selected role for future turns.
	var dispatchSlotRole config.SelectedModelType
	if requestedRole != "" && !requestedRole.builtin() {
		dispatchSlotRole = ""
	} else if requestedRole == "" && params.slotModel && callCreds == nil && hbSource != hbSourceSessionOverride {
		dispatchSlotRole = config.SelectedModelTypeSmart
		if role := callModelRole(ctx); role == "" || role == config.SelectedModelTypeSmart {
			if c.workerSubAgentActiveForCall(ctx, cfg) {
				dispatchSlotRole = config.SelectedModelTypeWorker
			}
		}
		if refreshed, ok := c.refreshSubAgentSlotModel(ctx, cfg, dispatchSlotRole, model); ok {
			model = refreshed
			hbSource = hbSourceSlotLive
		}
	}
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	var providerCfg config.ProviderConfig
	if credProviderCfg != nil {
		providerCfg = *credProviderCfg
	} else {
		var ok bool
		providerCfg, ok = cfg.Providers.Get(model.ModelCfg.Provider)
		if !ok {
			return fantasy.ToolResponse{}, errModelProviderNotConfigured
		}
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return fantasy.ToolResponse{}, err
	}

	// Run the agent. Keep the selected model pinned while refreshing the
	// provider identity; a refresh must not switch this call to new defaults.
	initialModel := model
	pinnedModel := &initialModel
	rebuildCall := func() error {
		if callCreds != nil {
			return errUnauthorizedRefreshUnavailable
		}
		cfg, freshProviderCfg, err := c.rebuildInputs(model.ModelCfg.Provider)
		if err != nil {
			return err
		}
		freshModel, err := c.rebuildPinnedModel(ctx, cfg, model, freshProviderCfg, true)
		if err != nil {
			return fmt.Errorf("failed to rebuild sub-agent model: %w", err)
		}
		pinnedModel = &freshModel
		model = freshModel
		providerCfg = freshProviderCfg
		maxTokens = model.CatwalkCfg.DefaultMaxTokens
		if model.ModelCfg.MaxTokens != 0 {
			maxTokens = model.ModelCfg.MaxTokens
		}
		return nil
	}

	if callCreds == nil && explicitRoleModel == nil && childRoleContract == nil && hbSource != hbSourceSessionOverride {
		if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
			slog.Error("Failed to refresh OAuth2 token before sub-agent run. Proceeding with existing token.", "error", err)
		} else if err := rebuildCall(); err != nil {
			return fantasy.ToolResponse{}, err
		}
	}

	// Snapshot the call shape BEFORE starting the child's turn and register it
	// as this child session's driver: a later wake for the child's own
	// async/background work (notifyAsyncCompletion) must resume it on this
	// SAME SessionAgent instead of racing a second one for the child's OS
	// session lock (task #1049; design doc
	// docs/plans/2026-09-27-async-structured-concurrency.md §3, "one driver
	// per session"). Copies pinnedModel's CURRENT value rather than sharing
	// its pointer: rebuildCall reassigns that local on a 401 retry, and the
	// driver must keep the model the delegation actually started with, not
	// whatever a later retry rebuilds it to.
	driverModel := *pinnedModel
	// Driver slot provenance is maintained independently of refresh outcome.
	driverSlotRole := dispatchSlotRole
	requestedSource := ""
	if requestedRole != "" {
		requestedSource = hbSource
	}
	childCallOptions := inheritWebPolicyCallOptions(ctx)
	if childCallOptions == nil {
		childCallOptions = &CallOptions{}
	}
	if childRoleContract != nil {
		childCallOptions = WithRoleContractCopy(childCallOptions, childRoleContract)
	}
	if childRoleContract != nil && childRoleContract.Name != "" && explicitRoleModel == nil && callCreds == nil {
		selected, source, resolveErr := currentOrSnapshotRoleModel(cfg, childRoleContract)
		if resolveErr != nil {
			return fantasy.ToolResponse{}, resolveErr
		}
		model, _, resolveErr = c.buildModelsFromCfg(ctx, cfg, selected, selected, true)
		if resolveErr != nil {
			return fantasy.ToolResponse{}, resolveErr
		}
		providerCfg, _ = cfg.Providers.Get(selected.Provider)
		hbSource = source
	}
	childCallOptions.MaxAgentDepth = childRemaining
	childCallOptions.AgentDepthSet = true
	ctx = WithCallOptions(ctx, childCallOptions)
	// ONE shaping point for the batch child's per-call toolset: both
	// the delegation turn and every later wake-driven turn must see
	// the same ask surface (wrapped for a route-ful incarnation,
	// stripped for a no-route one). Nil for non-batch delegations —
	// their turns keep the agent's own base toolset, byte-identical.
	var batchChildTools []fantasy.AgentTool
	var batchQuestionState *batchChildQuestionState
	if lifecycleBound {
		if sa, ok := params.Agent.(*sessionAgent); ok {
			batchChildTools, _ = buildBatchQuestionTools(sa.tools.Copy(), ctx)
		}
		// Freeze the same capability for the wake path: the driver template
		// (callTemplate) is what a later Drain turn re-runs, and that turn's
		// context — a background callback context — carries neither the
		// route marker nor the held-answer binding. A lifecycle-bound child
		// needs both; missing context cannot grant an answer route.
		binding, bindingOK := batchAgentQuestionBindingFrom(ctx)
		route, routeOK := batchQuestionRouteFrom(ctx)
		batchQuestionState = &batchChildQuestionState{
			Depth:     batchAgentQuestionDepth(ctx),
			Route:     routeOK && route && bindingOK && binding.Ask != nil,
			Binding:   binding,
			BindingOK: bindingOK,
		}
	}
	callTemplate := SessionAgentCall{
		SessionID:            session.ID,
		MaxOutputTokens:      maxTokens,
		ProviderOptions:      getProviderOptions(session.ID, model, providerCfg),
		Temperature:          model.ModelCfg.Temperature,
		TopP:                 model.ModelCfg.TopP,
		TopK:                 model.ModelCfg.TopK,
		FrequencyPenalty:     model.ModelCfg.FrequencyPenalty,
		PresencePenalty:      model.ModelCfg.PresencePenalty,
		NonInteractive:       true,
		RequestedModelRole:   requestedRole,
		RequestedModelSource: requestedSource,
		SmartModel:           &driverModel,
		Credentials:          callCreds,
		CallOptions:          childCallOptions,
		RoleContract:         cloneRoleContractForCall(childRoleContract),
	}
	if lifecycleBound {
		callTemplate.Tools = batchChildTools
		callTemplate.batchQuestion = batchQuestionState
	}
	delegatedFinalization := sessionpkg.CaptureAgentFinalizationContext(ctx)
	callTemplate.LogicalCallID, err = finalizationLogicalIdentity(delegatedFinalization, session.ID, callTemplate.LogicalCallID)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if params.pctHandoff != nil {
		pctReleases = params.pctHandoff.pctClaimDriver()
	}
	generation := c.subAgentDrivers.register(session.ID, subAgentDriver{
		agent: params.Agent, call: callTemplate, parentSessionID: params.SessionID,
		slotRole: driverSlotRole, pctReleases: pctReleases,
		finalizationAuthorities: delegatedFinalization,
	})
	pctDriverTransferred = generation != 0
	if !pctDriverTransferred {
		pctReleaseScopeRefs(pctReleases)
	}
	// R1-1: inherit the parent run's per-session restricted-run gate too, so
	// a delegated sub-agent's tool calls are judged by its own run's policy
	// rather than by whatever a concurrent run last armed on the
	// process-wide gate. Bound to THIS registration's generation (§6.2) so
	// releaseDriverIfScopeClosed can later clear exactly this entry, never a
	// newer resume_session_id's. Optional-interface assertion on purpose
	// (mirrors credentialRunner): test fakes of permission.Service must keep
	// compiling unchanged; the real service always implements it.
	if c.permissions != nil {
		if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok {
			mgr.InheritSessionRunAllowlistForGeneration(params.SessionID, session.ID, generation)
			// No `defer Clear...` here (phase 2 §6.2, unchanged by phase 3):
			// the child may still own async jobs/background shells after
			// THIS turn returns (structural concurrency, #1049) and a woken
			// turn re-inherits from subAgentDriver.parentSessionID on every
			// wake instead. The entry is now cleared by
			// releaseDriverIfScopeClosed once the child's scope actually
			// closes (§6.2), not left to grow unbounded (§6.3's diagnosis,
			// closed this phase).
		}
	}

	// runAwaitingAdmission (#1036): if the child's mailbox is busy when this
	// call is attempted (e.g. notifyAsyncCompletion just woke it for the
	// child's OWN async-job completion a moment earlier), a bare
	// params.Agent.Run would return (nil, nil) here -- not because the
	// child produced no output, but because its turn had not started yet.
	// subAgentOutput(nil) then reads as "" and the caller below reports
	// "Sub-agent completed but produced no text output" about a turn that
	// never ran. Blocking for the queued turn's real result instead closes
	// that gap; every remaining line of this function is unchanged.
	// Heartbeat attribution: delegation boundary re-parents onto the child
	// session; root inherited from the dispatching turn (design pt. 2).
	ctx = inheritHeartbeat(ctx, session.ID, "worker", hbSource)
	run := func() (*fantasy.AgentResult, error) {
		runOptions := *childCallOptions
		runOptions.MaxAgentDepth = childRemaining
		runOptions.AgentDepthSet = true
		runCtx := WithCallOptions(ctx, &runOptions)
		call := SessionAgentCall{
			SessionID:            session.ID,
			LogicalCallID:        callTemplate.LogicalCallID,
			driverGeneration:     generation,
			Prompt:               params.Prompt,
			MaxOutputTokens:      maxTokens,
			ProviderOptions:      getProviderOptions(session.ID, model, providerCfg),
			Temperature:          model.ModelCfg.Temperature,
			TopP:                 model.ModelCfg.TopP,
			TopK:                 model.ModelCfg.TopK,
			FrequencyPenalty:     model.ModelCfg.FrequencyPenalty,
			PresencePenalty:      model.ModelCfg.PresencePenalty,
			NonInteractive:       true,
			RequestedModelRole:   requestedRole,
			RequestedModelSource: requestedSource,
			SmartModel:           pinnedModel,
			Credentials:          callCreds,
			RoleContract:         cloneRoleContractForCall(runOptions.RoleContract),
			CallOptions:          &runOptions,
		}
		if lifecycleBound {
			call.Tools = batchChildTools
			call.batchQuestion = batchQuestionState
		}
		result, runErr, _ := c.runAwaitingAdmission(runCtx, params.Agent, call)
		return result, runErr
	}
	var result *fantasy.AgentResult
	err = c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var runErr error
		result, runErr = run()
		return runErr
	}, rebuildCall)
	// Notify only if still unauthorized after retry.
	if err != nil && c.isUnauthorized(err) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}
	// #1130: no parent charge here. The child pays into its own cost_self
	// (agent_turn_step's IncrementCost); "including children" is a subtree
	// query at read time, so there is nothing to transfer on any outcome.

	// A sub-agent that called ask_question stops its turn via
	// AwaitingAnswerError (see question_stop.go), not a genuine failure —
	// letting it fall into the generic branch below would tell the parent
	// model "the sub-agent FAILED", and it would likely redo the work itself
	// instead of answering. Recognize this case and return a normal
	// SUCCESSFUL tool response shaped as a question. Cost was already charged
	// above regardless of outcome.
	var awaitingAnswer *AwaitingAnswerError
	if errors.As(err, &awaitingAnswer) {
		return fantasy.NewTextResponse(subAgentQuestionWithPreamble(
			c.childQuestionPreamble(session.ID), subAgentQuestionText(session.ID, awaitingAnswer))), nil
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

// inheritBatchQuestionState pins a registered batch delegation's frozen
// question capability onto a freshly built call for the same child
// session, together with the delegation's shaped child toolset. Without
// it, a replacement call built from the parent's context runs the child
// as a non-batch session: the raw ask tool is reachable and descendant
// batch invocations default to route-ful. Top-level sessions (no
// registered driver) and non-batch delegations (no capability on the
// template) are untouched, byte-identical.
func (c *coordinator) inheritBatchQuestionState(ctx context.Context, childSessionID string, call *SessionAgentCall) error {
	if call == nil || call.batchQuestion != nil {
		return nil
	}
	driver, ok := c.subAgentDrivers.get(childSessionID)
	if !ok {
		state, err := c.rebuiltChildQuestionState(ctx, childSessionID)
		if err != nil {
			return err
		}
		call.batchQuestion = state
		if state != nil && call.Tools != nil {
			call.Tools = stripAskQuestionTools(call.Tools)
		}
		return nil
	}
	if driver.call.batchQuestion == nil {
		return nil
	}
	call.batchQuestion = driver.call.batchQuestion
	if driver.call.Tools != nil {
		call.Tools = driver.call.Tools
	}
	return nil
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}

// #1130: updateParentSessionCost/chargeChildToParent are gone with the
// transfer ledger. A child charges its own cost_self as it runs; every
// reader of "including children" sums the delegation subtree at read time.

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
