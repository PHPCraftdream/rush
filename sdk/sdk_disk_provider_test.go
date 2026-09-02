package sdk_test

// SDK-level tests for RunOverrides.DiskProvider (#859). Each test drives
// a real sdk.Client against a scripted openai-compat provider (httptest
// SSE server, the sdk_folder_scope_test.go pattern) and asserts on REAL
// tool results and the REAL filesystem:
//
//   - a disk-provider run's fs_write then fs_read round-trips through the
//     caller's fake filesystem WITHOUT ever touching the real disk;
//   - the negative control: the same script with no DiskProvider override
//     writes to the REAL disk;
//   - a DiskProvider without any FolderScopes fails Run BEFORE any
//     provider traffic (one of #859's two hard-error validations);
//   - a disk-provider run and a real-disk run proceed concurrently on two
//     DIFFERENT sessions with no cross-contamination in either direction.

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// fakeSDKFileInfo is the minimal fs.FileInfo fakeSDKDisk.Stat returns for
// a regular file or a directory.
type fakeSDKFileInfo struct {
	name  string
	isDir bool
}

func (fi fakeSDKFileInfo) Name() string       { return fi.name }
func (fi fakeSDKFileInfo) Size() int64        { return 0 }
func (fi fakeSDKFileInfo) Mode() fs.FileMode  { return 0o644 }
func (fi fakeSDKFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeSDKFileInfo) IsDir() bool        { return fi.isDir }
func (fi fakeSDKFileInfo) Sys() any           { return nil }

// fakeSDKDisk is a minimal in-memory sdk.DiskProvider, built entirely
// from public sdk.* aliases (no internal/agent/tools import) to prove the
// SDK surface is directly implementable by a host with zero conversion
// code.
//
// dirs tracks directories that exist in this virtual filesystem —
// needed because fs_write's ancestor scope-escape check (design doc
// §1.3/§4.2: "every ancestor it is about to create has already been
// scope-checked") walks UP past any ancestor Stat reports missing. A
// fake that only tracks files would report a pre-existing parent
// directory (e.g. a granted scope's root) as missing too, so the walk
// would climb one level further to the (unscoped) grandparent and the
// scope-escape check would legitimately — but here, spuriously — deny
// it. mkdir mirrors the real disk's pre-existing directory structure so
// the walk stops at exactly the same place OSDisk's would.
type fakeSDKDisk struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]bool
}

var _ sdk.DiskProvider = (*fakeSDKDisk)(nil)

func newFakeSDKDisk() *fakeSDKDisk {
	return &fakeSDKDisk{files: make(map[string]string), dirs: make(map[string]bool)}
}

