package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"math/rand"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

// batchTestLeaf builds a one-leaf spec with a tool payload.
func batchTestLeaf(kind, tool, group string) jobbatch.Spec {
	payload, err := json.Marshal(batchLeafPayload{Tool: tool, SessionID: "s"})
	if err != nil {
		panic(err)
	}
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: kind, Group: group, Payload: payload}}
}

var batchControlPlaneNames = []string{
	"await_tasks", "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop",
	"ask_question", "wakein", "wakeon", "loop", "wake_list", "wake_cancel",
	"inspect_agent", "inject_agent", "stop_agent",
}

var batchKinds = []string{
	batchdispatch.KindGeneric, batchdispatch.KindMCP, batchdispatch.KindShell, batchdispatch.KindArgv, batchdispatch.KindAgent,
}

func TestBatchExecutorRejectsRoleForbiddenGenericToolBeforeLeaf(t *testing.T) {
	allowed := []string{"view"}
	contract := &session.RoleContract{Version: session.RoleContractVersion, Name: "reader", Model: session.ModelCfg{Provider: "p", Model: "m"}, Tools: &allowed}
	started := false
	leaf := &batchGenericTestTool{name: "bash", run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		started = true
		return fantasy.NewTextResponse("unexpected"), nil
	}}
	env := batchLeafEnv{Leaves: func(string) (*toolwrap.PolicyLeafRegistry, error) {
		return newTestPolicyLeafRegistry(map[string]testPolicyLeaf{"bash": {Tool: leaf}}), nil
	}}
	family := newBatchExecutor(env, map[string]batchLeafFamily{batchdispatch.KindGeneric: newBatchGenericFamily()})
	payload, err := json.Marshal(batchLeafPayload{Tool: "bash", SessionID: "s", Policy: &batchLeafPolicyContext{RoleContract: contract}})
	require.NoError(t, err)
	_, err = family.Start(context.Background(), jobbatch.Launch{Task: jobbatch.TaskSpec{Kind: batchdispatch.KindGeneric, Payload: payload}}, func(jobbatch.Result) {})
	require.Error(t, err)
	require.False(t, started, "forbidden leaf must not dispatch")
}

func TestBatchClassifyControlPlane(t *testing.T) {
	t.Parallel()
	for _, name := range batchControlPlaneNames {
		for _, kind := range batchKinds {
			kind, name := kind, name
			t.Run(kind+"/"+name, func(t *testing.T) {
				t.Parallel()
				_, _, err := batchinst.ClassifyLeaf(kind, name, batchinst.MCPCapability{Reentrant: true, Annotated: true}, batchAdmissionPolicy())
				require.ErrorIs(t, err, batchinst.ErrBatchControlPlaneLeaf)
			})
		}
	}
}

// batchExpectedIOTools is the fixed literal expectation for the io allowlist.
// It is independent of the production map so dropping an allowlist entry
// fails a test.
var batchExpectedIOTools = []string{
	"view", "glob", "grep", "ls", "edit", "write", "multiedit", "fetch",
	"download", "web_fetch", "web_search", "run_command", "bash", "sourcegraph",
	"git_read", "fs_read", "fs_list", "fs_find", "fs_grep", "fs_write",
	"fs_write_lines", "fs_replace", "fs_delete",
	// Round-2 completeness (KNOWING tripwire update): todos MUTATES session
	// state, so batchIOTools classifies it WORK/io — the literal expectation
	// must include it or the symmetry test fails.
	"todos",
}

func TestBatchClassifyIOTable(t *testing.T) {
	t.Parallel()
	for _, tool := range batchExpectedIOTools {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			group, rank, err := batchinst.ClassifyLeaf(batchdispatch.KindGeneric, tool, batchinst.MCPCapability{}, batchAdmissionPolicy())
			require.NoError(t, err)
			require.Equal(t, batchGroupIO, group)
			require.Equal(t, 0, rank)
		})
	}
}

