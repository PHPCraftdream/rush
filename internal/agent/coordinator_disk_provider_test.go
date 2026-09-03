package agent

// #858 regression tests: per-call DiskProvider.
//
// Mirrors the shape of coordinator_tool_pinning_test.go's FolderScope
// tests: buildTools/resolveSessionModels must build the fs_* toolset from
// THIS call's CallOptions.DiskProvider, pin it onto the resolved/pinned
// tool slice, and never publish it onto the shared currentAgent or the
// global UpdateModels refresh.
//
// A successful fs_read at runtime additionally requires a granted
// permission.FolderScope: fs_batch.go's RunFSBatch calls
// batch.Scope.Check(abs, op) after resolving the path, and the zero
// FolderScope value denies every operation on every path (see
// folderscope.go's doc comment). So every test below that expects a
// successful read also grants FileOpRead via newFolderScope — this is a
// property of the existing scope-check pipeline (T7/T8), not something
// #858 changes, and it is why TestUpdateModels_NeverPublishesCallerDiskProvider
// (whose ctx has its CallOptions, and therefore any FolderScope, stripped
// by withoutCallOptions before buildTools) cannot observe a successful
// read at all and instead proves the negative directly: the fake was
// never called.

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFileInfo is the minimal fs.FileInfo fakeDiskProvider.Stat returns.
// fs_read only consumes IsDir (via resolveScopedPath) among FileInfo's
// fields, so the rest are harmless zero values.
type fakeFileInfo struct{ name string }

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return 0 }
func (fi fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return false }
func (fi fakeFileInfo) Sys() any           { return nil }

// fakeDiskProvider is a minimal in-memory tools.DiskProvider for these
// tests. It records every method invocation by name (with the ":name"
// receiver argument where relevant) so a test can prove the coordinator
// path under test either DID or DID NOT reach it — the second direction
// (zero calls recorded) is what TestUpdateModels_NeverPublishesCallerDiskProvider
// needs, since the zero-value FolderScope denies its read before content
// could ever prove which disk answered.
type fakeDiskProvider struct {
	mu    sync.Mutex
	files map[string]string
	calls []string
}

var _ tools.DiskProvider = (*fakeDiskProvider)(nil)

func newFakeDiskProvider(files map[string]string) *fakeDiskProvider {
	cp := make(map[string]string, len(files))
	for k, v := range files {
		cp[k] = v
	}
	return &fakeDiskProvider{files: cp}
}

func (f *fakeDiskProvider) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeDiskProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDiskProvider) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	f.record("Stat:" + name)
	f.mu.Lock()
	_, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	return fakeFileInfo{name: filepath.Base(name)}, nil
}

func (f *fakeDiskProvider) EvalSymlinks(_ context.Context, name string) (string, error) {
	f.record("EvalSymlinks:" + name)
	return filepath.Clean(name), nil
}

