package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Typed admission rejections. All are sentinels distinguishable via errors.Is.
var (
	errBatchControlPlaneLeaf        = errors.New("control-plane tools are never leaves")
	errBatchReentrantMCPUnannotated = errors.New("reentrant MCP tool lacks an admission annotation")
	errBatchUnknownTool             = errors.New("tool is not classifiable into a batch group")
	errBatchNilProfile              = errors.New("batch profile is nil or zero-value")
	errBatchGroupUncapped           = errors.New("batch group has no cap in the profile")
	errBatchGroupMismatch           = errors.New("declared group differs from the classified group")
	errBatchDepthExceeded           = errors.New("batch depth exceeds the budget")
	errBatchNodeLimitExceeded       = errors.New("batch node count exceeds the budget")
	errBatchMCPCapabilityUnknown    = errors.New("MCP tool capability is unresolved")
	errBatchRankOrder               = errors.New("dependency rank is not strictly below the waiter rank")
	errBatchSameRoot                = errors.New("waiter and dependency share a root")
	errBatchSharedGate              = errors.New("waiter and dependency share a Sequential gate or MaxParallel ancestor")
)

// batchIOTools is the closed set of generic tool names admitted into the io
// group. Enumerated from the New*Tool constructors and their ToolName
// constants in internal/agent/tools: view, glob, grep, ls, edit, write,
// multiedit, fetch, download, web_fetch, web_search, run_command, bash,
// sourcegraph, git_read, fs_read, fs_list, fs_find, fs_grep, fs_write,
// fs_write_lines, fs_replace, fs_delete. Control-plane names and delegation
// tools are absent; unknown generic names are rejected, never defaulted.
var batchIOTools = map[string]bool{
	"view":           true,
	"glob":           true,
	"grep":           true,
	"ls":             true,
	"edit":           true,
	"write":          true,
	"multiedit":      true,
	"fetch":          true,
	"download":       true,
	"web_fetch":      true,
	"web_search":     true,
	"run_command":    true,
	"bash":           true,
	"sourcegraph":    true,
	"git_read":       true,
	"fs_read":        true,
	"fs_list":        true,
	"fs_find":        true,
	"fs_grep":        true,
	"fs_write":       true,
	"fs_write_lines": true,
	"fs_replace":     true,
	"fs_delete":      true,
}

// batchMCPCapability describes an MCP tool for classification purposes.
type batchMCPCapability struct {
	Reentrant bool // This MCP tool can itself call back into Rush tools or wait on other roots.
	Annotated bool // Explicit admission annotation for a reentrant tool (set only after human/reviewer intent).
}

// batchMCPCapabilityResolver resolves the admission capability of an MCP
// tool. R11C supplies the implementation; the validator never builds one.
type batchMCPCapabilityResolver func(kind, tool string) (batchMCPCapability, bool)

