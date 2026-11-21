package batchinst

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Typed admission rejections. All are sentinels distinguishable via errors.Is.
var (
	ErrBatchControlPlaneLeaf        = errors.New("control-plane tools are never leaves")
	ErrBatchReentrantMCPUnannotated = errors.New("reentrant MCP tool lacks an admission annotation")
	ErrBatchUnknownTool             = errors.New("tool is not classifiable into a batch group")
	ErrBatchNilProfile              = errors.New("batch profile is nil or zero-value")
	ErrBatchGroupUncapped           = errors.New("batch group has no cap in the profile")
	ErrBatchGroupMismatch           = errors.New("declared group differs from the classified group")
	ErrBatchDepthExceeded           = errors.New("batch depth exceeds the budget")
	ErrBatchNodeLimitExceeded       = errors.New("batch node count exceeds the budget")
	ErrBatchMCPCapabilityUnknown    = errors.New("MCP tool capability is unresolved")
	ErrBatchRankOrder               = errors.New("dependency rank is not strictly below the waiter rank")
	ErrBatchSameRoot                = errors.New("waiter and dependency share a root")
	ErrBatchSharedGate              = errors.New("waiter and dependency share a Sequential gate or MaxParallel ancestor")
)

// batchIOTools is the closed generic-leaf allowlist. Todos is included
// because it changes session state, so it must go through batch ingress.
var batchIOTools = map[string]bool{
	"view": true, "glob": true, "grep": true, "ls": true, "edit": true, "write": true, "multiedit": true,
	"fetch": true, "download": true, "web_fetch": true, "web_search": true, "run_command": true, "bash": true, "sourcegraph": true, "git_read": true,
	"fs_read": true, "fs_list": true, "fs_find": true, "fs_grep": true,
	"fs_write": true, "fs_write_lines": true, "fs_replace": true, "fs_delete": true,
	"todos": true,
}

// MCPCapability describes an MCP tool for classification purposes.
type MCPCapability struct {
	Reentrant bool // This MCP tool can itself call back into Rush tools or wait on other roots.
	Annotated bool // Explicit admission annotation for a reentrant tool (set only after human/reviewer intent).
}

type MCPCapabilityResolver func(kind, tool string) (MCPCapability, bool)