func (f *fakeDiskProvider) Open(_ context.Context, name string) (io.ReadCloser, error) {
	f.record("Open:" + name)
	f.mu.Lock()
	content, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (f *fakeDiskProvider) ReadFile(_ context.Context, name string) ([]byte, error) {
	f.record("ReadFile:" + name)
	f.mu.Lock()
	content, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

func (f *fakeDiskProvider) MkdirAll(_ context.Context, dir string, _ fs.FileMode) error {
	f.record("MkdirAll:" + dir)
	return nil
}

func (f *fakeDiskProvider) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	f.record("WriteFile:" + name)
	f.mu.Lock()
	f.files[name] = string(data)
	f.mu.Unlock()
	return nil
}

func (f *fakeDiskProvider) Remove(_ context.Context, name string) error {
	f.record("Remove:" + name)
	f.mu.Lock()
	delete(f.files, name)
	f.mu.Unlock()
	return nil
}

func (f *fakeDiskProvider) List(_ context.Context, _ tools.ListRequest) (tools.ListResult, error) {
	f.record("List")
	return tools.ListResult{}, nil
}

func (f *fakeDiskProvider) Find(_ context.Context, _ tools.FindRequest) (tools.FindResult, error) {
	f.record("Find")
	return tools.FindResult{}, nil
}

func (f *fakeDiskProvider) Search(_ context.Context, _ tools.SearchRequest) (tools.DiskSearchResult, error) {
	f.record("Search")
	return tools.DiskSearchResult{}, nil
}

// absWorkingDir resolves dir (testEnv's fakeEnv.workingDir, spelled
// "/tmp/rush-test/<test>" with no Windows drive letter) to a fully
// qualified absolute path. Every real path this file constructs must go
// through the SAME resolution: resolveScopedPath's filepath.Abs adds the
// process's current drive to a driveless rooted path, and
// permission.FolderScope.Check does a cross-volume filepath.Rel that
// fails closed ("outside every folder scope") when the granted entry's
// Dir and the checked path disagree on volume — exactly what happens if
// the scope is built from the raw, driveless workingDir while the
// checked path was resolved with a drive letter attached.
//
// The absolute result is then ALSO symlink-canonicalized via
// filepath.EvalSymlinks, the same namespace hygiene one level up: /tmp
// is a symlink to /private/tmp on macOS (some Windows runners junction
// the temp drive too), so a raw /tmp/... spelling and any path the real
// disk resolved (resolveScopedPath canonicalizes through EvalSymlinks)
// land in two different namespaces, and a scope built from the raw form
// denies the resolved path. EvalSymlinks on an already-canonical path is
// a no-op, so this is safe and idempotent for callers whose dir is
// already resolved (e.g. testEnv's canonical workingDir). dir must exist
// on disk for the EvalSymlinks step; every caller passes testEnv output,
// which testEnv has just created.
func absWorkingDir(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)
	resolved, err := filepath.EvalSymlinks(abs)
	require.NoError(t, err)
	return resolved
}

// fsReadToolFrom extracts the fs_read tool from a pinned/published tool
// slice, failing the test if it is absent.
func fsReadToolFrom(t *testing.T, toolset []fantasy.AgentTool) fantasy.AgentTool {
	t.Helper()
	for _, tl := range toolset {
		if tl.Info().Name == tools.FSReadToolName {
			return tl
		}
	}
	t.Fatalf("fs_read tool not found in toolset")
	return nil
}

// runFSRead runs one single-item fs_read call against path and returns
// the raw response, without asserting success — callers check status
// themselves, since some tests expect a scope denial.
func runFSRead(t *testing.T, tool fantasy.AgentTool, path string) fantasy.ToolResponse {
	t.Helper()
	raw, err := json.Marshal(tools.FSReadParams{Items: []tools.FSReadItem{{Path: path}}})
	require.NoError(t, err)
	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-fs-read", Input: string(raw)})
	require.NoError(t, err)
	return resp
}

// diskProviderPublishRecorder mirrors toolPublishRecorder but also keeps
// the actual published tool slices (not just their names): proving
// TestUpdateModels_NeverPublishesCallerDiskProvider needs to Run the
// published fs_read tool, which a name-only recording cannot support.
type diskProviderPublishRecorder struct {
	*mockSessionAgent
	mu         sync.Mutex
	published  [][]fantasy.AgentTool
	modelCalls int
}

func (r *diskProviderPublishRecorder) SetTools(toolset []fantasy.AgentTool) {
	r.mu.Lock()
	r.published = append(r.published, toolset)
	r.mu.Unlock()
}

func (r *diskProviderPublishRecorder) SetModels(smart, fast Model) {
	r.mu.Lock()
	r.modelCalls++
	r.mu.Unlock()
}

func (r *diskProviderPublishRecorder) snapshot() ([][]fantasy.AgentTool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.published, r.modelCalls
}

