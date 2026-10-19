package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

const batchDenyExecChild = "RUSH_DENY_EXEC_BATCH_CHILD"

type denyExecBatchPerms struct {
	permission.Service
	calls int
}

func (p *denyExecBatchPerms) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	p.calls++
	return false, nil
}

func (p *denyExecBatchPerms) Subscribe(context.Context) <-chan pubsub.Event[permission.PermissionRequest] {
	return make(<-chan pubsub.Event[permission.PermissionRequest])
}
func (p *denyExecBatchPerms) GrantPersistent(permission.PermissionRequest) {}
func (p *denyExecBatchPerms) Grant(permission.PermissionRequest)           {}
func (p *denyExecBatchPerms) Deny(permission.PermissionRequest)            {}
func (p *denyExecBatchPerms) AutoApproveSession(string)                    {}
func (p *denyExecBatchPerms) InheritSessionAutoApprove(string, string)     {}
func (p *denyExecBatchPerms) SetSkipRequests(bool)                         {}
func (p *denyExecBatchPerms) SkipRequests() bool                           { return false }
func (p *denyExecBatchPerms) SetRunAllowlist(permission.RunAllowlist)      {}
func (p *denyExecBatchPerms) SubscribeNotifications(context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}
func (p *denyExecBatchPerms) ListSessionPermissions(context.Context, string) ([]db.SessionPermission, error) {
	return nil, nil
}
func (p *denyExecBatchPerms) DeletePermission(context.Context, string) error              { return nil }
func (p *denyExecBatchPerms) UpdatePermissionEnabled(context.Context, string, bool) error { return nil }

func TestBatchDenyExecPolicyHelper(t *testing.T) {
	mode := os.Getenv(batchDenyExecChild)
	if mode == "" {
		return
	}
	perms := &denyExecBatchPerms{}
	workingDir := t.TempDir()
	bash := tools.NewBashTool(perms, workingDir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, shell.NewBackgroundShellManager())
	argv := tools.NewRunCommandTool(perms, workingDir)
	bashJSON, err := json.Marshal(tools.BashParams{Command: "go test ./..."})
	require.NoError(t, err)
	argvJSON, err := json.Marshal(tools.RunCommandParams{Program: "go", Args: []string{"test", "./..."}})
	require.NoError(t, err)
	registry := newPolicyToolBuild([]fantasy.AgentTool{bash, argv}).leaves
	env := batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) { return registry, nil }}
	for _, tc := range []struct {
		family, tool string
		input        []byte
	}{{"shell", "bash", bashJSON}, {"argv", tools.RunCommandToolName, argvJSON}} {
		results := make(chan jobbatch.Result, 1)
		var family batchLeafFamily = batchShellFamily{}
		if tc.family == "argv" {
			family = batchArgvFamily{}
		}
		execution, err := family.start(context.Background(), env, jobbatch.Launch{}, batchLeafPayload{Tool: tc.tool, SessionID: "deny", ToolCallID: "call", Input: tc.input}, func(result jobbatch.Result) { results <- result })
		require.NoError(t, err)
		select {
		case result := <-results:
			require.Equal(t, jobbatch.StateFailed, result.State)
			require.Contains(t, result.Summary, "matched rule go test")
		case <-time.After(10 * time.Second):
			t.Fatal("policy denial did not report through batch family")
		}
		require.NoError(t, execution.(jobbatch.Disposer).Dispose(context.Background()))
	}
	require.Zero(t, perms.calls, "batch-denied commands must stop before permission requests")
}

func TestBatchDenyExecPolicyProcessSnapshot(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(binary, "-test.run=^TestBatchDenyExecPolicyHelper$")
	cmd.Env = append(os.Environ(), "RUSH_DENY_EXEC=ci-only", batchDenyExecChild+"=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}
