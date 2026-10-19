package sdk_test

// R12-H2: real SDK custom-provider acceptance under RUSH_BATCH_RUNTIME=1.
// This chunk adds canonical OS, non-comparable custom, typed-nil rejection,
// two-client isolation, and an external public-sentinel consumer trace.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKBatchRuntimeOnCustomDiskProviderSupported verifies the actual scoped
// fs_write/fs_read path under the batch runtime; first assertions inspect the
// provider contents and confirm the real path is absent.
func TestSDKBatchRuntimeOnCustomDiskProviderSupported(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	const path, content, marker = "scoped/batch-provider.txt", "batch-provider-data", "BATCH_CUSTOM_DISK_OK"
	fake := newFakeSDKDisk()
	fake.mkdir(filepath.Join(workDir, "scoped"))
	srv := diskProviderWriteThenReadServer(t, path, content, marker)
	writeProbeRushJSON(t, workDir, srv.URL)
	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-batch-disk-supported"
	seedTenantSession(t, workDir, sessionID)
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt: "write then read", Mode: sdk.RunModeJSON, ContinueSessionID: sessionID, HideSpinner: true,
		Overrides: sdk.RunOverrides{FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}}}, DiskProvider: fake},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, marker, res.FinalText)
	got, ok := fake.has(filepath.Join(workDir, filepath.FromSlash(path)))
	require.True(t, ok)
	require.Equal(t, content, got)
	_, err = os.Stat(filepath.Join(workDir, filepath.FromSlash(path)))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestSDKBatchRuntimeOnCanonicalOSDiskProviderSupported proves an explicit
// public OSDiskProvider is a supported flag-ON binding (not the nil/default
// path). First mutation-sensitive assertion: Stat on the scoped temporary
// file succeeds on the scoped real filesystem path.
func TestSDKBatchRuntimeOnCanonicalOSDiskProviderSupported(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	const path, content, marker = "scoped/os-provider.txt", "canonical-os-provider", "BATCH_OS_DISK_OK"
	srv := diskProviderWriteThenReadServer(t, path, content, marker)
	writeProbeRushJSON(t, workDir, srv.URL)
	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-batch-os-provider-supported"
	seedTenantSession(t, workDir, sessionID)
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt: "write then read using the explicit OS provider", Mode: sdk.RunModeJSON,
		ContinueSessionID: sessionID, HideSpinner: true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}}},
			DiskProvider: sdk.OSDiskProvider(),
		},
	})
	require.NoError(t, err)
	require.Equal(t, marker, res.FinalText)
	got, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(path)))
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}

// batchSDKSliceDisk embeds the public interface for unused methods but has
// a non-comparable dynamic value type; pointer identity cannot mask equality.
type batchSDKSliceDiskState struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]bool
	calls int
}
type batchSDKSliceDisk struct {
	sdk.DiskProvider
	tags  []string
	state *batchSDKSliceDiskState
}

func newBatchSDKSliceDisk() batchSDKSliceDisk {
	return batchSDKSliceDisk{DiskProvider: sdk.OSDiskProvider(), tags: []string{"slice"}, state: &batchSDKSliceDiskState{files: map[string]string{}, dirs: map[string]bool{}}}
}
func (d batchSDKSliceDisk) mark() { d.state.mu.Lock(); d.state.calls++; d.state.mu.Unlock() }
func (d batchSDKSliceDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	d.mark()
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	if _, ok := d.state.files[name]; ok {
		return fakeSDKFileInfo{name: filepath.Base(name)}, nil
	}
	if d.state.dirs[name] {
		return fakeSDKFileInfo{name: filepath.Base(name), isDir: true}, nil
	}
	return nil, fs.ErrNotExist
}

func (d batchSDKSliceDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	d.mark()
	return filepath.Clean(name), nil
}

func (d batchSDKSliceDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	d.mark()
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	v, ok := d.state.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(v)), nil
}

func (d batchSDKSliceDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	d.mark()
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	v, ok := d.state.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(v), nil
}

