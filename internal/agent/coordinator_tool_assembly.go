package agent

import (
	"context"
	"fmt"
	"slices"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/hooks"
)

// finishToolAssembly is the final construction boundary. Flag OFF (no batch
// binding): the historical branch, byte-for-byte — async wrappers, then
// hooks/restrictions, then the stall-detach wrapper. Flag ON: hooks and
// restrictions run once here, then every batch-admissible work tool is
// re-armed through the ONE authority (batchRuntime.Invoke) and the old
// async/stall wrappers are NOT installed. allowed is the build's control
// grant list: batchControlAvailability over the final filtered tools and
// AllowedTools, computed once in buildPolicyTools.
func (c *coordinator) finishToolAssembly(ctx context.Context, filteredTools []fantasy.AgentTool, hookRunner *hooks.Runner, isSubAgent bool, allowed []string, configs ...*config.Config) (policyToolBuild, error) {
	var cfg *config.Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	if bind := coordinatorBatchBindingFor(c); bind != nil {
		allowedControls := allowed
		if roleToolsConstrained(ctx) {
			allowedControls = filterRoleToolNames(ctx, allowed)
		}
		filteredTools = withoutBatchControlPlaceholders(filteredTools)
		filteredTools = decoratePolicyTools(filteredTools, restrictedRunAuthorizer(c.permissions), hookRunner, isSubAgent)
		filteredTools = filterRoleContractTools(ctx, filteredTools)
		buildCtx := ctx
		if current := roleContractFromContext(ctx); current != nil {
			buildCtx = withRoleToolContract(buildCtx, current)
		}
		inheritedContracts := roleContractsForTools(filteredTools)
		if current := roleContractFromContext(buildCtx); current != nil && !hasRoleContract(inheritedContracts, current) {
			inheritedContracts = append(inheritedContracts, current.Clone())
		}
		roleContract := roleContractForToolBuild(buildCtx, filteredTools)
		if roleContract != nil {
			buildCtx = withRoleToolContract(buildCtx, roleContract)
		}
		scope := callBuildScopeFrom(ctx)
		if scope == nil {
			if opts := callOptionsFrom(ctx); opts != nil && tools.IsCustomDiskProvider(opts.DiskProvider) {
				return policyToolBuild{}, ErrDiskProviderUnavailable
			}
		}
		shared := scope == nil
		if shared {
			scope = newBuildResourceScope()
			defer scope.close()
		}
		failBuildScope := func(err error) (policyToolBuild, error) {
			if !shared {
				scope.close()
			}
			return policyToolBuild{}, err
		}
		var built policyToolBuild
		if shared {
			built = newPolicyToolBuildForContext(buildCtx, filteredTools)
			fingerprint := batchToolsetFingerprint(built.tools)
			for i := 0; i < len(built.tools); i++ {
				tool := built.tools[i]
				name := tool.Info().Name
				if !roleContractAllowsName(roleContract, name) {
					built.tools = append(built.tools[:i], built.tools[i+1:]...)
					i--
					continue
				}
				if name == tools.AskQuestionToolName {
					built.tools[i] = batchAgentAskQuestionTool{AgentTool: tool}
					continue
				}
				if !batchWorkTarget(name) {
					continue
				}
				inv := BatchInvocation{Origin: "coordinator", Delivery: batchFamilyDelivery(name), BackgroundEscape: batchEscapeNever, WaiterRank: 0, Policy: batchLeafPolicyContext{RestrictedToolset: fingerprint, RoleContract: cloneRoleContractForCall(roleContract)}}
				if name == tools.BashToolName {
					if inlineWindowDisabledFor(bind) {
						inv.Delivery = batchDeliveryChoice{Mode: batchDeliveryDetached}
					} else {
						inv.BackgroundEscape = batchEscapeAuto
						inv.Delivery.Window = inlineWindow.String()
					}
				}
				built.tools[i] = &batchIngressTool{AgentTool: tool, registry: built.leaves, ingress: bind.ingress, inv: inv, bind: bind}
			}
		} else {
			var err error
			built, err = newBatchWorkToolBuildForContext(buildCtx, bind, filteredTools, scope, false)
			if err != nil {
				return failBuildScope(err)
			}
		}
		if bind.controls != nil && !shared {
			effectiveControls := make([]string, 0, len(allowedControls))
			for _, name := range allowedControls {
				if roleContractAllowsName(roleContract, name) && roleContractsAllowName(inheritedContracts, name) {
					effectiveControls = append(effectiveControls, name)
				}
			}
			requiredControls := make([]string, 0, len(effectiveControls))
			for _, name := range []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"} {
				if slices.Contains(effectiveControls, name) {
					requiredControls = append(requiredControls, name)
				}
			}
			if len(effectiveControls) > 0 {
				registry, err := bind.ingress.Registry(built.registryID)
				if err != nil {
					return failBuildScope(err)
				}
				inv := BatchInvocation{
					RegistryID:       built.registryID,
					Origin:           "coordinator",
					Delivery:         batchDeliveryChoice{Mode: batchDeliveryDetached},
					BackgroundEscape: batchEscapeNever,
					WorkerStopScope:  isSubAgent,
					roleConfig:       cfg,
					Policy:           batchLeafPolicyContext{RegistryID: built.registryID, RestrictedToolset: batchToolsetFingerprint(built.tools), RoleContract: cloneRoleContractForCall(roleContract)},
				}
				inv.SessionID, inv.MessageID = tools.GetSessionFromContext(ctx), tools.GetMessageFromContext(ctx)
				inv.AgentDepth, inv.AgentDepthSet = DefaultAgentDepth, true
				if opts := callOptionsFrom(ctx); opts != nil && opts.AgentDepthSet {
					inv.AgentDepth = opts.MaxAgentDepth
				}
				inv.Policy.MessageID, inv.Policy.SupportsImages, inv.Policy.ModelName = inv.MessageID, tools.GetSupportsImagesFromContext(ctx), tools.GetModelNameFromContext(ctx)
				inv.WaiterRank = batchGroupRanks[batchGroupDefault]
				inv.Policy.FolderScope, inv.Policy.RunAllowlist = folderScopeSpecFrom(ctx), runAllowlistSpecFrom(ctx)
				if opts := callOptionsFrom(ctx); opts != nil && opts.DiskBindingID != "" {
					inv.Policy.DiskBindingID, inv.Policy.DiskProvider = opts.DiskBindingID, true
				}
				controls, err := bind.controls.InstallBatchModelControls(registry, inv)
				if err != nil {
					return failBuildScope(err)
				}
				for _, control := range controls {
					name := control.Info().Name
					if name == tools.AwaitTasksToolName || !slices.Contains(requiredControls, name) {
						continue
					}
					controlContracts := cloneRoleToolContracts(inheritedContracts)
					if roleContract != nil && !hasRoleContract(controlContracts, roleContract) {
						controlContracts = append(controlContracts, roleContract.Clone())
					}
					guard := roleGuardTool{AgentTool: control, contracts: controlContracts}
					built.tools = append(built.tools, wrapToolsWithHooks(wrapToolsWithRestrictedRun([]fantasy.AgentTool{guard}, restrictedRunAuthorizer(c.permissions)), hookRunner, isSubAgent)[0])
				}
			}
			built.tools = filterRoleContractTools(buildCtx, built.tools)
			for _, name := range requiredControls {
				present := false
				for _, tool := range built.tools {
					if tool.Info().Name == name {
						present = true
						break
					}
				}
				if !present {
					return failBuildScope(fmt.Errorf("batch control %q was not installed", name))
				}
			}
		}
		return built, nil
	}
	// Per-call pinned tool slices bypass sessionAgent.SetTools, so apply the
	// restricted-run gate at this assembly boundary as well as at the agent
	// boundary. This keeps every actual dispatch path fail-closed. Hooks are
	// applied outside this wrapper below so PreToolUse runs first.
	filteredTools = decoratePolicyTools(filteredTools, restrictedRunAuthorizer(c.permissions), hookRunner, isSubAgent)
	filteredTools = filterRoleContractTools(ctx, filteredTools)
	// The outer role guard must run before hooks and every other policy layer.
	return newPolicyToolBuild(filteredTools), nil
}
