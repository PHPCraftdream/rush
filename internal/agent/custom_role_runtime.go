package agent

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// ResolveRoleContract is an admission-only resolver. It validates a custom
// role's exact configured definition and returns a detached versioned snapshot.
func ResolveRoleContract(cfg *config.Config, name string, inherited *session.RoleContract) (*session.RoleContract, error) {
	if err := config.ValidateCustomRoleName(name); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAgentRoleInvalid, err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("%w: custom role configuration unavailable", ErrAgentRoleUnconfigured)
	}
	definition, ok := cfg.Roles[name]
	if !ok {
		return nil, fmt.Errorf("%w: custom role %q", ErrAgentRoleUnconfigured, name)
	}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("%w: custom role %q: %v", ErrAgentRoleUnconfigured, name, err)
	}
	if _, ok := cfg.Providers.Get(definition.Provider); !ok {
		return nil, fmt.Errorf("%w: provider %q for custom role %q", ErrAgentRoleUnconfigured, definition.Provider, name)
	}
	groups, err := inheritedRoleCeilings(inherited)
	if err != nil {
		return nil, err
	}
	contract := &session.RoleContract{
		Version:   session.RoleContractVersion,
		Name:      name,
		Model:     toSessionModelCfg(cloneSelectedModel(definition.SelectedModel)),
		Tools:     cloneRoleTools(definition.Tools),
		Inherited: groups,
	}
	if err := contract.Validate(); err != nil {
		return nil, fmt.Errorf("%w: custom role %q contract: %v", ErrAgentRoleUnconfigured, name, err)
	}
	return contract, nil
}

func inheritedRoleCeilings(parent *session.RoleContract) ([][]string, error) {
	if parent == nil {
		return nil, nil
	}
	if err := parent.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid inherited role contract: %v", ErrAgentRoleUnconfigured, err)
	}
	groups := make([][]string, 0, len(parent.Inherited)+1)
	for _, group := range parent.Inherited {
		groups = append(groups, append([]string{}, group...))
	}
	if parent.Tools != nil {
		groups = append(groups, append([]string{}, (*parent.Tools)...))
	}
	return groups, nil
}

func cloneRoleTools(values *[]string) *[]string {
	if values == nil {
		return nil
	}
	copy := append([]string{}, (*values)...)
	return &copy
}

func cloneSelectedModel(model config.SelectedModel) config.SelectedModel {
	model.Temperature = cloneFloat64(model.Temperature)
	model.TopP = cloneFloat64(model.TopP)
	model.TopK = cloneInt64(model.TopK)
	model.FrequencyPenalty = cloneFloat64(model.FrequencyPenalty)
	model.PresencePenalty = cloneFloat64(model.PresencePenalty)
	model.ProviderOptions = cloneOptionMap(model.ProviderOptions)
	return model
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneOptionMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = cloneOptionValue(value)
	}
	return out
}

func cloneOptionValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneOptionMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneOptionValue(item)
		}
		return out
	case []string:
		return append([]string{}, typed...)
	default:
		return value
	}
}

func roleContractPromptSource(cfg *config.Config, contract *session.RoleContract) (config.SelectedModel, string, error) {
	if cfg == nil || contract == nil || contract.Name == "" {
		return currentOrSnapshotRoleModel(cfg, contract)
	}
	if err := contract.Validate(); err != nil {
		return config.SelectedModel{}, "", fmt.Errorf("%w: invalid custom role contract: %v", ErrAgentRoleUnconfigured, err)
	}
	if definition, ok := cfg.Roles[contract.Name]; ok {
		if err := definition.Validate(); err != nil {
			return config.SelectedModel{}, "", fmt.Errorf("%w: custom role %q: %v", ErrAgentRoleUnconfigured, contract.Name, err)
		}
		if _, ok := cfg.Providers.Get(definition.Provider); !ok {
			return config.SelectedModel{}, "", fmt.Errorf("%w: provider %q for custom role %q", ErrAgentRoleUnconfigured, definition.Provider, contract.Name)
		}
		return cloneSelectedModel(definition.SelectedModel), hbSourceSlotLive, nil
	}
	return currentOrSnapshotRoleModel(cfg, contract)
}