func (d batchSDKSliceDisk) MkdirAll(_ context.Context, name string, _ fs.FileMode) error {
	d.mark()
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	for dir := filepath.Clean(name); ; dir = filepath.Dir(dir) {
		d.state.dirs[dir] = true
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func (d batchSDKSliceDisk) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	d.mark()
	d.state.mu.Lock()
	d.state.files[name] = string(data)
	d.state.mu.Unlock()
	return nil
}

func (d batchSDKSliceDisk) Remove(_ context.Context, name string) error {
	d.mark()
	d.state.mu.Lock()
	delete(d.state.files, name)
	d.state.mu.Unlock()
	return nil
}

func (d batchSDKSliceDisk) stats() (int, map[string]string) {
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	files := make(map[string]string, len(d.state.files))
	for k, v := range d.state.files {
		files[k] = v
	}
	return d.state.calls, files
}

var _ sdk.DiskProvider = batchSDKSliceDisk{}

// TestSDKBatchRuntimeOnNonComparableProviderSupported exercises real batch
// ingress with a custom provider whose interface dynamic value has a slice.
func TestSDKBatchRuntimeOnNonComparableProviderSupported(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	const path, content, marker = "scoped/slice-provider.txt", "slice-provider-content", "BATCH_SLICE_DISK_OK"
	disk := newBatchSDKSliceDisk()
	// MkdirAll seeds the scoped dir AND all of its ancestors in the fake, so
	// the fs_write ancestor walk inside fs_write.go stays inside the grant.
	require.NoError(t, disk.MkdirAll(context.Background(), filepath.Join(workDir, "scoped"), 0o755))
	srv := diskProviderWriteThenReadServer(t, path, content, marker)
	writeProbeRushJSON(t, workDir, srv.URL)
	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-batch-slice-provider"
	seedTenantSession(t, workDir, sessionID)
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt: "write then read", Mode: sdk.RunModeJSON, ContinueSessionID: sessionID, HideSpinner: true,
		Overrides: sdk.RunOverrides{FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}}}, DiskProvider: disk},
	})
	require.NoError(t, err)
	require.Equal(t, marker, res.FinalText)
	calls, files := disk.stats()
	require.Greater(t, calls, 0)
	require.Equal(t, content, files[filepath.Join(workDir, filepath.FromSlash(path))])
	_, err = os.Stat(filepath.Join(workDir, filepath.FromSlash(path)))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestSDKBatchRuntimeTwoClientsIsolateEqualPaths places two real Clients on
