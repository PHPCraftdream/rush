package sdk_test

// Regression tests for R6-1 (P0, SDK review round 6, 2026-09-03): an
// ephemeral sdk.ModeLibrary session (no Options.WorkingDir, in-memory
// database, nothing ever on disk) previously still handed the model
// every legacy host-disk tool (bash, write, edit, ...) and the fs_*
// family backed by the REAL OS filesystem, rooted at the synthesized
// sdk.LibraryVirtualRoot -- a string the OS still interprets as a real
// path (drive-rooted on Windows, an ordinary absolute path on Unix).
// This contradicted sdk/README.md's "no files touched" / "nothing ever
// touches disk" guarantee for the README's own minimal ephemeral example.
//
// The fix (sdk/library_mode.go, internal/agent/tools/fs_library_virtual_root.go):
//
//   - buildLibraryConfig disables every host-disk/command-execution tool
//     by default for an ephemeral session (libraryEphemeralDisabledTools);
//   - tools.resolveScopedPath refuses (fails closed, before the first real
//     disk.Stat) any resolution that would touch the REAL disk under the
//     sentinel, so a call that opts into RunOverrides.FolderScopes without
//     also supplying a DiskProvider cannot reach the real disk either;
//   - LibraryVirtualRoot's value is now computed per-OS to satisfy the
//     REAL, platform-native filepath.IsAbs everywhere, keeping
//     DiskProvider's "every path is absolute" contract intact even for a
//     caller who DOES supply their own virtual provider.
//
// Four tests below cover the review's three required regressions plus
// the existing opt-in path (FolderScopes + DiskProvider) staying usable.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// r6_1DangerousToolNames mirrors libraryEphemeralDisabledTools
// (sdk/library_mode.go): every host-disk/command-execution tool an
// ephemeral session must never hand the model by default.
var r6_1DangerousToolNames = []string{
	"bash", "run_command", "download",
	"edit", "multiedit", "glob", "grep", "ls", "view", "write",
	"fs_list", "fs_find", "fs_grep", "fs_read",
	"fs_write", "fs_replace", "fs_write_lines", "fs_delete",
}

