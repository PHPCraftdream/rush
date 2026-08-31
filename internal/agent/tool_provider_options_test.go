package agent

import (
	"context"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/stretchr/testify/require"
)

type providerOptionsTestParams struct {
	Value string `json:"value" description:"anything"`
}

func newProviderOptionsTestTool(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, "test tool",
		func(context.Context, providerOptionsTestParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		})
}

func cacheOpts() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

// TestWithProviderOptionsOnLastLeavesSharedToolsUntouched pins the invariant
// the fix exists for: attaching a turn's cache-control marker must not write
// through to the tool objects, which sessionAgent.tools shares across every
// session and every concurrent turn on the agent.
func TestWithProviderOptionsOnLastLeavesSharedToolsUntouched(t *testing.T) {
	shared := []fantasy.AgentTool{
		newProviderOptionsTestTool("alpha"),
		newProviderOptionsTestTool("omega"),
	}
	last := shared[1]

	marked := withProviderOptionsOnLast(shared, cacheOpts())

	require.Len(t, marked, len(shared))
	require.Empty(t, last.ProviderOptions(), "the shared tool object must not be mutated")
	require.Empty(t, shared[1].ProviderOptions(), "the caller's slice must not be rewritten")
	require.Same(t, last, shared[1], "the caller's slice element must not be replaced")
	require.NotSame(t, last, marked[len(marked)-1], "the marked list must carry a wrapper, not the shared tool")
	require.Equal(t, cacheOpts(), marked[len(marked)-1].ProviderOptions())
	require.Equal(t, "omega", marked[len(marked)-1].Info().Name, "the wrapper must delegate Info")

	// Only the LAST tool is marked.
	require.Empty(t, marked[0].ProviderOptions())
}

// TestWithProviderOptionsOnLastEdgeCases covers the two no-op inputs so the
// helper can be called unconditionally at both turn-start and per-step.
func TestWithProviderOptionsOnLastEdgeCases(t *testing.T) {
	require.Nil(t, withProviderOptionsOnLast(nil, cacheOpts()))

	shared := []fantasy.AgentTool{newProviderOptionsTestTool("solo")}
	require.Same(t, shared[0], withProviderOptionsOnLast(shared, fantasy.ProviderOptions{})[0],
		"empty options must not allocate a wrapper")
}

// TestWithProviderOptionsOnLastConcurrentTurns is the -race regression for the
// original defect: two turns running at once on ONE sessionAgent each attach
// their own freshly built cache-control options to the SAME (name-sorted, so
// identical) last tool. The old code did that with
// AgentTool.SetProviderOptions, i.e. two unsynchronized writes of a new map
// header into fantasy's funcToolWrapper.providerOptions — reported by -race as
//
//	Write at 0x... by goroutine A: funcToolWrapper.SetProviderOptions <- runTurn
//	Previous write at 0x... by goroutine B: same
//
// Each goroutine must also still observe ITS OWN options, not a neighbour's.
func TestWithProviderOptionsOnLastConcurrentTurns(t *testing.T) {
	shared := []fantasy.AgentTool{
		newProviderOptionsTestTool("alpha"),
		newProviderOptionsTestTool("omega"),
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 50 {
				mine := cacheOpts()
				marked := withProviderOptionsOnLast(shared, mine)
				got := marked[len(marked)-1].ProviderOptions()
				require.Equal(t, mine, got)
			}
		})
	}
	wg.Wait()

	require.Empty(t, shared[1].ProviderOptions(), "no turn may leave state on the shared tool")
}
