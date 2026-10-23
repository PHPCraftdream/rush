package sdk_test

// R12-H2 chunk D, acceptance items 1, 2, 3 and 17 (design §3 row
// "Supported SDK/shapes/isolation").
//
// Entry point: the PUBLIC SDK run path — sdk.Open + Client.Run with
// RunOverrides.DiskProvider — driving a scripted openai-compat model
// (httptest SSE): round 1 fs_write, round 2 fs_read of the SAME path in the
// SAME turn (the first root completed and disposed; the read reuses the
// SAME pinned toolset — the construction-scope survival oracle), round 3
// stop. The default batch runtime is selected.
//
// Oracles:
//   - provider contents read back correctly: the fake (map-backed and
//     NON-comparable: its dynamic type carries a slice field) holds the
//     written content at the EXACT absolute target, preseeded with sentinel
//     content on the real disk, and the sentinel is UNCHANGED after the run
//     (the virtual provider never touched OS disk);
//   - the fs_read result carries the provider's content back;
//   - a second Client with IDENTICAL relative paths on a different working
//     directory is isolated: no cross-client routing, each read returns its
//     own client's content;
//   - an explicit OSDiskProvider is supported and lands the write on the
//     real disk at the exact absolute target;
//   - a typed-nil provider is rejected before any provider traffic.
//
// Item 17 (external sdk module): the testdata/embedder module exercises the
// public surface (sdk.Open/Run/RunRequest/close + the error sentinels); the
// public API exercised there is unchanged by this chunk (verified by
// re-reading embed_test.go against sdk.go), so it is left untouched.
//
// Production mutation caught: a construction scope that deleted the pinned
// toolset after the first root's dispose would make the same-turn fs_read
// fail with an unavailable dependency; routing by path equality instead of
// per-call binding would cross-contaminate the two clients; accepting a
// typed-nil provider would silently select OS disk.

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
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// shapesDisk is a map-backed, NON-COMPARABLE sdk.DiskProvider: the dynamic
// type carries a slice field, so no code path can fall back to interface
// value comparison; the provider instance identity must be carried by the
// binding handle.
type shapesDisk struct {
	files map[string]string
	dirs  map[string]bool
	tags  []string
}

var _ sdk.DiskProvider = shapesDisk{}

func newShapesDisk() shapesDisk {
	return shapesDisk{files: make(map[string]string), dirs: make(map[string]bool), tags: []string{"shapes"}}
}

