package mcp

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/config"
)

// Trusted-UI entry points.
//
// Every TrustedUI function below runs the exact same lifecycle machinery as
// its non-trusted counterpart: the same Owner identity and ConfigStore
// binding, leases, admission, epoch fencing, pending-add handling, uncertainty
// checks, and rollback semantics. The only difference is that the durable
// persistence call carries config.SettingsAuthorityTrustedUI, which lifts only
// the settings-lock veto. A failed persistence still preserves the runtime
// exactly as before (persist-before-invalidate).

// enableMCPConfigWithAuthority is enableMCPConfig with an explicit write
// authority for its durable persisters.
func enableMCPConfigWithAuthority(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig, authority config.SettingsWriteAuthority) (config.MCPMutationResult, error) {
	if pending != nil {
		return cfg.PersistMCPConfigResultWithAuthority(scope, name, *pending, authority)
	}
	return cfg.PersistMCPDisabledOverrideResultWithAuthority(scope, name, false, authority)
}

// enableMCPConfigTrustedUI persists an enable with trusted-UI write
// authority, which lifts only the settings-lock veto.
func enableMCPConfigTrustedUI(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
	return enableMCPConfigWithAuthority(cfg, scope, name, pending, config.SettingsAuthorityTrustedUI)
}

// DisableServerTrustedUI disables an MCP server using the same lifecycle
// machinery as Owner.DisableServer; only the durable persistence carries the
// trusted-UI write authority, which lifts only the settings-lock veto. A
// failed persistence still preserves the runtime exactly as before
// (persist-before-invalidate).
func DisableServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, name string) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return disableServerWithResultPersistenceForOwner(o, ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			if pending != nil {
				return cfg.PersistMCPConfigResultWithAuthority(scope, name, *pending, config.SettingsAuthorityTrustedUI)
			}
			return cfg.PersistMCPDisabledOverrideResultWithAuthority(scope, name, true, config.SettingsAuthorityTrustedUI)
		})
}

// DisableServerTrustedUI disables an MCP server using the same lifecycle
// machinery as the non-trusted DisableServer; only the durable persistence
// carries the trusted-UI write authority, which lifts only the settings-lock
// veto. A failed persistence still preserves the runtime exactly as before
// (persist-before-invalidate). A nil receiver resolves the process-current
// owner exactly like the package-level DisableServerTrustedUI function, so
// callers holding an optional owner can call the method unconditionally.
func (o *Owner) DisableServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, name string) error {
	if o == nil {
		return DisableServerTrustedUI(ctx, cfg, name)
	}
	return disableServerWithResultPersistenceForOwner(o, ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			if pending != nil {
				return cfg.PersistMCPConfigResultWithAuthority(scope, name, *pending, config.SettingsAuthorityTrustedUI)
			}
			return cfg.PersistMCPDisabledOverrideResultWithAuthority(scope, name, true, config.SettingsAuthorityTrustedUI)
		})
}

// EnableServerTrustedUI re-enables a disabled MCP server using the same
// lifecycle machinery as Owner.EnableServer, including the token-conditional
// enable rollback; only the durable persistence carries the trusted-UI write
// authority, which lifts only the settings-lock veto. A failed persistence
// still preserves the runtime exactly as before (persist-before-invalidate).
func EnableServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, name string) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return enableServerWithPersistenceAndInitializerAndRollbackForOwner(o, ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			return enableMCPConfigTrustedUI(cfg, scope, name, pending)
		}, initClientAdmitted, nil, config.SettingsAuthorityTrustedUI)
}

// EnableServerTrustedUI re-enables a disabled MCP server using the same
// lifecycle machinery as the non-trusted EnableServer, including the
// token-conditional enable rollback; only the durable persistence carries the
// trusted-UI write authority, which lifts only the settings-lock veto. A
// failed persistence still preserves the runtime exactly as before
// (persist-before-invalidate). A nil receiver resolves the process-current
// owner exactly like the package-level EnableServerTrustedUI function, so
// callers holding an optional owner can call the method unconditionally.
func (o *Owner) EnableServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, name string) error {
	if o == nil {
		return EnableServerTrustedUI(ctx, cfg, name)
	}
	return enableServerWithPersistenceAndInitializerAndRollbackForOwner(o, ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			return enableMCPConfigTrustedUI(cfg, scope, name, pending)
		}, initClientAdmitted, nil, config.SettingsAuthorityTrustedUI)
}

// AddServerTrustedUI validates and adds a new MCP server using the same
// lifecycle machinery as Owner.AddServer; only the durable persistence
// carries the trusted-UI write authority, which lifts only the settings-lock
// veto. A failed persistence still preserves the runtime exactly as before
// (persist-before-invalidate).
func AddServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return addServerWithPreparationAndPersistenceForOwner(o, ctx, cfg, name, mcpCfg, prepareClient,
		func(cfg *config.ConfigStore, scope config.Scope, name string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistMCPConfigResultWithAuthority(scope, name, mcpCfg, config.SettingsAuthorityTrustedUI)
		})
}

