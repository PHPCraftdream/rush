package cliprovider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const mcpDenyExecChild = "RUSH_DENY_EXEC_MCP_CHILD"

type policyMCPPermissionProbe struct {
	permission.Service
	calls int
	err   error
}

func (p *policyMCPPermissionProbe) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	p.calls++
	if p.err != nil {
		return false, p.err
	}
	return false, nil
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
	workingDir := t.TempDir()
	probe := &policyMCPPermissionProbe{err: errors.New("permission sentinel")}
	srv, err := newRushMCPServer(t.Context(), probe, nil, "policy-session", workingDir, "test-token", nil)
	require.NoError(t, err)
	t.Cleanup(srv.stop)
	client := connectMCPTestClient(t, srv)
	command := "go test ./..."
	if mode == "ci-allowed" {
		command = "go version"
	}
	result := callMCPTool(t, client, "Bash", mcpBashInput{Command: command, Description: "policy probe"})
	require.True(t, result.IsError)
	message := result.Content[0].(*mcp.TextContent).Text
	if mode == "ci-denied" {
		require.Contains(t, message, "matched rule go test")
		require.Zero(t, probe.calls)
		_, err = runShell(context.Background(), command, workingDir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "matched rule go test")
	} else {
		require.Equal(t, "permission request failed: permission sentinel", message)
		require.Equal(t, 1, probe.calls)
	}
}

func TestMCPDenyExecPolicyProcessSnapshot(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct{ value, mode string }{
		{"ci-only", "ci-denied"}, {"ci-only", "ci-allowed"}, {"", "disabled"},
	} {
		cmd := exec.Command(binary, "-test.run=^TestMCPDenyExecPolicyHelper$")
		cmd.Env = append(os.Environ(), "RUSH_DENY_EXEC="+tc.value, mcpDenyExecChild+"="+tc.mode)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}
