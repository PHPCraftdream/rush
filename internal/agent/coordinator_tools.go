// SessionAgent and tool-slice construction, including worker sub-agent
// toolset layering. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/hooks"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/shell"
)

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	// ONE snapshot for the entire build: models, prompt, and tools. Before
	// this (task #576/P1-3), models came from buildAgentModels' own
	// Snapshot() while the provider options/system-prompt-prefix/Options
	// fields below and buildTools' many reads each took a fresh, separately
	// timed c.cfg.Config(). A reload landing anywhere in between could hand
	// back an agent whose model, prompt (WorkerAvailable) and toolset
	// (worker tool layering, hooks, grep/ls options, attribution, skills
	// paths) disagreed with each other -- an internally inconsistent agent
	// built across two config generations. Threading this one cfg through
	// closes that gap for all of the above; only explicitly-declared live
	// *policy* reads remain (see PeakHoursCheck below), plus MCP tools,
	// which are deliberately NOT pinned -- see the MCP block in buildTools
	// for why.
	cfg, _ := c.cfg.Snapshot()

	smart, fast, err := c.buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
	if err != nil {
		return nil, err
	}

	smartProviderCfg, _ := cfg.Providers.Get(smart.ModelCfg.Provider)
	opts := cfg.Options
	var streamIdleTimeout time.Duration
	if opts != nil && opts.StreamIdleTimeoutSeconds > 0 {
		streamIdleTimeout = time.Duration(opts.StreamIdleTimeoutSeconds) * time.Second
	}
	// Never-freeze backstop: bound the watchdog's tool-pause.
	var toolMaxDuration time.Duration
	if opts != nil && opts.StreamToolTimeoutSeconds > 0 {
		toolMaxDuration = time.Duration(opts.StreamToolTimeoutSeconds) * time.Second
	}
	// Fork patch: batch 8 — mid-stream checkpoint interval.
	var checkpointInterval time.Duration
	if opts != nil {
		switch {
		case opts.CheckpointIntervalSeconds > 0:
			checkpointInterval = time.Duration(opts.CheckpointIntervalSeconds) * time.Second
		case opts.CheckpointIntervalSeconds == -1:
			checkpointInterval = 0 // explicitly disabled
		default:
			checkpointInterval = defaultCheckpointInterval
		}
	}
	// DisableAutoSummarize/DataDirectory must also tolerate a nil Options —
	// the streamIdle/toolMax/checkpointInterval reads above already do.
	var disableAutoSummarize bool
	var dataDirectory string
	if opts != nil {
		disableAutoSummarize = opts.DisableAutoSummarize
		dataDirectory = opts.DataDirectory
	}
	// R3-4: the turn-level activation of per-call restricted-run policies
	// needs the optional permission extension. Nil-safe like c.permissions
	// everywhere else in this function (bare test-fixture coordinators).
	var runAllowlists permission.SessionRunAllowlistManager
	if c.permissions != nil {
		if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok {
			runAllowlists = mgr
		}
	}
	result := NewSessionAgent(SessionAgentOptions{
		SmartModel:           smart,
		FastModel:            fast,
		SystemPromptPrefix:   smartProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: disableAutoSummarize,
		// permissions can be nil on bare test-fixture coordinators; treat
		// that as fail-closed (not YOLO) rather than panicking.
		IsYolo:             c.permissions != nil && c.permissions.SkipRequests(),
		Sessions:           c.sessions,
		Messages:           c.messages,
		Tools:              nil,
		Notify:             c.notify,
		StreamIdleTimeout:  streamIdleTimeout,
		ToolMaxDuration:    toolMaxDuration,
		DataDirectory:      dataDirectory,
		RunAllowlists:      runAllowlists,
		CheckpointInterval: checkpointInterval, // Fork patch: batch 8
		// Fork patch: peak-hours mid-turn re-check. Deliberately LIVE, not
		// pinned to cfg above: this is a runtime *policy* check re-evaluated
		// on every mid-turn tick for the lifetime of the turn (see
		// checkLivePeakHours), not a one-shot construction-time read, so
		// pinning it to the build-time generation would defeat its purpose
		// (a peak_hours edit made by another process while this turn is
		// running must still take effect).
		PeakHoursCheck: func() error {
			return c.checkLivePeakHours(smart.ModelCfg.Provider)
		},
	})

	// R1-1: resolve the per-call worker-active predicate SYNCHRONOUSLY,
	// before the async build goroutines below are registered — the value
	// they see is fixed at UpdateModels/buildAgent call time, never a lazy
	// read of shared state from inside an already-running goroutine (the
	// hooks/runner.go sync-capture pattern). With a CallOptions-carrying
	// context the value is this call's own ModelRole; without one it is
	// the legacy shared field, read here under its mutex rather than
	// later on a goroutine.
	workerActive := c.workerSubAgentActiveForCall(ctx, cfg)

	c.readyWg.Go(func() error {
		// Orchestrator block only ever applies to the top-level coder prompt
		// (isSubAgent false) — a sub-agent renders task.md.tpl via
		// taskPrompt, which doesn't reference WorkerAvailable, but guarding
		// on !isSubAgent here keeps this call's intent explicit rather than
		// relying on the template to ignore the field.
		// Same pinned cfg as the models above and buildTools below — see
		// this function's opening comment.
		systemPrompt, err := prompt.Build(ctx, smart.Model.Provider(), smart.Model.Model(), c.cfg, cfg, !isSubAgent && workerActive)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	})

	c.readyWg.Go(func() error {
		tools, err := c.buildTools(ctx, cfg, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	})

	return result, nil
}

