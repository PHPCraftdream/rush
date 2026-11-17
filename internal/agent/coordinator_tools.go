// SessionAgent and tool-slice construction, including worker sub-agent
// toolset layering. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
)

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	return newToolsComponent(c).buildAgent(ctx, prompt, agent, isSubAgent)
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
//
// Includes "await_tasks" (and its post-await batch_output read): the worker's await_tasks BLOCKS
// inside the turn (no StopTurn) and after its wake the worker must read the
// finished jobs' outputs; and a worker that started a background shell had
// no way to read it at all before. await_tasks.md already forbids looping
// on job_output.
var workerToolNames = []string{"edit", "multiedit", "write", "bash", "todos", "download", "fetch", tools.AskQuestionToolName, tools.AwaitTasksToolName}

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
	return newToolsComponent(c).buildToolsAgentConfigForCall(ctx, cfg, agent, isSubAgent)
}

// buildToolsAgentConfig is the legacy, context-less form of
// buildToolsAgentConfigForCall, kept for direct tests: it behaves exactly
// like a caller whose context carries no CallOptions.
func (c *coordinator) buildToolsAgentConfig(cfg *config.Config, agent config.Agent, isSubAgent bool) config.Agent {
	return newToolsComponent(c).buildToolsAgentConfig(cfg, agent, isSubAgent)
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
	return newToolsComponent(c).applyCallDisableSubAgents(ctx, cfg, agent, isSubAgent)
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
// writes arbitrary paths, git_read reads the whole repository, rush_logs
// reads the host log, the MCP resource tools would expose whatever the
// configured servers serve, and agentic_fetch builds its own unscooped
// view/glob/grep in a temp dir.
var folderScopeEscapeHatchTools = map[string]struct{}{
	tools.DownloadToolName: {}, tools.GitReadToolName: {},
	tools.AgenticFetchToolName: {}, tools.ListMCPResourcesToolName: {},
	tools.ReadMCPResourceToolName: {},
	tools.RushLogsToolName:        {},
}

// folderScopeOpForTool maps each fs_* tool to the single operation that
// gates its presence in a scoped toolset. fs_write is the one
// exception: it appears when EITHER create or overwrite is granted,
// because its per-item check picks the operation by path existence.
var folderScopeOpForTool = tools.FolderScopeToolOperations()

// withoutFolderScopeTools drops every fs_* tool from agent.AllowedTools.
func withoutFolderScopeTools(agent config.Agent) config.Agent {
	kept := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if _, ok := folderScopeOpForTool[name]; ok || name == tools.FSWriteToolName {
			continue
		}
		kept = append(kept, name)
	}
	agent.AllowedTools = kept
	return agent
}

// applyCallFolderScope implements the per-call scoped filesystem
// toolset: when ctx carries CallOptions.FolderScope, the agent's
// AllowedTools is replaced with the tools that scope allows and
// AllowedMCP becomes the "no MCPs" spelling, because an external
// filesystem MCP server would bypass the scope wholesale. It MUST run
// after buildToolsAgentConfigForCall: worker layering ADDS
// bash/edit/write to a worker sub-agent's list, and this filter has to
// see that final list to strip them again. A context without a
// FolderScope only loses the fs_* tools (they would deny every path).
//
// Every fs_* name is decided by the scope's Grants here and never left
// to fall through from the incoming list (which the post-T7 defaults DO
// contain): ungranted fs_* tools are removed, granted ones are
// re-added exactly once, and fs_write additionally requires create OR
// overwrite. Tools outside all four sets -- todos, ask_question, the
// agent delegation tool and so on -- are untouched: the scope narrows
// the filesystem surface, not the conversation.
func (c *coordinator) applyCallFolderScope(ctx context.Context, agent config.Agent) config.Agent {
	return newToolsComponent(c).applyCallFolderScope(ctx, agent)
}

// noRealWorkspaceForbiddenTools are the tools that can never be offered
// on a config whose Options.NoRealWorkspace is set (sdk.ModeLibrary with
// no WorkingDir): every legacy tool that reads or writes the real host
// filesystem via c.cfg.WorkingDir() -- for such a session, the synthetic
// library sentinel, not a sandbox -- plus every command-executing and
// arbitrary-write tool. rush_logs belongs here too (R15-1, P0, SDK
// review round 15): its log path is filepath.Join(Options.DataDirectory,
// "logs", "rush.log"), which collapses to the RELATIVE "logs/rush.log"
// when DataDirectory is empty and is then resolved against the HOST
// process's own working directory by a plain os.Stat/os.Open -- no
// WorkingDir, DiskProvider, or FolderScope seam involved. git_read
// belongs here too (R15-2, P1, SDK review round 15): it runs
// the real `git` binary as an OS subprocess with cmd.Dir set to
// c.cfg.WorkingDir() (internal/agent/tools/git_read.go) -- for such a
// session, the synthetic library sentinel, a real OS-interpreted host
// path, so a git repository that happens to exist at that path would
// have its status/diff/log/show/blame data read and sent to the
// provider. It never goes through a DiskProvider at all, so the fs_*
// custom-provider opt-in does not apply to it. agentic_fetch is listed
// explicitly as well (R14-7, P3, round-15 review follow-up): it was
// previously only INCIDENTALLY safe -- absent from workerToolNames and
// folderScopeOpForTool, so no existing per-call layering re-added it --
// and floor membership makes that denial unconditional rather than
// dependent on two unrelated positive re-add lists. This is the
// floor behind Options.DisabledTools
// (sdk.libraryEphemeralDisabledTools): the disabled list only filters
// the INITIAL AllowedTools, but per-call layering appends afterwards --
// R14-1 (P0, SDK review round 14) found buildToolsAgentConfigForCall
// re-adding write/bash/download (workerToolNames) on an ephemeral
// worker sub-agent with no check against the disabled list at all.
var noRealWorkspaceForbiddenTools = map[string]struct{}{
	tools.BashToolName: {}, tools.RunCommandToolName: {}, tools.DownloadToolName: {},
	tools.EditToolName: {}, tools.MultiEditToolName: {}, tools.WriteToolName: {},
	tools.ViewToolName: {}, tools.GlobToolName: {}, tools.GrepToolName: {},
	tools.LSToolName:           {},
	tools.RushLogsToolName:     {},
	tools.GitReadToolName:      {},
	tools.AgenticFetchToolName: {},
}

