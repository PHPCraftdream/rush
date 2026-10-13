package agent

import (
	"context"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// R11F (M3): flag-on tool assembly. The decorated tool slice is pinned into
// a fresh leaf registry and every batch-admissible WORK tool is re-armed so
// its invocation enters the ONE authority (batchRuntime.Invoke). The old
// async/stall wrappers are NOT installed on this path; hooks, restricted-run
// gating and permissions run exactly once — inside the pinned leaf the
// executor resolves.

// batchWorkTarget reports whether a constructed tool is a batch-admissible
// work leaf: the D1 complement (never a control-plane tool) restricted to
// the families the R11A admission validator can classify. Tools outside
// every family (todos, job_output, rush_info, ...) keep their direct pinned
// leaf — forcing them through the ingress would only convert every call
// into errBatchUnknownTool.
func batchWorkTarget(name string) bool {
	if batchControlTarget(name) {
		return false
	}
	if name == AgentToolName || name == tools.AgenticFetchToolName {
		return true
	}
	if batchIOTools[name] {
		return true
	}
	return strings.HasPrefix(name, "mcp_")
}

// batchIngressTool is the model-facing shell of one batch-armed work tool.
// Run resolves the pinned leaf (hooks/restricted wrappers retained,
// execution wrappers stripped) and enters the ONE authority.
type batchIngressTool struct {
	fantasy.AgentTool
	registry *policyLeafRegistry
	ingress  BatchWorkIngress
	inv      BatchInvocation
}

func (t *batchIngressTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	leaf, err := t.registry.resolve(call.Name)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return invokeBatchWithCallContext(ctx, t.ingress, t.inv, leaf, call)
}

// errBatchIngressTool fails closed: the ingress could not be armed (pin
// failure), so the tool refuses instead of falling back to the old path.
type errBatchIngressTool struct {
	fantasy.AgentTool
	err error
}

func (t errBatchIngressTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, t.err
}

// newBatchWorkToolBuild pins the decorated slice and re-arms the work tools.
// Per-build state: ONE fresh pinned registry identity per construction; the
// per-invocation delivery choice follows the family — bash keeps its
// auto-background window (sync + escape auto + the inline window), every
// other family is sync with escape never.
func newBatchWorkToolBuild(bind *batchCoordinatorBinding, list []fantasy.AgentTool) (policyToolBuild, error) {
	built := newPolicyToolBuild(list)
	registryID, err := bind.pin(built.leaves)
	if err != nil {
		return policyToolBuild{}, err
	}
	fingerprint := batchToolsetFingerprint(list)
	if fingerprint == "" {
		fingerprint = registryID
	}
	for i, tool := range built.tools {
		name := tool.Info().Name
		if !batchWorkTarget(name) {
			continue
		}
		inv := BatchInvocation{
			RegistryID:       registryID,
			Origin:           "coordinator",
			Delivery:         batchDeliveryChoice{Mode: batchDeliverySync},
			BackgroundEscape: batchEscapeNever,
			Policy: batchLeafPolicyContext{
				RegistryID:        registryID,
				RestrictedToolset: fingerprint,
			},
		}
		if name == tools.BashToolName {
			// Auto-background keeps its window: escape switches DELIVERY of
			// the existing root after the window, never its execution.
			inv.BackgroundEscape = batchEscapeAuto
			inv.Delivery.Window = inlineWindow.String()
		}
		built.tools[i] = &batchIngressTool{AgentTool: tool, registry: built.leaves, ingress: bind.ingress, inv: inv}
	}
	return built, nil
}

// batchToolsetFingerprint is the restricted-toolset identity of one build:
// the sorted tool names. Two builds with different toolsets carry different
// pinned registries AND different fingerprints.
func batchToolsetFingerprint(list []fantasy.AgentTool) string {
	names := make([]string, 0, len(list))
	for _, tool := range list {
		names = append(names, tool.Info().Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