func TestBatchClassifyKinds(t *testing.T) {
	t.Parallel()

	t.Run("shell bash", func(t *testing.T) {
		t.Parallel()
		group, rank, err := batchinst.ClassifyLeaf(batchdispatch.KindShell, "bash", batchinst.MCPCapability{}, batchAdmissionPolicy())
		require.NoError(t, err)
		require.Equal(t, batchGroupIO, group)
		require.Equal(t, 0, rank)
	})

	t.Run("argv run_command", func(t *testing.T) {
		t.Parallel()
		group, rank, err := batchinst.ClassifyLeaf(batchdispatch.KindArgv, "run_command", batchinst.MCPCapability{}, batchAdmissionPolicy())
		require.NoError(t, err)
		require.Equal(t, batchGroupIO, group)
		require.Equal(t, 0, rank)
	})

	t.Run("agent tools", func(t *testing.T) {
		t.Parallel()
		for _, tool := range []string{"agent", "agentic_fetch"} {
			group, rank, err := batchinst.ClassifyLeaf(batchdispatch.KindAgent, tool, batchinst.MCPCapability{}, batchAdmissionPolicy())
			require.NoError(t, err)
			require.Equal(t, batchGroupAgentDriver, group)
			require.Equal(t, 2, rank)
		}
	})

	t.Run("mcp non-reentrant", func(t *testing.T) {
		t.Parallel()
		group, rank, err := batchinst.ClassifyLeaf(batchdispatch.KindMCP, "some_server_tool", batchinst.MCPCapability{}, batchAdmissionPolicy())
		require.NoError(t, err)
		require.Equal(t, batchGroupIO, group)
		require.Equal(t, 0, rank)
	})

	t.Run("mcp reentrant annotated", func(t *testing.T) {
		t.Parallel()
		group, rank, err := batchinst.ClassifyLeaf(batchdispatch.KindMCP, "some_server_tool", batchinst.MCPCapability{Reentrant: true, Annotated: true}, batchAdmissionPolicy())
		require.NoError(t, err)
		require.Equal(t, batchGroupAgentDriver, group)
		require.Equal(t, 2, rank)
	})

	t.Run("mcp reentrant unannotated", func(t *testing.T) {
		t.Parallel()
		_, _, err := batchinst.ClassifyLeaf(batchdispatch.KindMCP, "some_server_tool", batchinst.MCPCapability{Reentrant: true}, batchAdmissionPolicy())
		require.ErrorIs(t, err, batchinst.ErrBatchReentrantMCPUnannotated)
	})

	t.Run("unknown generic tool", func(t *testing.T) {
		t.Parallel()
		_, _, err := batchinst.ClassifyLeaf(batchdispatch.KindGeneric, "mystery_tool", batchinst.MCPCapability{}, batchAdmissionPolicy())
		require.ErrorIs(t, err, batchinst.ErrBatchUnknownTool)
	})

	t.Run("unknown kind", func(t *testing.T) {
		t.Parallel()
		_, _, err := batchinst.ClassifyLeaf("widget", "view", batchinst.MCPCapability{}, batchAdmissionPolicy())
		require.ErrorIs(t, err, batchinst.ErrBatchUnknownTool)
	})
}

// batchValidCtx is a clean waiter context used as a test baseline.
func batchValidCtx(rank int) batchinst.RankContext {
	return batchinst.RankContext{Rank: rank, Root: jobbatch.ID(fmt.Sprintf("waiter-%d", rank))}
}

