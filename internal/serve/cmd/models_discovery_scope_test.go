package cmd

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestScopedDiscoveryProviders(t *testing.T) {
	t.Run("raw provider/model args", func(t *testing.T) {
		got := scopedDiscoveryProviders("zai/glm-5.3@low", "openai/gpt-5")
		slices.Sort(got)
		require.Equal(t, []string{"openai", "zai"}, got)
	})

	t.Run("atom args dedup to one provider", func(t *testing.T) {
		a, ok := atomRegistry["glm5_3"]
		require.True(t, ok, "glm5_3 atom must exist in the registry")
		require.Equal(t, "zai", a.Provider, "glm5_3 must map to the zai provider")
		require.Equal(t, []string{"zai"}, scopedDiscoveryProviders("glm5_3", "zai/glm-5.3@low"))
	})

	t.Run("unresolvable argument disables scoping", func(t *testing.T) {
		require.Nil(t, scopedDiscoveryProviders("definitely-not-an-atom-xyz"))
	})

	t.Run("mixed resolvable and unresolvable disables scoping", func(t *testing.T) {
		require.Nil(t, scopedDiscoveryProviders("glm5_3", "definitely-not-an-atom-xyz"))
	})

	t.Run("empty arguments disable scoping", func(t *testing.T) {
		require.Nil(t, scopedDiscoveryProviders())
	})
}

// newUseLikeCmd builds a fresh `rush models use` command tree with the flags
// commandDiscoveryScope reads, parses args into it, and returns the use
// command. The real modelsUseCmd's flag state is process-global, so tests
// must not reuse it for flag parsing.
func newUseLikeCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "rush"}
	models := &cobra.Command{Use: "models"}
	use := &cobra.Command{Use: "use"}
	root.AddCommand(models)
	models.AddCommand(use)
	for _, name := range []string{"smart", "fast", "worker", "reviewer"} {
		use.Flags().String(name, "", "")
	}
	require.NoError(t, use.Flags().Parse(args))
	return use
}

func TestCommandDiscoveryScope(t *testing.T) {
	t.Run("two positional args", func(t *testing.T) {
		cmd := newUseLikeCmd(t, "zai/glm-5.3@low", "openai/gpt-5")
		got := commandDiscoveryScope(cmd)
		slices.Sort(got)
		require.Equal(t, []string{"openai", "zai"}, got)
	})

	t.Run("worker flag only", func(t *testing.T) {
		cmd := newUseLikeCmd(t, "--worker", "zai/glm-5.3@low")
		require.Equal(t, []string{"zai"}, commandDiscoveryScope(cmd))
	})

	t.Run("unresolvable smart flag", func(t *testing.T) {
		cmd := newUseLikeCmd(t, "--smart", "definitely-not-an-atom-xyz")
		require.Nil(t, commandDiscoveryScope(cmd))
	})

	t.Run("no arguments", func(t *testing.T) {
		cmd := newUseLikeCmd(t)
		require.Nil(t, commandDiscoveryScope(cmd))
	})
}

// TestModelsUseCmdCommandPath guards the literal in commandDiscoveryScope's
// switch: if the real command tree's path ever changes, scoping would
// silently stop applying to `rush models use`.
func TestModelsUseCmdCommandPath(t *testing.T) {
	require.Equal(t, "rush models use", modelsUseCmd.CommandPath())
}