// workerToolNames are added on top of the sub-agent's existing (read-only)
// AllowedTools when it is acting as a worker (see workerSubAgentActive):
// enough hands to actually do delegated work, not just look around.
//
// Deliberately excludes "agent": a worker must not spawn sub-workers of its
// own — recursion guard, keeps the delegation tree exactly two levels deep
// (orchestrator -> worker).
//
// Includes "ask_question": runSubAgent now recognizes AwaitingAnswerError
// before its generic error branch and returns a question-shaped SUCCESSFUL
// tool response (see subAgentQuestionText in question_stop.go) instead of
// collapsing it into "Failed to generate response: ...". The orchestrator
// answers via AgentParams.ResumeSessionID, which continues the same
// sub-session rather than starting a fresh one. See
// docs/plans/2026-07-26-orchestrator-worker-e2e.md, phase 3, for the full
// round-trip design and why a synchronous blocking version is impossible.
var workerToolNames = []string{"edit", "multiedit", "write", "bash", "todos", "download", "fetch", tools.AskQuestionToolName}

// orchestratorStrippedToolNames are REMOVED from the top-level coder's
// AllowedTools when workerSubAgentActive(cfg) is true (worker configured AND
// active role empty-or-smart). The orchestrator must delegate all file
// mutation to the worker: as long as edit/multiedit/write are present, rule
// 7 in the prompt is only advice, and three real `rush run --role smart`
// runs showed the smart model doing all the hands-on work itself (0 agent
// calls out of 50/51/24 tool calls) with a configured worker never used.
//
// bash and every read tool are deliberately KEPT: rule 7's zero-trust pass
// requires the orchestrator to re-read files and re-run tests itself, and
// delegating verification would mean trusting a worker's report about
// checking a worker's report. The resulting hole — bash can still write
// files via sed, cat >, redirection — is accepted KNOWINGLY: it turns an
// accidental edit into a deliberate act, it does not make the ban airtight.
var orchestratorStrippedToolNames = []string{"edit", "multiedit", "write"}

// buildToolsAgentConfig returns the config.Agent buildTools should use to
// resolve AllowedTools for this build. Two mutations, both gated on
// workerSubAgentActive (worker configured AND active role empty-or-smart):
//
//   - a sub-agent acting as a worker gets a copy of agent with the worker
//     toolset layered on top of whatever was already allowed (today's
//     read-only set, in practice, since this only affects the AgentTask
//     sub-agent);
//   - the top-level coder gets a copy of agent with the edit tools in
//     orchestratorStrippedToolNames removed, so the orchestrator cannot
//     mutate files directly and must go through the `agent` tool.
//
// In every other case — no Worker configured, or an explicit non-smart
// active role (fast/worker/reviewer) — it returns agent unchanged, so
// behavior is byte-identical to before orchestrator mode existed.
// cfg is the caller's pinned snapshot (task #576/P1-3) — buildTools no
// longer takes its own live c.cfg.Config() reads, so this decision now
// agrees with every other config-derived choice buildAgent makes for the
// same build (models, prompt's WorkerAvailable, hooks, MCP, etc.).
//
// R3-1: the gate is now the PER-CALL predicate workerSubAgentActiveForCall,
// so an explicit --role fast/worker/reviewer call's tool set agrees with
// the prompt/model chosen for the SAME call instead of with the shared
// activeModelRole field.
func (c *coordinator) buildToolsAgentConfigForCall(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	if !c.workerSubAgentActiveForCall(ctx, cfg) {
		return agent
	}

	if isSubAgent {
		allowed := make([]string, len(agent.AllowedTools), len(agent.AllowedTools)+len(workerToolNames))
		copy(allowed, agent.AllowedTools)
		for _, name := range workerToolNames {
			if !slices.Contains(allowed, name) {
				allowed = append(allowed, name)
			}
		}
		agent.AllowedTools = allowed
		return agent
	}

	// Top-level coder (buildAgent is only ever called with isSubAgent=false
	// for the coder agent): strip the orchestrator's direct edit tools.
	allowed := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if !slices.Contains(orchestratorStrippedToolNames, name) {
			allowed = append(allowed, name)
		}
	}
	agent.AllowedTools = allowed
	return agent
}