// mkdir marks dir, and every ancestor up to the root, as an existing
// directory — mirroring what a real os.MkdirAll(dir, ...) would leave
// behind, and what tests use to seed a directory the real-disk sibling
// scenario already pre-created (e.g. via os.MkdirAll in the test body).
func (f *fakeSDKDisk) mkdir(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		f.dirs[dir] = true
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func (f *fakeSDKDisk) has(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[name]
	return content, ok
}

func (f *fakeSDKDisk) isDir(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[name]
}

func (f *fakeSDKDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	if _, ok := f.has(name); ok {
		return fakeSDKFileInfo{name: filepath.Base(name)}, nil
	}
	if f.isDir(name) {
		return fakeSDKFileInfo{name: filepath.Base(name), isDir: true}, nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeSDKDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	return filepath.Clean(name), nil
}

func (f *fakeSDKDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	content, ok := f.has(name)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (f *fakeSDKDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	content, ok := f.has(name)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

func (f *fakeSDKDisk) MkdirAll(_ context.Context, dir string, _ fs.FileMode) error {
	f.mkdir(dir)
	return nil
}

func (f *fakeSDKDisk) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	f.mu.Lock()
	f.files[name] = string(data)
	f.mu.Unlock()
	return nil
}

func (f *fakeSDKDisk) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	delete(f.files, name)
	f.mu.Unlock()
	return nil
}

func (f *fakeSDKDisk) List(_ context.Context, _ sdk.DiskListRequest) (sdk.DiskListResult, error) {
	return sdk.DiskListResult{}, nil
}

func (f *fakeSDKDisk) Find(_ context.Context, _ sdk.DiskFindRequest) (sdk.DiskFindResult, error) {
	return sdk.DiskFindResult{}, nil
}

func (f *fakeSDKDisk) Search(_ context.Context, _ sdk.DiskSearchRequest) (sdk.DiskSearchResult, error) {
	return sdk.DiskSearchResult{}, nil
}

// diskProviderWriteThenReadServer scripts a three-round turn: round 1
// hands back an fs_write tool call, round 2 (recognizing call_write in
// the request body) hands back an fs_read tool call for the same path,
// round 3 (recognizing call_read) ends the turn with marker text.
func diskProviderWriteThenReadServer(t *testing.T, relPath, content, marker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(`"call_read"`)):
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"call_write"`)):
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_read", "fs_read", map[string]any{
					"items": []map[string]any{{"path": relPath}},
				}),
				finishChunk("probe", "tool_calls"),
			})
		default:
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_write", "fs_write", map[string]any{
					"items": []map[string]any{{"path": relPath, "content": content}},
				}),
				finishChunk("probe", "tool_calls"),
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSDKRunWithDiskProviderRoundTripsWithoutTouchingRealDisk is #859's
// headline test: round 1 the model calls fs_write, round 2 it calls
// fs_read on the same path and the tool result carries the content back,
// round 3 it stops. The content must come back through fs_read, the fake
// must hold the file, and the real working directory must stay empty.
func TestSDKRunWithDiskProviderRoundTripsWithoutTouchingRealDisk(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))

	const (
		relPath = "scoped/virtual.txt"
		content = "FAKE_DISK_CONTENT_10e"
		marker  = "DISK_PROVIDER_ROUNDTRIP_OK"
	)
	fake := newFakeSDKDisk()
	// Mirror the real-disk sibling test's pre-created "scoped" directory
	// in the fake, so fs_write's ancestor scope-escape walk stops at
	// "scoped" exactly like it does against the real disk (see the
	// fakeSDKDisk.dirs doc comment).
	fake.mkdir(filepath.Join(workDir, "scoped"))
	srv := diskProviderWriteThenReadServer(t, relPath, content, marker)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-disk-provider-roundtrip"
	seedTenantSession(t, workDir, sessionID)

	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "write then read",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}},
			},
			DiskProvider: fake,
		},
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker, res.FinalText)

	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	readResult := fsToolResultOf(t, msgs, "fs_read")
	require.False(t, readResult.IsError, "content %q", readResult.Content)
	require.Contains(t, readResult.Content, content,
		"the fs_read result must carry the content back through the fake, not the real disk")

	absPath := filepath.Join(workDir, filepath.FromSlash(relPath))
	fakeContent, ok := fake.has(absPath)
	require.True(t, ok, "the fake disk must hold the written file at %s", absPath)
	require.Equal(t, content, fakeContent)

	// The real working directory must never see the write.
	entries, err := os.ReadDir(filepath.Join(workDir, "scoped"))
	require.NoError(t, err)
	require.Empty(t, entries, "the real disk must never be touched by a disk-provider run")
}

// TestSDKRunWithoutDiskProviderWritesToTheRealDisk is the negative
// control: the exact same script with no DiskProvider override lands the
// write on the real filesystem.
func TestSDKRunWithoutDiskProviderWritesToTheRealDisk(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))

	const (
		relPath = "scoped/real.txt"
		content = "REAL_DISK_CONTENT_10f"
		marker  = "DISK_PROVIDER_CONTROL_OK"
	)
	srv := diskProviderWriteThenReadServer(t, relPath, content, marker)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-disk-provider-control"
	seedTenantSession(t, workDir, sessionID)

	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "write then read, real disk",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}},
			},
			// DiskProvider deliberately left nil.
		},
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker, res.FinalText)

	got, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(relPath)))
	require.NoError(t, err, "without a DiskProvider override, the write must land on the real disk")
	require.Equal(t, content, string(got))
}

// TestSDKRunDiskProviderWithoutFolderScopesFailsBeforeProviderTraffic
// mirrors TestSDKRunInvalidFolderScopeEntryFailsBeforeProviderTraffic:
// one of #859's two mandatory hard-error validations — a DiskProvider set
// with an EMPTY FolderScopes fails the whole run before any provider
// traffic.
func TestSDKRunDiskProviderWithoutFolderScopesFailsBeforeProviderTraffic(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		sseChunks(t, w, []map[string]any{textChunk("probe", "MUST_NOT_BE_HIT"), finishChunk("probe", "stop")})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-disk-provider-no-scope"
	seedTenantSession(t, workDir, sessionID)

	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "this must never reach the provider",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			// FolderScopes deliberately left empty.
			DiskProvider: newFakeSDKDisk(),
		},
	})
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "disk provider requires at least one folder scope")
	require.EqualValues(t, 0, hits.Load(), "the provider must not be contacted when the disk-provider/scope precondition fails")
}

// TestSDKRunDiskProviderAndRealDiskRunConcurrentSessionsIsolated mirrors
// TestSDKRunScopedAndUnscopedConcurrentSessionsIsolated: a disk-provider
// run and a real-disk run proceed concurrently on two DIFFERENT sessions
// of one client with no cross-contamination in either direction.
func TestSDKRunDiskProviderAndRealDiskRunConcurrentSessionsIsolated(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	realTarget := filepath.Join(workDir, "scoped", "shared_target.txt")
	require.NoError(t, os.WriteFile(realTarget, []byte("REAL_ISOLATION_TARGET_10g\n"), 0o644))

	const (
		sessionFake  = "sdk-disk-provider-iso-fake"
		sessionReal  = "sdk-disk-provider-iso-real"
		fakeMarker   = "FAKE_ISO_OK"
		realMarker   = "REAL_ISO_OK"
		relPath      = "scoped/shared_target.txt"
		fakeContentV = "FAKE_ISOLATION_TARGET_10h"
	)

	fake := newFakeSDKDisk()
	fake.mu.Lock()
	fake.files[filepath.Join(workDir, filepath.FromSlash(relPath))] = fakeContentV
	fake.mu.Unlock()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits.Add(1)
		if bytes.Contains(body, []byte(`"call_1"`)) {
			marker := fakeMarker
			if bytes.Contains(body, []byte(`"real disk probe"`)) {
				marker = realMarker
			}
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
			return
		}
		sseChunks(t, w, []map[string]any{
			toolCallChunkNamed("probe", "call_1", "fs_read", map[string]any{
				"items": []map[string]any{{"path": relPath}},
			}),
			finishChunk("probe", "tool_calls"),
		})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	seedTenantSession(t, workDir, sessionFake)
	seedTenantSession(t, workDir, sessionReal)

	var (
		wg               sync.WaitGroup
		startCh          = make(chan struct{})
		resFake, resReal *sdk.RunResult
		errFake, errReal error
		bufFake, bufReal bytes.Buffer
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startCh
		resFake, errFake = client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "fake disk probe",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionFake,
			Stdout:            &bufFake,
			HideSpinner:       true,
			Overrides: sdk.RunOverrides{
				FolderScopes: []sdk.FolderScope{
					{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpRead}},
				},
				DiskProvider: fake,
			},
		})
	}()
	go func() {
		defer wg.Done()
		<-startCh
		resReal, errReal = client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "real disk probe",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionReal,
			Stdout:            &bufReal,
			HideSpinner:       true,
			Overrides: sdk.RunOverrides{
				FolderScopes: []sdk.FolderScope{
					{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpRead}},
				},
				// DiskProvider deliberately left nil: this session reads
				// the REAL disk.
			},
		})
	}()
	close(startCh)
	wg.Wait()

	require.NoError(t, errFake, "fake-disk output %q", bufFake.String())
	require.NoError(t, errReal, "real-disk output %q", bufReal.String())
	require.Equal(t, "end_turn", resFake.ExitReason)
	require.Equal(t, "end_turn", resReal.ExitReason)
	require.Equal(t, fakeMarker, resFake.FinalText)
	require.Equal(t, realMarker, resReal.FinalText)

	msgsFake, err := client.Messages(context.Background(), sessionFake)
	require.NoError(t, err)
	fakeResult := fsToolResultOf(t, msgsFake, "fs_read")
	require.False(t, fakeResult.IsError, "content %q", fakeResult.Content)
	require.Contains(t, fakeResult.Content, fakeContentV,
		"the disk-provider session must read the FAKE content")
	require.NotContains(t, fakeResult.Content, "REAL_ISOLATION_TARGET_10g",
		"the disk-provider session must never see the real disk's content")

	msgsReal, err := client.Messages(context.Background(), sessionReal)
	require.NoError(t, err)
	realResult := fsToolResultOf(t, msgsReal, "fs_read")
	require.False(t, realResult.IsError, "content %q", realResult.Content)
	require.Contains(t, realResult.Content, "REAL_ISOLATION_TARGET_10g",
		"the plain session must read the REAL disk's content")
	require.NotContains(t, realResult.Content, fakeContentV,
		"the real-disk session must never see the fake's content")

	require.EqualValues(t, 4, hits.Load())
}
