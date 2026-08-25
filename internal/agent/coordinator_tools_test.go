package agent

// buildTools/buildAgentModels tests: worker-model preference for
// sub-agents, the worker toolset gate (buildToolsAgentConfig), and
// ask_question / allToolNames wiring coverage.

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRoleModelTestCoordinator builds a coordinator wired with distinct Smart,
// Small, and (optionally) Worker model slots, each backed by its own
// offline-safe openai-type provider (building an openai.Provider only
// constructs a client, it never makes a network call, so this is safe to run
// without a real API key/network — see buildOpenaiProvider). Used by
// TestBuildAgentModels_WorkerPreference to exercise buildAgentModels' new
// Worker-substitution branch end to end.
func newRoleModelTestCoordinator(t *testing.T, env fakeEnv, includeWorker bool) *coordinator {
	t.Helper()
	// Isolate from the host machine's real global config (e.g. ~/.local/share/rush
	// or %LocalAppData%\rush): without this, config.Init falls back to
	// GlobalConfigData(), which reads the real machine-wide rush.json and can
	// leak a real models.worker entry into "worker NOT configured" scenarios.
	// See isolateAllGlobalConfigPaths's doc comment for why both
	// RUSH_GLOBAL_CONFIG and RUSH_GLOBAL_DATA (not just the latter) must be
	// isolated, and why they must point at different directories.
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: openai.Name,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}

	cfg.Config().Models[config.SelectedModelTypeSmart] = registerProvider("smart-provider", "smart-model")
	cfg.Config().Models[config.SelectedModelTypeFast] = registerProvider("fast-provider", "fast-model")
	if includeWorker {
		cfg.Config().Models[config.SelectedModelTypeWorker] = registerProvider("worker-provider", "worker-model")
	}

	return &coordinator{
		cfg:      cfg,
		sessions: env.sessions,
	}
}

// TestBuildAgentModels_WorkerPreference pins the "prefer Worker for
// sub-agents when parent is Smart" behavior added alongside
// SetActiveModelRole: buildAgentModels must swap in the Worker model config
// as the sub-agent's smart slot only when (a) this is a sub-agent build, (b)
// the active role is unset/"smart" (parent running smart, or unknown which
// is treated as smart), and (c) a Worker model is actually configured. Every
// other combination must fall through to today's behavior (Large for
// everything) unchanged.
func TestBuildAgentModels_WorkerPreference(t *testing.T) {
	t.Run("worker configured + isSubAgent + role unset uses Worker for smart slot", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, true)
		// activeModelRole left at zero value ("") deliberately: unset must be
		// treated the same as "smart" (smart).

		smart, fast, err := coord.buildAgentModels(t.Context(), true)
		require.NoError(t, err)
		assert.Equal(t, "worker-provider", smart.ModelCfg.Provider, "sub-agent smart slot must come from Worker")
		assert.Equal(t, "worker-model", smart.ModelCfg.Model)
		assert.Equal(t, "fast-provider", fast.ModelCfg.Provider, "fast slot must be unaffected")
	})

	t.Run("worker configured + isSubAgent + role explicitly smart uses Worker for smart slot", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, true)
		coord.SetActiveModelRole(config.SelectedModelTypeSmart)

		smart, _, err := coord.buildAgentModels(t.Context(), true)
		require.NoError(t, err)
		assert.Equal(t, "worker-provider", smart.ModelCfg.Provider)
	})

	t.Run("worker NOT configured + isSubAgent falls back to Large (backward compat)", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, false)

		smart, _, err := coord.buildAgentModels(t.Context(), true)
		require.NoError(t, err)
		assert.Equal(t, "smart-provider", smart.ModelCfg.Provider, "must fall back to Smart when Worker isn't configured")
		assert.Equal(t, "smart-model", smart.ModelCfg.Model)
	})

	for _, role := range []config.SelectedModelType{
		config.SelectedModelTypeFast,
		config.SelectedModelTypeWorker,
		config.SelectedModelTypeReviewer,
	} {
		t.Run("worker configured + isSubAgent + active role "+string(role)+" does not force Worker", func(t *testing.T) {
			env := testEnv(t)
			coord := newRoleModelTestCoordinator(t, env, true)
			coord.SetActiveModelRole(role)

			smart, _, err := coord.buildAgentModels(t.Context(), true)
			require.NoError(t, err)
			assert.Equal(t, "smart-provider", smart.ModelCfg.Provider, "an explicit non-smart role for the whole run must not be second-guessed for sub-agents")
		})
	}

	t.Run("top-level agent (isSubAgent=false) always uses Smart regardless of Worker config or active role", func(t *testing.T) {
		env := testEnv(t)
		coord := newRoleModelTestCoordinator(t, env, true)
		coord.SetActiveModelRole(config.SelectedModelTypeSmart)

		smart, _, err := coord.buildAgentModels(t.Context(), false)
		require.NoError(t, err)
		assert.Equal(t, "smart-provider", smart.ModelCfg.Provider)
		assert.Equal(t, "smart-model", smart.ModelCfg.Model)
	})
}

