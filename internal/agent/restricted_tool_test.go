package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/hooks"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

type dispatchProbeTool struct {
	name   string
	action string
	called bool
	call   fantasy.ToolCall
}

var _ interface{ RestrictedRunAction() string } = (*tools.Tool)(nil)

func (p *dispatchProbeTool) Info() fantasy.ToolInfo {
	if p.name == "" {
		p.name = "legacy_read"
	}
	return fantasy.ToolInfo{Name: p.name}
}

func (p *dispatchProbeTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.called = true
	p.call = call
	return fantasy.NewTextResponse("read result"), nil
}

func TestHooksRunBeforeRestrictedDispatch(t *testing.T) {
	t.Parallel()

	newLayered := func(t *testing.T, inner fantasy.AgentTool, runner *hooks.Runner, allowlist permission.RunAllowlist) fantasy.AgentTool {
		t.Helper()
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
		restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{inner}, svc.(permission.RestrictedRunAuthorizer))[0]
		return wrapToolsWithHooks([]fantasy.AgentTool{restricted}, runner, false)[0]
	}
	build := func(t *testing.T, spec permission.RunAllowlistSpec) permission.RunAllowlist {
		allowlist, err := permission.BuildRunAllowlist(spec)
		require.NoError(t, err)
		return allowlist
	}
	ctx := func(t *testing.T) context.Context {
		return context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	}

	t.Run("allow hook admits omitted legacy tool", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{name: "view"}
		tool := newLayered(t, probe, newRunner(t, `echo '{"decision":"allow"}'`), build(t, permission.RunAllowlistSpec{Restrict: true}))
		resp, err := tool.Run(ctx(t), fantasy.ToolCall{ID: "hook-allow", Name: "view"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, probe.called)
	})

	t.Run("silent hook remains denied", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{name: "view"}
		tool := newLayered(t, probe, newRunner(t, `exit 0`), build(t, permission.RunAllowlistSpec{Restrict: true}))
		resp, err := tool.Run(ctx(t), fantasy.ToolCall{ID: "hook-silent", Name: "view"})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.False(t, probe.called)
	})

	for _, decision := range []string{"deny", "halt"} {
		t.Run(decision+" hook never reaches inner tool", func(t *testing.T) {
			t.Parallel()
			probe := &dispatchProbeTool{name: "view"}
			output := fmt.Sprintf(`echo '{"decision":"deny","%s":true}'`, map[string]string{"halt": "halt", "deny": "reason"}[decision])
			if decision == "deny" {
				output = `echo '{"decision":"deny","reason":"blocked"}'`
			}
			tool := newLayered(t, probe, newRunner(t, output), build(t, permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"view"}}))
			resp, err := tool.Run(ctx(t), fantasy.ToolCall{ID: "hook-" + decision, Name: "view"})
			require.NoError(t, err)
			require.True(t, resp.IsError)
			require.False(t, probe.called)
		})
	}

	t.Run("updated input is evaluated by command gate", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{name: tools.BashToolName}
		tool := newLayered(t, probe, newRunner(t, `echo '{"updated_input":{"command":"echo allowed"}}'`), build(t, permission.RunAllowlistSpec{Restrict: true, AllowBash: []string{"exact:echo allowed"}}))
		resp, err := tool.Run(ctx(t), fantasy.ToolCall{ID: "hook-rewrite", Name: tools.BashToolName, Input: `{"command":"echo denied"}`})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, probe.called)
		require.JSONEq(t, `{"command":"echo allowed"}`, probe.call.Input)
	})
}

