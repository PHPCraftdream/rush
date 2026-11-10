package agent

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

type delegationRoleModel struct {
	Model       Model
	Provider    config.ProviderConfig
	Source      string
	Credentials *CredentialSet
}

// requestedRoleSlot reads one built-in profile from an already pinned config snapshot.
func requestedRoleSlot(cfg *config.Config, parent session.Session, role RequestedModelRole) (config.SelectedModel, string, error) {
	if !role.valid() || !role.builtin() {
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
	source := hbSourceSlotLive
	if provider != "" || model != "" {
		if provider == "" || model == "" {
			return config.SelectedModel{}, "", fmt.Errorf("%w: incomplete session slot for %s", ErrAgentRoleUnconfigured, role)
		}
		selected.Provider, selected.Model, selected.ReasoningEffort = provider, model, effort
		source = hbSourceSessionOverride
	}
	if selected.Provider == "" || selected.Model == "" {
		return config.SelectedModel{}, "", fmt.Errorf("%w: %s", ErrAgentRoleUnconfigured, role)
	}
	return selected, source, nil
}

func (c *coordinator) resolveRequestedDelegationModel(ctx context.Context, cfg *config.Config, parentID string, role RequestedModelRole) (delegationRoleModel, error) {
	if contract := selectedRoleContractFrom(ctx); contract != nil {
		if contract.Name == "" || contract.Name != string(role) {
			return delegationRoleModel{}, ErrAgentRoleMismatch
		}
		return c.resolveRoleContractModel(ctx, cfg, parentID, contract)
	}
	if !role.valid() {
		return delegationRoleModel{}, ErrAgentRoleInvalid
	}
	if !role.builtin() {
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
			return delegationRoleModel{Model: model, Provider: provider, Source: hbSourcePerCall, Credentials: creds}, nil
		}
		if !creds.AllowConfiguredRoleFallback {
			return delegationRoleModel{}, fmt.Errorf("%w: tenant does not cover %s", ErrAgentRoleUnconfigured, role)
		}
	}
	if creds == nil && c.drainCredentialBoundary(ctx, parentID) {
		return delegationRoleModel{}, ErrCredentialBindingUnavailable
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
			return delegationRoleModel{Model: model, Provider: provider, Source: hbSourcePerCall, Credentials: creds}, nil
		}
		if !creds.AllowConfiguredRoleFallback {
			return delegationRoleModel{}, fmt.Errorf("%w: tenant does not cover %s", ErrAgentRoleUnconfigured, contract.Name)
		}
	}
	selected, source, err := currentOrSnapshotRoleModel(cfg, contract)
	if err != nil {
		return delegationRoleModel{}, err
	}
	if creds == nil && parentID != "" && c.drainCredentialBoundary(ctx, parentID) {
		return delegationRoleModel{}, ErrCredentialBindingUnavailable
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
	if cfg, ok := ctx.Value(parentRoleConfigContextKey{}).(*config.Config); ok && cfg != nil {
		return cfg, true
	}
	return nil, false
}

type parentRoleConfigContextKey struct{}

func WithParentRoleConfigSnapshot(ctx context.Context, cfg *config.Config) context.Context {
	return context.WithValue(ctx, parentRoleConfigContextKey{}, cfg)
}

func parentRoleContractFrom(ctx context.Context) *session.RoleContract {
	return roleContractFromContext(ctx)
}