// AddServerTrustedUI validates and adds a new MCP server using the same
// lifecycle machinery as the non-trusted AddServer; only the durable
// persistence carries the trusted-UI write authority, which lifts only the
// settings-lock veto. A failed persistence still preserves the runtime
// exactly as before (persist-before-invalidate). A nil receiver resolves the
// process-current owner exactly like the package-level AddServerTrustedUI
// function, so callers holding an optional owner can call the method
// unconditionally.
func (o *Owner) AddServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) error {
	if o == nil {
		return AddServerTrustedUI(ctx, cfg, name, mcpCfg)
	}
	return addServerWithPreparationAndPersistenceForOwner(o, ctx, cfg, name, mcpCfg, prepareClient,
		func(cfg *config.ConfigStore, scope config.Scope, name string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistMCPConfigResultWithAuthority(scope, name, mcpCfg, config.SettingsAuthorityTrustedUI)
		})
}

// ReplaceServerTrustedUI prepares a new MCP session completely before changing
// the configured server, using the same lifecycle machinery as
// Owner.ReplaceServer; only the durable persistence carries the trusted-UI
// write authority, which lifts only the settings-lock veto. A failed
// persistence still preserves the runtime exactly as before
// (persist-before-invalidate).
func ReplaceServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return replaceServerWithResultPersistenceAndPreparationForOwner(o, ctx, cfg, oldName, newName, mcpCfg,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistReplaceMCPResultWithAuthority(scope, oldName, newName, mcpCfg, config.SettingsAuthorityTrustedUI)
		}, prepareClient)
}

// ReplaceServerTrustedUI prepares a new MCP session completely before changing
// the configured server, using the same lifecycle machinery as the
// non-trusted ReplaceServer; only the durable persistence carries the
// trusted-UI write authority, which lifts only the settings-lock veto. A
// failed persistence still preserves the runtime exactly as before
// (persist-before-invalidate). A nil receiver resolves the process-current
// owner exactly like the package-level ReplaceServerTrustedUI function, so
// callers holding an optional owner can call the method unconditionally.
func (o *Owner) ReplaceServerTrustedUI(ctx context.Context, cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
	if o == nil {
		return ReplaceServerTrustedUI(ctx, cfg, oldName, newName, mcpCfg)
	}
	return replaceServerWithResultPersistenceAndPreparationForOwner(o, ctx, cfg, oldName, newName, mcpCfg,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistReplaceMCPResultWithAuthority(scope, oldName, newName, mcpCfg, config.SettingsAuthorityTrustedUI)
		}, prepareClient)
}

// RemoveServerTrustedUI removes an MCP server using the same lifecycle
// machinery as Owner.RemoveServer, including the pending-add branch; only the
// durable persistence carries the trusted-UI write authority, which lifts
// only the settings-lock veto. A failed persistence still preserves the
// runtime exactly as before (persist-before-invalidate).
func RemoveServerTrustedUI(cfg *config.ConfigStore, name string) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return removeServerWithResultPersistenceForOwner(o, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
			if pending := pendingGlobalAddFor(cfg, name); pending != nil {
				return cfg.PersistRemovePendingMCPConfigResultWithAuthority(scope, name, config.SettingsAuthorityTrustedUI)
			}
			return cfg.PersistRemoveMCPConfigResultWithAuthority(scope, name, config.SettingsAuthorityTrustedUI)
		})
}

// RemoveServerTrustedUI removes an MCP server using the same lifecycle
// machinery as the non-trusted RemoveServer, including the pending-add
// branch; only the durable persistence carries the trusted-UI write
// authority, which lifts only the settings-lock veto. A failed persistence
// still preserves the runtime exactly as before (persist-before-invalidate).
// A nil receiver resolves the process-current owner exactly like the
// package-level RemoveServerTrustedUI function, so callers holding an
// optional owner can call the method unconditionally.
func (o *Owner) RemoveServerTrustedUI(cfg *config.ConfigStore, name string) error {
	if o == nil {
		return RemoveServerTrustedUI(cfg, name)
	}
	return removeServerWithResultPersistenceForOwner(o, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
			if pending := pendingGlobalAddFor(cfg, name); pending != nil {
				return cfg.PersistRemovePendingMCPConfigResultWithAuthority(scope, name, config.SettingsAuthorityTrustedUI)
			}
			return cfg.PersistRemoveMCPConfigResultWithAuthority(scope, name, config.SettingsAuthorityTrustedUI)
		})
}