func (d shapesDisk) mkdirAll(dir string) {
	for {
		d.dirs[dir] = true
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func (d shapesDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	if _, ok := d.files[name]; ok {
		return fakeSDKFileInfo{name: filepath.Base(name)}, nil
	}
	if d.dirs[name] {
		return fakeSDKFileInfo{name: filepath.Base(name), isDir: true}, nil
	}
	return nil, fs.ErrNotExist
}

func (d shapesDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	return filepath.Clean(name), nil
}

func (d shapesDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	content, ok := d.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (d shapesDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	content, ok := d.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

func (d shapesDisk) MkdirAll(_ context.Context, dir string, _ fs.FileMode) error {
	d.mkdirAll(dir)
	return nil
}

func (d shapesDisk) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	d.files[name] = string(data)
	return nil
}

func (d shapesDisk) Remove(_ context.Context, name string) error {
	delete(d.files, name)
	return nil
}

func (d shapesDisk) List(_ context.Context, _ sdk.DiskListRequest) (sdk.DiskListResult, error) {
	return sdk.DiskListResult{}, nil
}

func (d shapesDisk) Find(_ context.Context, _ sdk.DiskFindRequest) (sdk.DiskFindResult, error) {
	return sdk.DiskFindResult{}, nil
}

func (d shapesDisk) Search(_ context.Context, _ sdk.DiskSearchRequest) (sdk.DiskSearchResult, error) {
	return sdk.DiskSearchResult{}, nil
}

func TestSdkProviderRunShapesIsolation(t *testing.T) {
	isolateGlobalConfig(t)
	unsetBatchRuntimeEnvForTest(t)

	const (
		relPath     = "scoped/shapes.txt"
		contentA    = "SHAPES_CLIENT_A_CONTENT"
		contentB    = "SHAPES_CLIENT_B_CONTENT"
		sentinel    = "SHAPES_OS_SENTINEL_DO_NOT_TOUCH"
		markerA     = "SHAPES_A_OK"
		markerB     = "SHAPES_B_OK"
		osPath      = "scoped/shapes-os.txt"
		osContent   = "SHAPES_OS_PROVIDER_CONTENT"
		osMarker    = "SHAPES_OS_OK"
		nilMarker   = "SHAPES_TYPED_NIL_MUST_NOT_RUN"
		readCall    = "shapes_call_read"
		writeCall   = "shapes_call_write"
		stopCall    = "shapes_call_stop"
		osWriteCall = "shapes_os_write"
		writePrompt = "shapes write then read"
		isoPromptA  = "shapes client A write then read"
	)

	// Two isolated workdirs, identical RELATIVE paths; the EXACT absolute
	// OS targets are preseeded with sentinel content.
	workA, workB := evalTempDir(t), evalTempDir(t)
	for _, dir := range []string{workA, workB} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "scoped"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, filepath.FromSlash(relPath)), []byte(sentinel), 0o644))
	}
	diskA, diskB := newShapesDisk(), newShapesDisk()
	// Seed the virtual parent dirs: the fs_write ancestor walk needs an
	// existing virtual "scoped" dir (mirrors sdk_disk_provider_test.go).
	diskA.mkdirAll(filepath.Join(workA, "scoped"))
	diskB.mkdirAll(filepath.Join(workB, "scoped"))

	// The scripted model: write → read (same turn) → stop; the iso clients
	// get per-client write calls, reads and markers.
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits.Add(1)
		// NOTE: continued-session requests carry PRIOR turns' call ids in
		// their history, so the prompt-identity branches MUST be tested
		// before the call-id branches.
		switch {
		case bytes.Contains(body, []byte("shapes typed nil")):
			sseChunks(t, w, []map[string]any{textChunk("probe", nilMarker), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"`+osWriteCall+`"`)):
			// The OS write's own RESULT is in the history: emit the stop
			// text carrying the os marker.
			sseChunks(t, w, []map[string]any{textChunk("probe", osMarker), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte("shapes os provider")):
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", osWriteCall, "fs_write", map[string]any{
					"items": []map[string]any{{"path": osPath, "content": osContent}},
				}),
				finishChunk("probe", "tool_calls"),
			})
		case bytes.Contains(body, []byte(`"`+readCall+`"`)):
			marker := markerA
			if bytes.Contains(body, []byte(isoPromptA)) {
				marker = markerB
			}
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"`+writeCall+`"`)):
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", readCall, "fs_read", map[string]any{
					"items": []map[string]any{{"path": relPath}},
				}),
				finishChunk("probe", "tool_calls"),
			})
		case bytes.Contains(body, []byte(`"`+stopCall+`"`)):
			sseChunks(t, w, []map[string]any{textChunk("probe", osMarker), finishChunk("probe", "stop")})
		default:
			content := contentA
			if bytes.Contains(body, []byte(isoPromptA)) {
				content = contentB
			}
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", writeCall, "fs_write", map[string]any{
					"items": []map[string]any{{"path": relPath, "content": content}},
				}),
				finishChunk("probe", "tool_calls"),
			})
		}
	}))
	t.Cleanup(server.Close)
	writeProbeRushJSON(t, workA, server.URL)
	writeProbeRushJSON(t, workB, server.URL)

	clientA, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workA})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientA.Close() })
	clientB, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workB})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientB.Close() })
	const sessionA = "sdk-shapes-a"
	const sessionB = "sdk-shapes-b"
	seedTenantSession(t, workA, sessionA)
	seedTenantSession(t, workB, sessionB)

	// Client A: the full shapes run — fs_write (root completes and
	// disposes), then fs_read in the SAME turn on the SAME pinned toolset.
	var bufA bytes.Buffer
	resA, err := clientA.Run(context.Background(), sdk.RunRequest{
		Prompt: writePrompt, Mode: sdk.RunModeJSON, ContinueSessionID: sessionA,
		Stdout: &bufA, HideSpinner: true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}}},
			DiskProvider: diskA,
		},
	})
	require.NoError(t, err, "output %q", bufA.String())
	require.NotNil(t, resA)
	require.Equal(t, markerA, resA.FinalText, "error=%q warnings=%v", resA.Error, resA.Warnings)

	// Provider contents read back correctly; the EXACT absolute OS target
	// (preseeded with the sentinel) is unchanged.
	absA := filepath.Join(workA, filepath.FromSlash(relPath))
	gotA, ok := diskA.files[absA]
	require.True(t, ok, "the provider must hold the written file at %s", absA)
	require.Equal(t, contentA, gotA)
	sentinelA, err := os.ReadFile(absA)
	require.NoError(t, err)
	require.Equal(t, sentinel, string(sentinelA), "the exact OS target must be untouched by the virtual provider")

	msgsA, err := clientA.Messages(context.Background(), sessionA)
	require.NoError(t, err)
	readA := fsToolResultOf(t, msgsA, "fs_read")
	require.False(t, readA.IsError, "content %q", readA.Content)
	require.Contains(t, readA.Content, contentA, "the same-turn fs_read must read the provider's content back")

	// Second client, IDENTICAL paths: isolated, no cross-client routing.
	var bufB bytes.Buffer
	resB, err := clientB.Run(context.Background(), sdk.RunRequest{
		Prompt: isoPromptA, Mode: sdk.RunModeJSON, ContinueSessionID: sessionB,
		Stdout: &bufB, HideSpinner: true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}}},
			DiskProvider: diskB,
		},
	})
	require.NoError(t, err, "output %q", bufB.String())
	require.Equal(t, markerB, resB.FinalText)
	absB := filepath.Join(workB, filepath.FromSlash(relPath))
	require.Equal(t, contentB, diskB.files[absB])
	sentinelB, err := os.ReadFile(absB)
	require.NoError(t, err)
	require.Equal(t, sentinel, string(sentinelB))
	require.NotContains(t, diskA.files, absB, "no cross-client routing onto client A's provider")

	// Explicit OSDiskProvider is a supported shape: the write lands on the
	// real disk at the exact absolute target.
	resOS, err := clientA.Run(context.Background(), sdk.RunRequest{
		Prompt: "shapes os provider", Mode: sdk.RunModeJSON, ContinueSessionID: sessionA,
		HideSpinner: true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate}}},
			DiskProvider: sdk.OSDiskProvider(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, osMarker, resOS.FinalText)
	osTarget := filepath.Join(workA, filepath.FromSlash(osPath))
	osGot, err := os.ReadFile(osTarget)
	require.NoError(t, err)
	require.Equal(t, osContent, string(osGot))

	// Typed-nil provider is rejected BEFORE any provider traffic.
	before := hits.Load()
	var typedNil *shapesDisk
	_, err = clientA.Run(context.Background(), sdk.RunRequest{
		Prompt: "shapes typed nil", Mode: sdk.RunModeJSON, ContinueSessionID: sessionA,
		HideSpinner: true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate}}},
			DiskProvider: typedNil,
		},
	})
	require.Error(t, err)
	require.Equal(t, before, hits.Load(), "a typed-nil provider must be rejected before any model traffic")
}

// The typed-nil rejection is asserted by the traffic counter above; the
// nilMarker branch of the scripted server exists only so an accidental
// execution would still produce a visible wrong marker.