// classifyBatchLeaf maps a kind and tool to its group and rank. It fails
// closed everywhere: control-plane names are never leaves, an unannotated
// reentrant MCP tool is an error, and an unknown kind or generic tool name is
// an error.
func classifyBatchLeaf(kind, tool string, mcp batchMCPCapability) (string, int, error) {
	if batchControlTarget(tool) {
		return "", 0, fmt.Errorf("%w: %q", errBatchControlPlaneLeaf, tool)
	}
	switch kind {
	case batchKindAgent:
		switch tool {
		case "agent", "agentic_fetch":
			return batchGroupAgentDriver, batchGroupRanks[batchGroupAgentDriver], nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", errBatchUnknownTool, kind, tool)
	case batchKindShell:
		if tool == "bash" {
			return batchGroupIO, batchGroupRanks[batchGroupIO], nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", errBatchUnknownTool, kind, tool)
	case batchKindArgv:
		if tool == "run_command" {
			return batchGroupIO, batchGroupRanks[batchGroupIO], nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", errBatchUnknownTool, kind, tool)
	case batchKindMCP:
		switch {
		case !mcp.Reentrant:
			return batchGroupIO, batchGroupRanks[batchGroupIO], nil
		case mcp.Annotated:
			// A reentrant MCP tool behaves like a nested delegation.
			return batchGroupAgentDriver, batchGroupRanks[batchGroupAgentDriver], nil
		default:
			return "", 0, fmt.Errorf("%w: %q", errBatchReentrantMCPUnannotated, tool)
		}
	case batchKindGeneric:
		if batchIOTools[tool] {
			return batchGroupIO, batchGroupRanks[batchGroupIO], nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", errBatchUnknownTool, kind, tool)
	default:
		return "", 0, fmt.Errorf("%w: kind %q", errBatchUnknownTool, kind)
	}
}

// batchRankContext is the rank context of one side of a dependency. R11C
// supplies these; the validator never builds or stores them.
type batchRankContext struct {
	Rank            int
	UnderSequential bool // Has a Sequential-gate ancestor.
	UnderCap        bool // Has a MaxParallel>0 ancestor.
	Root            jobbatch.ID
}

// batchDependencyEdge is one dependency of a waiter on another root.
type batchDependencyEdge struct {
	Group           string // Dependency's group (rank looked up in the profile).
	Root            jobbatch.ID
	UnderSequential bool
	UnderCap        bool
}

// batchLeafRef is a classified leaf discovered while walking a spec.
type batchLeafRef struct {
	kind       string
	tool       string
	group      string
	classified string
}

// batchSpecNodes walks a spec tree and returns its computed depth, total
// node count and every task leaf. It mirrors the engine's accounting
// exactly: EVERY Spec node (task and batch) counts toward nodes, and the
// root sits at depth 1, so a task root is depth 1 and a batch root
// containing a leaf is depth 2.
func batchSpecNodes(specs []jobbatch.Spec) (depth, nodes int, leaves []batchLeafRef, err error) {
	depth = 0
	for _, spec := range specs {
		nodes++
		switch {
		case spec.Task != nil:
			leaf := batchLeafRef{kind: spec.Task.Kind, group: spec.Task.Group}
			var p batchLeafPayload
			if len(spec.Task.Payload) > 0 {
				if err := json.Unmarshal(spec.Task.Payload, &p); err != nil {
					return 0, 0, nil, fmt.Errorf("batch payload: %w", err)
				}
			}
			leaf.tool = p.Tool
			leaves = append(leaves, leaf)
			if 1 > depth {
				depth = 1
			}
		case spec.Batch != nil:
			// A batch node is itself one node at depth 1 relative; its
			// children build at that depth plus one, like the engine.
			d := 1
			childDepth, childNodes, childLeaves, err := batchSpecNodes(spec.Batch.Items)
			if err != nil {
				return 0, 0, nil, err
			}
			leaves = append(leaves, childLeaves...)
			nodes += childNodes
			if childDepth+1 > d {
				d = childDepth + 1
			}
			if d > depth {
				depth = d
			}
		}
	}
	return depth, nodes, leaves, nil
}

// ValidateSubmit validates a root submit against the closed profile: closed
// groups, declared-group agreement, depth and node budgets, and ranked
// separate-root dependencies. Depth and node budgets mirror the engine's
// submit: the root batch is one node at depth 1 and every Spec node counts.
// Resolver resolves MCP leaf capabilities; a nil resolver rejects any MCP
// leaf as unresolved. Non-MCP leaves never consult it.
func ValidateSubmit(profile *BatchProfile, waiter batchRankContext, edges []batchDependencyEdge, spec jobbatch.Spec, maxDepth, maxNodes int, resolver batchMCPCapabilityResolver) error {
	return validateBatch(profile, waiter, edges, []jobbatch.Spec{spec}, maxDepth, maxNodes, resolver)
}

// ValidateAdd validates new items against an EXISTING root's context and
// already-used budget: depth budget is maxDepth-existingDepth and node budget
// is maxNodes-existingNodes, applied to the new items only. ExistingDepth is
// the depth of the node the items attach under (the engine builds the items
// at existingDepth+1); existingNodes is the node count of the whole root
// tree including batch nodes. Resolver semantics match ValidateSubmit.
func ValidateAdd(profile *BatchProfile, existingRoot batchRankContext, edges []batchDependencyEdge, items jobbatch.Spec, existingDepth, existingNodes, maxDepth, maxNodes int, resolver batchMCPCapabilityResolver) error {
	return validateBatch(profile, existingRoot, edges, []jobbatch.Spec{items}, maxDepth-existingDepth, maxNodes-existingNodes, resolver)
}

// validateBatch is the shared check set of Submit and Add.
func validateBatch(profile *BatchProfile, waiter batchRankContext, edges []batchDependencyEdge, specs []jobbatch.Spec, depthBudget, nodeBudget int, resolver batchMCPCapabilityResolver) error {
	if profile == nil || profile.caps == nil {
		return fmt.Errorf("%w", errBatchNilProfile)
	}
	depth, nodes, leaves, err := batchSpecNodes(specs)
	if err != nil {
		return err
	}
	for _, leaf := range leaves {
		mcp := batchMCPCapability{}
		if leaf.kind == batchKindMCP {
			// Fail closed: an unresolved MCP capability never classifies.
			if resolver == nil {
				return fmt.Errorf("%w: %q", errBatchMCPCapabilityUnknown, leaf.tool)
			}
			capability, ok := resolver(leaf.kind, leaf.tool)
			if !ok {
				return fmt.Errorf("%w: %q", errBatchMCPCapabilityUnknown, leaf.tool)
			}
			mcp = capability
		}
		group, _, err := classifyBatchLeaf(leaf.kind, leaf.tool, mcp)
		if err != nil {
			return err
		}
		leaf.classified = group
		if leaf.group != "" && leaf.group != group {
			return fmt.Errorf("%w: declared %q classified %q", errBatchGroupMismatch, leaf.group, group)
		}
		if _, ok := profile.GroupCap(group); !ok {
			return fmt.Errorf("%w: %q", errBatchGroupUncapped, group)
		}
	}
	if depth > depthBudget {
		return fmt.Errorf("%w: depth %d > budget %d", errBatchDepthExceeded, depth, depthBudget)
	}
	if nodes > nodeBudget {
		return fmt.Errorf("%w: nodes %d > budget %d", errBatchNodeLimitExceeded, nodes, nodeBudget)
	}
	if waiter.UnderSequential {
		return fmt.Errorf("%w: waiter sequential", errBatchSharedGate)
	}
	if waiter.UnderCap {
		return fmt.Errorf("%w: waiter max_parallel", errBatchSharedGate)
	}
	for _, edge := range edges {
		targetRank, ok := profile.GroupRank(edge.Group)
		if !ok {
			return fmt.Errorf("%w: %q", errBatchGroupUncapped, edge.Group)
		}
		if edge.Root == waiter.Root {
			return fmt.Errorf("%w: %q", errBatchSameRoot, edge.Root)
		}
		if edge.UnderSequential {
			return fmt.Errorf("%w: edge sequential", errBatchSharedGate)
		}
		if waiter.UnderCap || edge.UnderCap {
			return fmt.Errorf("%w: max_parallel", errBatchSharedGate)
		}
		if targetRank >= waiter.Rank {
			return fmt.Errorf("%w: target rank %d >= waiter rank %d", errBatchRankOrder, targetRank, waiter.Rank)
		}
	}
	return nil
}
