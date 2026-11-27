package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
	"github.com/google/uuid"
)

type privateFetchIngressKey struct{}

// privateFetchIngressTool is the root compatibility alias over the moved
// private fetch ingress shell
// (internal/agent/runtime/policy/toolwrap/ingress.go); the ingress
// assertions keep compiling until their files move.
type privateFetchIngressTool = toolwrap.PrivateFetchIngressTool

// newPrivateFetchIngressShell arms one private tool through the moved
// shell: the root-owned pinned registry and the pre-built childagent.PinnedInvoke
// pass through the shell's Registry/Invoke fields — no package-agent
// identifiers cross into toolwrap.
func newPrivateFetchIngressShell(tool fantasy.AgentTool, leaves *toolwrap.PolicyLeafRegistry, invoke childagent.PinnedInvoke) *toolwrap.PrivateFetchIngressTool {
	return &toolwrap.PrivateFetchIngressTool{
		AgentTool: tool,
		Registry:  leaves,
		Invoke: func(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return invoke(ctx, leaf, call)
		},
	}
}

func withPrivateFetchIngress(ctx context.Context, invoke childagent.PinnedInvoke) context.Context {
	return context.WithValue(ctx, privateFetchIngressKey{}, invoke)
}

// buildPrivateFetchTools builds the delegated sub-agent's private toolset.
// R12-H2a round 2: the CALLER owns the build-resource scope and passes it
// explicitly — this function reads nothing from ctx for scoping and has NO
// fallback. A nil scope with an armed ingress fails closed through
// armPrivateFetchIngress.
func buildPrivateFetchTools(ctx context.Context, list []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer, scope *buildResourceScope) toolwrap.PolicyToolBuild {
	built := toolwrap.NewPolicyToolBuildForContext(ctx, toolwrap.DecoratePolicyTools(list, authorizer, nil, true))
	// R11F (M3) flag-on: resolve the ONE authority (explicit ctx binding
	// only — the process-global fallback is eliminated, R12-H2 §1.3), pin
	// this build's leaves under a fresh identity and re-arm every private
	// tool through the R7d childagent.PinnedInvoke. No binding leaves the legacy
	// in-process tools.
	ingress := privateFetchIngressFor(ctx)
	if ingress != nil {
		return armPrivateFetchIngress(ctx, built, ingress, scope)
	}
	invoke, _ := ctx.Value(privateFetchIngressKey{}).(childagent.PinnedInvoke)
	if invoke == nil {
		return built
	}
	ingressList := make([]fantasy.AgentTool, len(built.Tools))
	for i, tool := range built.Tools {
		ingressList[i] = newPrivateFetchIngressShell(tool, built.Leaves, invoke)
	}
	built.Tools = ingressList
	return built
}

// privateFetchIngressFor resolves the batch ingress for the private fetch
// build: ONLY an explicitly bound value. R12-H2 §1.3: the provider-bound
// process-global fallback is eliminated — delegation/private-fetch
// resolution never touches processCoordinatorBatchBinding.
func privateFetchIngressFor(ctx context.Context) BatchWorkIngress {
	ingress, _ := ctx.Value(privateFetchIngressKey{}).(BatchWorkIngress)
	return ingress
}

// armPrivateFetchIngress pins the build's leaf registry and wraps every
// private tool so its Run enters the ONE authority. A pin failure fails
// closed: the tools refuse rather than fall back to the old path.
// R11F round-2 (P1-5c): the invocation Policy template carries the pinned
// RegistryID/RestrictedToolset identity; see batchArmingPolicyNote (batch_
// install_policy.go) for the documented contract on FolderScope/RunAllowlist/
// CustomDisk at this arming point.
// R12-H2 §1.2: the pin takes a Construction owner released in the call's
// build-resource scope; a missing manager or scope fails closed.
func armPrivateFetchIngress(ctx context.Context, built toolwrap.PolicyToolBuild, ingress BatchWorkIngress, scope *buildResourceScope) toolwrap.PolicyToolBuild {
	registryID, err := ingress.PinLeafRegistry(built.Leaves)
	if err == nil {
		// Record the pinned identity on the build so owners of the build can
		// observe the CONSTRUCTION they pin (the capability manager deletes
		// the identity index entry when its last owner goes).
		built.RegistryID = registryID
		var caps *diskCapabilityManager
		if bw, ok := ingress.(*batchWorkIngress); ok {
			caps = bw.caps
		}
		switch {
		case caps == nil:
			err = ErrDiskProviderUnavailable
		case scope == nil:
			err = errors.New("agentic fetch: no build-resource scope in the trusted call state")
		default:
			constructionKey := diskOwnerKey{kind: diskOwnerConstruction, instance: uuid.NewString(), registryID: registryID}
			if err = caps.acquireConstruction("", registryID, constructionKey.instance); err == nil {
				scope.Register(func() { caps.release(constructionKey) })
			}
		}
	}
	if err != nil {
		for i, tool := range built.Tools {
			built.Tools[i] = errBatchIngressTool{AgentTool: tool, err: err}
		}
		return built
	}
	tpl := BatchInvocation{
		RegistryID: registryID,
		Origin:     "agentic_fetch",
		// R12-H2a (P1-3): the restricted toolset is the REAL toolset
		// fingerprint of the pinned build (the executor's hasToolset checks
		// the comma-joined leaf names), not the registry ID — a
		// provider-backed private payload must pass hasToolset at the
		// executor without bypassing it.
		Policy: batchLeafPolicyContext{RegistryID: registryID, RestrictedToolset: installwire.ToolsetFingerprint(built.Tools), RoleContract: agentcall.CloneRoleContractForCall(toolwrap.RoleContractForToolBuild(ctx, built.Tools))},
	}
	invoke := batchPinnedInvoke(ingress, tpl)
	ingressList := make([]fantasy.AgentTool, len(built.Tools))
	for i, tool := range built.Tools {
		ingressList[i] = newPrivateFetchIngressShell(tool, built.Leaves, invoke)
	}
	built.Tools = ingressList
	return built
}