// newWorkerToolTestCoordinator builds a coordinator with distinct Smart,
// Small, and (optionally) Worker model slots plus every service buildTools
// needs to actually construct tool instances (permissions/history/
// filetracker/messages) — a superset of newRoleModelTestCoordinator (which
// only wires the model slots, sufficient for buildAgentModels but not
// buildTools) and TestBuildTools_CoderHasAskQuestion's inline fixture.
func newWorkerToolTestCoordinator(t *testing.T, env fakeEnv, includeWorker bool) *coordinator {
	t.Helper()
	// Isolate from the host machine's real global config — see the identical
	// comment in newRoleModelTestCoordinator above for why this is required.
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: openai.Name,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}

	cfg.Config().Models[config.SelectedModelTypeSmart] = registerProvider("smart-provider", "smart-model")
	cfg.Config().Models[config.SelectedModelTypeFast] = registerProvider("fast-provider", "fast-model")
	if includeWorker {
		cfg.Config().Models[config.SelectedModelTypeWorker] = registerProvider("worker-provider", "worker-model")
	}

	// Explicitly populate the coder/task agent definitions. config.Load only
	// calls SetupAgents when IsConfigured() (i.e. at least one provider was
	// configured from the host env: an API key, or a local CLI provider like
	// `claude` on PATH) — on a from-scratch CI runner with no provider
	// credentials and no CLI binaries on PATH, IsConfigured() is false, Load
	// returns early, and Agents stays empty. This test injects its own
	// synthetic providers AFTER config.Init returns, so the Agents map would
	// otherwise never be built (and coord.cfg.Config().Agents[AgentTask] /
	// [AgentCoder] lookups in this file's tests would miss). Calling
	// SetupAgents here makes the test independent of whatever
	// providers the host machine happens to have configured.
	cfg.SetupAgents()

	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		modelCache:  csync.NewMap[string, cachedModelPair](),
	}
}

// buildSubAgentToolNames runs buildTools for the AgentTask sub-agent config
// and returns the resulting tool names, for assertions in
// TestBuildTools_WorkerToolset below.
func buildSubAgentToolNames(t *testing.T, coord *coordinator) []string {
	t.Helper()
	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "task agent must be configured")

	built, err := coord.buildTools(t.Context(), coord.cfg.Config(), taskCfg, true)
	require.NoError(t, err)

	names := make([]string, 0, len(built))
	for _, tool := range built {
		names = append(names, tool.Info().Name)
	}
	return names
}

