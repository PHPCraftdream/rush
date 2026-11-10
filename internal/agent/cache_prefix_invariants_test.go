package agent

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type cachePrefixIdentityTool struct {
	fantasy.AgentTool
}

func TestCachePrefix_ContextBuildDeterministicDedupeAndRefresh(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	root := t.TempDir()
	files := map[string]string{
		"a-rules.md": "shared instructions\n",
		"m-rules.md": "middle instructions\n",
		"z-rules.md": "shared instructions\n",
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}

	paths := []string{"z-rules.md", "m-rules.md", "a-rules.md"}
	build := func(paths []string) string {
		t.Helper()
		cfg := &config.Config{Options: &config.Options{ContextPaths: paths}}
		store := config.NewLibraryStore(cfg, root)
		p, err := prompt.NewPrompt("cache-prefix-context", `{{range .ContextFiles}}<file path="{{.Path}}">{{.Content}}</file>{{end}}`)
		require.NoError(t, err)
		got, err := p.Build(context.Background(), "", "", store, cfg, false)
		require.NoError(t, err)
		return got
	}

	first := build(paths)
	reversed := slices.Clone(paths)
	slices.Reverse(reversed)
	want := `<file path="` + filepath.Join(root, "a-rules.md") + `">shared instructions
</file><file path="` + filepath.Join(root, "m-rules.md") + `">middle instructions
</file>`
	require.Equal(t, want, first)
	require.Equal(t, first, build(reversed), "reversing configured context paths must not change rendered bytes")
	require.NotContains(t, first, "z-rules.md", "byte-identical content keeps only the lexically first path")

	require.NoError(t, os.WriteFile(filepath.Join(root, "m-rules.md"), []byte("updated middle instructions\n"), 0o600))
	updatedWant := `<file path="` + filepath.Join(root, "a-rules.md") + `">shared instructions
</file><file path="` + filepath.Join(root, "m-rules.md") + `">updated middle instructions
</file>`
	require.Equal(t, updatedWant, build(paths), "a fresh Build must render changed file content")
}

func TestCachePrefix_TodoChangesOnlyAppendReminder(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt").(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "todo-cache-prefix")
	require.NoError(t, err)
	for _, entry := range []struct {
		role message.MessageRole
		text string
	}{
		{role: message.User, text: "preserve the original request exactly"},
		{role: message.Assistant, text: "preserve the original response exactly"},
	} {
		_, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  entry.role,
			Parts: []message.ContentPart{message.TextContent{Text: entry.text}},
		})
		require.NoError(t, err)
	}
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	first, _ := sa.preparePrompt(msgs, []session.Todo{{Content: "compile fixture", Status: session.TodoStatusInProgress}}, "")
	changed, _ := sa.preparePrompt(msgs, []session.Todo{{Content: "review fixture", Status: session.TodoStatusPending}}, "")
	require.Len(t, first, 3)
	require.Len(t, changed, len(first))
	require.Equal(t, first[:len(first)-1], changed[:len(changed)-1], "todo mutations must leave every history message byte-for-byte unchanged")
	require.Equal(t, "preserve the original request exactly", first[0].Content[0].(fantasy.TextPart).Text)
	require.Equal(t, "preserve the original response exactly", first[1].Content[0].(fantasy.TextPart).Text)
	firstReminder := first[len(first)-1].Content[0].(fantasy.TextPart).Text
	changedReminder := changed[len(changed)-1].Content[0].(fantasy.TextPart).Text
	require.Contains(t, firstReminder, "compile fixture")
	require.NotContains(t, firstReminder, "review fixture")
	require.Contains(t, changedReminder, "review fixture")
	require.NotContains(t, changedReminder, "compile fixture")
	require.NotEqual(t, firstReminder, changedReminder)
}