func (c *coordinator) agenticFetchTool(_ context.Context, client *http.Client) (fantasy.AgentTool, error) {
	if client == nil {
		// SSRF-guarded by default: agentic_fetch takes a model-controlled
		// URL (and hands it, plus this same client, to the web_fetch/
		// web_search/sourcegraph tools of the spawned sub-agent), so it
		// carries the same loopback/private/metadata exfiltration risk
		// download/fetch were guarded against — see
		// internal/agent/tools/ssrf_guard.go.
		client = tools.NewSSRFGuardedClient(30*time.Second, false)
	}

	return fantasy.NewParallelAgentTool(
		tools.AgenticFetchToolName,
		toolwrap.AgenticFetchToolDescription(),
		func(ctx context.Context, params tools.AgenticFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			remaining, depthErr := MaxAgentDepthFrom(ctx)
			if depthErr != nil {
				return fantasy.ToolResponse{}, depthErr
			}
			if remaining == 0 {
				return fantasy.NewTextErrorResponse(ErrAgentDepthExceeded.Error()), nil
			}
			childCtx, depthErr := WithAgentDepth(ctx, remaining)
			if depthErr != nil {
				return fantasy.ToolResponse{}, depthErr
			}
			// R12-H2a (P1-3): stamp the coordinator's OWN batch binding's
			// ingress into the sub-agent context so the delegated child run
			// (and the leaf invocation context it derives) resolves private
			// fetch dispatch through the ONE per-App authority instead of the
			// eliminated process-global fallback. No binding, no stamp — the
			// private tools keep their legacy in-process behavior.
			if ingress := coordinatorPrivateFetchIngress(c); ingress != nil {
				childCtx = context.WithValue(childCtx, privateFetchIngressKey{}, ingress)
			}
			validationResult, err := toolwrap.ValidateAgenticFetchParams(childCtx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			remaining, depthErr = effectiveParentAgentDepth(childCtx, c.sessions, validationResult.SessionID)
			if depthErr != nil {
				return fantasy.ToolResponse{}, depthErr
			}
			if remaining == 0 {
				return fantasy.NewTextErrorResponse(ErrAgentDepthExceeded.Error()), nil
			}
			childCtx, depthErr = WithAgentDepth(childCtx, remaining)
			if depthErr != nil {
				return fantasy.ToolResponse{}, depthErr
			}

			// Determine description based on mode.
			var description string
			if params.URL != "" {
				description = fmt.Sprintf("Fetch and analyze content from URL: %s", params.URL)
			} else {
				description = "Search the web and analyze results"
			}

			p, err := c.permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   validationResult.SessionID,
					Path:        c.cfg.WorkingDir(),
					ToolCallID:  call.ID,
					ToolName:    tools.AgenticFetchToolName,
					Action:      "fetch",
					Description: description,
					Params:      tools.AgenticFetchPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return tools.NewPermissionDeniedResponse(), nil
			}

			tmpDir, err := os.MkdirTemp(c.cfg.Config().Options.DataDirectory, "rush-fetch-*")
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create temporary directory: %s", err)), nil
			}
			defer os.RemoveAll(tmpDir)

			var fullPrompt string

			if params.URL != "" {
				// URL mode: fetch the URL content first.
				content, err := tools.FetchURLAndConvert(ctx, client, params.URL)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to fetch URL: %s", err)), nil
				}

				hasLargeContent := len(content) > tools.LargeContentThreshold

				if hasLargeContent {
					tempFile, err := os.CreateTemp(tmpDir, "page-*.md")
					if err != nil {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create temporary file: %s", err)), nil
					}
					tempFilePath := tempFile.Name()

					if _, err := tempFile.WriteString(content); err != nil {
						tempFile.Close()
						return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to write content to file: %s", err)), nil
					}
					tempFile.Close()

					fullPrompt = fmt.Sprintf("%s\n\nThe web page from %s has been saved to: %s\n\nUse the view and grep tools to analyze this file and extract the requested information.", params.Prompt, params.URL, tempFilePath)
				} else {
					fullPrompt = fmt.Sprintf("%s\n\nWeb page URL: %s\n\n<webpage_content>\n%s\n</webpage_content>", params.Prompt, params.URL, content)
				}
			} else {
				// Search mode: let the sub-agent search and fetch as needed.
				fullPrompt = fmt.Sprintf("%s\n\nUse the web_search tool to find relevant information. Break down the question into smaller, focused searches if needed. After searching, use web_fetch to get detailed content from the most relevant results.", params.Prompt)
			}

			// Heartbeat attribution: fetch's real call-point role is fast, its
			// model is an explicit per-call build (design pt. 1/3).
			childCtx = callbeat.WithHeartbeat(childCtx, validationResult.SessionID, heartbeat.PurposeFetch, "fast", callbeat.HbSourcePerCall)

			promptOpts := []prompt.Option{
				prompt.WithWorkingDir(tmpDir),
			}

			promptTemplate, err := prompt.NewPrompt("agentic_fetch", string(toolwrap.AgenticFetchPromptTmpl()), promptOpts...)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error creating prompt: %s", err)
			}

			_, fast, err := c.buildAgentModels(ctx, true)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error building models: %s", err)
			}

			// Pinned explicitly: this tool resolves its model just above, and
			// the prompt must come from the same generation.
			systemPrompt, err := promptTemplate.Build(ctx, fast.Model.Provider(), fast.Model.Model(), c.cfg, c.cfg.Config(), false)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error building system prompt: %s", err)
			}

			fastProviderCfg, ok := c.cfg.Config().Providers.Get(fast.ModelCfg.Provider)
			if !ok {
				return fantasy.ToolResponse{}, errors.New("fast model provider not configured")
			}

			webFetchTool := tools.NewWebFetchTool(tmpDir, client)
			webSearchTool := tools.NewWebSearchTool(client)
			// Not wrapped for error logging here: NewSessionAgent wraps
			// whatever it is handed, so this slice is covered by
			// construction rather than by remembering to do it at each
			// site. See logged_tool.go.
			fetchTools := []fantasy.AgentTool{
				webFetchTool,
				webSearchTool,
				tools.NewGlobTool(tmpDir, c.permissions),
				tools.NewGrepTool(tmpDir, c.cfg.Config().Tools.Grep, c.permissions),
				tools.NewSourcegraphTool(client),
				tools.NewViewTool(c.permissions, c.filetracker, nil, tmpDir),
			}

			// R12-H2a round 2: the private build pins its construction in an
			// EXPLICIT scope — the child incarnation's own scope when this
			// runs as a batch agent leaf (the driver stamps one onto the run
			// context), otherwise a handler-local scope closed when the
			// fetch returns (the handler blocks until the delegated child
			// completes). The coordinator's shared scope is no longer a
			// fallback here.
			scope := callBuildScopeFrom(childCtx)
			if scope == nil {
				scope = newBuildResourceScope()
				defer scope.Close()
			}
			fetchBuild := buildPrivateFetchTools(childCtx, fetchTools, toolwrap.RestrictedRunAuthorizer(c.permissions), scope)

			// Sub-agent tools run without hook interception. The top-level
			// `agentic_fetch` call itself is already wrapped from the coder's
			// side; firing hooks again for every inner tool call would run
			// the user's hooks N times per delegated turn.

			agent := NewSessionAgent(SessionAgentOptions{
				SmartModel:           fast, // Use fast model for both (fetch does not need the smart slot)
				FastModel:            fast,
				SystemPromptPrefix:   fastProviderCfg.SystemPromptPrefix,
				SystemPrompt:         systemPrompt,
				DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
				IsYolo:               c.permissions.SkipRequests(),
				// IsSubAgent: true — this agent runs via runSubAgent below,
				// so it must be classified as a sub-agent for
				// effectiveToolCleanupGrace() (see agent.go, task #205) to
				// give it NO grace on its own watchdog. Left unset (false)
				// here made this nested delegation invisible to that
				// distinction: it was treated as top-level, so it got the
				// same 90s grace as the actual parent — recreating #200's
				// symmetric-cancel-out bug specifically for the
				// agentic_fetch path. Found by @oh's review.
				IsSubAgent:     true,
				Sessions:       c.sessions,
				Messages:       c.messages,
				Config:         c.cfg,
				Tools:          fetchBuild.Tools,
				RestrictedRuns: toolwrap.RestrictedRunAuthorizer(c.permissions),
			})

			return c.runSubAgent(childCtx, subAgentParams{
				Agent:          agent,
				SessionID:      validationResult.SessionID,
				AgentMessageID: validationResult.AgentMessageID,
				ToolCallID:     call.ID,
				Prompt:         fullPrompt,
				SessionTitle:   "Fetch Analysis",
				SessionSetup: func(sessionID string) {
					c.permissions.AutoApproveSession(sessionID)
				},
			})
		},
	), nil
}