// TestBuildTools_WorkerToolset is the BUG-2 fix's regression + behavior
// suite: the AgentTask sub-agent (spawned by the "agent" tool) must stay
// read-only in every case except when it is genuinely acting as a worker
// (Worker model configured AND the parent run's active role is smart
// or unset). Getting this wrong in either direction is bad: granting
// edit/write/bash unconditionally would let a plain search-and-context
// sub-agent mutate the filesystem in the ordinary interactive TUI/web path;
// never granting them makes the whole "smart orchestrator delegates
// hands-on work to a cheap Worker model" feature (see
// docs/plans/2026-07-26-orchestrator-worker-e2e.md, BUG-2) impossible.
func TestBuildTools_WorkerToolset(t *testing.T) {
	workerOnlyTools := []string{"edit", "multiedit", "write", "bash"}
	readOnlyTools := []string{"glob", "grep", "ls", "sourcegraph", "view"}

	t.Run("worker NOT configured, sub-agent stays exactly read-only (backward compat)", func(t *testing.T) {
		env := testEnv(t)
		coord := newWorkerToolTestCoordinator(t, env, false)

		names := buildSubAgentToolNames(t, coord)

		for _, name := range workerOnlyTools {
			assert.NotContains(t, names, name, "worker tool %q must be absent when no Worker model is configured", name)
		}
		for _, name := range readOnlyTools {
			assert.Contains(t, names, name, "read-only tool %q must still be present", name)
		}
		assert.NotContains(t, names, AgentToolName, "sub-agent must never get the agent tool")
	})

	activeSmartRoles := []config.SelectedModelType{"", config.SelectedModelTypeSmart}
	for _, role := range activeSmartRoles {
		t.Run("worker configured + active role "+string(role)+" (unset-or-large), sub-agent gets worker toolset", func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, true)
			if role != "" {
				coord.SetActiveModelRole(role)
			}
			// role == "" left unset deliberately: unset must be treated the
			// same as "smart" (smart), mirroring buildAgentModels semantics.

			names := buildSubAgentToolNames(t, coord)

			for _, name := range workerOnlyTools {
				assert.Contains(t, names, name, "worker tool %q must be present when Worker is configured and role is smart", name)
			}
			for _, name := range readOnlyTools {
				assert.Contains(t, names, name, "read-only tool %q must still be present for the worker", name)
			}
			assert.NotContains(t, names, AgentToolName, "worker must not get the agent tool: recursion guard against sub-workers spawning sub-workers")
			assert.Contains(t, names, tools.AskQuestionToolName, "worker must get ask_question now that runSubAgent frames a sub-agent question as a successful, resumable tool result instead of a generic error (see subAgentQuestionText in question_stop.go and the resume_session_id path in runSubAgent)")
		})
	}

	nonSmartRoles := []config.SelectedModelType{
		config.SelectedModelTypeFast,
		config.SelectedModelTypeWorker,
		config.SelectedModelTypeReviewer,
	}
	for _, role := range nonSmartRoles {
		t.Run("worker configured but active role "+string(role)+" falls back to read-only", func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, true)
			coord.SetActiveModelRole(role)

			names := buildSubAgentToolNames(t, coord)

			for _, name := range workerOnlyTools {
				assert.NotContains(t, names, name, "worker tool %q must be absent when the operator explicitly chose a non-smart role for the whole run", name)
			}
			for _, name := range readOnlyTools {
				assert.Contains(t, names, name, "read-only tool %q must still be present", name)
			}
		})
	}

	// The top-level coder's toolset under every Worker/role combination is
	// covered by TestBuildTools_OrchestratorEditToolsStripped below.
}