// buildToolsAgentConfig is the legacy, context-less form of
// buildToolsAgentConfigForCall, kept for direct tests: it behaves exactly
// like a caller whose context carries no CallOptions.
func (c *coordinator) buildToolsAgentConfig(cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	return c.buildToolsAgentConfigForCall(context.Background(), cfg, agent, isSubAgent)
}

// applyCallDisableSubAgents implements the per-call `--agents single`
// sub-agent ban (R1-1). It replaces ExecuteRun's former mutation of the
// PUBLISHED coder AllowedTools (disableSubAgentToolsInConfig +
// UpdateAgentAllowedTools): that write raced every concurrent run's
// toolset build for the whole duration of the mutating run, so a
// DisableSubAgents:false call could observe the delegating call's
// stripped toolset until its defer restored the list. The per-call
// filter touches nothing shared: it applies to THIS build only, and only
// to the top-level coder (isSubAgent false) — exactly the agent config
// the old mutation targeted (cfg.Agents[AgentCoder]).
//
// Semantics mirror the app-side shouldBypassSubAgentBan: strip both
// delegation tools, except when the run explicitly runs the smart role
// AND a Worker model is configured — delegation IS the intended workflow
// there, so only agentic_fetch stays stripped. A context without
// CallOptions (legacy callers) changes nothing.
func (c *coordinator) applyCallDisableSubAgents(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	opts := callOptionsFrom(ctx)
	if opts == nil || !opts.DisableSubAgents || isSubAgent {
		return agent
	}

	bypass := opts.ModelRole == config.SelectedModelTypeSmart
	if bypass {
		workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
		bypass = ok && workerModelCfg.Model != ""
	}

	stripped := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if name == tools.AgenticFetchToolName {
			continue
		}
		if name == AgentToolName && !bypass {
			continue
		}
		stripped = append(stripped, name)
	}
	agent.AllowedTools = stripped
	return agent
}

// folderScopeLegacyFileTools are the single-target file tools a scoped
// call never sees: the fs_* family replaces all of them, with batch
// items in place of the single path parameter.
var folderScopeLegacyFileTools = map[string]struct{}{
	tools.ViewToolName: {}, tools.GlobToolName: {}, tools.GrepToolName: {},
	tools.LSToolName: {}, tools.WriteToolName: {}, tools.EditToolName: {},
	tools.MultiEditToolName: {},
}

// folderScopeEscapeHatchTools read or write outside any path policy a
// folder scope can express, so a scoped call never sees them: download
// writes arbitrary paths, git_read reads the whole repository, the MCP
// resource tools would expose whatever the configured servers serve
// (AllowedMCP is cleared below for the same reason), and agentic_fetch
// builds its own unscooped view/glob/grep in a temp dir.
var folderScopeEscapeHatchTools = map[string]struct{}{
	tools.DownloadToolName: {}, tools.GitReadToolName: {},
	tools.AgenticFetchToolName: {}, tools.ListMCPResourcesToolName: {},
	tools.ReadMCPResourceToolName: {},
}

// folderScopeCommandTools execute shell commands, which trivially
// bypass any filesystem scope. They stay only when the scope says so
// (FolderScope.KeepsCommandTools, set by the run plumbing from the
// restricted-run bash allowlist): keeping them is a deliberate grant,
// not a default.
var folderScopeCommandTools = map[string]struct{}{
	tools.BashToolName: {}, tools.RunCommandToolName: {},
	tools.JobOutputToolName: {}, tools.JobKillToolName: {},
}