func TestBatchAdmissionValidateSubmit(t *testing.T) {
	t.Parallel()
	profile, err := NewBatchProfileDefault()
	require.NoError(t, err)

	t.Run("legal descending chain accepted", func(t *testing.T) {
		t.Parallel()
		waiter := batchValidCtx(2)
		edges := []batchinst.DependencyEdge{
			{Group: batchGroupCLIDriver, Root: "dep-a"},
			{Group: batchGroupIO, Root: "dep-b"},
		}
		spec := batchTestLeaf(batchdispatch.KindAgent, "agent", batchGroupAgentDriver)
		require.NoError(t, ValidateSubmit(profile, waiter, edges, spec, 8, 500, nil))
	})

	t.Run("equal rank edge rejected", func(t *testing.T) {
		t.Parallel()
		waiter := batchValidCtx(1)
		edges := []batchinst.DependencyEdge{{Group: batchGroupCLIDriver, Root: "dep-a"}}
		err := ValidateSubmit(profile, waiter, edges, batchTestLeaf(batchdispatch.KindShell, "bash", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchRankOrder)
	})

	t.Run("ascending rank edge rejected", func(t *testing.T) {
		t.Parallel()
		waiter := batchValidCtx(0)
		edges := []batchinst.DependencyEdge{{Group: batchGroupAgentDriver, Root: "dep-a"}}
		err := ValidateSubmit(profile, waiter, edges, batchTestLeaf(batchdispatch.KindGeneric, "view", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchRankOrder)
	})

	t.Run("same root rejected", func(t *testing.T) {
		t.Parallel()
		waiter := batchValidCtx(2)
		waiter.Root = "same"
		edges := []batchinst.DependencyEdge{{Group: batchGroupIO, Root: "same"}}
		err := ValidateSubmit(profile, waiter, edges, batchTestLeaf(batchdispatch.KindAgent, "agent", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchSameRoot)
	})

	t.Run("sequential gate on either side rejected", func(t *testing.T) {
		t.Parallel()
		edge := batchinst.DependencyEdge{Group: batchGroupIO, Root: "dep-a"}
		err := ValidateSubmit(profile, batchinst.RankContext{Rank: 2, UnderSequential: true, Root: "w1"}, []batchinst.DependencyEdge{edge},
			batchTestLeaf(batchdispatch.KindAgent, "agent", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchSharedGate)

		edge.UnderSequential = true
		err = ValidateSubmit(profile, batchValidCtx(2), []batchinst.DependencyEdge{edge},
			batchTestLeaf(batchdispatch.KindAgent, "agent", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchSharedGate)
	})

	t.Run("cap flag on either side rejected", func(t *testing.T) {
		t.Parallel()
		edge := batchinst.DependencyEdge{Group: batchGroupIO, Root: "dep-a"}
		err := ValidateSubmit(profile, batchinst.RankContext{Rank: 2, UnderCap: true, Root: "w1"}, []batchinst.DependencyEdge{edge},
			batchTestLeaf(batchdispatch.KindAgent, "agent", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchSharedGate)

		edge.UnderCap = true
		err = ValidateSubmit(profile, batchValidCtx(2), []batchinst.DependencyEdge{edge},
			batchTestLeaf(batchdispatch.KindAgent, "agent", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchSharedGate)
	})

	t.Run("nested agent-in-agent same rank rejected", func(t *testing.T) {
		t.Parallel()
		waiter := batchValidCtx(2)
		edges := []batchinst.DependencyEdge{{Group: batchGroupAgentDriver, Root: "dep-a"}}
		err := ValidateSubmit(profile, waiter, edges, batchTestLeaf(batchdispatch.KindAgent, "agent", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchRankOrder)
	})

	t.Run("unknown edge group rejected", func(t *testing.T) {
		t.Parallel()
		waiter := batchValidCtx(2)
		edges := []batchinst.DependencyEdge{{Group: "agents", Root: "dep-a"}}
		err := ValidateSubmit(profile, waiter, edges, batchTestLeaf(batchdispatch.KindAgent, "agent", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchGroupUncapped)
	})

	t.Run("default-group leaf with no default cap rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewBatchProfileFromMap(map[string]int{
			batchGroupIO: 5, batchGroupCLIDriver: 1, batchGroupAgentDriver: 1,
		}, 7)
		require.ErrorIs(t, err, errBatchMissingGroup)

		// A profile object without a "" cap (as R11C may receive it) makes
		// the validator's closure check fail closed, including for an edge
		// targeting the default group.
		noDefault := &BatchProfile{caps: map[string]int{
			batchGroupIO: 5, batchGroupCLIDriver: 1, batchGroupAgentDriver: 1,
		}, global: 7}
		err = ValidateSubmit(noDefault, batchValidCtx(2), []batchinst.DependencyEdge{{Group: batchGroupDefault, Root: "dep-a"}},
			batchTestLeaf(batchdispatch.KindGeneric, "view", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchGroupUncapped)

		err = ValidateSubmit(nil, batchValidCtx(2), nil, batchTestLeaf(batchdispatch.KindGeneric, "view", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchNilProfile)
	})

	t.Run("group mismatch rejected", func(t *testing.T) {
		t.Parallel()
		err := ValidateSubmit(profile, batchValidCtx(2), nil,
			batchTestLeaf(batchdispatch.KindAgent, "agent", batchGroupIO), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchGroupMismatch)
	})

	t.Run("uncapped classified leaf rejected", func(t *testing.T) {
		t.Parallel()
		noIO := &BatchProfile{caps: map[string]int{
			batchGroupCLIDriver: 1, batchGroupAgentDriver: 1, batchGroupDefault: 1,
		}, global: 3}
		err := ValidateSubmit(noIO, batchValidCtx(2), nil, batchTestLeaf(batchdispatch.KindShell, "bash", ""), 8, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchGroupUncapped)
	})

	t.Run("depth excess rejected", func(t *testing.T) {
		t.Parallel()
		spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{
			{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{
				batchTestLeaf(batchdispatch.KindGeneric, "view", ""),
			}}},
		}}}
		err := ValidateSubmit(profile, batchValidCtx(2), nil, spec, 2, 500, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchDepthExceeded)
		require.NoError(t, ValidateSubmit(profile, batchValidCtx(2), nil, spec, 3, 500, nil))
	})

	t.Run("node excess rejected", func(t *testing.T) {
		t.Parallel()
		spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{
			batchTestLeaf(batchdispatch.KindGeneric, "view", ""),
			batchTestLeaf(batchdispatch.KindGeneric, "grep", ""),
			batchTestLeaf(batchdispatch.KindGeneric, "ls", ""),
		}}}
		// The batch root is a node too: 3 leaves + 1 batch = 4 nodes,
		// mirroring the engine's count of every Spec node.
		err := ValidateSubmit(profile, batchValidCtx(2), nil, spec, 8, 3, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchNodeLimitExceeded)
		require.NoError(t, ValidateSubmit(profile, batchValidCtx(2), nil, spec, 8, 4, nil))
	})
}

func TestBatchValidateAdd(t *testing.T) {
	t.Parallel()
	profile, err := NewBatchProfileDefault()
	require.NoError(t, err)
	const maxDepth, maxNodes = 4, 10

	t.Run("happy path accepted", func(t *testing.T) {
		t.Parallel()
		root := batchValidCtx(2)
		items := batchTestLeaf(batchdispatch.KindAgent, "agent", batchGroupAgentDriver)
		edges := []batchinst.DependencyEdge{{Group: batchGroupIO, Root: "dep-a"}}
		require.NoError(t, ValidateAdd(profile, root, edges, items, 2, 3, maxDepth, maxNodes, nil))
	})

	t.Run("existing depth consumes budget", func(t *testing.T) {
		t.Parallel()
		root := batchValidCtx(2)
		items := batchTestLeaf(batchdispatch.KindGeneric, "view", "")
		err := ValidateAdd(profile, root, nil, items, maxDepth, 1, maxDepth, maxNodes, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchDepthExceeded)
	})

	t.Run("existing nodes consume budget", func(t *testing.T) {
		t.Parallel()
		root := batchValidCtx(2)
		items := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{
			batchTestLeaf(batchdispatch.KindGeneric, "view", ""),
			batchTestLeaf(batchdispatch.KindGeneric, "grep", ""),
		}}}
		err := ValidateAdd(profile, root, nil, items, 1, maxNodes-1, maxDepth, maxNodes, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchNodeLimitExceeded)
	})

	t.Run("existing sequential gate rejected", func(t *testing.T) {
		t.Parallel()
		root := batchValidCtx(2)
		root.UnderSequential = true
		edges := []batchinst.DependencyEdge{{Group: batchGroupIO, Root: "dep-a"}}
		err := ValidateAdd(profile, root, edges, batchTestLeaf(batchdispatch.KindGeneric, "view", ""), 1, 1, maxDepth, maxNodes, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchSharedGate)
	})

	t.Run("nil profile rejected", func(t *testing.T) {
		t.Parallel()
		err := ValidateAdd(nil, batchValidCtx(2), nil, batchTestLeaf(batchdispatch.KindGeneric, "view", ""), 1, 1, maxDepth, maxNodes, nil)
		require.ErrorIs(t, err, batchinst.ErrBatchNilProfile)
	})
}

func TestBatchAdmissionProperty(t *testing.T) {
	t.Parallel()
	sentinels := []error{
		batchinst.ErrBatchControlPlaneLeaf, batchinst.ErrBatchReentrantMCPUnannotated, batchinst.ErrBatchUnknownTool,
		batchinst.ErrBatchNilProfile, batchinst.ErrBatchGroupUncapped, batchinst.ErrBatchGroupMismatch,
		batchinst.ErrBatchDepthExceeded, batchinst.ErrBatchNodeLimitExceeded, batchinst.ErrBatchRankOrder,
		batchinst.ErrBatchSameRoot, batchinst.ErrBatchSharedGate, errBatchCapInvalid,
		errBatchGlobalTooSmall, errBatchMissingGroup, errBatchUnknownGroupName,
		errBatchRankConflict, batchinst.ErrBatchMCPCapabilityUnknown,
	}
	genericTools := []string{"view", "grep", "bash", "run_command", "mystery_tool", "await_tasks"}
	agentTools := []string{"agent", "agentic_fetch"}
	groups := []string{batchGroupIO, batchGroupCLIDriver, batchGroupAgentDriver, batchGroupDefault, "agents"}

	run := func(t *testing.T) []bool {
		t.Helper()
		const iterations = 2000
		decisions := make([]bool, iterations)
		rng := rand.New(rand.NewSource(20261012))
		for i := 0; i < iterations; i++ {
			io, cli, agent, def := 1+rng.Intn(3), 1, 1, 1
			profile, perr := NewBatchProfile(BatchProfileCaps{
				IO: io, CLIDriver: cli, AgentDriver: agent, Default: def,
				Global: io + cli + agent + def,
			})
			require.NoError(t, perr)

			waiter := batchinst.RankContext{
				Rank:            rng.Intn(4),
				UnderSequential: rng.Intn(4) == 0,
				UnderCap:        rng.Intn(4) == 0,
				Root:            "w",
			}
			edges := make([]batchinst.DependencyEdge, rng.Intn(3))
			for j := range edges {
				edges[j] = batchinst.DependencyEdge{
					Group:           groups[rng.Intn(len(groups))],
					Root:            jobbatch.ID(fmt.Sprintf("d%d", rng.Intn(2))),
					UnderSequential: rng.Intn(4) == 0,
					UnderCap:        rng.Intn(4) == 0,
				}
			}
			// batchTestTree sometimes nests batches of 1-3 children
			// recursively instead of emitting a single leaf.
			var batchTestTree func(left int) jobbatch.Spec
			batchTestTree = func(left int) jobbatch.Spec {
				if left <= 0 || rng.Intn(2) == 0 {
					switch rng.Intn(3) {
					case 0:
						return batchTestLeaf(batchdispatch.KindGeneric, genericTools[rng.Intn(len(genericTools))], "")
					case 1:
						return batchTestLeaf(batchdispatch.KindAgent, agentTools[rng.Intn(len(agentTools))], "")
					default:
						return batchTestLeaf(batchdispatch.KindMCP, "srv_tool", "")
					}
				}
				items := make([]jobbatch.Spec, 1+rng.Intn(3))
				for i := range items {
					items[i] = batchTestTree(left - 1)
				}
				return jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Items: items}}
			}
			spec := batchTestTree(rng.Intn(3))
			// Occasionally make the profile nil to exercise batchinst.ErrBatchNilProfile.
			if rng.Intn(10) == 0 {
				profile = nil
			}
			err := ValidateSubmit(profile, waiter, edges, spec, 1+rng.Intn(2), 1+rng.Intn(3), nil)
			accepted := err == nil
			decisions[i] = accepted
			if accepted {
				// A total accept: closed profile, strictly descending ranks,
				// separate roots and no gate flags.
				require.NotNil(t, profile)
				require.False(t, waiter.UnderSequential)
				require.False(t, waiter.UnderCap)
				for _, e := range edges {
					rank, ok := profile.GroupRank(e.Group)
					require.True(t, ok)
					require.Less(t, rank, waiter.Rank)
					require.NotEqual(t, e.Root, waiter.Root)
					require.False(t, e.UnderSequential)
					require.False(t, e.UnderCap)
				}
			} else {
				matched := false
				for _, sentinel := range sentinels {
					if errors.Is(err, sentinel) {
						matched = true
						break
					}
				}
				require.True(t, matched, "rejection %v matches no sentinel", err)
			}
		}
		return decisions
	}

	first := run(t)
	second := run(t)
	require.Equal(t, first, second)
}

