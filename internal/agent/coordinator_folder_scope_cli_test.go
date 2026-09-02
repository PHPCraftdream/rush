package agent

// T9 regression tests: a folder-scoped call whose resolved provider is a
// CLI provider (Type "cli") must be REFUSED at the model resolvers — the
// CLI provider executes file tools inside a subprocess whose tool
// implementations cannot see the FolderScope, so the scope would silently
// mean nothing. The refusal fires before any provider traffic; the tests
// below never spawn a real claude/codex/gemini/qwen binary.

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFolderScopeCLICoordinator builds a coordinator like
// newToolPinningCoordinator, but registers the smart/worker slots against
// a provider whose Type is configurable, so the tests can resolve a
// "cli"-typed provider through the ordinary config path.
func newFolderScopeCLICoordinator(t *testing.T, env fakeEnv, smartType, workerType catwalk.Type, includeWorker bool) *coordinator {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string, providerType catwalk.Type) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: providerType,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}
	// "cli-claude-sonnet" is the model id stubCLIAvailability reports, so
	// the smart slot resolves on the CLI provider without a real binary.
	cfg.Config().Models[config.SelectedModelTypeSmart] =
		registerProvider("smart-provider", "cli-claude-sonnet", smartType)
	cfg.Config().Models[config.SelectedModelTypeFast] =
		registerProvider("fast-provider", "fast-model", openai.Name)
	if includeWorker {
		cfg.Config().Models[config.SelectedModelTypeWorker] =
			registerProvider("worker-provider", "worker-model", workerType)
	}
	cfg.SetupAgents()

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		prompt:      p,
		modelCache:  csync.NewMap[string, cachedModelPair](),
	}
}

// stubCLIAvailability replaces cliprovider's binary detection with a stub:
// the unscoped control calls below build models from the "cli"-typed
// provider, and that must not probe for real claude/codex binaries.
func stubCLIAvailability(t *testing.T) {
	t.Helper()
	orig := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec {
		return []cliprovider.CLISpec{{ModelID: "cli-claude-sonnet", ModelName: "Claude Sonnet"}}
	}
	t.Cleanup(func() { cliprovider.AvailableFunc = orig })
}

// TestFolderScope_RejectsCLISmartProvider proves the smart-role refusal:
// a scoped call resolved onto a "cli"-typed provider errors at the
// resolver naming the role and the provider type, while the SAME call
// without a FolderScope succeeds.
func TestFolderScope_RejectsCLISmartProvider(t *testing.T) {
	stubCLIAvailability(t)
	env := testEnv(t)
	coord := newFolderScopeCLICoordinator(t, env, cliprovider.ProviderType, openai.Name, false)

	sess, err := coord.sessions.Create(t.Context(), "cli-smart-scoped")
	require.NoError(t, err)

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctxScoped := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	_, err = coord.resolveSessionModels(ctxScoped, sess.ID)
	require.Error(t, err, "a folder-scoped call on a CLI provider must be refused")
	assert.Contains(t, err.Error(), "smart")
	assert.Contains(t, err.Error(), "cli")

	// Control: the SAME resolution without a FolderScope succeeds.
	_, err = coord.resolveSessionModels(WithCallOptions(t.Context(), &CallOptions{}), sess.ID)
	require.NoError(t, err)
}

// TestFolderScope_NonCLIProviderStillSucceeds proves the refusal is
// CLI-specific: a folder-scoped call with the default mock provider still
// resolves normally.
func TestFolderScope_NonCLIProviderStillSucceeds(t *testing.T) {
	env := testEnv(t)
	coord := newFolderScopeCLICoordinator(t, env, openai.Name, openai.Name, false)

	sess, err := coord.sessions.Create(t.Context(), "openai-scoped")
	require.NoError(t, err)

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctxScoped := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	pinned, err := coord.resolveSessionModels(ctxScoped, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pinned.tools)
	assert.Contains(t, pinnedToolNames(pinned.tools), "fs_read")
}

// TestFolderScope_RejectsCLIWorkerProvider proves the worker-role refusal:
// a scoped call whose Worker slot resolves to a "cli"-typed provider is
// refused too, because the call's sub-agent spawns resolve through the
// Worker slot.
func TestFolderScope_RejectsCLIWorkerProvider(t *testing.T) {
	stubCLIAvailability(t)
	env := testEnv(t)
	coord := newFolderScopeCLICoordinator(t, env, openai.Name, cliprovider.ProviderType, true)

	sess, err := coord.sessions.Create(t.Context(), "cli-worker-scoped")
	require.NoError(t, err)

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctxScoped := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	_, err = coord.resolveSessionModels(ctxScoped, sess.ID)
	require.Error(t, err, "a folder-scoped call on a CLI worker provider must be refused")
	assert.Contains(t, err.Error(), "worker")
	assert.True(t, strings.Contains(err.Error(), "cli"))

	// Control: the SAME resolution without a FolderScope succeeds.
	_, err = coord.resolveSessionModels(WithCallOptions(t.Context(), &CallOptions{}), sess.ID)
	require.NoError(t, err)
}
