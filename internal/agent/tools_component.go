package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/prompt/skills"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/safety/notify"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/agent/tools/hooks"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/filetracker"
	"github.com/PHPCraftdream/rush/internal/store/history"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

type toolsHost interface {
	coord() *coordinator
	cfg() *config.ConfigStore
	sessions() coresession.Service
	messages() message.Service
	permissions() permission.Service
	history() history.Service
	filetracker() filetracker.Service
	notify() pubsub.Publisher[notify.Notification]
	diskCaps() *DiskProviderRegistry
	background() *shell.BackgroundShellManager
	allSkills() []*skills.Skill
	activeSkills() []*skills.Skill
	skillTracker() *skills.Tracker
	mcpOwner() *mcp.Owner
	mcpOutputAccess() *tools.MCPOutputAccess
	onSessionIdleHook(sessionID string)
	readyWg() *readyGate
	agentTool(ctx context.Context) (fantasy.AgentTool, error)
	agenticFetchTool(ctx context.Context, client *http.Client) (fantasy.AgentTool, error)
	notifyBackgroundJobDone(sessionID string, sh *shell.BackgroundShell)
	buildAgentModelsFromCfg(ctx context.Context, cfg *config.Config, isSubAgent bool) (Model, Model, error)
	workerSubAgentActiveForCall(ctx context.Context, cfg *config.Config) bool
	workerSubAgentActive(cfg *config.Config) bool
	rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error
	sharedBuildScopeMu() *sync.Mutex
	sharedBuildScope() *buildResourceScope
	setSharedBuildScope(*buildResourceScope)
}

type (
	toolsComponent       struct{ host toolsHost }
	coordinatorToolsHost struct{ coordinator *coordinator }
)

func newToolsComponent(c *coordinator) *toolsComponent {
	return &toolsComponent{host: coordinatorToolsHost{coordinator: c}}
}

func (h coordinatorToolsHost) coord() *coordinator              { return h.coordinator }
func (h coordinatorToolsHost) cfg() *config.ConfigStore         { return h.coordinator.cfg }
func (h coordinatorToolsHost) sessions() coresession.Service    { return h.coordinator.sessions }
func (h coordinatorToolsHost) messages() message.Service        { return h.coordinator.messages }
func (h coordinatorToolsHost) permissions() permission.Service  { return h.coordinator.permissions }
func (h coordinatorToolsHost) history() history.Service         { return h.coordinator.history }
func (h coordinatorToolsHost) filetracker() filetracker.Service { return h.coordinator.filetracker }
func (h coordinatorToolsHost) notify() pubsub.Publisher[notify.Notification] {
	return h.coordinator.notify
}
func (h coordinatorToolsHost) diskCaps() *DiskProviderRegistry { return h.coordinator.diskCaps }
func (h coordinatorToolsHost) background() *shell.BackgroundShellManager {
	return h.coordinator.background
}
func (h coordinatorToolsHost) allSkills() []*skills.Skill    { return h.coordinator.allSkills }
func (h coordinatorToolsHost) activeSkills() []*skills.Skill { return h.coordinator.activeSkills }
func (h coordinatorToolsHost) skillTracker() *skills.Tracker { return h.coordinator.skillTracker }
func (h coordinatorToolsHost) mcpOwner() *mcp.Owner          { return h.coordinator.mcpOwner }
func (h coordinatorToolsHost) mcpOutputAccess() *tools.MCPOutputAccess {
	return h.coordinator.mcpOutputAccess()
}

func (h coordinatorToolsHost) onSessionIdleHook(sessionID string) {
	h.coordinator.onSessionIdleHook(sessionID)
}
func (h coordinatorToolsHost) readyWg() *readyGate { return &h.coordinator.readyWg }
func (h coordinatorToolsHost) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	return h.coordinator.agentTool(ctx)
}

func (h coordinatorToolsHost) agenticFetchTool(ctx context.Context, client *http.Client) (fantasy.AgentTool, error) {
	return h.coordinator.agenticFetchTool(ctx, client)
}

