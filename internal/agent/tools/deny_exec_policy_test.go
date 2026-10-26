package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

const denyExecChild = "RUSH_DENY_EXEC_CHILD"

type policyPermissionProbe struct {
	*pubsub.Broker[permission.PermissionRequest]
	calls int
	allow bool
}

func (p *policyPermissionProbe) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	p.calls++
	return p.allow, nil
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
	cmd := platform.Command(t.Context(), binary, "-test.run=^TestDenyExecPolicyHelper$")
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
	if mode != "denied" && mode != "allowed" && mode != "disabled" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	probe := &policyPermissionProbe{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "policy")
	response := fantasy.WithResponseMetadata(fantasy.NewTextResponse("spawn sentinel"), map[string]string{"origin": "fake-spawn"})
	t.Cleanup(func() { testCommandSpawn = nil })
	for _, surface := range []string{"bash", "argv"} {
		calls := 0
		var gotKind string
		var gotArgs []string
		testCommandSpawn = func(_ context.Context, kind string, args []string) (fantasy.ToolResponse, error) {
			calls++
			gotKind = kind
			gotArgs = append([]string(nil), args...)
			return response, nil
		}
		switch surface {
		case "bash":
			tool := NewBashTool(probe, dir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, shell.NewBackgroundShellManager())
			switch mode {
			case "denied":
				resp := runBashTool(t, tool, ctx, BashParams{Command: "go test ./..."})
				require.True(t, resp.IsError)
				require.Contains(t, resp.Content, "matched rule go test")
				require.Zero(t, calls)
			case "allowed", "disabled":
				cmd := "go version"
				if mode == "disabled" {
					cmd = "go test ./..."
				}
				resp := runBashTool(t, tool, ctx, BashParams{Command: cmd})
				require.Equal(t, response, resp)
				require.Equal(t, 1, calls)
				require.Equal(t, BashToolName, gotKind)
				require.Equal(t, []string{cmd}, gotArgs)
			}
		case "argv":
			tool := NewRunCommandTool(probe, dir)
			params := RunCommandParams{Program: "go", Args: []string{"test", "./..."}}
			if mode == "allowed" {
				params.Args = []string{"version"}
			}
			raw, err := json.Marshal(params)
			require.NoError(t, err)
			resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: RunCommandToolName, Input: string(raw)})
			require.NoError(t, err)
			switch mode {
			case "denied":
				require.True(t, resp.IsError)
				require.Contains(t, resp.Content, "matched rule go test")
				require.Zero(t, calls)
			case "allowed", "disabled":
				require.Equal(t, response, resp)
				require.Equal(t, 1, calls)
				require.Equal(t, RunCommandToolName, gotKind)
				require.Equal(t, append([]string{params.Program}, params.Args...), gotArgs)
			}
		}
	}
}

func TestDenyExecPolicyProcessSnapshot(t *testing.T) {
	runPolicyChild(t, "ci-only", "denied")
	runPolicyChild(t, "ci-only", "allowed")
	runPolicyChild(t, "", "disabled")
}