// TestResolveSessionModels_PinsPerCallDiskProvider proves the per-call
// DiskProvider decides which filesystem the PINNED fs_read tool reads
// from, is isolated between two different sessions in both resolution
// orders, and never reaches the shared agent.
func TestResolveSessionModels_PinsPerCallDiskProvider(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false /* no worker configured */)
	rec := &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = rec

	workingDir := absWorkingDir(t, env.workingDir)
	scope := newFolderScope(t, workingDir, permission.FileOpRead)

	realPath := filepath.Join(workingDir, "real-content.txt")
	require.NoError(t, os.WriteFile(realPath, []byte("hello from the real disk"), 0o644))

	fakePath := filepath.Join(workingDir, "fake-only.txt")
	fake := newFakeDiskProvider(map[string]string{fakePath: "hello from the fake disk"})

	sessFake, err := coord.sessions.Create(t.Context(), "disk-provider-session")
	require.NoError(t, err)
	sessPlain, err := coord.sessions.Create(t.Context(), "plain-disk-session")
	require.NoError(t, err)

	ctxFake := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope, DiskProvider: fake})
	ctxPlain := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	assertFakeRead := func() {
		pinned, err := coord.resolveSessionModels(ctxFake, sessFake.ID)
		require.NoError(t, err)
		require.NotNil(t, pinned.tools, "the provider-carrying call must get a pinned toolset")
		resp := runFSRead(t, fsReadToolFrom(t, pinned.tools), fakePath)
		assert.False(t, resp.IsError, "content: %s", resp.Content)
		assert.Contains(t, resp.Content, "hello from the fake disk")
	}
	assertPlainRead := func() {
		pinned, err := coord.resolveSessionModels(ctxPlain, sessPlain.ID)
		require.NoError(t, err)
		require.NotNil(t, pinned.tools, "the plain call must get a pinned toolset")
		resp := runFSRead(t, fsReadToolFrom(t, pinned.tools), realPath)
		assert.False(t, resp.IsError, "content: %s", resp.Content)
		assert.Contains(t, resp.Content, "hello from the real disk")
	}

	// Order 1: resolve the provider-carrying session first, then the
	// plain one.
	assertFakeRead()
	assertPlainRead()

	// Order 2 (same two sessions): resolve the plain one first this
	// time — proves no bleed depends on which call resolves first.
	assertPlainRead()
	assertFakeRead()

	// THE KEY ASSERTION: resolveSessionModels must never publish to the
	// shared agent (R3-1).
	names, modelCalls := rec.snapshot()
	assert.Empty(t, names, "resolveSessionModels must never SetTools the shared currentAgent")
	assert.Zero(t, modelCalls, "resolveSessionModels must never SetModels the shared currentAgent")
}

// TestUpdateModels_NeverPublishesCallerDiskProvider pins the global-publish
// contract: UpdateModels calls withoutCallOptions before buildTools, so a
// single call's DiskProvider must never decide what the shared,
// published toolset resolves against.
//
// The zero-value FolderScope UpdateModels' stripped context leaves in
// place denies fs_read's read unconditionally (see this file's header
// comment), so a successful read cannot be the proof here. Instead: the
// fake records every method call it receives, so if buildTools had
// (incorrectly) captured it, running the published fs_read against a
// real, existing path would still invoke the fake's Stat/EvalSymlinks
// during path resolution (which runs BEFORE the scope check denies the
// call). Proving the fake recorded ZERO calls is proof the published
// tool's captured disk is the real one, not the caller's fake.
func TestUpdateModels_NeverPublishesCallerDiskProvider(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	rec := &diskProviderPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = rec

	realPath := filepath.Join(env.workingDir, "update-models-real.txt")
	require.NoError(t, os.WriteFile(realPath, []byte("real disk content"), 0o644))

	fake := newFakeDiskProvider(nil)
	ctx := WithCallOptions(t.Context(), &CallOptions{DiskProvider: fake})
	require.NoError(t, coord.UpdateModels(ctx))

	published, modelCalls := rec.snapshot()
	require.Len(t, published, 1, "UpdateModels must publish exactly one global toolset")
	assert.Equal(t, 1, modelCalls)

	resp := runFSRead(t, fsReadToolFrom(t, published[0]), realPath)
	assert.True(t, resp.IsError, "the global toolset's fs_read has a zero FolderScope and must deny every read")
	assert.Equal(t, 0, fake.callCount(),
		"the caller's DiskProvider must never be reached by the globally published toolset")
}

