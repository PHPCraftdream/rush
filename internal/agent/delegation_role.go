package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter/delegation"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// Historical package-agent surface retained while implementations live in the
// runtime delegation adapter.
const (
	DefaultAgentDepth      = delegation.DefaultAgentDepth
	MaxAgentDepthForResume = delegation.MaxAgentDepthForResume
)

var ErrAgentDepthExceeded = delegation.ErrAgentDepthExceeded

func MaxAgentDepthFrom(ctx context.Context) (int, error) { return delegation.MaxAgentDepthFrom(ctx) }

func WithAgentDepth(ctx context.Context, remaining int) (context.Context, error) {
	return delegation.WithAgentDepth(ctx, remaining)
}

func effectiveParentAgentDepth(ctx context.Context, sessions session.Service, parentID string) (int, error) {
	return delegation.EffectiveParentAgentDepth(ctx, sessions, parentID)
}

func WithPersistedSessionAgentDepth(ctx context.Context, sessions session.Service, sessionID string) (context.Context, error) {
	return delegation.WithPersistedSessionAgentDepth(ctx, sessions, sessionID)
}

func withPersistedCallAgentDepth(ctx context.Context, sessions session.Service, call *SessionAgentCall) (context.Context, int, error) {
	return delegation.WithPersistedCallAgentDepth(ctx, sessions, call)
}

func PersistSessionAgentDepth(ctx context.Context, sessions session.Service, sessionID string, requested int) (int, error) {
	return delegation.PersistSessionAgentDepth(ctx, sessions, sessionID, requested)
}

// The "delegation waits for an answer" state (#1157, A30), batch edition: a
// delegated child that called ask_question while its delegation is still held
// is visible to the parent through the batch held-question facts
// (BatchCutoverFacts.LiveHeldQuestions, the fenced current-incarnation rows)
// and answers through the batch held-answer bridge
// (deliverBatchHeldAnswer, coordinator_agent_inject_batch.go). This file keeps
// the supervision/inspect surface: the per-delegation AWAITING-YOUR-ANSWER
// summary line, derived from history plus batch facts.
// childAwaitingQuestionSummary is supervision's per-delegation line for a
// delegation whose child is paused on a question. ok=false when the child is
// not question-paused (busy, never asked, or its last finished turn did not
// end on a question stop). The delegation's identity is the batch delegation
// node that names childID (its tool call id, read from the parent's live
// delegation facts); the running-own-work count is the child's own live batch
// roots.
func (c *coordinator) childAwaitingQuestionSummary(childID string, elapsed time.Duration) (line string, ok bool) {
	if c == nil || childID == "" {
		return "", false
	}
	if ag := c.agentFor(childID); ag != nil && ag.IsSessionBusy(childID) {
		return "", false
	}
	msg, awaiting := c.childLastActivity(context.Background(), childID)
	if !awaiting {
		return "", false
	}
	question := ""
	if msg != nil {
		if block, is := subAgentQuestionBlockFromFinish(msg.FinishPart()); is {
			question = block
		}
	}
	if question == "" {
		return "", false
	}
	question = delegation.TrimChildQuestion(question)
	delegation := childID
	ownJobs := 0
	facts := coordinatorBatchCutoverFacts(c)
	if parent := c.childParentSession(childID); parent != "" {
		if nodes, err := facts.LiveDelegationNodes(context.Background(), parent); err == nil {
			for _, node := range nodes {
				if node.ChildSessionID == childID {
					if node.ToolCallID != "" {
						delegation = node.ToolCallID
					}
					break
				}
			}
		}
	}
	if own, _, err := facts.LiveWorkSummary(context.Background(), childID); err == nil {
		ownJobs = own
	}
	return fmt.Sprintf(
		"- %s (sub-agent, child session %s): AWAITING YOUR ANSWER for %s -- it asked: %s. Its own %d task(s) still run. Answer: agent(resume_session_id=%q, prompt=\"...\"); give up: stop_agent(child_session_id=%q).",
		delegation, childID, elapsed.Round(time.Second), question, ownJobs, childID, childID,
	), true
}

// childParentSession resolves childID's parent session id ("" when the
// session is unreadable or has no parent).
func (c *coordinator) childParentSession(childID string) string {
	if c == nil || c.sessions == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := c.sessions.Get(ctx, childID)
	if err != nil {
		return ""
	}
	return sess.ParentSessionID
}

// validateAgentRoleInput checks model-only control fields before batch admission.
func validateAgentRoleInput(input json.RawMessage) error {
	var fields struct {
		Role RequestedModelRole `json:"role"`
	}
	if err := json.Unmarshal(input, &fields); err != nil {
		return fmt.Errorf("invalid agent role input: %w", err)
	}
	return nil
}