func TestRealBashHookApprovalReachesInnerRestrictedGate(t *testing.T) {
	t.Parallel()

	newBash := func(t *testing.T, svc permission.Service, runner *hooks.Runner) fantasy.AgentTool {
		t.Helper()
		bash := tools.NewBashTool(svc, t.TempDir(), &config.Attribution{}, "", nil)
		restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{bash}, svc.(permission.RestrictedRunAuthorizer))[0]
		return wrapToolsWithHooks([]fantasy.AgentTool{restricted}, runner, false)[0]
	}
	ctx := func(id string) context.Context {
		return context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	}
	arm := func(t *testing.T, svc permission.Service, bash ...string) {
		t.Helper()
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true, AllowBash: bash})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
	}

	t.Run("safe echo is allowed by exact hook approval", func(t *testing.T) {
		t.Parallel()
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		arm(t, svc)
		tool := newBash(t, svc, newRunner(t, `echo '{"decision":"allow"}'`))
		resp, err := tool.Run(ctx("bash-allow"), fantasy.ToolCall{ID: "bash-allow", Name: tools.BashToolName, Input: `{"command":"echo hook-approved"}`})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Content, "hook-approved")
	})

	t.Run("gated command is allowed by exact hook approval", func(t *testing.T) {
		t.Parallel()
		work := t.TempDir()
		svc := permission.NewPermissionService(t.Context(), work, false, nil, nil)
		arm(t, svc)
		bash := tools.NewBashTool(svc, work, &config.Attribution{}, "", nil)
		restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{bash}, svc.(permission.RestrictedRunAuthorizer))[0]
		tool := wrapToolsWithHooks([]fantasy.AgentTool{restricted}, newRunner(t, `echo '{"decision":"allow"}'`), false)[0]
		resp, err := tool.Run(ctx("bash-gated"), fantasy.ToolCall{ID: "bash-gated", Name: tools.BashToolName, Input: `{"command":"mkdir hooked-dir"}`})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		_, statErr := os.Stat(filepath.Join(work, "hooked-dir"))
		require.NoError(t, statErr)
	})

	t.Run("silent hook produces one central denial", func(t *testing.T) {
		t.Parallel()
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		arm(t, svc)
		notifications := svc.SubscribeNotifications(t.Context())
		tool := newBash(t, svc, newRunner(t, `exit 0`))
		resp, err := tool.Run(ctx("bash-silent"), fantasy.ToolCall{ID: "bash-silent", Name: tools.BashToolName, Input: `{"command":"echo denied"}`})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		select {
		case event := <-notifications:
			require.Equal(t, "bash-silent", event.Payload.ToolCallID)
			require.True(t, event.Payload.Denied)
		default:
			t.Fatal("silent gated bash produced no terminal denial")
		}
	forDrain:
		for {
			select {
			case event := <-notifications:
				t.Fatalf("duplicate terminal notification: %+v", event.Payload)
			default:
				break forDrain
			}
		}
	})

	t.Run("approval for another call ID does not bypass", func(t *testing.T) {
		t.Parallel()
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		arm(t, svc)
		tool := newBash(t, svc, newRunner(t, `exit 0`))
		wrongIDCtx := permission.WithHookApproval(ctx("bash-real"), "other-call")
		resp, err := tool.Run(wrongIDCtx, fantasy.ToolCall{ID: "bash-real", Name: tools.BashToolName, Input: `{"command":"echo denied"}`})
		require.NoError(t, err)
		require.True(t, resp.IsError)
	})

	t.Run("updated input is assessed after hook", func(t *testing.T) {
		t.Parallel()
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		arm(t, svc, "exact:echo updated")
		tool := newBash(t, svc, newRunner(t, `echo '{"updated_input":{"command":"echo updated"}}'`))
		resp, err := tool.Run(ctx("bash-rewrite"), fantasy.ToolCall{ID: "bash-rewrite", Name: tools.BashToolName, Input: `{"command":"echo denied"}`})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Content, "updated")
	})
}