func (h coordinatorToolsHost) notifyBackgroundJobDone(sessionID string, sh *shell.BackgroundShell) {
	h.coordinator.notifyBackgroundJobDone(sessionID, sh)
}

func (h coordinatorToolsHost) workerSubAgentActiveForCall(ctx context.Context, cfg *config.Config) bool {
	return h.coordinator.workerSubAgentActiveForCall(ctx, cfg)
}

func (h coordinatorToolsHost) workerSubAgentActive(cfg *config.Config) bool {
	return h.coordinator.workerSubAgentActive(cfg)
}

func (h coordinatorToolsHost) buildAgentModelsFromCfg(ctx context.Context, cfg *config.Config, isSubAgent bool) (Model, Model, error) {
	return h.coordinator.buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
}

func (h coordinatorToolsHost) rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error {
	return h.coordinator.rejectScopedCallOnCLIProvider(ctx, role, providerCfg)
}

func (h coordinatorToolsHost) sharedBuildScopeMu() *sync.Mutex {
	return &h.coordinator.sharedBuildScopeMu
}

func (h coordinatorToolsHost) sharedBuildScope() *buildResourceScope {
	return h.coordinator.sharedBuildScope
}

func (h coordinatorToolsHost) setSharedBuildScope(s *buildResourceScope) {
	h.coordinator.sharedBuildScope = s
}

func (p *toolsComponent) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	// ONE snapshot for the entire build: models, prompt, and tools. Before
	// this (task #576/P1-3), models came from buildAgentModels' own
	// Snapshot() while the provider options/system-prompt-prefix/Options
	// fields below and buildTools' many reads each took a fresh, separately
	// timed p.host.cfg().Config(). A reload landing anywhere in between could hand
	// back an agent whose model, prompt (WorkerAvailable) and toolset
	// (worker tool layering, hooks, grep/ls options, attribution, skills
	// paths) disagreed with each other -- an internally inconsistent agent
	// built across two config generations. Threading this one cfg through
	// closes that gap for all of the above; only explicitly-declared live
	// *policy* reads remain, plus MCP tools,
	// which are deliberately NOT pinned -- see the MCP block in buildTools
	// for why.
	cfg, _ := p.host.cfg().Snapshot()

	smart, fast, err := p.host.buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
	if err != nil {
		return nil, err
	}

	var smartProviderCfg config.ProviderConfig
	if creds := callCredentialsFrom(ctx); creds != nil {
		if cred, ok := creds.Credential(smart.ModelCfg.Provider); ok {
			smartProviderCfg = credentialProviderConfig(cred)
		}
	} else {
		smartProviderCfg, _ = cfg.Providers.Get(smart.ModelCfg.Provider)
	}
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
	// needs the optional permission extension. Nil-safe like p.host.permissions()
	// everywhere else in this function (bare test-fixture coordinators).
	var runAllowlists permission.SessionRunAllowlistManager
	if p.host.permissions() != nil {
		if mgr, ok := p.host.permissions().(permission.SessionRunAllowlistManager); ok {
			runAllowlists = mgr
		}
	}
	result := NewSessionAgent(SessionAgentOptions{
		// R12-H2b §2.4: the agent's durable enqueue producer holds
		// row-keyed Continuation owners through the coordinator's
		// capability manager (nil on bare fixtures, same as diskCaps).
		diskCaps:             p.host.diskCaps(),
		SmartModel:           smart,
		FastModel:            fast,
		SystemPromptPrefix:   smartProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: disableAutoSummarize,
		// permissions can be nil on bare test-fixture coordinators; treat
		// that as fail-closed (not YOLO) rather than panicking.
		IsYolo:            p.host.permissions() != nil && p.host.permissions().SkipRequests(),
		Sessions:          p.host.sessions(),
		Messages:          p.host.messages(),
		Tools:             nil,
		Config:            p.host.cfg(),
		Notify:            p.host.notify(),
		StreamIdleTimeout: streamIdleTimeout,
		ToolMaxDuration:   toolMaxDuration,
		DataDirectory:     dataDirectory,
		RunAllowlists:     runAllowlists,
		Coord:             p.host.coord(),
		// Phase 3's by-construction release trigger (sessionAgent.
		// onSessionIdle's doc, docs/plans/2026-09-28-async-phase3-spec.md
		// orchestrator decision item 1) PLUS supervision's turn-end deadline
		// push (supervision.go's onSessionIdleHook) -- wired identically for
		// the root agent and every delegated child driver, since buildAgent
		// builds both; both callees are no-ops for a session with nothing to
		// react to.
		OnSessionIdle:      p.host.onSessionIdleHook,
		RestrictedRuns:     restrictedRunAuthorizer(p.host.permissions()),
		CheckpointInterval: checkpointInterval, // Fork patch: batch 8
	})

	// R1-1: resolve the per-call worker-active predicate SYNCHRONOUSLY,
	// before the async build goroutines below are registered — the value
	// they see is fixed at UpdateModels/buildAgent call time, never a lazy
	// read of shared state from inside an already-running goroutine (the
	// hooks/runner.go sync-capture pattern). With a CallOptions-carrying
	// context the value is this call's own ModelRole; without one it is
	// the legacy shared field, read here under its mutex rather than
	// later on a goroutine.
	workerActive := p.host.workerSubAgentActiveForCall(ctx, cfg)

	gate := readyGateFor(ctx, p.host.readyWg())
	gate.Go(func() error {
		// Orchestrator block only ever applies to the top-level coder prompt
		// (isSubAgent false) — a sub-agent renders task.md.tpl via
		// taskPrompt, which doesn't reference WorkerAvailable, but guarding
		// on !isSubAgent here keeps this call's intent explicit rather than
		// relying on the template to ignore the field.
		// Same pinned cfg as the models above and buildTools below — see
		// this function's opening comment.
		systemPrompt, err := prompt.Build(ctx, smart.Model.Provider(), smart.Model.Model(), p.host.cfg(), cfg, !isSubAgent && workerActive)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	})

	gate.Go(func() error {
		tools, err := p.buildTools(ctx, cfg, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	})

	return result, nil
}