// batchTestResolver returns a resolver answering every lookup with the given
// capability and ok flag.
func batchTestResolver(cap batchinst.MCPCapability, ok bool) batchinst.MCPCapabilityResolver {
	return func(kind, tool string) (batchinst.MCPCapability, bool) {
		return cap, ok
	}
}

func batchTestMCPSpec(group string) jobbatch.Spec {
	return batchTestLeaf(batchdispatch.KindMCP, "srv_tool", group)
}

// TestBatchValidatMCPResolver covers the fail-closed MCP resolution through
// both ValidateSubmit and ValidateAdd.
func TestBatchValidatMCPResolver(t *testing.T) {
	t.Parallel()
	profile, err := NewBatchProfileDefault()
	require.NoError(t, err)

	scenarios := []struct {
		name     string
		resolver batchinst.MCPCapabilityResolver
		group    string
		want     error
	}{
		{name: "nil resolver rejected", resolver: nil, group: "", want: batchinst.ErrBatchMCPCapabilityUnknown},
		{name: "unknown tool rejected", resolver: batchTestResolver(batchinst.MCPCapability{}, false), group: "", want: batchinst.ErrBatchMCPCapabilityUnknown},
		{name: "reentrant unannotated rejected", resolver: batchTestResolver(batchinst.MCPCapability{Reentrant: true}, true), group: "", want: batchinst.ErrBatchReentrantMCPUnannotated},
		{name: "reentrant annotated accepted", resolver: batchTestResolver(batchinst.MCPCapability{Reentrant: true, Annotated: true}, true), group: ""},
		{name: "plain non-reentrant accepted as io", resolver: batchTestResolver(batchinst.MCPCapability{}, true), group: ""},
		{name: "annotated declared-io mismatch rejected", resolver: batchTestResolver(batchinst.MCPCapability{Reentrant: true, Annotated: true}, true), group: batchGroupIO, want: batchinst.ErrBatchGroupMismatch},
	}

	for _, scenario := range scenarios {
		scenario := scenario
		t.Run("submit/"+scenario.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSubmit(profile, batchValidCtx(2), nil, batchTestMCPSpec(scenario.group), 8, 500, scenario.resolver)
			if scenario.want != nil {
				require.ErrorIs(t, err, scenario.want)
				return
			}
			require.NoError(t, err)
		})
		t.Run("add/"+scenario.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAdd(profile, batchValidCtx(2), nil, batchTestMCPSpec(scenario.group), 1, 1, 8, 500, scenario.resolver)
			if scenario.want != nil {
				require.ErrorIs(t, err, scenario.want)
				return
			}
			require.NoError(t, err)
		})
	}

	t.Run("annotated rank-2 leaf under rank-2 waiter fine", func(t *testing.T) {
		t.Parallel()
		// The leaf's classified rank never constrains the waiter; only the
		// edge rank does, and there is no edge here.
		require.NoError(t, ValidateSubmit(profile, batchValidCtx(2), nil,
			batchTestMCPSpec(""), 8, 500, batchTestResolver(batchinst.MCPCapability{Reentrant: true, Annotated: true}, true)))
		require.NoError(t, ValidateAdd(profile, batchValidCtx(2), nil,
			batchTestMCPSpec(""), 1, 1, 8, 500, batchTestResolver(batchinst.MCPCapability{Reentrant: true, Annotated: true}, true)))
	})

	t.Run("non-MCP leaves never consult the resolver", func(t *testing.T) {
		t.Parallel()
		resolver := func(kind, tool string) (batchinst.MCPCapability, bool) {
			t.Errorf("resolver consulted for kind %q tool %q", kind, tool)
			return batchinst.MCPCapability{}, false
		}
		require.NoError(t, ValidateSubmit(profile, batchValidCtx(2), nil,
			batchTestLeaf(batchdispatch.KindGeneric, "view", ""), 8, 500, resolver))
	})
}