func TestRestrictedRunGateUsesExactToolActions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tool      string
		allowed   string
		shouldRun bool
	}{
		{"view write does not authorize read", "view", "view:write", false},
		{"view read authorizes read", "view", "view:read", true},
		{"ls read does not authorize list", "ls", "ls:read", false},
		{"ls list authorizes list", "ls", "ls:list", true},
		{"edit write remains compatible", "edit", "edit:write", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			probe := &dispatchProbeTool{name: test.tool}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
				Restrict:   true,
				AllowTools: []string{test.allowed},
			})
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			agent := NewSessionAgent(SessionAgentOptions{
				Tools:          []fantasy.AgentTool{probe},
				RestrictedRuns: svc.(permission.RestrictedRunAuthorizer),
			}).(*sessionAgent)
			tool, ok := agent.tools.Get(0)
			require.True(t, ok)
			ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
			resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "action-call", Name: test.tool})
			require.NoError(t, err)
			require.Equal(t, test.shouldRun, probe.called)
			require.Equal(t, !test.shouldRun, resp.IsError)
		})
	}
}

func TestRestrictedRunGatePreservesPermissionFastPaths(t *testing.T) {
	t.Parallel()

	t.Run("global tool action grant bypasses restricted gate", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{name: "view"}
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, []string{"view"}, nil)
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
		wrapped := NewSessionAgent(SessionAgentOptions{Tools: []fantasy.AgentTool{probe}, RestrictedRuns: svc.(permission.RestrictedRunAuthorizer)}).(*sessionAgent)
		tool, ok := wrapped.tools.Get(0)
		require.True(t, ok)
		resp, err := tool.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{ID: "global-action", Name: "view"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, probe.called)
	})

	t.Run("global matching tool action grant bypasses restricted gate", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{name: "view"}
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, []string{"view:read"}, nil)
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"view:write"}})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
		wrapped := NewSessionAgent(SessionAgentOptions{Tools: []fantasy.AgentTool{probe}, RestrictedRuns: svc.(permission.RestrictedRunAuthorizer)}).(*sessionAgent)
		tool, ok := wrapped.tools.Get(0)
		require.True(t, ok)
		resp, err := tool.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{ID: "global-action", Name: "view"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, probe.called)
	})

	t.Run("skip grants dispatch before restricted gate", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{name: "view"}
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), true, nil, nil)
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
		wrapped := NewSessionAgent(SessionAgentOptions{Tools: []fantasy.AgentTool{probe}, RestrictedRuns: svc.(permission.RestrictedRunAuthorizer)}).(*sessionAgent)
		tool, ok := wrapped.tools.Get(0)
		require.True(t, ok)
		resp, err := tool.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{ID: "skip", Name: "view"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, probe.called)
	})

	t.Run("global command grant bypasses restricted gate", func(t *testing.T) {
		t.Parallel()
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, []string{"bash"}, nil)
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
		wrapped := NewSessionAgent(SessionAgentOptions{
			Tools:          []fantasy.AgentTool{tools.NewBashTool(svc, t.TempDir(), &config.Attribution{}, "", nil)},
			RestrictedRuns: svc.(permission.RestrictedRunAuthorizer),
		}).(*sessionAgent)
		tool, ok := wrapped.tools.Get(0)
		require.True(t, ok)
		resp, err := tool.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{
			ID:    "global-command",
			Name:  tools.BashToolName,
			Input: `{"command":"echo central-bypass"}`,
		})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Content, "central-bypass")
	})
}

func TestRestrictedDispatchDenialPublishesDecision(t *testing.T) {
	t.Parallel()

	probe := &dispatchProbeTool{name: "view"}
	svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
	allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true})
	require.NoError(t, err)
	svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
	notifications := svc.SubscribeNotifications(t.Context())
	wrapped := NewSessionAgent(SessionAgentOptions{Tools: []fantasy.AgentTool{probe}, RestrictedRuns: svc.(permission.RestrictedRunAuthorizer)}).(*sessionAgent)
	tool, ok := wrapped.tools.Get(0)
	require.True(t, ok)
	resp, err := tool.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{ID: "central-deny", Name: "view"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.False(t, probe.called)
	select {
	case event := <-notifications:
		require.Equal(t, "central-deny", event.Payload.ToolCallID)
		require.True(t, event.Payload.Denied)
		require.False(t, event.Payload.Granted)
	default:
		t.Fatal("central denial did not publish a decided permission notification")
	}
}