// the same tool target, holds both provider turns concurrently, then proves
// each run's read result came from its own call-local provider binding.
func TestSDKBatchRuntimeTwoClientsIsolateEqualPaths(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	workA, workB := evalTempDir(t), evalTempDir(t)
	for _, dir := range []string{workA, workB} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "scoped"), 0o755))
	}
	const path = "scoped/shared.txt"
	const markerA, markerB = "BATCH_CLIENT_A_OK", "BATCH_CLIENT_B_OK"
	fakeA, fakeB := newBatchSDKSliceDisk(), newBatchSDKSliceDisk()
	require.NoError(t, fakeA.MkdirAll(context.Background(), filepath.Join(workA, "scoped"), 0o755))
	require.NoError(t, fakeB.MkdirAll(context.Background(), filepath.Join(workB, "scoped"), 0o755))
	const contentA, contentB = "same-path-A", "same-path-B"
	barrier := make(chan struct{})
	var arrived atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(`"call_read"`)):
			marker := markerA
			if bytes.Contains(body, []byte(contentB)) {
				marker = markerB
			}
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"call_write"`)):
			arrived.Add(1)
			if arrived.Load() >= 2 {
				select {
				case <-barrier:
				default:
					close(barrier)
				}
			}
			<-barrier
			sseChunks(t, w, []map[string]any{toolCallChunkNamed("probe", "call_read", "fs_read", map[string]any{"items": []map[string]any{{"path": path}}}), finishChunk("probe", "tool_calls")})
		default:
			content := contentB
			if bytes.Contains(body, []byte("client A write then read")) {
				content = contentA
			}
			sseChunks(t, w, []map[string]any{toolCallChunkNamed("probe", "call_write", "fs_write", map[string]any{"items": []map[string]any{{"path": path, "content": content}}}), finishChunk("probe", "tool_calls")})
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
	const sessionA, sessionB = "sdk-batch-binding-client-a", "sdk-batch-binding-client-b"
	seedTenantSession(t, workA, sessionA)
	seedTenantSession(t, workB, sessionB)
	var wg sync.WaitGroup
	wg.Add(2)
	var resultA, resultB *sdk.RunResult
	var errA, errB error
	run := func(client *sdk.Client, work, session, prompt string, disk batchSDKSliceDisk, result **sdk.RunResult, out *error) {
		defer wg.Done()
		*result, *out = client.Run(context.Background(), sdk.RunRequest{
			Prompt: prompt, Mode: sdk.RunModeJSON, ContinueSessionID: session, HideSpinner: true,
			Overrides: sdk.RunOverrides{FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}}}, DiskProvider: disk},
		})
	}
	go run(clientA, workA, sessionA, "client A write then read", fakeA, &resultA, &errA)
	go run(clientB, workB, sessionB, "client B write then read", fakeB, &resultB, &errB)
	wg.Wait()
	require.NoError(t, errA)
	require.NoError(t, errB)
	require.Equal(t, markerA, resultA.FinalText)
	require.Equal(t, markerB, resultB.FinalText)
	_, filesA := fakeA.stats()
	_, filesB := fakeB.stats()
	require.Equal(t, contentA, filesA[filepath.Join(workA, filepath.FromSlash(path))])
	require.Equal(t, contentB, filesB[filepath.Join(workB, filepath.FromSlash(path))])
	msgsA, err := clientA.Messages(context.Background(), sessionA)
	require.NoError(t, err)
	require.Contains(t, fsToolResultOf(t, msgsA, "fs_read").Content, contentA)
	msgsB, err := clientB.Messages(context.Background(), sessionB)
	require.NoError(t, err)
	require.Contains(t, fsToolResultOf(t, msgsB, "fs_read").Content, contentB)
}

// TestSDKBatchRuntimeTypedNilProviderRejected checks that a typed-nil SDK
// provider fails closed before a model request rather than selecting OS disk.
func TestSDKBatchRuntimeTypedNilProviderRejected(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	workDir := evalTempDir(t)
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		sseChunks(t, w, []map[string]any{textChunk("probe", "unexpected"), finishChunk("probe", "stop")})
	}))
	t.Cleanup(server.Close)
	writeProbeRushJSON(t, workDir, server.URL)
	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	var typedNil *batchSDKSliceDisk
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt: "typed nil", Mode: sdk.RunModeJSON, HideSpinner: true,
		Overrides: sdk.RunOverrides{FolderScopes: []sdk.FolderScope{{Dir: ".", Ops: []sdk.FileOp{sdk.FileOpCreate}}}, DiskProvider: typedNil},
	})
	require.Error(t, err)
	require.Nil(t, res)
	require.Zero(t, hits.Load())
}

// TestSDKExternalUnavailableSentinelClassifies is compile-time-safe from
// package sdk_test and demonstrates errors.Is with the exported contract.
func TestSDKExternalUnavailableSentinelClassifies(t *testing.T) {
	wrapped := fmt.Errorf("operation: %w", sdk.ErrDiskProviderUnavailable)
	if !errors.Is(wrapped, sdk.ErrDiskProviderUnavailable) {
		t.Fatal("errors.Is(wrapped, sdk.ErrDiskProviderUnavailable) = false, want true")
	}
}

func TestSDKBatchRuntimeOnRunContextCancellation(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	workDir := evalTempDir(t)
	const marker = "BATCH_RUNTIME_CANCEL_RECOVERY_OK"
	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Hold the first turn open until the test cancels (or the request
		// context dies with it); after release the turn finishes cleanly.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-batch-cancel"
	const sessionID2 = "sdk-batch-cancel-recovery"
	seedTenantSession(t, workDir, sessionID)
	seedTenantSession(t, workDir, sessionID2)

	runCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, rerr := client.Run(runCtx, sdk.RunRequest{
			Prompt:            "cancelled mid-turn",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionID,
			HideSpinner:       true,
		})
		errCh <- rerr
	}()

	// First real-schema assertion: the provider actually saw the in-turn
	// request, so the cancel below races nothing.
	deadline := time.Now().Add(60 * time.Second)
	for hits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the flag-ON run never reached the provider")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled,
			"cancelling the caller's context must abort the flag-ON run, got %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the cancelled flag-ON run never returned")
	}

	// The client must survive the cancellation: a follow-up run on a fresh
	// session (the cancelled turn's session may still unwind) completes.
	close(release)
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "after cancellation",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID2,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, marker, res.FinalText)
}

// sdkGatedDisk is a DiskProvider whose WriteFile BLOCKS on a test-owned gate
// AFTER signaling it is inside the physical write; context cancellation does
// not release the physical I/O (mirrors internal/app's shutdownGatedDisk
// contract). All other calls go to the real OS provider.
type sdkGatedDisk struct {
	sdk.DiskProvider
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

func newSDKGatedDisk() *sdkGatedDisk {
	return &sdkGatedDisk{DiskProvider: sdk.OSDiskProvider(), gate: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (d *sdkGatedDisk) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gate != nil {
		close(d.gate)
		d.gate = nil
	}
}

func (d *sdkGatedDisk) WriteFile(_ context.Context, name string, data []byte, perm fs.FileMode) error {
	d.mu.Lock()
	gate := d.gate
	d.mu.Unlock()
	if gate != nil {
		select {
		case d.entered <- struct{}{}:
		default:
		}
		<-gate
	}
	return d.DiskProvider.WriteFile(context.Background(), name, data, perm)
}

// sdkBatchStateTerminal mirrors internal/agent/batchAwaitTerminalState's
// terminal set (internal/agent/batch_await.go, case "completed", ...).
func sdkBatchStateTerminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

// waitForBatchNodes polls the REAL persisted batch_nodes tree for sessionID
// until done holds (same .rush dataDir the client opened). It fails with the
// last observed state distribution when the budget expires.
func waitForBatchNodes(t *testing.T, conn *sql.DB, sessionID string, budget time.Duration, done func(nonterminal, roots int, states map[string]int) bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var nonterminal, roots int
	var states map[string]int
	for {
		require.NoError(t, conn.PingContext(context.Background()))
		rows, err := conn.QueryContext(context.Background(),
			`SELECT state, count(*), count(*) FILTER (WHERE parent_id IS NULL)
			 FROM batch_nodes WHERE owner_session_id = ?1 GROUP BY state`, sessionID)
		require.NoError(t, err)
		states, nonterminal, roots = map[string]int{}, 0, 0
		for rows.Next() {
			var state string
			var n, r int
			require.NoError(t, rows.Scan(&state, &n, &r))
			states[state] = n
			if !sdkBatchStateTerminal(state) {
				nonterminal += n
			}
			roots += r
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		if done(nonterminal, roots, states) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (last states=%v roots=%d nonterminal=%d)", msg, states, roots, nonterminal)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSDKBatchRuntimeCancelSettlesAcceptedWork: REAL-work cancellation over
// the public SDK context. The scripted model emits a REAL fs_write tool call;
// the tool's physical execution blocks INSIDE the gated provider AFTER the
// batch work has been accepted and is observable as persisted batch_nodes
// state (owner_session_id = the continued session). The run is then
// cancelled through the caller's context — the production caller-cancel path
// (agent/batch_cancel.go cancelBatchCall) submits the Runner Stop under a
// bounded, non-cancelled control context, so a cancelled caller still cancels
// the accepted root; the tree settles terminal with CauseStop => state
// 'cancelled' (jobbatch/tree_apply.go causeState) once the physical work
// unwinds.
//
// Mutations caught:
//   - cancelling before acceptance (no accepted work observable): the
//     gated-provider "entered" wait and/or the pre-cancel batch_nodes
//     nonterminal wait both time out before cancel() is reached.
//   - skipping settlement on cancel (Stop never reaches the accepted root):
//     the Run still returns context.Canceled, but the post-cancel
//     waitForBatchNodes poll for a terminal 'cancelled' tree never holds and
//     fails with the last observed state distribution.
func TestSDKBatchRuntimeCancelSettlesAcceptedWork(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	const path, content = "scoped/cancel-work.txt", "cancel-work-data"
	disk := newSDKGatedDisk()
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(disk.release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"call_w"`)) {
			sseChunks(t, w, []map[string]any{textChunk("probe", "CANCEL_SETTLE_UNREACHABLE"), finishChunk("probe", "stop")})
			return
		}
		sseChunks(t, w, []map[string]any{toolCallChunkNamed("probe", "call_w", "fs_write", map[string]any{
			"items": []map[string]any{{"path": path, "content": content}},
		}), finishChunk("probe", "tool_calls")})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-batch-cancel-realwork"
	seedTenantSession(t, workDir, sessionID)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, rerr := client.Run(runCtx, sdk.RunRequest{
			Prompt:            "cancel accepted work",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionID,
			HideSpinner:       true,
			Overrides: sdk.RunOverrides{
				FolderScopes: []sdk.FolderScope{{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}}},
				DiskProvider: disk,
			},
		})
		errCh <- rerr
	}()

	// The REAL work tool is executing inside the gated provider: the model
	// emitted fs_write and the batch executor dispatched it.
	select {
	case <-disk.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the scripted fs_write never entered the gated provider call")
	}

	// The accepted work is observable as PERSISTED batch state BEFORE the
	// cancel: a nonterminal batch tree is owned by the continued session.
	dataDir := filepath.Join(workDir, ".rush")
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	waitForBatchNodes(t, conn, sessionID, 60*time.Second,
		func(nonterminal, roots int, states map[string]int) bool { return nonterminal > 0 },
		"the accepted fs_write batch tree never became visible in batch_nodes")

	// PUBLIC SDK cancellation while the physical work is in flight.
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled,
			"cancelling the caller's context must abort the run, got %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the cancelled run never returned")
	}

	// Release the physical write; the accepted tree settles: NO nonterminal
	// node remains and the CauseStop terminal state 'cancelled' is recorded.
	releaseOnce.Do(disk.release)
	waitForBatchNodes(t, conn, sessionID, 60*time.Second,
		func(nonterminal, roots int, states map[string]int) bool {
			return nonterminal == 0 && states["cancelled"] >= 1
		},
		"the cancelled batch tree never settled terminal 'cancelled'")

	// The REAL work actually executed: the physical write landed through the
	// OS provider before the settlement.
	got, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(path)))
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}
