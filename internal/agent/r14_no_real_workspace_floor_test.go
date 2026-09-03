package agent

// applyNoRealWorkspaceToolFloor unit tests (R14-1, P0, SDK review round
// 14): the final AllowedTools filter behind Options.NoRealWorkspace must
// strip every forbidden legacy tool and, unless a custom DiskProvider backs
// the call, the whole fs_* family — no matter that worker-toolset layering
// appended some of them after the initial disabled-tools pass.

import (
	"context"
	"io"
	"io/fs"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// fakeFloorDisk is a do-nothing DiskProvider used purely for identity:
// applyNoRealWorkspaceToolFloor only compares the provider against nil and
// tools.OSDisk(), so every method can return zero values.
type fakeFloorDisk struct{}

func (fakeFloorDisk) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	return nil, nil
}

func (fakeFloorDisk) EvalSymlinks(ctx context.Context, name string) (string, error) {
	return name, nil
}

func (fakeFloorDisk) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	return nil, nil
}

func (fakeFloorDisk) ReadFile(ctx context.Context, name string) ([]byte, error) {
	return nil, nil
}

func (fakeFloorDisk) MkdirAll(ctx context.Context, dir string, perm fs.FileMode) error {
	return nil
}

func (fakeFloorDisk) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	return nil
}

func (fakeFloorDisk) Remove(ctx context.Context, name string) error {
	return nil
}

func (fakeFloorDisk) List(ctx context.Context, req tools.ListRequest) (tools.ListResult, error) {
	return tools.ListResult{}, nil
}

func (fakeFloorDisk) Find(ctx context.Context, req tools.FindRequest) (tools.FindResult, error) {
	return tools.FindResult{}, nil
}

func (fakeFloorDisk) Search(ctx context.Context, req tools.SearchRequest) (tools.DiskSearchResult, error) {
	return tools.DiskSearchResult{}, nil
}

// floorInputTools is the layered list a worker sub-agent would present:
// read-only/conversation tools plus every appended workerToolNames member
// (minus todos, which stays) plus a few fs_* names.
var floorInputTools = []string{
	"view", "todos", tools.AskQuestionToolName, "fetch",
	"edit", "multiedit", "write", "bash", "download",
	"fs_read", "fs_write", "fs_list",
}

// TestApplyNoRealWorkspaceToolFloorStripsLayeredWorkerTools pins the
// R14-1 regression: worker toolset layering cannot punch through the
// no-real-workspace floor. With nil disk the worker-appended write tools
// AND the fs_* family are stripped while conversation tools survive.
func TestApplyNoRealWorkspaceToolFloorStripsLayeredWorkerTools(t *testing.T) {
	coord := newRoleModelTestCoordinator(t, testEnv(t), false)
	cfg := coord.cfg.Config()
	cfg.Options = &config.Options{NoRealWorkspace: true}

	got := coord.applyNoRealWorkspaceToolFloor(cfg, config.Agent{AllowedTools: floorInputTools}, nil)

	require.Equal(t,
		[]string{"todos", tools.AskQuestionToolName, "fetch"}, got.AllowedTools)
}

// TestApplyNoRealWorkspaceToolFloorKeepsFsWithCustomDisk pins the README
// opt-in: a custom (non-nil, non-OSDisk) DiskProvider keeps the scoped
// fs_* family usable while the legacy forbidden tools are still stripped.
func TestApplyNoRealWorkspaceToolFloorKeepsFsWithCustomDisk(t *testing.T) {
	coord := newRoleModelTestCoordinator(t, testEnv(t), false)
	cfg := coord.cfg.Config()
	cfg.Options = &config.Options{NoRealWorkspace: true}

	got := coord.applyNoRealWorkspaceToolFloor(
		cfg, config.Agent{AllowedTools: floorInputTools}, fakeFloorDisk{})

	require.Equal(t,
		[]string{
			"todos", tools.AskQuestionToolName, "fetch",
			"fs_read", "fs_write", "fs_list",
		}, got.AllowedTools)
}

// TestApplyNoRealWorkspaceToolFloorRealOSDiskIsNotCustom pins that the
// real OS disk counts as "no custom provider": the fs_* family is stripped
// exactly as with a nil provider.
func TestApplyNoRealWorkspaceToolFloorRealOSDiskIsNotCustom(t *testing.T) {
	coord := newRoleModelTestCoordinator(t, testEnv(t), false)
	cfg := coord.cfg.Config()
	cfg.Options = &config.Options{NoRealWorkspace: true}

	got := coord.applyNoRealWorkspaceToolFloor(
		cfg, config.Agent{AllowedTools: floorInputTools}, tools.OSDisk())

	require.Equal(t,
		[]string{"todos", tools.AskQuestionToolName, "fetch"}, got.AllowedTools)
}

// TestApplyNoRealWorkspaceToolFloorInertWithoutCapability pins that the
// floor is a pure no-op without the capability: the input slice is
// returned byte-identical, order included, never reallocated or reordered.
func TestApplyNoRealWorkspaceToolFloorInertWithoutCapability(t *testing.T) {
	coord := newRoleModelTestCoordinator(t, testEnv(t), false)
	cfg := coord.cfg.Config()
	cfg.Options = &config.Options{NoRealWorkspace: false}

	input := append([]string(nil), floorInputTools...)
	got := coord.applyNoRealWorkspaceToolFloor(cfg, config.Agent{AllowedTools: input}, nil)

	require.Equal(t, floorInputTools, got.AllowedTools)
}

// TestApplyNoRealWorkspaceToolFloorNilSafe pins the nil-safety contract: a
// nil cfg and a cfg with nil Options both return the agent untouched.
func TestApplyNoRealWorkspaceToolFloorNilSafe(t *testing.T) {
	coord := newRoleModelTestCoordinator(t, testEnv(t), false)
	input := config.Agent{AllowedTools: floorInputTools}

	got := coord.applyNoRealWorkspaceToolFloor(nil, input, nil)
	require.Equal(t, floorInputTools, got.AllowedTools)

	nilOpts := coord.cfg.Config()
	nilOpts.Options = nil
	got = coord.applyNoRealWorkspaceToolFloor(nilOpts, input, nil)
	require.Equal(t, floorInputTools, got.AllowedTools)
}
