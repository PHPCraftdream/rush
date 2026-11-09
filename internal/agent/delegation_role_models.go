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

// requestedRoleSlot reads one profile from an already pinned config snapshot.
func requestedRoleSlot(cfg *config.Config, parent session.Session, role RequestedModelRole) (config.SelectedModel, string, error) {
	if !role.valid() {
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
	if !role.valid() {
		return delegationRoleModel{}, ErrAgentRoleInvalid
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
