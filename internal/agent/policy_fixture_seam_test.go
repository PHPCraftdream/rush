package agent

import (
	"context"
	"sort"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
)

// TEST-ONLY fixture seams over the moved policy toolset build and registry
// (internal/agent/runtime/policy/toolwrap/role_policy.go). These exist so the
// root integration tests keep compiling against the exported lower-package
// API without new production edits; minimize them when the owning test files
// move into their packages.

// testPolicyLeaf is the read-only fixture view of one pinned registry entry.
// The map KEY of newTestPolicyLeafRegistry is the pinned name verbatim: an
// intentionally mismatched key (e.g. a name mapped to a different tool) is
// preserved exactly as given. Layers may stay nil; the production build
// derives the baseline ["tool"] layer for a bare leaf tool, which matches
// every fixture that previously passed []string{"tool"} explicitly.
type testPolicyLeaf struct {
	Tool   fantasy.AgentTool
	Layers []string
}

// fixtureLeafPlaceholderTool is a non-executing stand-in used only to give
// NewPolicyToolBuild a real entry under each pinned name; CloneLeaves then
// swaps in the fixture's Tool while preserving its pointer identity.
type fixtureLeafPlaceholderTool struct{ name string }

func (t *fixtureLeafPlaceholderTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: t.name}
}

func (t *fixtureLeafPlaceholderTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, errFixturePlaceholderRun
}

func (t *fixtureLeafPlaceholderTool) ProviderOptions() fantasy.ProviderOptions { return nil }

func (t *fixtureLeafPlaceholderTool) SetProviderOptions(fantasy.ProviderOptions) {}

var errFixturePlaceholderRun = errorStringFixture("fixture leaf placeholder must never execute")

type errorStringFixture string

func (e errorStringFixture) Error() string { return string(e) }

// newTestPolicyLeafRegistry builds a pinned registry through the production
// NewPolicyToolBuild/CloneLeaves path instead of a removed exported fixture
// constructor. Placeholders are named after the pinned keys so the clone
// transform can key each entry back to its fixture leaf unchanged.
func newTestPolicyLeafRegistry(leaves map[string]testPolicyLeaf) *toolwrap.PolicyLeafRegistry {
	names := make([]string, 0, len(leaves))
	for name := range leaves {
		names = append(names, name)
	}
	sort.Strings(names)
	placeholders := make([]fantasy.AgentTool, 0, len(names))
	for _, name := range names {
		placeholders = append(placeholders, &fixtureLeafPlaceholderTool{name: name})
	}
	build := toolwrap.NewPolicyToolBuild(placeholders)
	return build.Leaves.CloneLeaves(func(name string, _ fantasy.AgentTool) fantasy.AgentTool {
		return leaves[name].Tool
	})
}

// pctCapturedToolFrom / pctCapturedScopeFromTool keep the frozen root
// ingress construction call sites (batch_install_tools.go) compiling over
// the moved captured walks; nil is returned as a nil interface so the
// ingress's typed-nil normalization keeps working.
func pctCapturedToolFrom(leaf fantasy.AgentTool) toolwrap.CapturedRegistrar {
	if captured := childagent.CapturedAgentToolFrom(leaf); captured != nil {
		return captured
	}
	return nil
}

func pctCapturedScopeFromTool(tool fantasy.AgentTool) *toolwrap.PctChildToolScope {
	return toolwrap.CapturedScopeFromTool(tool)
}