func (p *toolsComponent) buildToolsAgentConfigForCall(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	if remaining, err := MaxAgentDepthFrom(ctx); err != nil || remaining == 0 {
		stripped := make([]string, 0, len(agent.AllowedTools))
		for _, name := range agent.AllowedTools {
			if name != AgentToolName && name != tools.AgenticFetchToolName {
				stripped = append(stripped, name)
			}
		}
		agent.AllowedTools = stripped
	}
	if !p.host.workerSubAgentActiveForCall(ctx, cfg) {
		return agent
	}
	if isSubAgent {
		allowed := make([]string, len(agent.AllowedTools), len(agent.AllowedTools)+len(workerToolNames))
		copy(allowed, agent.AllowedTools)
		for _, name := range workerLayeredToolNames() {
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

func (p *toolsComponent) buildToolsAgentConfig(cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	return p.buildToolsAgentConfigForCall(context.Background(), cfg, agent, isSubAgent)
}

func (p *toolsComponent) applyCallDisableSubAgents(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	opts := callOptionsFrom(ctx)
	if remaining, err := MaxAgentDepthFrom(ctx); err != nil || remaining == 0 {
		stripped := make([]string, 0, len(agent.AllowedTools))
		for _, name := range agent.AllowedTools {
			if name != AgentToolName && name != tools.AgenticFetchToolName {
				stripped = append(stripped, name)
			}
		}
		agent.AllowedTools = stripped
		return agent
	}
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

func (p *toolsComponent) applyCallFolderScope(ctx context.Context, agent config.Agent) config.Agent {
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.FolderScope == nil {
		// No scope: fs_* would be built with the zero scope and deny every
		// path, so the model burns turns on refusals. Hide them.
		return withoutFolderScopeTools(agent)
	}
	scope := *opts.FolderScope

	commandTools := folderScopeCommandToolSet()
	stripped := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if _, ok := folderScopeLegacyFileTools[name]; ok {
			continue
		}
		if _, ok := folderScopeEscapeHatchTools[name]; ok {
			continue
		}
		if _, ok := commandTools[name]; ok && !scope.KeepsCommandTools() {
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

func (p *toolsComponent) applyNoRealWorkspaceToolFloor(cfg *config.Config, agent config.Agent, disk tools.DiskProvider) config.Agent {
	if cfg == nil || cfg.Options == nil || !cfg.Options.NoRealWorkspace {
		return agent
	}
	customDisk := tools.IsCustomDiskProvider(disk)
	stripped := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if _, ok := noRealWorkspaceForbiddenTools[name]; ok {
			continue
		}
		if !customDisk {
			if _, ok := noRealWorkspaceScopedFSTools[name]; ok {
				continue
			}
		}
		stripped = append(stripped, name)
	}
	agent.AllowedTools = stripped
	return agent
}

func (p *toolsComponent) rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error {
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

func (p *toolsComponent) buildTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	if opts := callOptionsFrom(ctx); opts != nil && callBuildScopeFrom(ctx) == nil {
		EnsureCallBuildScope(opts)
		defer opts.BuildScope.Close()
	}
	built, err := p.buildPolicyTools(ctx, cfg, agent, isSubAgent)
	return built.tools, err
}

func (p *toolsComponent) buildPolicyTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) (policyToolBuild, error) {
	if opts := callOptionsFrom(ctx); opts != nil {
		if opts.RoleContract != nil {
			ctx = WithParentRoleConfigSnapshot(ctx, cfg)
		}
	}
	agent = p.applyCallDisableSubAgents(ctx, cfg, agent, isSubAgent)
	agent = p.buildToolsAgentConfigForCall(ctx, cfg, agent, isSubAgent)
	agent = p.applyCallFolderScope(ctx, agent)
	agent = applyCallReviewerReadOnly(ctx, agent, isSubAgent)

	// The scope and disk provider the fs_* tools constructed below are
	// built with: THIS call's FolderScope/DiskProvider, or their zero
	// values for a call with no CallOptions (or a CallOptions that sets
	// neither). The zero scope grants nothing, so the constructions
	// themselves are inert; which fs_* tools reach the returned slice is
	// decided by agent.AllowedTools membership -- applyCallFolderScope
	// for a scoped call, the config defaults for an unscoped one. The
	// zero (nil) disk provider means each constructor normalises to the
	// real filesystem (diskOrOS) -- see CallOptions.DiskProvider's doc
	// comment for why it is deliberately not part of that same
	// zero-scope-denies-everything story.
	var scope permission.FolderScope
	var disk tools.DiskProvider
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		if callOpts.FolderScope != nil {
			scope = *callOpts.FolderScope
		}
		disk = callOpts.DiskProvider
	}

	// R14-1 (P0, SDK review round 14): final capability floor. Must run
	// LAST -- after worker layering and the folder-scope re-adds above
	// -- so the disabled-tools list is a floor no layering can punch
	// through (see applyNoRealWorkspaceToolFloor's doc).
	agent = p.applyNoRealWorkspaceToolFloor(cfg, agent, disk)
	agent = applyBatchQuestionRouteFloor(ctx, agent)

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
		if cfg.Options.DataDirectory != "" {
			logFile = filepath.Join(cfg.Options.DataDirectory, "logs", "rush.log")
		}
	}
	fetchClient := func(timeout time.Duration) *http.Client {
		if !allowPrivateNetworkFetch {
			return nil
		}
		return tools.NewSSRFGuardedClient(timeout, true)
	}

	outputAccess := p.host.mcpOutputAccess()
	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		if isSubAgent {
			// Recursion guard (task #1050): this build is for the task
			// agent itself -- the only isSubAgent=true build in production
			// (see agent_tool.go's c.buildAgent call). Handing it the
			// "agent" tool would let c.agentTool build a fresh copy of the
			// same task agent, whose buildTools would see AgentToolName
			// again and recurse forever, each level registering two more
			// goroutines on the shared c.readyWg that every run entry point
			// blocks on via readyWg.Wait(). Deny unconditionally, no matter
			// what AllowedTools says.
			nestedAgentToolWarnOnce.Do(func() {
				slog.Warn("task agent AllowedTools includes the agent tool; denying it to prevent nested delegation recursion", "agent", agent.Name)
			})
		} else {
			agentTool, err := p.host.agentTool(ctx)
			if err != nil {
				return policyToolBuild{}, err
			}
			allTools = append(allTools, agentTool)
		}
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := p.host.agenticFetchTool(ctx, fetchClient(30*time.Second))
		if err != nil {
			return policyToolBuild{}, err
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
		hookRunner = hooks.NewRunner(preToolHooks, p.host.cfg().WorkingDir(), p.host.cfg().WorkingDir())
	}

	// Background-job completion notification. When a bash command
	// auto-backgrounds and later finishes, notifyBackgroundJobDone persists a
	// bg_shell_done session_notices row (wake=1); the owning session's next
	// turn pulls it into history (a running turn at a step boundary). A turn
	// is started for it by the web process (AutoResumeOnJobDone, at most
	// maxConsecutiveAutoResumes per human message) or by a `rush run` loop's
	// own reaction turn (the same policy: with AutoResumeOnJobDone off a
	// bg-shell-only notice waits for the next natural turn; the loop has no
	// cap). Kill-switch defaults to ON. A `rush run` process registers this
	// callback too, so the row can also appear while its loop waits.
	opts := cfg.Options
	notifyDone := opts == nil || opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
	backgroundManager := p.host.background()
	if backgroundManager == nil {
		backgroundManager = shell.NewBackgroundShellManager()
	}
	var onBgDone func(string, *shell.BackgroundShell)
	if notifyDone {
		onBgDone = func(sessionID string, sh *shell.BackgroundShell) {
			p.host.notifyBackgroundJobDone(sessionID, sh)
		}
	}

	allTools = append(
		allTools,
		tools.NewAskQuestionTool(),
		tools.NewBashTool(p.host.permissions(), p.host.cfg().WorkingDir(), attribution, modelID, onBgDone, backgroundManager),
		tools.NewRushInfoTool(p.host.cfg(), p.host.allSkills(), p.host.activeSkills(), p.host.skillTracker()),
		tools.NewInspectAgentTool(p.host.coord()),
		tools.NewInjectAgentTool(p.host.coord()),
		tools.NewStopAgentTool(p.host.coord()),
		tools.NewWakeinTool(p.host.coord()),
		tools.NewAwaitTasksTool(p.host.coord()),
		tools.NewWakeonTool(p.host.coord()),
		tools.NewWakeLoopTool(p.host.coord()),
		tools.NewWakeListTool(p.host.coord()),
		tools.NewWakeCancelTool(p.host.coord()),
		tools.NewDownloadTool(p.host.permissions(), p.host.cfg().WorkingDir(), fetchClient(5*time.Minute)),
		tools.NewEditTool(p.host.permissions(), p.host.history(), p.host.filetracker(), p.host.cfg().WorkingDir()),
		tools.NewMultiEditTool(p.host.permissions(), p.host.history(), p.host.filetracker(), p.host.cfg().WorkingDir()),
		tools.NewFetchTool(p.host.permissions(), p.host.cfg().WorkingDir(), fetchClient(30*time.Second)),
		tools.NewGitReadTool(p.host.cfg().WorkingDir()),
		tools.NewGlobTool(p.host.cfg().WorkingDir(), p.host.permissions()),
		tools.NewGrepTool(p.host.cfg().WorkingDir(), cfg.Tools.Grep, p.host.permissions()),
		tools.NewLsTool(p.host.permissions(), p.host.cfg().WorkingDir(), cfg.Tools.Ls),
		tools.NewReadDelegationTranscriptTool(p.host.sessions(), p.host.messages()),
		tools.NewRunCommandTool(p.host.permissions(), p.host.cfg().WorkingDir()),
		tools.NewSourcegraphTool(fetchClient(30*time.Second)),
		tools.NewTodosTool(p.host.sessions()),
		tools.NewViewTool(p.host.permissions(), p.host.filetracker(), p.host.skillTracker(), p.host.cfg().WorkingDir(), skillsPaths...),
		tools.NewWriteTool(p.host.permissions(), p.host.history(), p.host.filetracker(), p.host.cfg().WorkingDir()),

		// Scoped, batch-capable fs_* family, constructed with THIS
		// call's scope (see the extraction above; the zero value denies
		// everything) and THIS call's disk provider (nil normalises to
		// the real disk inside each constructor). NewFSGrepTool takes
		// workingDir first and scope second; the other seven take scope
		// first. Every constructor's LAST parameter is the DiskProvider
		// precisely because of that reversed-order outlier: a trailing
		// parameter needs no per-constructor special case.
		tools.NewFSListTool(scope, p.host.cfg().WorkingDir(), cfg.Tools.Ls, disk),
		tools.NewFSFindTool(scope, p.host.cfg().WorkingDir(), disk),
		tools.NewFSReadTool(scope, p.host.filetracker(), p.host.cfg().WorkingDir(), disk),
		tools.NewFSGrepTool(p.host.cfg().WorkingDir(), scope, cfg.Tools.Grep, disk),
		tools.NewFSWriteTool(scope, p.host.permissions(), p.host.history(), p.host.filetracker(), p.host.cfg().WorkingDir(), disk),
		tools.NewFSReplaceTool(scope, p.host.permissions(), p.host.history(), p.host.filetracker(), p.host.cfg().WorkingDir(), disk),
		tools.NewFSWriteLinesTool(scope, p.host.permissions(), p.host.history(), p.host.filetracker(), p.host.cfg().WorkingDir(), disk),
		tools.NewFSDeleteTool(scope, p.host.permissions(), p.host.cfg().WorkingDir(), disk),
	)

	// R15-1 (P0, SDK review round 15): rush_logs reads a real host
	// file -- Options.DataDirectory's logs/rush.log. With no data
	// directory (an ephemeral SDK session) there is no log file to
	// read, and falling back to the RELATIVE logs/rush.log would
	// resolve against the host process's own working directory, so
	// the tool is simply not constructed or offered at all.
	if logFile != "" {
		allTools = append(allTools, tools.NewRushLogsTool(logFile))
	}

	// cfg.MCP presence is pinned config data (any MCP server configured at
	// all, enabled or not), unlike the tool set itself below -- see this
	// function's doc comment.
	if len(cfg.MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(p.host.cfg(), p.host.permissions(), p.host.mcpOwner()),
			tools.NewReadMCPResourceTool(p.host.cfg(), p.host.permissions(), p.host.mcpOwner(), outputAccess),
		)
	}

	filteredTools := make([]fantasy.AgentTool, 0, len(allTools))
	for _, tool := range allTools {
		if slices.Contains(agent.AllowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	allowedControls := batchControlAvailability(filteredTools, agent.AllowedTools)
	allowedControls = filterRoleToolNames(ctx, allowedControls)
	{
		if bind := coordinatorBatchBindingFor(p.host.coord()); bind != nil && bind.controls != nil {
			for _, name := range allowedControls {
				if name == tools.AwaitTasksToolName {
					for i, tool := range filteredTools {
						if tool.Info().Name == name {
							filteredTools[i] = tools.NewBatchAwaitTasksTool(p.host.coord())
						}
					}
					continue
				}
				filteredTools = append(filteredTools, newBatchControlPlaceholder(name))
			}
		}
	}

	// GetMCPTools reads the live MCP tool registry; apply the configured MCP allowlist below.
	for _, tool := range tools.GetMCPTools(p.host.permissions(), p.host.cfg(), p.host.cfg().WorkingDir(), p.host.mcpOwner(), outputAccess) {
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
	filteredTools = filterRoleContractTools(ctx, filteredTools)
	admitMCPOutputSources(outputAccess, filteredTools)
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})
	return p.finishToolAssembly(ctx, filteredTools, hookRunner, isSubAgent, allowedControls, cfg)
}

func (p *toolsComponent) sharedBuildScopeFor() *buildResourceScope {
	p.host.sharedBuildScopeMu().Lock()
	defer p.host.sharedBuildScopeMu().Unlock()
	if p.host.sharedBuildScope() == nil {
		p.host.setSharedBuildScope(newBuildResourceScope())
	}
	return p.host.sharedBuildScope()
}

func (p *toolsComponent) finishToolAssembly(ctx context.Context, filteredTools []fantasy.AgentTool, hookRunner *hooks.Runner, isSubAgent bool, allowed []string, configs ...*config.Config) (policyToolBuild, error) {
	var cfg *config.Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	if bind := coordinatorBatchBindingFor(p.host.coord()); bind != nil {
		allowedControls := allowed
		if roleToolsConstrained(ctx) {
			allowedControls = filterRoleToolNames(ctx, allowed)
		}
		filteredTools = withoutBatchControlPlaceholders(filteredTools)
		filteredTools = decoratePolicyTools(filteredTools, restrictedRunAuthorizer(p.host.permissions()), hookRunner, isSubAgent)
		filteredTools = filterRoleContractTools(ctx, filteredTools)
		buildCtx := ctx
		if current := roleContractFromContext(ctx); current != nil {
			buildCtx = withRoleToolContract(buildCtx, current)
		}
		inheritedContracts := roleContractsForTools(filteredTools)
		if current := roleContractFromContext(buildCtx); current != nil && !hasRoleContract(inheritedContracts, current) {
			inheritedContracts = append(inheritedContracts, current.Clone())
		}
		roleContract := roleContractForToolBuild(buildCtx, filteredTools)
		if roleContract != nil {
			buildCtx = withRoleToolContract(buildCtx, roleContract)
		}
		scope := callBuildScopeFrom(ctx)
		if scope == nil {
			if opts := callOptionsFrom(ctx); opts != nil && tools.IsCustomDiskProvider(opts.DiskProvider) {
				return policyToolBuild{}, ErrDiskProviderUnavailable
			}
		}
		shared := scope == nil
		if shared {
			scope = newBuildResourceScope()
			defer scope.Close()
		}
		failBuildScope := func(err error) (policyToolBuild, error) {
			if !shared {
				scope.Close()
			}
			return policyToolBuild{}, err
		}
		var built policyToolBuild
		if shared {
			built = newPolicyToolBuildForContext(buildCtx, filteredTools)
			fingerprint := batchToolsetFingerprint(built.tools)
			for i := 0; i < len(built.tools); i++ {
				tool := built.tools[i]
				name := tool.Info().Name
				if !roleContractAllowsName(roleContract, name) {
					built.tools = append(built.tools[:i], built.tools[i+1:]...)
					i--
					continue
				}
				if name == tools.AskQuestionToolName {
					built.tools[i] = batchAgentAskQuestionTool{AgentTool: tool}
					continue
				}
				if !batchWorkTarget(name) {
					continue
				}
				inv := BatchInvocation{Origin: "coordinator", Delivery: batchFamilyDelivery(name), BackgroundEscape: batchEscapeNever, WaiterRank: 0, Policy: batchLeafPolicyContext{RestrictedToolset: fingerprint, RoleContract: cloneRoleContractForCall(roleContract)}}
				if name == tools.BashToolName {
					if inlineWindowDisabledFor(bind) {
						inv.Delivery = batchDeliveryChoice{Mode: batchDeliveryDetached}
					} else {
						inv.BackgroundEscape = batchEscapeAuto
						inv.Delivery.Window = inlineWindow.String()
					}
				}
				built.tools[i] = &batchIngressTool{AgentTool: tool, registry: built.leaves, ingress: bind.ingress, inv: inv, bind: bind}
			}
		} else {
			var err error
			built, err = newBatchWorkToolBuildForContext(buildCtx, bind, filteredTools, scope, false)
			if err != nil {
				return failBuildScope(err)
			}
		}
		if bind.controls != nil && !shared {
			effectiveControls := make([]string, 0, len(allowedControls))
			for _, name := range allowedControls {
				if roleContractAllowsName(roleContract, name) && roleContractsAllowName(inheritedContracts, name) {
					effectiveControls = append(effectiveControls, name)
				}
			}
			requiredControls := make([]string, 0, len(effectiveControls))
			for _, name := range []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"} {
				if slices.Contains(effectiveControls, name) {
					requiredControls = append(requiredControls, name)
				}
			}
			if len(effectiveControls) > 0 {
				registry, err := bind.ingress.Registry(built.registryID)
				if err != nil {
					return failBuildScope(err)
				}
				inv := BatchInvocation{
					RegistryID:       built.registryID,
					Origin:           "coordinator",
					Delivery:         batchDeliveryChoice{Mode: batchDeliveryDetached},
					BackgroundEscape: batchEscapeNever,
					WorkerStopScope:  isSubAgent,
					roleConfig:       cfg,
					Policy:           batchLeafPolicyContext{RegistryID: built.registryID, RestrictedToolset: batchToolsetFingerprint(built.tools), RoleContract: cloneRoleContractForCall(roleContract)},
				}
				inv.SessionID, inv.MessageID = tools.GetSessionFromContext(ctx), tools.GetMessageFromContext(ctx)
				inv.AgentDepth, inv.AgentDepthSet = DefaultAgentDepth, true
				if opts := callOptionsFrom(ctx); opts != nil && opts.AgentDepthSet {
					inv.AgentDepth = opts.MaxAgentDepth
				}
				inv.Policy.MessageID, inv.Policy.SupportsImages, inv.Policy.ModelName = inv.MessageID, tools.GetSupportsImagesFromContext(ctx), tools.GetModelNameFromContext(ctx)
				inv.WaiterRank = batchGroupRanks[batchGroupDefault]
				inv.Policy.FolderScope, inv.Policy.RunAllowlist = folderScopeSpecFrom(ctx), runAllowlistSpecFrom(ctx)
				if opts := callOptionsFrom(ctx); opts != nil && opts.DiskBindingID != "" {
					inv.Policy.DiskBindingID, inv.Policy.DiskProvider = opts.DiskBindingID, true
				}
				controls, err := bind.controls.InstallBatchModelControls(registry, inv)
				if err != nil {
					return failBuildScope(err)
				}
				for _, control := range controls {
					name := control.Info().Name
					if name == tools.AwaitTasksToolName || !slices.Contains(requiredControls, name) {
						continue
					}
					controlContracts := cloneRoleToolContracts(inheritedContracts)
					if roleContract != nil && !hasRoleContract(controlContracts, roleContract) {
						controlContracts = append(controlContracts, roleContract.Clone())
					}
					guard := roleGuardTool{AgentTool: control, contracts: controlContracts}
					built.tools = append(built.tools, wrapToolsWithHooks(wrapToolsWithRestrictedRun([]fantasy.AgentTool{guard}, restrictedRunAuthorizer(p.host.permissions())), hookRunner, isSubAgent)[0])
				}
			}
			built.tools = filterRoleContractTools(buildCtx, built.tools)
			for _, name := range requiredControls {
				present := false
				for _, tool := range built.tools {
					if tool.Info().Name == name {
						present = true
						break
					}
				}
				if !present {
					return failBuildScope(fmt.Errorf("batch control %q was not installed", name))
				}
			}
		}
		// Controls are installed after leaf admission; canonicalize the final
		// model-facing set, not just the earlier placeholder/leaf inventory.
		slices.SortStableFunc(built.tools, func(a, b fantasy.AgentTool) int {
			return strings.Compare(a.Info().Name, b.Info().Name)
		})
		return built, nil
	}
	// Per-call pinned tool slices bypass sessionAgent.SetTools, so apply the
	// restricted-run gate at this assembly boundary as well as at the agent
	// boundary. This keeps every actual dispatch path fail-closed. Hooks are
	// applied outside this wrapper below so PreToolUse runs first.
	filteredTools = decoratePolicyTools(filteredTools, restrictedRunAuthorizer(p.host.permissions()), hookRunner, isSubAgent)
	filteredTools = filterRoleContractTools(ctx, filteredTools)
	// The outer role guard must run before hooks and every other policy layer.
	return newPolicyToolBuild(filteredTools), nil
}