func currentOrSnapshotRoleModel(cfg *config.Config, contract *session.RoleContract) (config.SelectedModel, string, error) {
	if cfg == nil || contract == nil || contract.Name == "" {
		return config.SelectedModel{}, "", fmt.Errorf("%w: custom role contract is missing", ErrAgentRoleUnconfigured)
	}
	if err := contract.Validate(); err != nil {
		return config.SelectedModel{}, "", fmt.Errorf("%w: invalid custom role contract: %v", ErrAgentRoleUnconfigured, err)
	}
	selected := fromSessionModelCfg(contract.Model)
	if selected.Provider == "" || selected.Model == "" {
		return config.SelectedModel{}, "", fmt.Errorf("%w: custom role snapshot is incomplete", ErrAgentRoleUnconfigured)
	}
	if _, ok := cfg.Providers.Get(selected.Provider); !ok {
		return config.SelectedModel{}, "", fmt.Errorf("%w: snapshot provider %q for custom role %q", ErrAgentRoleUnconfigured, selected.Provider, contract.Name)
	}
	return cloneSelectedModel(selected), hbSourceSessionOverride, nil
}

func cloneRoleContractFromCallOptions(opts *CallOptions) *session.RoleContract {
	if opts == nil {
		return nil
	}
	return cloneRoleContractForCall(opts.RoleContract)
}

func cloneRoleContractForCall(contract *session.RoleContract) *session.RoleContract {
	if contract == nil {
		return nil
	}
	return contract.Clone()
}

func roleContractAllowsTool(contract *session.RoleContract, name string) bool {
	return roleContractAllowsName(contract, name)
}

func copyInheritedRoleContract(parent *session.RoleContract) (*session.RoleContract, error) {
	groups, err := inheritedRoleCeilings(parent)
	if err != nil {
		return nil, err
	}
	contract := &session.RoleContract{Version: session.RoleContractVersion, Inherited: groups}
	if err := contract.Validate(); err != nil {
		return nil, fmt.Errorf("%w: inherited role contract: %v", ErrAgentRoleUnconfigured, err)
	}
	return contract, nil
}

func roleContractWithName(model config.SelectedModel, name string, inherited *session.RoleContract) *session.RoleContract {
	groups, err := inheritedRoleCeilings(inherited)
	if err != nil {
		return nil
	}
	contract := &session.RoleContract{Version: session.RoleContractVersion, Name: name, Model: toSessionModelCfg(cloneSelectedModel(model)), Inherited: groups}
	if contract.Validate() != nil {
		return nil
	}
	return contract
}

// WithSelectedRoleContract transports a batch-admitted immutable role choice.
func WithSelectedRoleContract(ctx context.Context, contract *session.RoleContract) context.Context {
	return context.WithValue(ctx, selectedRoleContractContextKey{}, cloneRoleContractForCall(contract))
}

type selectedRoleContractContextKey struct{}

func selectedRoleContractFrom(ctx context.Context) *session.RoleContract {
	contract, _ := ctx.Value(selectedRoleContractContextKey{}).(*session.RoleContract)
	return contract
}

// WithCallRoleContract transports a detached immutable role snapshot through a call context.
func WithCallRoleContract(ctx context.Context, contract *session.RoleContract) context.Context {
	opts := callOptionsFrom(ctx)
	return WithCallOptions(ctx, WithRoleContractCopy(opts, contract))
}

// WithRoleContractCopy returns options with a detached contract, preserving the
// call's resource scope and other execution policy.
func WithRoleContractCopy(opts *CallOptions, contract *session.RoleContract) *CallOptions {
	if opts == nil {
		opts = &CallOptions{}
	} else {
		copy := *opts
		opts = &copy
	}
	opts.RoleContract = cloneRoleContractForCall(contract)
	return opts
}