// TestBatchDiffEngineLimits is the differential test against the real engine.
// Equivalence asserted: for well-formed specs (parallel batch root, generic
// io tasks with a non-empty kind) the only possible engine rejection is an
// ErrInvalidSpec caused by the depth or node limits, and the validator
// rejects with its matching typed sentinel exactly when the engine rejects
// (both directions).
func TestBatchDiffEngineLimits(t *testing.T) {
	t.Parallel()
	profile, err := NewBatchProfileDefault()
	require.NoError(t, err)
	tools := append([]string(nil), batchExpectedIOTools...)

	rng := rand.New(rand.NewSource(20261012))
	var gen func(left int) jobbatch.Spec
	gen = func(left int) jobbatch.Spec {
		if left <= 0 || rng.Intn(3) == 0 {
			return batchTestLeaf(batchdispatch.KindGeneric, tools[rng.Intn(len(tools))], "")
		}
		items := make([]jobbatch.Spec, 1+rng.Intn(3))
		for i := range items {
			items[i] = gen(left - 1)
		}
		return jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Items: items}}
	}

	const iterations = 2500
	stats := struct{ both, engineOnly, validatorOnly int }{}
	for i := 0; i < iterations; i++ {
		spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{
			Mode:  jobbatch.ModeParallel,
			Items: []jobbatch.Spec{gen(3)},
		}}
		maxDepth := 1 + rng.Intn(5)
		maxNodes := 2 + rng.Intn(7)

		verr := ValidateSubmit(profile, batchValidCtx(2), nil, spec, maxDepth, maxNodes, nil)

		engine, eerr := jobbatch.NewEngine(profile.Limits(maxDepth, maxNodes))
		require.NoError(t, eerr)
		_, aerr := engine.Apply(jobbatch.Event{Type: jobbatch.EventSubmit, Root: "r", Spec: spec})

		if aerr == nil {
			require.NoError(t, verr, "engine accepted, validator rejected (depth %d nodes %d)", maxDepth, maxNodes)
			stats.both++
			continue
		}
		require.Error(t, verr, "engine rejected with %v, validator accepted", aerr)
		// Only limits can reject: the generator produces only well-formed
		// tasks and parallel batch roots.
		require.ErrorIs(t, aerr, jobbatch.ErrInvalidSpec)
		require.True(t,
			errors.Is(verr, batchinst.ErrBatchDepthExceeded) || errors.Is(verr, batchinst.ErrBatchNodeLimitExceeded),
			"validator error %v is not a limit rejection", verr)
		stats.engineOnly++
	}
	require.Greater(t, stats.both, 100)
	require.Greater(t, stats.engineOnly, 100)
}