// TestBuildTools_OrchestratorEditToolsStripped is the #629 regression +
// behavior suite for the top-level coder: when a Worker model is configured
// AND the active role is smart or unset (workerSubAgentActive), the coder
// must LOSE edit/multiedit/write (so rule 7 is structural, not advice —
// three real `rush run --role smart` runs produced 0 agent-tool calls out
// of 50/51/24 with the worker never used) while KEEPING bash, every read
// tool, and the agent tool itself (zero-trust verification and delegation
// both require them). In every other configuration the coder's toolset must
// be byte-identical to before: no Worker configured, or an explicit
// non-smart role (fast/worker/reviewer), keeps the edit tools.
func TestBuildTools_OrchestratorEditToolsStripped(t *testing.T) {
	editTools := []string{"edit", "multiedit", "write"}
	keptTools := []string{"bash", "glob", "grep", "ls", "sourcegraph", "view", AgentToolName}

	buildCoderToolNames := func(t *testing.T, coord *coordinator) []string {
		t.Helper()
		coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
		require.True(t, ok, "coder agent must be configured")

		built, err := coord.buildTools(t.Context(), coord.cfg.Config(), coderCfg, false)
		require.NoError(t, err)

		names := make([]string, 0, len(built))
		for _, tool := range built {
			names = append(names, tool.Info().Name)
		}
		return names
	}

	// Byte-identical backward-compat cases: edit tools STAY.
	unchangedCases := []struct {
		name          string
		includeWorker bool
		role          config.SelectedModelType
	}{
		{"worker NOT configured, role unset (backward compat)", false, ""},
		{"worker NOT configured, role smart (backward compat)", false, config.SelectedModelTypeSmart},
		{"worker configured, role fast", true, config.SelectedModelTypeFast},
		{"worker configured, role worker", true, config.SelectedModelTypeWorker},
		{"worker configured, role reviewer", true, config.SelectedModelTypeReviewer},
	}
	for _, tc := range unchangedCases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, tc.includeWorker)
			if tc.role != "" {
				coord.SetActiveModelRole(tc.role)
			}

			names := buildCoderToolNames(t, coord)

			for _, name := range editTools {
				assert.Contains(t, names, name,
					"coder must keep %q when no Worker is configured or the role is explicitly non-smart — behavior must be byte-identical to before orchestrator mode", name)
			}
			for _, name := range keptTools {
				assert.Contains(t, names, name, "coder already has %q unconditionally; the strip must not touch it", name)
			}
		})
	}

	// Orchestrator-mode cases: edit tools REMOVED, everything else kept.
	for _, role := range []config.SelectedModelType{"", config.SelectedModelTypeSmart} {
		roleName := string(role)
		if roleName == "" {
			roleName = "unset"
		}
		t.Run("worker configured + role "+roleName+" - coder loses edit tools, keeps bash and read tools and agent", func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, true)
			if role != "" {
				coord.SetActiveModelRole(role)
			}

			names := buildCoderToolNames(t, coord)

			for _, name := range editTools {
				assert.NotContains(t, names, name,
					"coder must NOT have %q when a Worker is configured and the role is smart/unset — the orchestrator must delegate file mutation", name)
			}
			for _, name := range keptTools {
				assert.Contains(t, names, name,
					"coder must keep %q in orchestrator mode: read tools and bash for the zero-trust pass, agent for delegation", name)
			}
		})
	}
}

// TestBuildTools_WorkerResolvedToolsetByName reports, as an executable
// assertion, the worker sub-agent's FINAL resolved toolset (AgentTask base
// + workerToolNames layering): every search/read tool must survive the
// layering. #629 required verifying the worker can search by test, not by
// inspection. REVERT CHECK: remove any entry from workerToolNames (or from
// resolveReadOnlyTools in internal/config) and this goes red naming it.
func TestBuildTools_WorkerResolvedToolsetByName(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, true)

	names := buildSubAgentToolNames(t, coord)

	// The worker's complete expected toolset, by name: read-only base
	// (resolveReadOnlyTools) plus workerToolNames.
	expected := []string{
		"glob", "grep", "ls", "sourcegraph", "view", // read-only base
		"edit", "multiedit", "write", "bash", // hands-on
		"todos", "download", "fetch", tools.AskQuestionToolName,
	}
	for _, name := range expected {
		assert.Contains(t, names, name, "worker must have %q in its resolved toolset", name)
	}
	assert.NotContains(t, names, AgentToolName, "worker must never get the agent tool (recursion guard)")
}