// folderScopeOpForTool maps each fs_* tool to the single operation that
// gates its presence in a scoped toolset. fs_write is the one
// exception: it appears when EITHER create or overwrite is granted,
// because its per-item check picks the operation by path existence.
var folderScopeOpForTool = map[string]permission.FileOp{
	tools.FSListToolName:       permission.FileOpList,
	tools.FSFindToolName:       permission.FileOpFind,
	tools.FSGrepToolName:       permission.FileOpGrep,
	tools.FSReadToolName:       permission.FileOpRead,
	tools.FSReplaceToolName:    permission.FileOpReplace,
	tools.FSWriteLinesToolName: permission.FileOpWriteLines,
	tools.FSDeleteToolName:     permission.FileOpDelete,
}

// applyCallFolderScope implements the per-call scoped filesystem
// toolset: when ctx carries CallOptions.FolderScope, the agent's
// AllowedTools is replaced with the tools that scope allows and
// AllowedMCP becomes the "no MCPs" spelling, because an external
// filesystem MCP server would bypass the scope wholesale. It MUST run
// after buildToolsAgentConfigForCall: worker layering ADDS
// bash/edit/write to a worker sub-agent's list, and this filter has to
// see that final list to strip them again. A context without
// CallOptions changes nothing, exactly like applyCallDisableSubAgents.
//
// Every fs_* name is decided by the scope's Grants here and never left
// to fall through from the incoming list (which the post-T7 defaults DO
// contain): ungranted fs_* tools are removed, granted ones are
// re-added exactly once, and fs_write additionally requires create OR
// overwrite. Tools outside all four sets -- todos, ask_question, the
// agent delegation tool and so on -- are untouched: the scope narrows
// the filesystem surface, not the conversation.
func (c *coordinator) applyCallFolderScope(ctx context.Context, agent config.Agent) config.Agent {
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.FolderScope == nil {
		return agent
	}
	scope := *opts.FolderScope

	stripped := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if _, ok := folderScopeLegacyFileTools[name]; ok {
			continue
		}
		if _, ok := folderScopeEscapeHatchTools[name]; ok {
			continue
		}
		if _, ok := folderScopeCommandTools[name]; ok && !scope.KeepsCommandTools() {
			continue
		}
		if _, ok := folderScopeOpForTool[name]; ok {
			// Re-decided by the Grants pass below; never fall through.
			continue
		}
		if name == tools.FSWriteToolName {
			// create OR overwrite, decided below.
			continue
		}
		stripped = append(stripped, name)
	}
	for toolName, op := range folderScopeOpForTool {
		if scope.Grants(op) {
			stripped = append(stripped, toolName)
		}
	}
	if scope.Grants(permission.FileOpCreate) || scope.Grants(permission.FileOpOverwrite) {
		stripped = append(stripped, tools.FSWriteToolName)
	}
	agent.AllowedTools = stripped
	agent.AllowedMCP = map[string][]string{}
	return agent
}

// rejectScopedCallOnCLIProvider refuses a folder-scoped call whose
// RESOLVED provider for the given role is a CLI provider (Type "cli"):
// that provider executes the file tools inside a CLI subprocess (claude,
// codex, gemini, qwen) whose MCP tool implementations cannot see the
// FolderScope, so the scope would silently mean nothing. The refusal is a
// hard error so it fires BEFORE any provider traffic or subprocess spawn.
// Unscoped calls and non-CLI providers pass untouched.
func (c *coordinator) rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error {
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.FolderScope == nil {
		return nil
	}
	if providerCfg.Type != cliprovider.ProviderType {
		return nil
	}
	return fmt.Errorf(
		"folder-scoped call cannot run on the %s role's CLI provider %q (type %q): the subprocess's file tools cannot see the folder scope",
		role, providerCfg.ID, cliprovider.ProviderType)
}