func (c *coordinator) refreshRequestedDelegationCall(ctx context.Context, parentID string, call SessionAgentCall) (SessionAgentCall, error) {
	ctx = withCallCredentials(ctx, call.Credentials)
	if call.CallOptions != nil {
		ctx = WithCallOptions(ctx, call.CallOptions)
	}
	if call.RoleContract != nil {
		if call.CallOptions == nil {
			call.CallOptions = &CallOptions{}
		}
		call.CallOptions.RoleContract = agentcall.CloneRoleContractForCall(call.RoleContract)
		ctx = WithCallOptions(ctx, call.CallOptions)
	}
	if call.CallOptions == nil || call.CallOptions.RoleContract == nil {
		if contract, contractErr := loadChildRoleContract(ctx, c.sessions, call.SessionID); contractErr != nil {
			return SessionAgentCall{}, contractErr
		} else if contract != nil {
			ctx = WithCallRoleContract(ctx, contract)
			call.CallOptions = callOptionsFrom(ctx)
		}
	}
	cfg, _ := c.cfg.Snapshot()
	selected, err := c.resolveRequestedDelegationModel(ctx, cfg, parentID, call.RequestedModelRole)
	if err != nil {
		return SessionAgentCall{}, err
	}
	if call.CallOptions == nil {
		call.CallOptions = callOptionsFrom(ctx)
	} else if call.CallOptions.RoleContract == nil {
		call.CallOptions = callOptionsFrom(ctx)
	}
	call = drainctl.ApplyDrainModelOptions(call, selected.Model)
	call.ProviderOptions = modelkit.GetProviderOptions(call.SessionID, selected.Model, selected.Provider)
	call.RequestedModelSource = selected.Source
	return call, nil
}

// restoreRequestedDelegationCall validates the role without changing call authority.
func (c *coordinator) restoreRequestedDelegationCall(ctx context.Context, call SessionAgentCall) (SessionAgentCall, error) {
	if call.RoleContract != nil {
		call.CallOptions = WithRoleContractCopy(call.CallOptions, call.RoleContract)
		ctx = WithCallOptions(ctx, call.CallOptions)
	} else if call.CallOptions == nil || call.CallOptions.RoleContract == nil {
		contract, err := loadChildRoleContract(ctx, c.sessions, call.SessionID)
		if err != nil {
			return SessionAgentCall{}, err
		}
		if contract != nil {
			call.CallOptions = WithRoleContractCopy(call.CallOptions, contract)
			call.RoleContract = contract.Clone()
			ctx = WithCallOptions(ctx, call.CallOptions)
		}
	}
	if call.RequestedModelRole != "" && !call.RequestedModelRole.Valid() {
		return SessionAgentCall{}, ErrAgentRoleInvalid
	}
	if call.RequestedModelRole != "" && !call.RequestedModelRole.Builtin() {
		if call.CallOptions == nil || call.CallOptions.RoleContract == nil || call.CallOptions.RoleContract.Name != string(call.RequestedModelRole) {
			return SessionAgentCall{}, ErrAgentRoleUnconfigured
		}
		stored, err := loadChildRoleContract(ctx, c.sessions, call.SessionID)
		if err != nil {
			return SessionAgentCall{}, err
		}
		if stored == nil || !stored.Equal(call.CallOptions.RoleContract) {
			return SessionAgentCall{}, ErrAgentRoleMismatch
		}
		child, err := c.sessions.Get(ctx, call.SessionID)
		if err != nil {
			return SessionAgentCall{}, err
		}
		if child.ParentSessionID == "" {
			return SessionAgentCall{}, fmt.Errorf("%w: delegated role has no parent", ErrAgentRoleMismatch)
		}
		if call.Credentials == nil && c.drainCredentialBoundary(ctx, child.ID) {
			return SessionAgentCall{}, diskport.ErrCredentialBindingUnavailable
		}
		if call.SmartModel == nil {
			return c.refreshRequestedDelegationCall(ctx, child.ParentSessionID, call)
		}
		return call, nil
	}
	store, supported := c.sessions.(session.AgentModelRoleStore)
	if !supported {
		if call.RequestedModelRole != "" {
			return SessionAgentCall{}, fmt.Errorf("%w: model-role persistence unavailable", ErrAgentRoleUnconfigured)
		}
		return call, nil
	}
	saved, _, err := store.GetAgentModelRole(ctx, call.SessionID)
	if err != nil {
		return SessionAgentCall{}, err
	}
	if call.RequestedModelRole != "" && string(call.RequestedModelRole) != saved {
		return SessionAgentCall{}, ErrAgentRoleMismatch
	}
	if saved == "" {
		return call, nil
	}
	call.RequestedModelRole = RequestedModelRole(saved)
	if !call.RequestedModelRole.Valid() {
		return SessionAgentCall{}, ErrAgentRoleInvalid
	}
	child, err := c.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return SessionAgentCall{}, err
	}
	if child.ParentSessionID == "" {
		return SessionAgentCall{}, fmt.Errorf("%w: delegated role has no parent", ErrAgentRoleMismatch)
	}
	if call.Credentials == nil && c.drainCredentialBoundary(ctx, child.ID) {
		return SessionAgentCall{}, diskport.ErrCredentialBindingUnavailable
	}
	return c.refreshRequestedDelegationCall(ctx, child.ParentSessionID, call)
}