// TestBuildToolsAgentConfig_UnconditionalApplicationWouldBreakBackwardCompat
// proves that regression guard (a) in TestBuildTools_WorkerToolset actually
// guards something: if buildToolsAgentConfig's gate were removed (i.e. the
// worker toolset applied unconditionally to every sub-agent build, worker
// configured or not), the read-only backward-compat case would fail. We
// simulate "unconditional" by calling the config-mutation helper directly
// with a coordinator that satisfies isSubAgent but deliberately has no
// Worker model configured and no active role set -- i.e. exactly the
// backward-compat scenario -- and confirm workerSubAgentActive (the gate)
// correctly reports false, which is what keeps buildToolsAgentConfig from
// mutating AllowedTools in that case. This documents, executably, why the
// gate in buildToolsAgentConfig cannot be dropped.
func TestBuildToolsAgentConfig_UnconditionalApplicationWouldBreakBackwardCompat(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false) // no Worker configured

	require.False(t, coord.workerSubAgentActive(coord.cfg.Config()),
		"backward-compat scenario (no Worker configured) must not read as worker-active")

	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok)
	original := append([]string(nil), taskCfg.AllowedTools...)

	// The gated call: must be a no-op copy of taskCfg.
	gated := coord.buildToolsAgentConfig(coord.cfg.Config(), taskCfg, true)
	assert.Equal(t, original, gated.AllowedTools, "gated call must leave AllowedTools untouched when Worker isn't configured")

	// The unconditional variant this test guards against: manually apply the
	// worker toolset the way buildToolsAgentConfig would if it had no gate at
	// all. If this is what shipped, the regression test above would fail
	// because edit/write/bash would leak into the interactive, no-worker,
	// read-only sub-agent.
	unconditional := append([]string(nil), taskCfg.AllowedTools...)
	unconditional = append(unconditional, workerToolNames...)
	assert.NotEqual(t, original, unconditional,
		"sanity check: applying the worker toolset unconditionally would visibly change AllowedTools, proving the gate is load-bearing")
	for _, name := range workerToolNames {
		assert.Contains(t, unconditional, name)
	}
}

// TestBuildTools_CoderHasAskQuestion is a regression test for a wiring bug
// where tools.NewAskQuestionTool() was constructed in buildTools but
// "ask_question" was never added to allToolNames(), so the AllowedTools
// filter in buildTools silently dropped it for every agent (including the
// top-level coder). The tool object existed and its own unit tests passed,
// and the exit_reason "awaiting_answer" plumbing tested fine in isolation,
// but the two were never wired together end to end — the model could never
// see the tool. This test goes through the real buildTools/AllowedTools
// path (unlike ask_question_test.go, which only constructs the tool
// directly) so it fails if the wiring regresses again.
func TestBuildTools_CoderHasAskQuestion(t *testing.T) {
	env := testEnv(t)
	// Isolate from the host machine's real global config — see the identical
	// comment in newRoleModelTestCoordinator above for why this is required.
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: openai.Name,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}
	cfg.Config().Models[config.SelectedModelTypeSmart] = registerProvider("smart-provider", "smart-model")
	cfg.Config().Models[config.SelectedModelTypeFast] = registerProvider("fast-provider", "fast-model")

	// config.Load only calls SetupAgents when IsConfigured() — false on a
	// from-scratch CI runner with no provider credentials and no CLI binaries
	// on PATH — so Agents would otherwise be empty. See the fuller note in
	// newWorkerToolTestCoordinator.
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}

	coderCfg, ok := cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	require.Contains(t, coderCfg.AllowedTools, tools.AskQuestionToolName,
		"allToolNames() must include ask_question or the coder agent will never be allowed to use it")

	built, err := coord.buildTools(t.Context(), cfg.Config(), coderCfg, false)
	require.NoError(t, err)

	names := make([]string, 0, len(built))
	for _, tool := range built {
		names = append(names, tool.Info().Name)
	}
	assert.Contains(t, names, tools.AskQuestionToolName,
		"buildTools must return ask_question for the top-level coder agent, not silently drop it in the AllowedTools filter")
}