// TestBuildTools_SubAgentInheritsCallerDiskProvider proves buildAgent
// registers its async tool build (readyWg) with the CALLER's context, so
// a plain sub-agent built underneath a DiskProvider-carrying call also
// resolves its fs_read against that same provider — mirroring how the
// FolderScope tests already prove sub-agent inheritance
// (TestFolderScope_PlainSubAgentGetsGrantedReadSideFsTools).
func TestBuildTools_SubAgentInheritsCallerDiskProvider(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "task agent must be configured")

	workingDir := absWorkingDir(t, env.workingDir)
	scope := newFolderScope(t, workingDir, permission.FileOpRead)
	fakePath := filepath.Join(workingDir, "sub-agent-fake.txt")
	fake := newFakeDiskProvider(map[string]string{fakePath: "sub-agent fake content"})

	ctxProvider := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope, DiskProvider: fake})

	tp, err := taskPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	subAgent, err := coord.buildAgent(ctxProvider, tp, taskCfg, true)
	require.NoError(t, err)
	require.NoError(t, coord.readyWg.Wait(), "the sub-agent's async build must succeed")

	sa, ok := subAgent.(*sessionAgent)
	require.True(t, ok, "buildAgent must return a *sessionAgent")

	resp := runFSRead(t, fsReadToolFrom(t, sa.tools.Copy()), fakePath)
	assert.False(t, resp.IsError, "content: %s", resp.Content)
	assert.Contains(t, resp.Content, "sub-agent fake content")

	// Control: the same sub-agent build WITHOUT a DiskProvider (but with
	// the same scope, so the read is not denied for an unrelated reason)
	// must NOT see the fake's content — proving the match above is
	// attributable to the caller's provider, not some default fallback
	// that always reads the fake.
	realPath := filepath.Join(workingDir, "sub-agent-real.txt")
	require.NoError(t, os.WriteFile(realPath, []byte("sub-agent real content"), 0o644))
	ctxPlain := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})
	subAgentPlain, err := coord.buildAgent(ctxPlain, tp, taskCfg, true)
	require.NoError(t, err)
	require.NoError(t, coord.readyWg.Wait())
	saPlain := subAgentPlain.(*sessionAgent)
	respPlain := runFSRead(t, fsReadToolFrom(t, saPlain.tools.Copy()), realPath)
	assert.False(t, respPlain.IsError, "content: %s", respPlain.Content)
	assert.Contains(t, respPlain.Content, "sub-agent real content")
}

// TestBuildTools_NilDiskProviderKeepsRealDisk proves a nil
// CallOptions.DiskProvider resolves to real-disk behavior end to end
// through buildTools, exactly like every pre-#858 caller: nil is not a
// separate code path, it is diskOrOS's normal normalisation.
func TestBuildTools_NilDiskProviderKeepsRealDisk(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	cfgSnap, _ := coord.cfg.Snapshot()

	workingDir := absWorkingDir(t, env.workingDir)
	scope := newFolderScope(t, workingDir, permission.FileOpRead)
	realPath := filepath.Join(workingDir, "nil-provider-real.txt")
	require.NoError(t, os.WriteFile(realPath, []byte("nil provider still reads the real disk"), 0o644))

	// DiskProvider left at its zero value (nil) deliberately.
	ctx := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	toolset := mustBuildTools(t, coord, ctx, cfgSnap, coderCfg, false)
	resp := runFSRead(t, fsReadToolFrom(t, toolset), realPath)
	assert.False(t, resp.IsError, "content: %s", resp.Content)
	assert.Contains(t, resp.Content, "nil provider still reads the real disk")
}
