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
// Completeness inventory (R11F round-2), per non-wrapped tool of the D1
// complement:
//
//   - todos: (iii) WORK — it MUTATES session state (writes the session's
//     todo list that the worker turns read back). It IS wrapped below: it
//     is batch-admissible (batchIOTools), so its invocation enters the ONE
//     authority like any other state-changing leaf. The admission validator
//     classifies it in the io group.
//   - job_output, job_kill: (i) D1-ADJACENT LEGACY CONTROLS — the legacy
//     async-ledger read/kill surfaces slated for removal in R12/D6; they
//     are control-plane (batchControlTarget/BatchNonWorkTarget) and are
//     never batch-wrapped. Anchor: the legacy workLedger
//     (async_job_bgshell.go / coordinator recheck loop) they operate on.
//   - rush_info, rush_logs: (ii) NOT WORK — pure read-only host/process
//     introspection with no external effect and no session-state mutation;
//     direct pinned leaf.
//   - read_delegation_transcript: (ii) NOT WORK — read-only transcript
//     inspection; direct pinned leaf.
//   - list_mcp_resources, read_mcp_resource: (ii) NOT WORK — read-only MCP
//     resource listing/reading (no execution, no external mutation); direct
//     pinned leaf.
//   - ask-adjacent (ask_question and every wake/loop/inspect/inject
//     control): (i) D1 control (batchControlTarget) — never wrapped.
//
// Every category-(iii) tool above is covered by the batchIOTools /
// agent / agentic_fetch / mcp_ admission below; if a future WORK tool is
// added, it MUST join one of those families or it silently keeps a direct
// pinned leaf (this comment is the review anchor).
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
	built.registryID = registryID
	fingerprint := batchToolsetFingerprint(list)
	if fingerprint == "" {
		fingerprint = registryID
	}
	for i, tool := range built.tools {
		name := tool.Info().Name
		if name == tools.AskQuestionToolName {
			built.tools[i] = batchAgentAskQuestionTool{AgentTool: tool}
			continue
		}
		if !batchWorkTarget(name) {
			continue
		}
		inv := BatchInvocation{
			RegistryID:       registryID,
			Origin:           "coordinator",
			Delivery:         batchDeliveryChoice{Mode: batchDeliverySync},
			BackgroundEscape: batchEscapeNever,
			WaiterRank:       0,
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