// buildTools builds the tool slice for agent. cfg is the pinned
// *config.Config the caller captured for this whole buildAgent call (task
// #576/P1-3) -- every config-derived choice below (worker tool layering,
// SSRF escape hatch, model metadata, hooks, background-job notify,
// attribution, grep/ls options, skills paths) now reads from this one
// generation instead of each taking its own, separately timed
// c.cfg.Config() call. c.cfg.WorkingDir() stays live: it is process-stable
// (does not change across a config reload), same precedent as
// prompt.Build's store.WorkingDir()/store.Resolver() reads.
//
// MCP tools are the one deliberate exception (task #591/P2-1): the actual
// tool set below still comes from tools.GetMCPTools, which enumerates the
// package-level mcp.Tools() registry -- live MCP client connections and
// their current tool schemas, refreshed asynchronously by each server's own
// ToolListChangedHandler, EnableServer/DisableServer/RemoveServer, and
// startup Initialize, none of which are driven by or synchronized with a
// ConfigStore generation. That registry cannot be pinned to cfg without
// either freezing it to a stale tool schema/connection set (breaking
// reconnection and live tool-list updates) or snapshotting live network
// state, which is not what config generations represent. So len(cfg.MCP)
// (the presence gate for ListMCPResourcesTool/ReadMCPResourceTool, the only
// MCP-derived value that IS plain config data) is pinned like everything
// else here, but the MCP tool set itself, and the AllowedMCP filter applied
// to it below, read whatever the live registry holds at call time. A reload
// landing mid-build can therefore still pair this build's pinned
// prompt/options/allow-list with MCP tool implementations from a different
// generation than the rest of the agent -- narrower than the pre-#576
// torn read (every other tool and the prompt agree with each other and with
// cfg), but real. See tools.GetMCPTools's doc for the registry side of this.
//
// R3-1: when ctx carries CallOptions, the returned slice is scoped to that
// ONE call (the DisableSubAgents filter, the per-call role gate and the folder-scope filter above
// read the context). Callers must pin it to the call — resolvedOverrides.tools
// / SessionAgentCall.Tools — and must NOT publish it onto the shared agent
// with SetTools; only UpdateModels (which strips CallOptions from its ctx
// first) may publish a global toolset.
func (c *coordinator) buildTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	agent = c.applyCallDisableSubAgents(ctx, cfg, agent, isSubAgent)
	agent = c.buildToolsAgentConfigForCall(ctx, cfg, agent, isSubAgent)
	agent = c.applyCallFolderScope(ctx, agent)

	// The scope value the fs_* tools constructed below are built with:
	// THIS call's FolderScope, or the zero value for an unscoped call.
	// The zero value grants nothing, so the constructions themselves are
	// inert; which fs_* tools reach the returned slice is decided by
	// agent.AllowedTools membership -- applyCallFolderScope for a scoped
	// call, the config defaults for an unscoped one.
	var scope permission.FolderScope
	if callOpts := callOptionsFrom(ctx); callOpts != nil && callOpts.FolderScope != nil {
		scope = *callOpts.FolderScope
	}

	// SSRF guard escape hatch (Options.AllowPrivateNetworkFetch, off by
	// default): when enabled, every model-facing HTTP tool below gets an
	// explicit allowPrivate=true client instead of letting its own nil
	// fallback build the guarded default. See ssrf_guard.go.
	//
	// Options can be nil on bare test fixtures; every Options read below
	// tolerates that.
	var allowPrivateNetworkFetch bool
	var attribution *config.Attribution
	var skillsPaths []string
	var logFile string
	if cfg.Options != nil {
		allowPrivateNetworkFetch = cfg.Options.AllowPrivateNetworkFetch
		attribution = cfg.Options.Attribution
		skillsPaths = cfg.Options.SkillsPaths
		logFile = filepath.Join(cfg.Options.DataDirectory, "logs", "rush.log")
	}
	fetchClient := func(timeout time.Duration) *http.Client {
		if !allowPrivateNetworkFetch {
			return nil
		}
		return tools.NewSSRFGuardedClient(timeout, true)
	}

	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agentTool)
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, fetchClient(30*time.Second))
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	// Get the model name for the agent
	modelID := ""
	if modelCfg, ok := cfg.Models[agent.Model]; ok {
		if model := cfg.GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := cfg.Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	// Background-job completion notification (web/interactive only).
	// When a bash command auto-backgrounds and later finishes, push a
	// one-message completion notice into the owning session via the
	// existing InjectMessage path. Kill-switch defaults to ON; a session
	// that is BUSY merges it into the running turn, IDLE sessions get a
	// persisted message (no auto-resume). rush run is single-turn and
	// never receives it.
	opts := cfg.Options
	notifyDone := opts == nil || opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
	var onBgDone func(string, *shell.BackgroundShell)
	if notifyDone {
		onBgDone = func(sessionID string, sh *shell.BackgroundShell) {
			c.notifyBackgroundJobDone(sessionID, sh)
		}
	}

	allTools = append(
		allTools,
		tools.NewAskQuestionTool(),
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), attribution, modelID, onBgDone),
		tools.NewRushInfoTool(c.cfg, c.allSkills, c.activeSkills, c.skillTracker),
		tools.NewRushLogsTool(logFile),
		tools.NewJobOutputTool(),
		tools.NewJobKillTool(),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), fetchClient(5*time.Minute)),
		tools.NewEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), fetchClient(30*time.Second)),
		tools.NewGitReadTool(c.cfg.WorkingDir()),
		tools.NewGlobTool(c.cfg.WorkingDir()),
		tools.NewGrepTool(c.cfg.WorkingDir(), cfg.Tools.Grep),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), cfg.Tools.Ls),
		tools.NewReadDelegationTranscriptTool(c.sessions, c.messages),
		tools.NewRunCommandTool(c.permissions, c.cfg.WorkingDir()),
		tools.NewSourcegraphTool(fetchClient(30*time.Second)),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.permissions, c.filetracker, c.skillTracker, c.cfg.WorkingDir(), skillsPaths...),
		tools.NewWriteTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),

		// Scoped, batch-capable fs_* family, constructed with THIS
		// call's scope (see the extraction above; the zero value denies
		// everything). NewFSGrepTool takes workingDir first and scope
		// second; the other seven take scope first. Every constructor's
		// LAST parameter is a DiskProvider (nil here means the real
		// disk) precisely because of that reversed-order outlier: a
		// trailing parameter needs no per-constructor special case.
		// Wiring a caller-supplied provider through CallOptions is a
		// separate, later task; this nil is the forced, zero-behaviour-
		// change consequence of these constructors gaining the
		// parameter at all.
		tools.NewFSListTool(scope, c.cfg.WorkingDir(), cfg.Tools.Ls, nil),
		tools.NewFSFindTool(scope, c.cfg.WorkingDir(), nil),
		tools.NewFSReadTool(scope, c.filetracker, c.cfg.WorkingDir(), nil),
		tools.NewFSGrepTool(c.cfg.WorkingDir(), scope, nil),
		tools.NewFSWriteTool(scope, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir(), nil),
		tools.NewFSReplaceTool(scope, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir(), nil),
		tools.NewFSWriteLinesTool(scope, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir(), nil),
		tools.NewFSDeleteTool(scope, c.permissions, c.cfg.WorkingDir(), nil),
	)

	// cfg.MCP presence is pinned config data (any MCP server configured at
	// all, enabled or not), unlike the tool set itself below -- see this
	// function's doc comment.
	if len(cfg.MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.permissions),
			tools.NewReadMCPResourceTool(c.cfg, c.permissions),
		)
	}

	var filteredTools []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(agent.AllowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	// GetMCPTools reads the live mcp.Tools() registry, not cfg -- see this
	// function's doc comment for why that is deliberate. agent.AllowedMCP
	// below IS from the pinned cfg/agent, so the allow-list decision itself
	// is consistent with the rest of this build; only the candidate tool set
	// it is applied to can be from a different generation.
	for _, tool := range tools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir()) {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filteredTools = append(filteredTools, tool)
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, tools := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(tools) == 0 || slices.Contains(tools, tool.MCPToolName()) {
				filteredTools = append(filteredTools, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// Wrap tools with hook interception for the top-level agent only.
	// Sub-agents (the `agent` task tool, `agentic_fetch`, etc.) run
	// without hook interception to avoid firing the user's hook N times
	// per delegated turn. The top-level invocation of the sub-agent tool
	// itself is still wrapped from the coder's side.
	filteredTools = wrapToolsWithHooks(filteredTools, hookRunner, isSubAgent)

	// Error logging is NOT applied here. It used to be, and that was the
	// bug: this function is only one of the places a tool slice is built,
	// so agentic_fetch's own tools were never covered. NewSessionAgent and
	// SetTools wrap instead — every slice reaches the agent through one of
	// them, and the wrapper still ends up outermost, since what they wrap
	// is the already-hooked slice returned here. See logged_tool.go.
	return filteredTools, nil
}
