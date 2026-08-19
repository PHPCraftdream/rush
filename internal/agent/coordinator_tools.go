// SessionAgent and tool-slice construction, including worker sub-agent
// toolset layering. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/shell"
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
	result := NewSessionAgent(SessionAgentOptions{
		SmartModel:           smart,
		FastModel:            fast,
		SystemPromptPrefix:   smartProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: opts.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                nil,
		Notify:               c.notify,
		StreamIdleTimeout:    streamIdleTimeout,
		ToolMaxDuration:      toolMaxDuration,
		DataDirectory:        opts.DataDirectory,
		CheckpointInterval:   checkpointInterval, // Fork patch: batch 8
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

	c.readyWg.Go(func() error {
		// Orchestrator block only ever applies to the top-level coder prompt
		// (isSubAgent false) — a sub-agent renders task.md.tpl via
		// taskPrompt, which doesn't reference WorkerAvailable, but guarding
		// on !isSubAgent here keeps this call's intent explicit rather than
		// relying on the template to ignore the field.
		// Same pinned cfg as the models above and buildTools below — see
		// this function's opening comment.
		systemPrompt, err := prompt.Build(ctx, smart.Model.Provider(), smart.Model.Model(), c.cfg, cfg, !isSubAgent && c.workerSubAgentActive(cfg))
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

// buildToolsAgentConfig returns the config.Agent buildTools should use to
// resolve AllowedTools for this build. For a sub-agent acting as a worker
// (see workerSubAgentActive), it returns a copy of agent with the worker
// toolset layered on top of whatever was already allowed (today's read-only
// set, in practice, since this only affects the AgentTask sub-agent). In
// every other case — including the top-level coder, and the sub-agent when
// no Worker is configured or the active role isn't smart — it returns agent
// unchanged, so behavior is byte-identical to before this method existed.
// cfg is the caller's pinned snapshot (task #576/P1-3) — buildTools no
// longer takes its own live c.cfg.Config() reads, so this decision now
// agrees with every other config-derived choice buildAgent makes for the
// same build (models, prompt's WorkerAvailable, hooks, MCP, etc.).
func (c *coordinator) buildToolsAgentConfig(cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	if !isSubAgent || !c.workerSubAgentActive(cfg) {
		return agent
	}

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
func (c *coordinator) buildTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	agent = c.buildToolsAgentConfig(cfg, agent, isSubAgent)

	// SSRF guard escape hatch (Options.AllowPrivateNetworkFetch, off by
	// default): when enabled, every model-facing HTTP tool below gets an
	// explicit allowPrivate=true client instead of letting its own nil
	// fallback build the guarded default. See ssrf_guard.go.
	allowPrivateNetworkFetch := cfg.Options.AllowPrivateNetworkFetch
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

	logFile := filepath.Join(cfg.Options.DataDirectory, "logs", "crush.log")

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
	// persisted message (no auto-resume). crush run is single-turn and
	// never receives it.
	opts := cfg.Options
	notifyDone := opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
	var onBgDone func(string, *shell.BackgroundShell)
	if notifyDone {
		onBgDone = func(sessionID string, sh *shell.BackgroundShell) {
			c.notifyBackgroundJobDone(sessionID, sh)
		}
	}

	allTools = append(
		allTools,
		tools.NewAskQuestionTool(),
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), cfg.Options.Attribution, modelID, onBgDone),
		tools.NewCrushInfoTool(c.cfg, c.allSkills, c.activeSkills, c.skillTracker),
		tools.NewCrushLogsTool(logFile),
		tools.NewJobOutputTool(),
		tools.NewJobKillTool(),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), fetchClient(5*time.Minute)),
		tools.NewEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), fetchClient(30*time.Second)),
		tools.NewGlobTool(c.cfg.WorkingDir()),
		tools.NewGrepTool(c.cfg.WorkingDir(), cfg.Tools.Grep),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), cfg.Tools.Ls),
		tools.NewReadDelegationTranscriptTool(c.sessions, c.messages),
		tools.NewSourcegraphTool(fetchClient(30*time.Second)),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.permissions, c.filetracker, c.skillTracker, c.cfg.WorkingDir(), cfg.Options.SkillsPaths...),
		tools.NewWriteTool(c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
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