func ClassifyLeaf(kind, tool string, mcp MCPCapability, policy AdmissionPolicy) (string, int, error) {
	if policy.ControlTarget(tool) {
		return "", 0, fmt.Errorf("%w: %q", ErrBatchControlPlaneLeaf, tool)
	}
	switch kind {
	case "agent":
		switch tool {
		case "agent", "agentic_fetch":
			return policy.AgentGroup, policy.AgentRank, nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", ErrBatchUnknownTool, kind, tool)
	case "shell":
		if tool == "bash" {
			return policy.IOGroup, policy.IORank, nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", ErrBatchUnknownTool, kind, tool)
	case "argv":
		if tool == "run_command" {
			return policy.IOGroup, policy.IORank, nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", ErrBatchUnknownTool, kind, tool)
	case "mcp":
		switch {
		case !mcp.Reentrant:
			return policy.IOGroup, policy.IORank, nil
		case mcp.Annotated:
			// A reentrant MCP tool behaves like a nested delegation.
			return policy.AgentGroup, policy.AgentRank, nil
		default:
			return "", 0, fmt.Errorf("%w: %q", ErrBatchReentrantMCPUnannotated, tool)
		}
	case "generic":
		if IsIOTool(tool) {
			return policy.IOGroup, policy.IORank, nil
		}
		return "", 0, fmt.Errorf("%w: kind %q tool %q", ErrBatchUnknownTool, kind, tool)
	default:
		return "", 0, fmt.Errorf("%w: kind %q", ErrBatchUnknownTool, kind)
	}
}

type RankContext struct {
	Rank            int
	UnderSequential bool // Has a Sequential-gate ancestor.
	UnderCap        bool // Has a MaxParallel>0 ancestor.
	Root            jobbatch.ID
}

type DependencyEdge struct {
	Group           string // Dependency's group (rank looked up in the profile).
	Root            jobbatch.ID
	UnderSequential bool
	UnderCap        bool
}

type LeafRef struct {
	kind       string
	tool       string
	group      string
	classified string
}

// SpecNodes counts every node (root depth is 1) and collects task leaves.
func SpecNodes(specs []jobbatch.Spec, decodeTool func(json.RawMessage) (string, error)) (depth, nodes int, leaves []LeafRef, err error) {
	depth = 0
	for _, spec := range specs {
		nodes++
		switch {
		case spec.Task != nil:
			leaf := LeafRef{kind: spec.Task.Kind, group: spec.Task.Group}
			if len(spec.Task.Payload) > 0 {
				tool, err := decodeTool(spec.Task.Payload)
				if err != nil {
					return 0, 0, nil, fmt.Errorf("batch payload: %w", err)
				}
				leaf.tool = tool
			}
			leaves = append(leaves, leaf)
			depth = max(depth, 1)
		case spec.Batch != nil:
			childDepth, childNodes, childLeaves, err := SpecNodes(spec.Batch.Items, decodeTool)
			if err != nil {
				return 0, 0, nil, err
			}
			leaves = append(leaves, childLeaves...)
			nodes += childNodes
			depth = max(depth, childDepth+1)
		}
	}
	return depth, nodes, leaves, nil
}

// ValidateSubmit checks closed groups, budgets and ranked separate-root dependencies.
// A nil resolver rejects MCP leaves; non-MCP leaves never consult it.
func ValidateSubmit(profile AdmissionProfile, waiter RankContext, edges []DependencyEdge, spec jobbatch.Spec, maxDepth, maxNodes int, resolver MCPCapabilityResolver, policy AdmissionPolicy) error {
	return ValidateBatch(profile, waiter, edges, []jobbatch.Spec{spec}, maxDepth, maxNodes, resolver, policy)
}

// ValidateAdd applies the remaining depth and node budgets to the added items.
func ValidateAdd(profile AdmissionProfile, existingRoot RankContext, edges []DependencyEdge, items jobbatch.Spec, existingDepth, existingNodes, maxDepth, maxNodes int, resolver MCPCapabilityResolver, policy AdmissionPolicy) error {
	return ValidateBatch(profile, existingRoot, edges, []jobbatch.Spec{items}, maxDepth-existingDepth, maxNodes-existingNodes, resolver, policy)
}

// ValidateBatch is the shared check set of Submit and Add.
func ValidateBatch(profile AdmissionProfile, waiter RankContext, edges []DependencyEdge, specs []jobbatch.Spec, depthBudget, nodeBudget int, resolver MCPCapabilityResolver, policy AdmissionPolicy) error {
	if profile.GroupCap == nil {
		return fmt.Errorf("%w", ErrBatchNilProfile)
	}
	depth, nodes, leaves, err := SpecNodes(specs, policy.DecodeTool)
	if err != nil {
		return err
	}
	for _, leaf := range leaves {
		mcp := MCPCapability{}
		if leaf.kind == "mcp" {
			// Fail closed: an unresolved MCP capability never classifies.
			if resolver == nil {
				return fmt.Errorf("%w: %q", ErrBatchMCPCapabilityUnknown, leaf.tool)
			}
			capability, ok := resolver(leaf.kind, leaf.tool)
			if !ok {
				return fmt.Errorf("%w: %q", ErrBatchMCPCapabilityUnknown, leaf.tool)
			}
			mcp = capability
		}
		group, _, err := ClassifyLeaf(leaf.kind, leaf.tool, mcp, policy)
		if err != nil {
			return err
		}
		leaf.classified = group
		if leaf.group != "" && leaf.group != group {
			return fmt.Errorf("%w: declared %q classified %q", ErrBatchGroupMismatch, leaf.group, group)
		}
		if _, ok := profile.GroupCap(group); !ok {
			return fmt.Errorf("%w: %q", ErrBatchGroupUncapped, group)
		}
	}
	if depth > depthBudget {
		return fmt.Errorf("%w: depth %d > budget %d", ErrBatchDepthExceeded, depth, depthBudget)
	}
	if nodes > nodeBudget {
		return fmt.Errorf("%w: nodes %d > budget %d", ErrBatchNodeLimitExceeded, nodes, nodeBudget)
	}
	if waiter.UnderSequential {
		return fmt.Errorf("%w: waiter sequential", ErrBatchSharedGate)
	}
	if waiter.UnderCap {
		return fmt.Errorf("%w: waiter max_parallel", ErrBatchSharedGate)
	}
	for _, edge := range edges {
		targetRank, ok := profile.GroupRank(edge.Group)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBatchGroupUncapped, edge.Group)
		}
		if edge.Root == waiter.Root {
			return fmt.Errorf("%w: %q", ErrBatchSameRoot, edge.Root)
		}
		if edge.UnderSequential {
			return fmt.Errorf("%w: edge sequential", ErrBatchSharedGate)
		}
		if waiter.UnderCap || edge.UnderCap {
			return fmt.Errorf("%w: max_parallel", ErrBatchSharedGate)
		}
		if targetRank >= waiter.Rank {
			return fmt.Errorf("%w: target rank %d >= waiter rank %d", ErrBatchRankOrder, targetRank, waiter.Rank)
		}
	}
	return nil
}

// AdmissionProfile is the cap and rank lookup of the caller's pinned profile.
type AdmissionProfile struct {
	GroupCap  func(string) (int, bool)
	GroupRank func(string) (int, bool)
}

// AdmissionPolicy binds classification and payload decoding to the caller's
// current batch tool catalog and wire format.
type AdmissionPolicy struct {
	ControlTarget       func(string) bool
	DecodeTool          func(json.RawMessage) (string, error)
	IOGroup, AgentGroup string
	IORank, AgentRank   int
}

// IsIOTool reports whether name is in the closed generic IO allowlist.
func IsIOTool(name string) bool { return batchIOTools[name] }