func TestCachePrefix_ToolOrderStableAndMarkerPreservesPinnedSlices(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	base := coord.cfg.Config()
	forwardCfg := *base
	forwardCfg.Agents = maps.Clone(base.Agents)
	forwardAgent := forwardCfg.Agents[config.AgentCoder]
	forwardAgent.AllowedTools = []string{"wakein", "view", "inspect_agent", "inject_agent", "todos"}
	forwardAgent.AllowedMCP = map[string][]string{}
	forwardCfg.Agents[config.AgentCoder] = forwardAgent

	reverseCfg := *base
	reverseCfg.Agents = maps.Clone(base.Agents)
	reverseAgent := reverseCfg.Agents[config.AgentCoder]
	reverseAgent.AllowedTools = []string{"todos", "inject_agent", "view", "wakein", "inspect_agent"}
	reverseAgent.AllowedMCP = map[string][]string{}
	reverseCfg.Agents[config.AgentCoder] = reverseAgent

	build := func(cfg *config.Config, agentCfg config.Agent) []fantasy.AgentTool {
		t.Helper()
		got, err := coord.buildTools(t.Context(), cfg, agentCfg, false)
		require.NoError(t, err)
		return got
	}
	want := []string{"inject_agent", "inspect_agent", "todos", "view", "wakein"}
	forward := build(&forwardCfg, forwardAgent)
	reverse := build(&reverseCfg, reverseAgent)
	require.Equal(t, want, pinnedToolNames(forward), "assembly must sort constructor order into the canonical tool order")
	require.Equal(t, want, pinnedToolNames(reverse), "reversing AllowedTools must not alter the assembled order")
	for i, name := range want {
		require.Equal(t, forward[i].Info(), reverse[i].Info(), "reversed AllowedTools changed the definition for %q", name)
	}

	callCtx := WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: true})
	pinnedForward, err := coord.pinCallTools(callCtx, &forwardCfg)
	require.NoError(t, err)
	pinnedReverse, err := coord.pinCallTools(callCtx, &reverseCfg)
	require.NoError(t, err)
	require.Equal(t, want, pinnedToolNames(pinnedForward), "pinCallTools must preserve canonical sorted assembly order")
	require.Equal(t, want, pinnedToolNames(pinnedReverse))
	for i, name := range want {
		require.Equal(t, forward[i].Info(), reverse[i].Info(), "buildTools definition differs for %q", name)
		require.Equal(t, forward[i].Info(), pinnedForward[i].Info(), "pinCallTools definition differs for %q", name)
		require.Equal(t, forward[i].Info(), pinnedReverse[i].Info(), "reversed pinCallTools definition differs for %q", name)
	}

	// Production tools may be values; use explicit pointer identities for the marker contract.
	for i, tool := range pinnedForward {
		pinnedForward[i] = &cachePrefixIdentityTool{AgentTool: tool}
	}
	original := slices.Clone(pinnedForward)
	marked := withProviderOptionsOnLast(pinnedForward, cacheOpts())
	require.Equal(t, pinnedToolNames(original), pinnedToolNames(marked))
	for i := 0; i < len(marked)-1; i++ {
		require.Empty(t, marked[i].ProviderOptions(), "only the final assembled tool may carry cache options")
	}
	for i := range original {
		require.Same(t, original[i], pinnedForward[i], "marking must not replace elements in the pinned caller slice")
	}
	require.Empty(t, original[len(original)-1].ProviderOptions(), "the shared final tool must not acquire per-call cache options")
	require.Equal(t, cacheOpts(), marked[len(marked)-1].ProviderOptions())
	require.NotSame(t, original[len(original)-1], marked[len(marked)-1])
	for i := 0; i < len(original)-1; i++ {
		require.Same(t, original[i], marked[i])
	}

	customOrder := []fantasy.AgentTool{
		&cachePrefixIdentityTool{AgentTool: newProviderOptionsTestTool("zeta-custom")},
		&cachePrefixIdentityTool{AgentTool: newProviderOptionsTestTool("alpha-custom")},
	}
	customOriginal := slices.Clone(customOrder)
	customMarked := withProviderOptionsOnLast(customOrder, cacheOpts())
	require.Equal(t, []string{"zeta-custom", "alpha-custom"}, pinnedToolNames(customMarked), "marking must preserve intentional pinned order, not sort it")
	require.Same(t, customOriginal[0], customOrder[0])
	require.Same(t, customOriginal[1], customOrder[1])
	require.Same(t, customOrder[0], customMarked[0])
	require.Empty(t, customOrder[1].ProviderOptions())
	require.Equal(t, cacheOpts(), customMarked[1].ProviderOptions())
}
