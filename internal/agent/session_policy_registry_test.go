package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestReadonlySessionToolsExistAndAreReadOnlyInProductionRegistry(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	coder, ok := coord.cfg.Config().Agents["coder"]
	require.True(t, ok)
	built, err := coord.buildTools(t.Context(), coord.cfg.Config(), coder, false)
	require.NoError(t, err)

	registered := make(map[string]struct{}, len(built))
	for _, tool := range built {
		registered[tool.Info().Name] = struct{}{}
	}
	for _, name := range permission.ReadonlySessionTools {
		// The fs_* family only reaches a folder-scoped toolset; an unscoped build omits it.
		if _, scopedOnly := folderScopeOpForTool[name]; !scopedOnly {
			_, exists := registered[name]
			require.Truef(t, exists, "readonly policy tool %q is absent from production buildTools registry", name)
		}
		action, hasAction := restrictedToolActions[name]
		require.Truef(t, hasAction, "readonly policy tool %q has no restricted action metadata", name)
		require.Containsf(t, []string{"read", "list"}, action, "readonly policy tool %q has non-read action %q", name, action)
	}
}