// TestBuildTools_CoderHasAskQuestion_AllRoles pins the claim that
// ask_question's presence in the TOP-LEVEL coder's tool set is
// role-independent: buildToolsAgentConfig (coordinator_tools.go) only ever
// modifies the coder's AllowedTools to REMOVE edit/multiedit/write (see
// orchestratorStrippedToolNames; it never removes ask_question), so
// for the top-level coder (isSubAgent=false, as buildTools is always called
// for the coder agent) the AllowedTools passed through is always
// allToolNames() verbatim (modulo DisabledTools, unset here) regardless of
// SetActiveModelRole. This test exercises buildTools for the coder under
// every SetActiveModelRole value (smart, fast, worker, reviewer, and the
// unset/"" case that TestBuildTools_CoderHasAskQuestion already covered
// without a role) and asserts ask_question survives the AllowedTools filter
// in every single one -- a regression guard one level more specific than
// TestBuildTools_CoderHasAskQuestion (which only ever tested the unset-role
// case).
func TestBuildTools_CoderHasAskQuestion_AllRoles(t *testing.T) {
	roles := []config.SelectedModelType{
		config.SelectedModelTypeSmart,
		config.SelectedModelTypeFast,
		config.SelectedModelTypeWorker,
		config.SelectedModelTypeReviewer,
		"", // unset -- treated as smart by workerSubAgentActive/buildAgentModels
	}

	for _, role := range roles {
		name := string(role)
		if name == "" {
			name = "unset"
		}
		t.Run("role="+name, func(t *testing.T) {
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, true) // Worker configured too, to stress the sub-agent-only gate
			if role != "" {
				coord.SetActiveModelRole(role)
			}

			coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
			require.True(t, ok, "coder agent must be configured")
			require.Contains(t, coderCfg.AllowedTools, tools.AskQuestionToolName,
				"allToolNames() must include ask_question or the coder agent will never be allowed to use it")

			built, err := coord.buildTools(t.Context(), coord.cfg.Config(), coderCfg, false)
			require.NoError(t, err)

			names := make([]string, 0, len(built))
			for _, tool := range built {
				names = append(names, tool.Info().Name)
			}
			assert.Contains(t, names, tools.AskQuestionToolName,
				"buildTools must return ask_question for the top-level coder agent under active role %q -- role must never gate ask_question at the top level, only isSubAgent+workerSubAgentActive does (see buildToolsAgentConfig)", role)
		})
	}
}

// TestAllToolNames_CoversUnconditionallyBuiltTools is a guard against this
// entire class of bug in the future: buildTools constructs a fixed slice of
// tools unconditionally (everything except the "agent" and "agentic_fetch"
// tools, which are gated on AllowedTools before construction), and then
// filters ALL of allTools through slices.Contains(agent.AllowedTools, ...).
// If a tool is added to that unconditional-construction list in buildTools
// but its name is never added to allToolNames() (internal/config/config.go),
// it is built and then silently discarded for every agent, exactly like
// ask_question was. This test enumerates the same set of tool names
// buildTools unconditionally constructs and asserts each one is present in
// the coder agent's resolved AllowedTools, which -- with no DisabledTools
// configured -- is exactly allToolNames() (see
// resolveAllowedTools/SetupAgents in internal/config/config.go). We go
// through Agents[AgentCoder].AllowedTools rather than calling allToolNames()
// directly because that function is unexported to internal/config.
func TestAllToolNames_CoversUnconditionallyBuiltTools(t *testing.T) {
	// Mirrors the unconditional append(...) block in coordinator.go's
	// buildTools (currently lines ~1358-1377) that runs regardless of
	// agent.AllowedTools. Keep in sync with that block: if a tool is added
	// there, add its name here too, and this test will catch the case
	// where allToolNames() itself wasn't updated to match.
	unconditionallyBuilt := []string{
		tools.AskQuestionToolName,
		"bash",
		"rush_info",
		"rush_logs",
		"job_output",
		"job_kill",
		"download",
		"edit",
		"multiedit",
		"fetch",
		"glob",
		"grep",
		"ls",
		tools.ReadDelegationTranscriptToolName,
		"sourcegraph",
		"todos",
		"view",
		"write",
	}

	env := testEnv(t)
	// Isolate from the host machine's real global config, then explicitly set
	// up agents: config.Load only calls SetupAgents when IsConfigured(), which
	// is false on a from-scratch CI runner (no provider credentials, no CLI
	// binaries on PATH). Without this the Agents map is empty and the
	// Agents[AgentCoder] lookup below misses. See newWorkerToolTestCoordinator
	// for the fuller note.
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coderCfg, ok := cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	require.Empty(t, cfg.Config().Options.DisabledTools,
		"test assumes no DisabledTools so AllowedTools == allToolNames() verbatim")

	for _, name := range unconditionallyBuilt {
		assert.Contains(t, coderCfg.AllowedTools, name,
			"tool %q is unconditionally constructed by buildTools but missing from allToolNames(); it will be silently dropped by the AllowedTools filter for every agent", name)
	}
}