func (c *coordinator) refuseLostRequestedRoleDriver(ctx context.Context, id string) error {
	store, supported := c.sessions.(session.AgentModelRoleStore)
	if !supported {
		return nil
	}
	role, _, err := store.GetAgentModelRole(ctx, id)
	if err != nil {
		return err
	}
	if role != "" {
		return fmt.Errorf("%w: delegated model-role driver unavailable", ErrAgentRoleUnconfigured)
	}
	return nil
}

type delegationRoleModel = delegation.DelegationRoleModel

// requestedRoleSlot remains the EX-0L built-in-slot helper.
func requestedRoleSlot(cfg *config.Config, parent session.Session, role RequestedModelRole) (config.SelectedModel, string, error) {
	if !role.Valid() || !role.Builtin() {
		return config.SelectedModel{}, "", ErrAgentRoleInvalid
	}
	selected := cfg.Models[config.SelectedModelType(role)]
	var provider, model, effort string
	switch config.SelectedModelType(role) {
	case config.SelectedModelTypeSmart:
		provider, model, effort = parent.SmartModelProvider, parent.SmartModelID, parent.SmartModelReasoningEffort
	case config.SelectedModelTypeFast:
		provider, model, effort = parent.FastModelProvider, parent.FastModelID, parent.FastModelReasoningEffort
	case config.SelectedModelTypeWorker:
		provider, model, effort = parent.WorkerModelProvider, parent.WorkerModelID, parent.WorkerModelReasoningEffort
	case config.SelectedModelTypeReviewer:
		provider, model, effort = parent.ReviewerModelProvider, parent.ReviewerModelID, parent.ReviewerModelReasoningEffort
	}
	source := callbeat.HbSourceSlotLive
	if provider != "" || model != "" {
		if provider == "" || model == "" {
			return config.SelectedModel{}, "", fmt.Errorf("%w: incomplete session slot for %s", ErrAgentRoleUnconfigured, role)
		}
		selected.Provider, selected.Model, selected.ReasoningEffort = provider, model, effort
		source = callbeat.HbSourceSessionOverride
	}
	if selected.Provider == "" || selected.Model == "" {
		return config.SelectedModel{}, "", fmt.Errorf("%w: %s", ErrAgentRoleUnconfigured, role)
	}
	return selected, source, nil
}

func (c *coordinator) resolveRequestedDelegationModel(ctx context.Context, cfg *config.Config, parentID string, role RequestedModelRole) (delegationRoleModel, error) {
	if contract := modelkit.SelectedRoleContractFrom(ctx); contract != nil {
		if contract.Name == "" || contract.Name != string(role) {
			return delegationRoleModel{}, ErrAgentRoleMismatch
		}
		if !role.Builtin() {
			return c.resolveRoleContractModel(ctx, cfg, parentID, contract)
		}
	}
	if !role.Valid() {
		return delegationRoleModel{}, ErrAgentRoleInvalid
	}
	if !role.Builtin() {
		contract, err := ResolveRoleContract(cfg, string(role), parentRoleContractFrom(ctx))
		if err != nil {
			return delegationRoleModel{}, err
		}
		return c.resolveRoleContractModel(ctx, cfg, parentID, contract)
	}
	creds := callCredentialsFrom(ctx)
	if creds != nil {
		if choice, covered := creds.Models[Role(role)]; covered {
			if choice.Provider == "" || choice.Model == "" {
				return delegationRoleModel{}, fmt.Errorf("%w: incomplete tenant %s slot", ErrAgentRoleUnconfigured, role)
			}
			model, provider, err := c.buildCredentialModel(ctx, cfg, creds, choice)
			if err != nil {
				return delegationRoleModel{}, fmt.Errorf("%w: tenant %s slot: %w", ErrAgentRoleUnconfigured, role, err)
			}
			return delegationRoleModel{Model: model, Provider: provider, Source: callbeat.HbSourcePerCall, Credentials: creds}, nil
		}
		if !creds.AllowConfiguredRoleFallback {
			return delegationRoleModel{}, fmt.Errorf("%w: tenant does not cover %s", ErrAgentRoleUnconfigured, role)
		}
	}
	if creds == nil && c.drainCredentialBoundary(ctx, parentID) {
		return delegationRoleModel{}, diskport.ErrCredentialBindingUnavailable
	}
	parent, err := c.sessions.Get(ctx, parentID)
	if err != nil {
		return delegationRoleModel{}, fmt.Errorf("resolve requested role parent: %w", err)
	}
	selected, source, err := requestedRoleSlot(cfg, parent, role)
	if err != nil {
		return delegationRoleModel{}, err
	}
	provider, ok := cfg.Providers.Get(selected.Provider)
	if !ok {
		return delegationRoleModel{}, fmt.Errorf("%w: provider for %s", ErrAgentRoleUnconfigured, role)
	}
	model, _, err := c.buildModelsFromCfg(ctx, cfg, selected, selected, true)
	if err != nil {
		return delegationRoleModel{}, fmt.Errorf("%w: %s model: %w", ErrAgentRoleUnconfigured, role, err)
	}
	return delegationRoleModel{Model: model, Provider: provider, Source: source, Credentials: creds}, nil
}