func TestRestrictedRunGateUsesDynamicMCPActionProvider(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		allow     string
		shouldRun bool
	}{
		{"execute allowed", "mcp_server_tool:execute", true},
		{"unrelated action denied", "mcp_server_tool:read", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			probe := &dispatchProbeTool{name: "mcp_server_tool", action: "execute"}
			svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
			allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{test.allow}})
			require.NoError(t, err)
			svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
			restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer))[0]
			tool := wrapToolsWithHooks([]fantasy.AgentTool{restricted}, newRunner(t, `exit 0`), false)[0]
			resp, err := tool.Run(context.WithValue(t.Context(), tools.SessionIDContextKey, "session"), fantasy.ToolCall{ID: "mcp-action", Name: "mcp_server_tool"})
			require.NoError(t, err)
			require.Equal(t, test.shouldRun, probe.called)
			require.Equal(t, !test.shouldRun, resp.IsError)
		})
	}
}

func TestRestrictedToolActionMetadataCoversBuiltIns(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	coder, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok)
	built, err := coord.buildTools(t.Context(), coord.cfg.Config(), coder, false)
	require.NoError(t, err)
	require.NotEmpty(t, built, "default coordinator buildTools must produce an auditable toolset")
	for _, tool := range built {
		name := tool.Info().Name
		if name == "bash" || name == "run_command" {
			continue
		}
		if strings.HasPrefix(name, "mcp_") {
			continue
		}
		_, declared := restrictedToolActions[name]
		require.True(t, declared, "built-in %q from coordinator buildTools lacks restricted dispatch action metadata", name)
	}
}

func (p *dispatchProbeTool) ProviderOptions() fantasy.ProviderOptions { return nil }

func (p *dispatchProbeTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (p *dispatchProbeTool) RestrictedRunAction() string { return p.action }

func TestRestrictedRunGateCoversLegacyToolDispatch(t *testing.T) {
	t.Parallel()

	t.Run("omitted tool is denied before execution through SetTools", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{}
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
			Restrict:   true,
			AllowTools: []string{"other_tool"},
		})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)

		agent := NewSessionAgent(SessionAgentOptions{
			RestrictedRuns: svc.(permission.RestrictedRunAuthorizer),
		}).(*sessionAgent)
		agent.SetTools([]fantasy.AgentTool{probe})
		tool, ok := agent.tools.Get(0)
		require.True(t, ok)

		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: "legacy_read"})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.False(t, probe.called)
	})

	t.Run("allowlisted legacy tool executes", func(t *testing.T) {
		t.Parallel()
		probe := &dispatchProbeTool{}
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
			Restrict:   true,
			AllowTools: []string{"legacy_read"},
		})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)

		agent := NewSessionAgent(SessionAgentOptions{
			Tools:          []fantasy.AgentTool{probe},
			RestrictedRuns: svc.(permission.RestrictedRunAuthorizer),
		}).(*sessionAgent)
		tool, ok := agent.tools.Get(0)
		require.True(t, ok)

		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: "legacy_read"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, probe.called)
	})

	t.Run("real glob dispatch cannot read an omitted tool", func(t *testing.T) {
		t.Parallel()
		workspace := t.TempDir()
		secret := filepath.Join(workspace, "secret.txt")
		require.NoError(t, os.WriteFile(secret, []byte("must not be returned"), 0o600))
		svc := permission.NewPermissionService(t.Context(), workspace, false, nil, nil)
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true})
		require.NoError(t, err)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)

		agent := NewSessionAgent(SessionAgentOptions{
			Tools:          []fantasy.AgentTool{tools.NewGlobTool(workspace)},
			RestrictedRuns: svc.(permission.RestrictedRunAuthorizer),
		}).(*sessionAgent)
		tool, ok := agent.tools.Get(0)
		require.True(t, ok)
		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.GlobToolName, Input: `{"pattern":"*"}`})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.NotContains(t, resp.Content, "secret.txt")
	})
}
