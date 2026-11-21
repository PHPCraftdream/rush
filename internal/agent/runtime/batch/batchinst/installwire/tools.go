package installwire

import (
	"context"
	"errors"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/google/uuid"
)

// WorkTarget is the complement of control-plane tools restricted to the
// agent, agentic_fetch, admission I/O (including todos), and MCP families.
// Non-work leaves such as read-only host tools keep their direct pinned leaf.
func WorkTarget(name string, control, io func(string) bool) bool {
	if control(name) {
		return false
	}
	return IsDelegation(name) || io(name) || strings.HasPrefix(name, "mcp_")
}

func IsDelegation(name string) bool {
	return name == "agent" || name == tools.AgenticFetchToolName
}

// FamilyDelivery keeps delegation detached so the parent can service child
// questions and gates. Other work stays synchronous.
func FamilyDelivery[D any](name string, detached, synchronous D) D {
	if IsDelegation(name) {
		return detached
	}
	return synchronous
}

// RefusedTool never falls back to direct dispatch after an arming failure.
type RefusedTool struct {
	fantasy.AgentTool
	err error
}

func NewRefusedTool(tool fantasy.AgentTool, err error) RefusedTool {
	return RefusedTool{AgentTool: tool, err: err}
}

func (t RefusedTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, t.err
}

// ConstructionOwners is the ownership port for a pinned tool build.
type ConstructionOwners interface {
	AcquireConstruction(registryID, instance string) error
	Release(registryID, instance string)
}

// WorkToolTemplate carries the build-selected values for one armed invocation.
// The caller supplies its own invocation and role types at the arming point.
type WorkToolTemplate struct {
	RegistryID, RestrictedToolset string
	Detached, AutoEscape          bool
}

// WorkToolPorts binds build policy to the coordinator's registry, role and
// ingress authority without passing the coordinator itself down.
type WorkToolPorts[R any] struct {
	Pin          func(*toolwrap.PolicyLeafRegistry) (string, error)
	Owners       ConstructionOwners
	Role         func(context.Context, []fantasy.AgentTool) R
	Allows       func(R, string) bool
	Control, IO  func(string) bool
	WrapQuestion func(fantasy.AgentTool) fantasy.AgentTool
	NoWindow     func() bool
	Arm          func(fantasy.AgentTool, *toolwrap.PolicyLeafRegistry, R, WorkToolTemplate, bool) fantasy.AgentTool
}

// BuildWorkTools builds the policy leaves before publishing their registry.
// Deferred builds publish nothing here: their ingress pins on first dispatch.
// A pinned build registers release immediately after acquiring ownership, even
// if the scope is already closed (in which case release runs immediately).
func BuildWorkTools[R any](ctx context.Context, list []fantasy.AgentTool, registerRelease func(func()), deferred bool, ports WorkToolPorts[R]) (toolwrap.PolicyToolBuild, error) {
	if registerRelease == nil {
		return toolwrap.PolicyToolBuild{}, errors.New("batch tool build: no build-resource scope in the trusted call state")
	}
	built := toolwrap.NewPolicyToolBuildForContext(ctx, list)
	fingerprint := ToolsetFingerprint(built.Tools)
	role := ports.Role(ctx, built.Tools)
	if !deferred {
		id, err := ports.Pin(built.Leaves)
		if err != nil {
			return toolwrap.PolicyToolBuild{}, err
		}
		instance := uuid.NewString()
		owners := ports.Owners
		if err := owners.AcquireConstruction(id, instance); err != nil {
			return toolwrap.PolicyToolBuild{}, err
		}
		registerRelease(func() { owners.Release(id, instance) })
		built.RegistryID = id
		if fingerprint == "" {
			fingerprint = id
		}
	}
	for i, tool := range built.Tools {
		name := tool.Info().Name
		if !ports.Allows(role, name) {
			continue
		}
		if name == tools.AskQuestionToolName {
			built.Tools[i] = ports.WrapQuestion(tool)
			continue
		}
		if !WorkTarget(name, ports.Control, ports.IO) {
			continue
		}
		template := WorkToolTemplate{
			RegistryID: built.RegistryID, RestrictedToolset: fingerprint,
			Detached: FamilyDelivery(name, true, false),
		}
		if name == tools.BashToolName {
			template.Detached = ports.NoWindow()
			template.AutoEscape = !template.Detached
		}
		built.Tools[i] = ports.Arm(tool, built.Leaves, role, template, deferred)
	}
	return built, nil
}

func ToolsetFingerprint(list []fantasy.AgentTool) string {
	names := make([]string, 0, len(list))
	for _, tool := range list {
		names = append(names, tool.Info().Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

type (
	MCPCapability         = batchinst.MCPCapability
	MCPCapabilityResolver = batchinst.MCPCapabilityResolver
)

// NonReentrantMCPResolver admits MCP tools only as non-reentrant I/O.
func NonReentrantMCPResolver() MCPCapabilityResolver {
	return func(string, string) (MCPCapability, bool) { return MCPCapability{}, true }
}
