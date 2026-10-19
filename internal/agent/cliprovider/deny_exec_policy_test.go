package cliprovider

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const (
	mcpDenyExecChild = "RUSH_DENY_EXEC_MCP_CHILD"
	mcpSpawnSentinel = "MCP spawn sentinel"
)

type policyMCPPermissionProbe struct {
	permission.Service
	calls atomic.Int32
	err   error
	allow bool
}

func (p *policyMCPPermissionProbe) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	p.calls.Add(1)
	if p.err != nil {
		return false, p.err
	}
	return p.allow, nil
}

func (p *policyMCPPermissionProbe) Subscribe(context.Context) <-chan pubsub.Event[permission.PermissionRequest] {
	return make(<-chan pubsub.Event[permission.PermissionRequest])
}

func (p *policyMCPPermissionProbe) SubscribeNotifications(context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func TestMCPDenyExecPolicyHelper(t *testing.T) {
	mode := os.Getenv(mcpDenyExecChild)
	if mode == "" {
		return
	}
	if mode != "ci-denied" && mode != "ci-allowed" && mode != "disabled" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	if mode == "disabled" {
		t.Setenv("RUSH_DENY_EXEC", "")
	}
	wd := t.TempDir()
	probe := &policyMCPPermissionProbe{allow: true}
	var registered, direct atomic.Int32
	var gotCmd, gotDir atomic.Value
	testMCPBashSpawn = func(_ context.Context, kind, command string) (string, error) {
		registered.Add(1)
		gotCmd.Store(command)
		gotDir.Store(kind)
		return mcpSpawnSentinel, nil
	}
	testRunShellSpawn = func(_ context.Context, command, dir string) (string, error) {
		direct.Add(1)
		gotCmd.Store(command)
		gotDir.Store(dir)
		return mcpSpawnSentinel, nil
	}
	t.Cleanup(func() { testMCPBashSpawn = nil; testRunShellSpawn = nil })
	srv, err := newRushMCPServer(t.Context(), probe, nil, "policy-session", wd, "test-token", nil)
	require.NoError(t, err)
	t.Cleanup(srv.stop)
	client := connectMCPTestClient(t, srv)
	command := "go test ./..."
	expected := int32(1)
	if mode == "ci-denied" {
		expected = 0
	}
	if mode == "ci-allowed" {
		command = "go version"
	}
	result := callMCPTool(t, client, "Bash", mcpBashInput{Command: command, Description: "policy probe"})
	if mode == "ci-denied" {
		require.True(t, result.IsError)
		require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "matched rule go test")
	} else {
		require.False(t, result.IsError)
		require.Len(t, result.Content, 1)
		require.Equal(t, mcpSpawnSentinel, result.Content[0].(*mcp.TextContent).Text)
		require.Equal(t, command, gotCmd.Load())
		require.Equal(t, "Bash", gotDir.Load())
	}
	require.Equal(t, expected, registered.Load())
	require.Equal(t, expected, probe.calls.Load())
	out, runErr := runShell(context.Background(), command, wd)
	if mode == "ci-denied" {
		require.Error(t, runErr)
		require.Contains(t, runErr.Error(), "matched rule go test")
		require.Empty(t, out)
	} else {
		require.NoError(t, runErr)
		require.Equal(t, mcpSpawnSentinel, out)
		require.Equal(t, command, gotCmd.Load())
		require.Equal(t, wd, gotDir.Load())
	}
	require.Equal(t, expected, direct.Load())
	if mode == "ci-denied" {
		_, err = runShell(context.Background(), "bash -c '$tool test'", wd)
		require.Error(t, err)
		require.Contains(t, err.Error(), "opaque-wrapper-command")
	}
}

func TestMCPDenyExecPolicyProcessSnapshot(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct{ value, mode string }{{"ci-only", "ci-denied"}, {"ci-only", "ci-allowed"}, {"", "disabled"}} {
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestMCPDenyExecPolicyHelper$")
		cmd.Env = append(os.Environ(), "RUSH_DENY_EXEC="+tc.value, mcpDenyExecChild+"="+tc.mode)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}
