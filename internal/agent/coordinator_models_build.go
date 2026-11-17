package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	return newModelsComponent(c).buildAgentModels(ctx, isSubAgent)
}

// buildAgentModelsFromCfg is buildAgentModels' body, taking the config
// snapshot as a parameter instead of capturing its own (task #576/P1-3).
// buildAgent (coordinator_tools.go) pins ONE *config.Config for its entire
// build -- models, prompt, and tools -- and passes it here so the models it
// builds always agree with the prompt/toolset built from the same call. A
// caller with nothing to pin (buildAgentModels above) takes its own fresh
// Snapshot() and delegates here, so behavior for those callers is unchanged.
func (c *coordinator) buildAgentModelsFromCfg(ctx context.Context, cfg *config.Config, isSubAgent bool) (Model, Model, error) {
	return newModelsComponent(c).buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
}

func (c *coordinator) buildRoleContractModels(ctx context.Context, cfg *config.Config, contract *session.RoleContract) (Model, Model, error) {
	return newModelsComponent(c).buildRoleContractModels(ctx, cfg, contract)
}

// buildModelsFromCfg builds Model objects from explicit SelectedModel configs.
// The cfg parameter must be from a single atomic Snapshot() call to ensure
// consistency across all provider reads (task #341, P1-3).
func (c *coordinator) buildModelsFromCfg(ctx context.Context, cfg *config.Config, smartModelCfg, fastModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	return newModelsComponent(c).buildModelsFromCfg(ctx, cfg, smartModelCfg, fastModelCfg, isSubAgent)
}

// Model returns the globally configured smart model from config.
// This is used for display/status queries and does NOT reflect any per-session
// model overrides. After the per-session model isolation fix, callers that need
// the actual model for a specific session should use resolveSessionModels instead.
func (c *coordinator) Model() Model { return newModelsComponent(c).Model() }

// updateModelsReadyGateSeam is a test hook after registration and before the
// local drain, used to force cancellation/failure without affecting admission.
var updateModelsReadyGateSeam func(context.Context, *readyGate)

// UpdateModels refreshes the coordinator-wide default agent (models, prompt
// prefix, toolset) from the LATEST config: config reloads (SetupAgents) and
// credential refreshes use it to republish clients and tools — including
// live MCP tool-list changes — for LEGACY callers whose turns still read the
// shared state. It is NOT a per-run hook (R3-1): any CallOptions carried by
// ctx are stripped first, so a single run's DisableSubAgents/ModelRole can
// never decide what every other in-flight turn sees. Per-call toolsets are
// built and pinned by resolveSessionModels/applyModelOverrides instead.
func (c *coordinator) UpdateModels(ctx context.Context) error {
	return newModelsComponent(c).UpdateModels(ctx)
}

// clearModelCache empties the model cache, forcing the next resolveSessionModels
// call to rebuild models from the current config. This is called after credential
// updates (OAuth token refresh, API key re-resolution) to ensure cached models
// with stale clients are not reused (task #341, P1-3).
func (c *coordinator) clearModelCache() { newModelsComponent(c).clearModelCache() }