func (c *coordinator) resolveRoleContractModel(ctx context.Context, cfg *config.Config, parentID string, contract *session.RoleContract) (delegationRoleModel, error) {
	if contract == nil || contract.Validate() != nil {
		return delegationRoleModel{}, fmt.Errorf("%w: invalid custom role contract", ErrAgentRoleUnconfigured)
	}
	creds := callCredentialsFrom(ctx)
	if creds != nil {
		if choice, covered := creds.Models[Role(contract.Name)]; covered {
			if choice.Provider == "" || choice.Model == "" {
				return delegationRoleModel{}, fmt.Errorf("%w: incomplete tenant %s slot", ErrAgentRoleUnconfigured, contract.Name)
			}
			model, provider, buildErr := c.buildCredentialModel(ctx, cfg, creds, choice)
			if buildErr != nil {
				return delegationRoleModel{}, fmt.Errorf("%w: tenant %s slot: %w", ErrAgentRoleUnconfigured, contract.Name, buildErr)
			}
			return delegationRoleModel{Model: model, Provider: provider, Source: callbeat.HbSourcePerCall, Credentials: creds}, nil
		}
		if !creds.AllowConfiguredRoleFallback {
			return delegationRoleModel{}, fmt.Errorf("%w: tenant does not cover %s", ErrAgentRoleUnconfigured, contract.Name)
		}
	}
	selected, source, err := modelkit.CurrentOrSnapshotRoleModel(cfg, contract)
	if err != nil {
		return delegationRoleModel{}, err
	}
	if creds == nil && parentID != "" && c.drainCredentialBoundary(ctx, parentID) {
		return delegationRoleModel{}, diskport.ErrCredentialBindingUnavailable
	}
	provider, ok := cfg.Providers.Get(selected.Provider)
	if !ok {
		return delegationRoleModel{}, fmt.Errorf("%w: provider for %s", ErrAgentRoleUnconfigured, contract.Name)
	}
	model, _, err := c.buildModelsFromCfg(ctx, cfg, selected, selected, true)
	if err != nil {
		return delegationRoleModel{}, fmt.Errorf("%w: %s model: %w", ErrAgentRoleUnconfigured, contract.Name, err)
	}
	return delegationRoleModel{Model: model, Provider: provider, Source: source, Credentials: creds}, nil
}

func parentRoleConfigFrom(ctx context.Context) (*config.Config, bool) {
	return delegation.ParentRoleConfigFrom(ctx)
}

func WithParentRoleConfigSnapshot(ctx context.Context, cfg *config.Config) context.Context {
	return delegation.WithParentRoleConfigSnapshot(ctx, cfg)
}

// parentRoleContractFrom deliberately remains a documented graph exception:
// its authority must come from the root toolwrap accessor (role_policy.go:70).
func parentRoleContractFrom(ctx context.Context) *session.RoleContract {
	return toolwrap.RoleContractFromContext(ctx)
}

func resolveDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested RequestedModelRole) (RequestedModelRole, error) {
	return delegation.ResolveDelegationRequestedRole(ctx, sessions, childID, requested)
}

func bindDelegationRoleContract(ctx context.Context, sessions session.Service, childID string, contract *session.RoleContract) error {
	return delegation.BindDelegationRoleContract(ctx, sessions, childID, contract)
}

func bindDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested RequestedModelRole) error {
	return delegation.BindDelegationRequestedRole(ctx, sessions, childID, requested)
}
