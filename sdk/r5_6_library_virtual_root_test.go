package sdk_test

// Regression test for R5-6 (P1 SDK review finding): an ephemeral
// sdk.ModeLibrary session (no Options.WorkingDir, in-memory database,
// nothing ever on disk) synthesizes a logical filesystem root
// (sdk.LibraryVirtualRoot()) so that BOTH a RELATIVE RunOverrides.FolderScopes
// entry (the README's own natural example, {Dir: "src", ...}) and a
// RELATIVE model-emitted fs_* item path resolve against that synthesized
// root -- never against the Rush host process's own real CWD/drive, and
// never touching the real host disk.
//
// Reuses sdk_disk_provider_test.go's harness (fakeSDKDisk, newFakeSDKDisk)
// and library_mode_test.go's libraryConfigFor: the same fs_write-then-
// fs_read scripted round trip #859's disk-provider tests already use,
// just opened in sdk.ModeLibrary with no WorkingDir instead of
// sdk.ModeApplication with a real temp dir. The scripted server below
// extends diskProviderWriteThenReadServer with one additional branch:
// session title generation (internal/agent's needsTitle) fires in the
// background on every turn until a real title is saved, using the SAME
// provider URL, so it must be answered with plain text (title-gen
// configures no tools) rather than falling into the tool-call branch
// meant for the main turn.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// libraryVirtualRootRoundTripServer mirrors diskProviderWriteThenReadServer
// (sdk_disk_provider_test.go) with one extra leading branch for session
// title generation, which fires in the background on the same client and
// must never be handed a tool-call response (its agent has no tools
// configured).
func libraryVirtualRootRoundTripServer(t *testing.T, relPath, content, marker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("Generate a concise title")):
			sseChunks(t, w, []map[string]any{textChunk("probe", "Virtual Root Test"), finishChunk("probe", "stop")})
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

// TestSDKLibraryModeEphemeralRelativeFolderScopeResolvesAgainstVirtualRootNotHostCWD
// is R5-6's headline regression: ModeLibrary + empty persistence
// WorkingDir + a custom DiskProvider + a RELATIVE folder scope + a
// RELATIVE model-emitted item path, asserting zero host-disk access and
// resolution against the synthesized virtual root, not the host CWD.
func TestSDKLibraryModeEphemeralRelativeFolderScopeResolvesAgainstVirtualRootNotHostCWD(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const (
		relPath = "scoped/virtual.txt"
		content = "LIBRARY_VIRTUAL_ROOT_CONTENT_10i"
		marker  = "LIBRARY_VIRTUAL_ROOT_ROUNDTRIP_OK"
	)

	// The fake's ancestor-walk needs the granted scope's root to
	// pre-exist -- exactly like the real-disk-provider sibling test's
	// os.MkdirAll (sdk_disk_provider_test.go), mirrored here against the
	// SYNTHESIZED root instead of a real temp dir.
	fake := newFakeSDKDisk()
	scopedDir := filepath.Clean(filepath.Join(sdk.LibraryVirtualRoot(), "scoped"))
	fake.mkdir(scopedDir)

	srv := libraryVirtualRootRoundTripServer(t, relPath, content, marker)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-library-secret"),
		// WorkingDir deliberately omitted: ephemeral, in-memory session,
		// the exact combination R5-6 left untested.
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	const sessionID = "sdk-library-virtual-root"
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "write then read against the virtual root",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			// A RELATIVE folder scope: pre-R5-6 this alone made
			// permission.BuildFolderScope hard-fail with "WorkingDir is
			// empty" before any provider traffic at all.
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
		"fs_read must carry back the content the fake wrote under the virtual root")

	// Positive oracle: the RELATIVE model-emitted item path
	// ("scoped/virtual.txt") must resolve EXACTLY against the
	// synthesized virtual root -- not against any drive- or
	// CWD-substituted variant of it. This is the assertion that would
	// fail if either half of the R5-6 fix were reverted: the
	// empty-WorkingDir hard-fail (no virtual-root synthesis in
	// sdk/library_mode.go) never even reaches this point (Run itself
	// errors), and a filepath.Abs-against-real-CWD resolution
	// (resolveScopedPath unfixed in internal/agent/tools/fs_scope.go)
	// would store the write under a DIFFERENT key than this one.
	expected := filepath.Clean(filepath.Join(sdk.LibraryVirtualRoot(), filepath.FromSlash(relPath)))
	fakeContent, ok := fake.has(expected)
	require.True(t, ok, "the fake disk must hold the written file at the virtual-root path %s", expected)
	require.Equal(t, content, fakeContent)

	// Negative oracle: the OLD bug's specific mechanism -- a relative
	// item path silently resolved via filepath.Abs against the REAL
	// Rush host process's own current working directory -- must not
	// have happened.
	realCWD, err := os.Getwd()
	require.NoError(t, err)
	wrongPath := filepath.Join(realCWD, filepath.FromSlash(relPath))
	_, wrongOK := fake.has(wrongPath)
	require.False(t, wrongOK,
		"the write must never land under the host process's own CWD (%s) -- that is exactly the R5-6 bug", wrongPath)

	// No real file was ever created on the actual host disk under the
	// relative path's naive (host-CWD-joined) interpretation either.
	_, statErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(statErr), "no real file must ever be created on the host disk (%s)", wrongPath)
}