// r6_1ToolRequest is the minimal shape needed to read the tool names an
// openai-compat chat-completion request offers the model.
type r6_1ToolRequest struct {
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// r6_1ToolNamesFromBody decodes one captured request body's offered tool
// names. Returns nil (not an error) for a body with no "tools" field at
// all (e.g. the title-generation agent, which is configured with none).
func r6_1ToolNamesFromBody(t *testing.T, body []byte) []string {
	t.Helper()
	var req r6_1ToolRequest
	require.NoError(t, json.Unmarshal(body, &req))
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

// r6_1CapturingServer records every request body it sees and replies
// with plain marker text (never a tool call) so a single round finishes
// the turn -- exactly enough to inspect what tools the FIRST real turn
// request offered.
type r6_1CapturingServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newR6_1CapturingServer(t *testing.T, marker string) *r6_1CapturingServer {
	t.Helper()
	cs := &r6_1CapturingServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.bodies = append(cs.bodies, body)
		cs.mu.Unlock()
		sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

// mainTurnBody returns the first captured body that is NOT the
// background session-title-generation request (which carries no tools
// and a "Generate a concise title" prompt -- see r5_6_library_virtual_root_test.go).
func (cs *r6_1CapturingServer) mainTurnBody(t *testing.T) []byte {
	t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, b := range cs.bodies {
		if !bytes.Contains(b, []byte("Generate a concise title")) {
			return b
		}
	}
	t.Fatal("no captured request body outside of title generation")
	return nil
}

// r6_1PathSnapshot records the observable state of one host path so a
// later comparison can prove the path was not created, deleted, or
// modified in between -- whether or not it already existed beforehand.
type r6_1PathSnapshot struct {
	path    string
	statErr string // "" when os.Stat succeeded
	mode    fs.FileMode
	size    int64
	modTime time.Time
	entries []string // sorted immediate child names, when a directory
}

// r6_1SnapshotPath captures the current state of root. A stat error --
// including ones Go does not classify as NotExist, e.g. Windows
// ERROR_NOT_READY for a media-less removable drive -- is recorded as
// part of the state instead of failing the snapshot: the sentinel root
// may sit on any kind of host path, and the only property the snapshot
// needs is that an unchanged path compares equal to itself.
func r6_1SnapshotPath(t *testing.T, root string) r6_1PathSnapshot {
	t.Helper()
	snap := r6_1PathSnapshot{path: root}
	info, err := os.Stat(root)
	if err != nil {
		snap.statErr = err.Error()
		return snap
	}
	snap.mode, snap.size, snap.modTime = info.Mode(), info.Size(), info.ModTime()
	if info.IsDir() {
		entries, err := os.ReadDir(root)
		require.NoError(t, err, "snapshot of %q: ReadDir failed", root)
		for _, e := range entries {
			snap.entries = append(snap.entries, e.Name())
		}
		sort.Strings(snap.entries)
	}
	return snap
}

// r6_1RequirePathUnchanged fails the test when the path's state differs
// in any way from the earlier snapshot: existence, mode, size, mtime,
// or immediate directory entries.
func r6_1RequirePathUnchanged(t *testing.T, before r6_1PathSnapshot) {
	t.Helper()
	require.Equal(t, before, r6_1SnapshotPath(t, before.path),
		"the sentinel root %q must not be created, deleted, or modified by this test", before.path)
}

// TestSDKLibraryModeEphemeralDefaultToolsetExcludesRealDiskAndCommandTools
// is R6-1's headline regression: the README's minimal ephemeral example
// (ModeLibrary, no WorkingDir, no DiskProvider) must never offer the
// model any real-disk or command-execution tool. Inspects the ACTUAL
// tool schema sent to the provider -- not merely one probed write
// attempt -- exactly what the round-6 review found the R5-6 test did not
// do.
func TestSDKLibraryModeEphemeralDefaultToolsetExcludesRealDiskAndCommandTools(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const marker = "R6_1_TOOLSET_PROBE_OK"
	srv := newR6_1CapturingServer(t, marker)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.srv.URL, "sk-library-secret"),
		// WorkingDir and every RunOverrides field deliberately left at
		// their zero value: the README's own minimal shape.
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:      "reply with the marker text and nothing else",
		Mode:        sdk.RunModeJSON,
		Stdout:      &buf,
		HideSpinner: true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, marker, res.FinalText)

	names := r6_1ToolNamesFromBody(t, srv.mainTurnBody(t))
	require.NotEmpty(t, names, "the main turn must offer SOME tools (a broken/empty toolset would trivially pass a naive absence check)")

	for _, dangerous := range r6_1DangerousToolNames {
		require.NotContains(t, names, dangerous,
			"an ephemeral library-mode session must never offer %q by default", dangerous)
	}

	// Positive control: a non-disk, non-command tool the default set
	// still legitimately offers, proving the filter is targeted rather
	// than an accidental empty toolset.
	require.Contains(t, names, "agent",
		"the delegation tool is not a real-disk/command tool and must still be offered")
}

// TestSDKLibraryModeEphemeralWriteAndBashAttemptsNeverTouchRealDisk
// exercises actual write and command tool-call attempts against the
// default (no-DiskProvider) ephemeral configuration: the model asks for
// "write" against a caller-chosen absolute host path, then (once refused)
// asks for "bash". Both must be refused gracefully (the tools are simply
// absent from the built toolset) and, crucially, nothing must ever be
// created on the real disk -- neither at the caller-selected host path
// nor anywhere under the OS interpretation of the sentinel root.
func TestSDKLibraryModeEphemeralWriteAndBashAttemptsNeverTouchRealDisk(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const marker = "R6_1_WRITE_BASH_DENIED_OK"
	hostTarget := filepath.Join(t.TempDir(), "should-never-be-created.txt")

	// Capture the sentinel root's state BEFORE any client/provider
	// activity, so the end-of-test comparison can prove this run
	// touched nothing under it whether or not the path already
	// exists on this host (see the tail assertion below).
	sentinelBefore := r6_1SnapshotPath(t, sdk.LibraryVirtualRoot())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("Generate a concise title")):
			sseChunks(t, w, []map[string]any{textChunk("probe", "title"), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"call_bash"`)):
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"call_write"`)):
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_bash", "bash", map[string]any{
					"command": "echo should-never-run", "description": "R6-1 probe",
				}),
				finishChunk("probe", "tool_calls"),
			})
		default:
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_write", "write", map[string]any{
					"file_path": hostTarget, "content": "SHOULD_NEVER_BE_WRITTEN",
				}),
				finishChunk("probe", "tool_calls"),
			})
		}
	}))
	t.Cleanup(srv.Close)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	const sessionID = "sdk-r6-1-write-bash-denied"
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "attempt write then bash",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker, res.FinalText)

	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	writeResult := fsToolResultOf(t, msgs, "write")
	require.True(t, writeResult.IsError, "content %q", writeResult.Content)
	require.Contains(t, writeResult.Content, "tool not found",
		"an unoffered tool must be refused as not-found, never executed")
	bashResult := fsToolResultOf(t, msgs, "bash")
	require.True(t, bashResult.IsError, "content %q", bashResult.Content)
	require.Contains(t, bashResult.Content, "tool not found")

	// Nothing was ever created at the caller-selected absolute host path.
	_, statErr := os.Stat(hostTarget)
	require.True(t, os.IsNotExist(statErr), "the write attempt must never reach the real disk (%s)", hostTarget)

	// Nothing under the OS interpretation of the sentinel root was
	// created, deleted, or modified either. This is deliberately a
	// before/after STATE comparison, not an existence assertion: a
	// real host collision with this exact path (a mapped K: drive on
	// Windows, a pre-existing /rush-library-mode-root on Unix) is
	// explicitly tolerated by the production guard --
	// internal/agent/tools/fs_library_virtual_root.go refuses every
	// real-disk operation resolving under the sentinel BEFORE the
	// first disk.Stat, whether or not something real is already
	// there -- so asserting the path's absence would encode a
	// host-topology assumption that fails on collision machines even
	// though production is correct (R14-5, same class as the two CI
	// failures fixed in 73878311). What proves the security property
	// is "this test's run changed nothing under the sentinel",
	// whichever state the host started in.
	r6_1RequirePathUnchanged(t, sentinelBefore)
}

