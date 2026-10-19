package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

const denyExecChild = "RUSH_DENY_EXEC_CHILD"

type policyPermissionProbe struct {
	*pubsub.Broker[permission.PermissionRequest]
	calls int
}

func (p *policyPermissionProbe) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	p.calls++
	return false, nil
}

func (p *policyPermissionProbe) Grant(permission.PermissionRequest)           {}
func (p *policyPermissionProbe) Deny(permission.PermissionRequest)            {}
func (p *policyPermissionProbe) GrantPersistent(permission.PermissionRequest) {}
func (p *policyPermissionProbe) AutoApproveSession(string)                    {}
func (p *policyPermissionProbe) InheritSessionAutoApprove(string, string)     {}
func (p *policyPermissionProbe) SetSkipRequests(bool)                         {}
func (p *policyPermissionProbe) SetRunAllowlist(permission.RunAllowlist)      {}
func (p *policyPermissionProbe) SkipRequests() bool                           { return false }
func (p *policyPermissionProbe) SubscribeNotifications(context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}
func (p *policyPermissionProbe) ListSessionPermissions(context.Context, string) ([]db.SessionPermission, error) {
	return nil, nil
}
func (p *policyPermissionProbe) UpdatePermissionEnabled(context.Context, string, bool) error {
	return nil
}
func (p *policyPermissionProbe) DeletePermission(context.Context, string) error { return nil }
func (p *policyPermissionProbe) Subscribe(ctx context.Context) <-chan pubsub.Event[permission.PermissionRequest] {
	return p.Broker.Subscribe(ctx)
}

func runPolicyChild(t *testing.T, value, mode string) string {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(binary, "-test.run=^TestDenyExecPolicyHelper$")
	cmd.Env = append(os.Environ(), "RUSH_DENY_EXEC="+value, denyExecChild+"="+mode)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func TestDenyExecPolicyHelper(t *testing.T) {
	mode := os.Getenv(denyExecChild)
	if mode == "" {
		return
	}
	if mode != "disabled" && mode != "bash" && mode != "argv" && mode != "allowed" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	probe := &policyPermissionProbe{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "policy")
	if mode == "bash" {
		tool := NewBashTool(probe, dir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, shell.NewBackgroundShellManager())
		resp := runBashTool(t, tool, ctx, BashParams{Command: "go test ./..."})
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "matched rule go test")
		require.Zero(t, probe.calls)
	} else if mode == "argv" {
		tool := NewRunCommandTool(probe, dir)
		raw, err := json.Marshal(RunCommandParams{Program: "go", Args: []string{"test", "./..."}})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: RunCommandToolName, Input: string(raw)})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "matched rule go test")
		require.Zero(t, probe.calls)
	} else if mode == "disabled" {
		args := []string{"test", "./..."}
		tool := NewRunCommandTool(probe, dir)
		raw, err := json.Marshal(RunCommandParams{Program: "go", Args: args})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: RunCommandToolName, Input: string(raw)})
		require.NoError(t, err)
		require.Equal(t, NewPermissionDeniedResponse().Content, resp.Content)
		require.Equal(t, 1, probe.calls)
	} else if mode == "allowed" {
		tool := NewRunCommandTool(probe, dir)
		raw, err := json.Marshal(RunCommandParams{Program: "go", Args: []string{"version"}})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: RunCommandToolName, Input: string(raw)})
		require.NoError(t, err)
		require.Equal(t, NewPermissionDeniedResponse().Content, resp.Content)
		require.Equal(t, 1, probe.calls)
	}
}

func TestDenyExecPolicyProcessSnapshot(t *testing.T) {
	runPolicyChild(t, "ci-only", "bash")
	runPolicyChild(t, "ci-only", "argv")
	runPolicyChild(t, "", "disabled")
	runPolicyChild(t, "ci-only", "allowed")
}
