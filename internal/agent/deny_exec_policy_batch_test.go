package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync/atomic"
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

const (
	batchDenyExecChild = "RUSH_DENY_EXEC_BATCH_CHILD"
	batchSpawnText     = "batch spawn sentinel"
)

type denyExecBatchPerms struct {
	permission.Service
	calls atomic.Int32
	allow bool
}

func (p *denyExecBatchPerms) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	p.calls.Add(1)
	return p.allow, nil
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

func (p *denyExecBatchPerms) DeletePermission(context.Context, string) error { return nil }

func (p *denyExecBatchPerms) UpdatePermissionEnabled(context.Context, string, bool) error { return nil }

func TestBatchDenyExecPolicyHelper(t *testing.T) {
	mode := os.Getenv(batchDenyExecChild)
	if mode == "" {
		return
	}
	if mode != "denied" && mode != "inherited" && mode != "allowed" && mode != "disabled" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	if mode == "inherited" {
		t.Setenv("RUSH_DENY_EXEC", "")
	}
	perms := &denyExecBatchPerms{allow: true}
	wd := t.TempDir()
	bash := tools.NewBashTool(perms, wd, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, shell.NewBackgroundShellManager())
	argv := tools.NewRunCommandTool(perms, wd)
	registry := newPolicyToolBuild([]fantasy.AgentTool{bash, argv}).leaves
	env := batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) { return registry, nil }}
	for _, tc := range []struct {
		family, tool        string
		denied, allowed     []byte
		denyArgv, allowArgv []string
	}{{"shell", tools.BashToolName, mustBatchJSON(t, tools.BashParams{Command: "go test ./..."}), mustBatchJSON(t, tools.BashParams{Command: "go version"}), []string{"go test ./..."}, []string{"go version"}}, {"argv", tools.RunCommandToolName, mustBatchJSON(t, tools.RunCommandParams{Program: "go", Args: []string{"test", "./..."}}), mustBatchJSON(t, tools.RunCommandParams{Program: "go", Args: []string{"version"}}), []string{"go", "test", "./..."}, []string{"go", "version"}}} {
		perms.calls.Store(0)
		input, want := tc.denied, tc.denyArgv
		if mode == "allowed" {
			input, want = tc.allowed, tc.allowArgv
		}
		denied := mode == "denied" || mode == "inherited"
		var calls atomic.Int32
		var kind atomic.Value
		var got atomic.Value
		wantResponse := fantasy.WithResponseMetadata(fantasy.NewTextResponse(batchSpawnText), map[string]string{"origin": "fake-batch-spawn"})
		spawn := func(_ context.Context, k string, a []string) (fantasy.ToolResponse, error) {
			calls.Add(1)
			kind.Store(k)
			got.Store(append([]string(nil), a...))
			return wantResponse, nil
		}
		ctx := tools.WithCommandSpawnProbe(context.Background(), spawn)
		var family batchLeafFamily = batchShellFamily{}
		if tc.family == "argv" {
			family = batchArgvFamily{}
		}
		results := make(chan jobbatch.Result, 1)
		execution, err := family.start(ctx, env, jobbatch.Launch{}, batchLeafPayload{Tool: tc.tool, SessionID: "deny", ToolCallID: "call", Input: input}, func(r jobbatch.Result) { results <- r })
		require.NoError(t, err)
		var result jobbatch.Result
		select {
		case result = <-results:
		case <-time.After(10 * time.Second):
			t.Fatal("batch policy execution did not report")
		}
		var response fantasy.ToolResponse
		require.NoError(t, json.Unmarshal(result.Details, &response))
		if denied {
			require.Equal(t, jobbatch.StateFailed, result.State)
			require.Contains(t, result.Summary, "matched rule go test")
			require.Zero(t, calls.Load())
		} else {
			require.Equal(t, jobbatch.StateCompleted, result.State)
			require.Equal(t, wantResponse, response)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, want, got.Load())
			expected := tools.BashToolName
			if tc.family == "argv" {
				expected = tools.RunCommandToolName
			}
			require.Equal(t, expected, kind.Load())
		}
		wantPermission := int32(0)
		if !denied {
			wantPermission = 1
		}
		require.Equal(t, wantPermission, perms.calls.Load())
		require.NoError(t, execution.(jobbatch.Disposer).Dispose(context.Background()))
	}
}

func mustBatchJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestBatchDenyExecPolicyProcessSnapshot(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct{ value, mode string }{{"ci-only", "denied"}, {"ci-only", "inherited"}, {"ci-only", "allowed"}, {"", "disabled"}} {
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestBatchDenyExecPolicyHelper$")
		cmd.Env = append(os.Environ(), "RUSH_DENY_EXEC="+tc.value, batchDenyExecChild+"="+tc.mode)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}