// TestSDKLibraryModeEphemeralDownloadAttemptNeverTouchesRealDisk closes a
// gap the review's own list (and this fix's first draft) missed: "download"
// (internal/agent/tools/download.go) is a legacy tool with no DiskProvider
// parameter at all -- it writes straight to the real OS filesystem via
// c.cfg.WorkingDir() and filepathext.SmartJoin, the exact same shape of bug
// as "write". It is also not in folderScopeOpForTool
// (internal/agent/coordinator_tools.go), so unlike fs_* it has no
// FolderScopes opt-in and must stay hard-denied unconditionally, same as
// bash/run_command.
func TestSDKLibraryModeEphemeralDownloadAttemptNeverTouchesRealDisk(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const marker = "R6_1_DOWNLOAD_DENIED_OK"
	hostTarget := filepath.Join(t.TempDir(), "should-never-be-downloaded.txt")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("Generate a concise title")):
			sseChunks(t, w, []map[string]any{textChunk("probe", "title"), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"call_download"`)):
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		default:
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_download", "download", map[string]any{
					"url": "https://example.invalid/payload", "file_path": hostTarget,
				}),
				finishChunk("probe", "tool_calls"),
			})
		}
	}))
	t.Cleanup(srv.Close)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	const sessionID = "sdk-r6-1-download-denied"
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "attempt download",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker, res.FinalText)

	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	downloadResult := fsToolResultOf(t, msgs, "download")
	require.True(t, downloadResult.IsError, "content %q", downloadResult.Content)
	require.Contains(t, downloadResult.Content, "tool not found",
		"an unoffered tool must be refused as not-found, never executed")

	_, statErr := os.Stat(hostTarget)
	require.True(t, os.IsNotExist(statErr), "the download attempt must never reach the real disk (%s)", hostTarget)
}

// TestSDKLibraryModeEphemeralFolderScopeWithoutDiskProviderFailsClosed
// is the required fail-closed backstop: a caller who opts an ephemeral
// session into RunOverrides.FolderScopes WITHOUT also supplying a
// DiskProvider would, pre-fix, get fs_* tools backed by the REAL OS disk
// rooted at the sentinel. Run must now refuse the whole call before any
// provider traffic, exactly like the sibling "invalid folder scopes" /
// "disk provider requires a folder scope" hard errors.
//
// R14-2 (SDK review round 14) moved this refusal even earlier: the
// no-real-workspace precondition now fails the whole call before any
// canonicalization at all, for RELATIVE and ABSOLUTE scope entries
// alike; the R6-1 sentinel guard remains as defense-in-depth for
// direct internal callers.
func TestSDKLibraryModeEphemeralFolderScopeWithoutDiskProviderFailsClosed(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		sseChunks(t, w, []map[string]any{textChunk("probe", "MUST_NOT_BE_HIT"), finishChunk("probe", "stop")})
	}))
	t.Cleanup(srv.Close)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:      "this must never reach the provider",
		Mode:        sdk.RunModeJSON,
		HideSpinner: true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}},
			},
			// DiskProvider deliberately left nil: the exact residual gap
			// R6-1 flagged (a scope-only opt-in normalizes fs_* to the
			// real disk, rooted at the ephemeral sentinel).
		},
	})
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "require a custom DiskProvider",
		"the failure must come from the R14-2 no-real-workspace precondition, not some unrelated error")
	require.EqualValues(t, 0, hits.Load(), "the provider must not be contacted when this precondition fails")
}

// recordingDisk wraps fakeSDKDisk and records every path handed to it,
// so a test can assert the documented DiskProvider contract
// (internal/agent/tools/fs_provider.go: "every method receives an
// absolute path") holds for every single call, not just the round-trip's
// final content.
type recordingDisk struct {
	inner *fakeSDKDisk
	mu    sync.Mutex
	paths []string
}

var _ sdk.DiskProvider = (*recordingDisk)(nil)

func newRecordingDisk(inner *fakeSDKDisk) *recordingDisk {
	return &recordingDisk{inner: inner}
}

func (r *recordingDisk) record(p string) {
	r.mu.Lock()
	r.paths = append(r.paths, p)
	r.mu.Unlock()
}

func (r *recordingDisk) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

func (r *recordingDisk) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	r.record(name)
	return r.inner.Stat(ctx, name)
}

func (r *recordingDisk) EvalSymlinks(ctx context.Context, name string) (string, error) {
	r.record(name)
	return r.inner.EvalSymlinks(ctx, name)
}

func (r *recordingDisk) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	r.record(name)
	return r.inner.Open(ctx, name)
}

func (r *recordingDisk) ReadFile(ctx context.Context, name string) ([]byte, error) {
	r.record(name)
	return r.inner.ReadFile(ctx, name)
}

func (r *recordingDisk) MkdirAll(ctx context.Context, dir string, perm fs.FileMode) error {
	r.record(dir)
	return r.inner.MkdirAll(ctx, dir, perm)
}

func (r *recordingDisk) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	r.record(name)
	return r.inner.WriteFile(ctx, name, data, perm)
}

func (r *recordingDisk) Remove(ctx context.Context, name string) error {
	r.record(name)
	return r.inner.Remove(ctx, name)
}

func (r *recordingDisk) List(ctx context.Context, req sdk.DiskListRequest) (sdk.DiskListResult, error) {
	r.record(req.Dir)
	return r.inner.List(ctx, req)
}

func (r *recordingDisk) Find(ctx context.Context, req sdk.DiskFindRequest) (sdk.DiskFindResult, error) {
	r.record(req.Dir)
	return r.inner.Find(ctx, req)
}

func (r *recordingDisk) Search(ctx context.Context, req sdk.DiskSearchRequest) (sdk.DiskSearchResult, error) {
	r.record(req.Dir)
	return r.inner.Search(ctx, req)
}

// TestSDKLibraryModeEphemeralCustomDiskProviderPathsAreAlwaysAbsolute is
// the third required regression: on Windows (this machine IS Windows),
// every path this mode ever delivers to a custom DiskProvider must
// satisfy the REAL, platform-native filepath.IsAbs -- the exact contract
// (internal/agent/tools/fs_provider.go) the R5-6 sentinel literal
// violated on Windows (no drive letter). Reuses the r5_6 write-then-read
// round trip, wrapped to record every path the fake actually received.
func TestSDKLibraryModeEphemeralCustomDiskProviderPathsAreAlwaysAbsolute(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const (
		relPath = "scoped/virtual.txt"
		content = "R6_1_ABS_PATH_CONTENT"
		marker  = "R6_1_ABS_PATH_ROUNDTRIP_OK"
	)

	fake := newFakeSDKDisk()
	scopedDir := filepath.Clean(filepath.Join(sdk.LibraryVirtualRoot(), "scoped"))
	fake.mkdir(scopedDir)
	rec := newRecordingDisk(fake)

	srv := libraryVirtualRootRoundTripServer(t, relPath, content, marker)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	const sessionID = "sdk-r6-1-abs-path"
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "write then read against the virtual root",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}},
			},
			DiskProvider: rec,
		},
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker, res.FinalText)

	paths := rec.recorded()
	require.NotEmpty(t, paths, "the custom DiskProvider must actually have been called")
	for _, p := range paths {
		require.True(t, filepath.IsAbs(p),
			"every path delivered to a custom DiskProvider in ephemeral library mode must be absolute (got %q)", p)
	}
}