// noRealWorkspaceScopedFSTools is the fs_* family's contribution to the
// same floor. Unlike the legacy tools above, the fs_* family has a
// legitimate opt-in on a no-real-workspace session: a call supplying a
// CUSTOM DiskProvider (not nil and not tools.OSDisk()) backs every fs_*
// operation with caller-supplied storage, and applyCallFolderScope has
// already scoped it. When the call carries no custom provider, fs_*
// normalizes to the real OS disk, so the floor strips the whole family
// too (internal/app's ExecuteRun already refuses such calls outright
// for sdk callers -- R14-2; this is the coordinator-side backstop for
// direct internal callers).
var noRealWorkspaceScopedFSTools = map[string]struct{}{
	tools.FSListToolName: {}, tools.FSFindToolName: {}, tools.FSGrepToolName: {},
	tools.FSReadToolName: {}, tools.FSWriteToolName: {}, tools.FSReplaceToolName: {},
	tools.FSWriteLinesToolName: {}, tools.FSDeleteToolName: {},
}

// applyNoRealWorkspaceToolFloor is buildTools' FINAL AllowedTools
// filter (R14-1, P0, SDK review round 14): it runs after
// buildToolsAgentConfigForCall (worker toolset layering) and after
// applyCallFolderScope (folder-scope re-adds), stripping every tool a
// no-real-workspace config must never offer regardless of who added
// it. This makes the ephemeral disabled-tools list a floor later
// layering cannot punch through, rather than a one-time initial
// filter. The call's resolved DiskProvider decides the fs_* half: a
// custom provider keeps the scoped family usable (the README's
// opt-in), nil or the real OSDisk strips it alongside the rest.
func (c *coordinator) applyNoRealWorkspaceToolFloor(cfg *config.Config, agent config.Agent, disk tools.DiskProvider) config.Agent {
	return newToolsComponent(c).applyNoRealWorkspaceToolFloor(cfg, agent, disk)
}

// rejectScopedCallOnCLIProvider refuses a folder-scoped call whose
// RESOLVED provider for the given role is a CLI provider (Type "cli"):
// that provider executes the file tools inside a CLI subprocess (claude,
// codex, gemini, qwen) whose MCP tool implementations cannot see the
// FolderScope, so the scope would silently mean nothing. The refusal is a
// hard error so it fires BEFORE any provider traffic or subprocess spawn.
// Unscoped calls and non-CLI providers pass untouched.
func (c *coordinator) rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error {
	return newToolsComponent(c).rejectScopedCallOnCLIProvider(ctx, role, providerCfg)
}

// nestedAgentToolWarnOnce rate-limits the task #1050 nested-delegation
// warning below to once per process, since a misconfigured task agent
// would otherwise log on every buildTools call for every sub-agent turn.
var nestedAgentToolWarnOnce sync.Once

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
// MCP tool schemas are the one deliberate live-data exception (task #591/P2-1):
// the tool set below comes from tools.GetMCPTools, which enumerates the
// package-level mcp.Tools() registry -- live MCP client connections and their
// current schemas, refreshed asynchronously by each server's own
// ToolListChangedHandler, EnableServer/DisableServer/RemoveServer, and startup
// Initialize. The registry cannot be pinned to cfg without
// either freezing it to a stale tool schema/connection set (breaking
// reconnection and live tool-list updates) or snapshotting live network
// state, which is not what config generations represent. So len(cfg.MCP)
// (the presence gate for ListMCPResourcesTool/ReadMCPResourceTool, the only
// MCP-derived value that IS plain config data) is pinned like everything
// else here, while GetMCPTools applies the consuming ConfigStore's name filter
// to the live schemas before the AllowedMCP filter. A reload landing mid-build
// can still pair this build's pinned prompt/options/allow-list with a newer
// schema for an owned server, which is the intended live-update behavior. See
// tools.GetMCPTools's doc for the registry side of this.
//
// R3-1: when ctx carries CallOptions, the returned slice is scoped to that
// ONE call (the DisableSubAgents filter, the per-call role gate and the folder-scope filter above
// read the context). Callers must pin it to the call — resolvedOverrides.tools
// / SessionAgentCall.Tools — and must NOT publish it onto the shared agent
// with SetTools; only UpdateModels (which strips CallOptions from its ctx
// first) may publish a global toolset.
func (c *coordinator) buildTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	return newToolsComponent(c).buildTools(ctx, cfg, agent, isSubAgent)
}

func (c *coordinator) buildPolicyTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) (policyToolBuild, error) {
	return newToolsComponent(c).buildPolicyTools(ctx, cfg, agent, isSubAgent)
}

// sharedBuildScopeFor lazily creates the coordinator-level build scope for
// tool assemblies without trusted call state (the shared session toolset).
func (c *coordinator) sharedBuildScopeFor() *buildResourceScope {
	return newToolsComponent(c).sharedBuildScopeFor()
}
