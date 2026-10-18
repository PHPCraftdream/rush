package app

// R12-H2a fix round (review P1 #4): shutdown acceptance over the PUBLIC App
// shutdown entry point and REAL stores — no executor seam injection, no fake
// release callbacks, no direct DrainAndRelease driving.
//
// Production entry points exercised:
//   - App built through the REAL New() with RUSH_BATCH_RUNTIME=1 (the app
//     install path: real Runner budgets — ControlTimeout 60s, CommitTimeout
//     5s, RecordRetention 256 — real default batchExecutor, real commit
//     hook, real HostStore host identity and OS host lock).
//   - Provider-backed work submitted through the app's own run path:
//     ExecuteRun with RunOverrides.DiskProvider (a custom gated provider) +
//     FolderScopes and a scripted openaicompat model emitting a REAL
//     fs_write tool call. Under the flag the pinned fs_write leaf executes
//     through the REAL ingress Invoke → REAL default executor — no seam.
//   - app.ShutdownWithResult (the public CLI-shape shutdown): CancelAgents
//     → the batch drain gate (DrainAndRelease via releaseBatchRuntimeGate)
//     → graceful: drainResidualConstructions + host release (HostStore.Close
//     releases the OS lock) + writer release (per-reference db release);
//     forced: CloseKeepLock pins the OS lock, the writer and the manager's
//     remaining owners are retained, the Runner stays the drain target.
//
// Oracles (REAL, not callbacks):
//   - Graceful: ShutdownResult.Forced is false with no cleanup errors, the original DB handle is closed, a fresh distinct handle answers a ping, and the REAL host lock is gone (verified by a different process). The borrowed provider's Close is NEVER called.
//   - Forced: a physical write is in flight, shutdown reports Forced, the
//     host lock is STILL HELD (verified by a different process), the batch
//     node remains running, and writer/provider ownership is retained.
//
// Production mutation caught: a shutdown that released the host lock or the
// DB writer beneath a still-draining Runner (ordering mutation — the live
// lock probe fails), a forced path that bulk-released the capability
// manager's owners or closed the borrowed provider, or a fixture-level
// shutdown that bypassed the public Close path entirely (the review's P1-4:
// zero runner budgets made the old fixture fail during installation).
//
// Substitution note (blocked-Dispose variant): blocking a physical Dispose
// is not injectable through the public App path (the real executor's
// Disposer is the agent family; the executor seam is deliberately NOT
// injected here). The agent-layer dispose/release contract is covered by
// internal/agent's lifecycle tests (batch_disk_provider_lifecycle_test.go:
// Release → giveUp → maybeDispose → owner release), and the app-layer forced
// variant below covers the required "drain past its bound retains lock +
// writer + owners" acceptance through the public path.

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	jobbatch "github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// shutdownGatedDisk is a custom DiskProvider whose WriteFile can be gated
// INSIDE the provider call: after taking the gate it signals "entered" and
// then blocks, ignoring context cancellation (physical I/O Stop cannot
// preempt). Close is the borrowed-provider hook: production must never call
// it — not even on a forced shutdown.
type shutdownGatedDisk struct {
	mu      sync.Mutex
	files   map[string]string
	dirs    map[string]struct{}
	gate    chan struct{}
	entered chan struct{}
	close   int
}

var _ tools.DiskProvider = (*shutdownGatedDisk)(nil)

func newShutdownGatedDisk() *shutdownGatedDisk {
	return &shutdownGatedDisk{files: map[string]string{}, dirs: map[string]struct{}{}}
}

func (d *shutdownGatedDisk) arm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gate == nil {
		d.gate = make(chan struct{})
		d.entered = make(chan struct{}, 1)
	}
}

func (d *shutdownGatedDisk) letGo() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gate != nil {
		close(d.gate)
		d.gate = nil
	}
}

// enteredCh fires once WriteFile is INSIDE the gated provider call.
func (d *shutdownGatedDisk) enteredCh() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entered
}

func (d *shutdownGatedDisk) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.close
}

func (d *shutdownGatedDisk) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.close++
	return nil
}

func (d *shutdownGatedDisk) seedDir(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Seed dir AND every ancestor up to the root, mirroring what a real
	// os.MkdirAll leaves behind (sdk/sdk_disk_provider_test.go fakeSDKDisk
	// .mkdir). The fs_write ancestor walk (tools/fs_write.go) stops at the
	// FIRST EXISTING ancestor above the write's parent: with only the
	// scoped dir seeded, it Stats <dataDir> itself, gets ErrNotExist, and
	// scope.Check denies the walk step with "outside every folder scope"
	// even though the write target is inside the granted scope.
	for {
		d.dirs[dir] = struct{}{}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func (d *shutdownGatedDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.dirs[name]; ok {
		return shutdownProbeDir{}, nil
	}
	if _, ok := d.files[name]; ok {
		return shutdownProbeFile{}, nil
	}
	return nil, fs.ErrNotExist
}

type shutdownProbeDir struct{}

func (shutdownProbeDir) Name() string       { return "scoped" }
func (shutdownProbeDir) Size() int64        { return 0 }
func (shutdownProbeDir) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (shutdownProbeDir) ModTime() time.Time { return time.Time{} }
func (shutdownProbeDir) IsDir() bool        { return true }
func (shutdownProbeDir) Sys() any           { return nil }

type shutdownProbeFile struct{}

func (shutdownProbeFile) Name() string       { return "out.txt" }
func (shutdownProbeFile) Size() int64        { return 0 }
func (shutdownProbeFile) Mode() fs.FileMode  { return 0o644 }
func (shutdownProbeFile) ModTime() time.Time { return time.Time{} }
func (shutdownProbeFile) IsDir() bool        { return false }
func (shutdownProbeFile) Sys() any           { return nil }

func (d *shutdownGatedDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	return name, nil
}

func (d *shutdownGatedDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	d.mu.Lock()
	v, ok := d.files[name]
	d.mu.Unlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(v)), nil
}

func (d *shutdownGatedDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	d.mu.Lock()
	v, ok := d.files[name]
	d.mu.Unlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(v), nil
}

func (d *shutdownGatedDisk) MkdirAll(_ context.Context, dir string, _ fs.FileMode) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dirs[dir] = struct{}{}
	return nil
}

func (d *shutdownGatedDisk) WriteFile(ctx context.Context, name string, data []byte, _ os.FileMode) error {
	d.mu.Lock()
	gate, entered := d.gate, d.entered
	d.mu.Unlock()
	if gate != nil {
		// Signal "inside the provider call", then block. Context
		// cancellation (Stop / shutdown cancellation) deliberately does not
		// release the physical I/O.
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		<-gate
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[name] = string(data)
	_ = ctx
	return nil
}

func (d *shutdownGatedDisk) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.files, name)
	return nil
}

// readFile is a test accessor over the provider's virtual contents.
func (d *shutdownGatedDisk) readFile(t *testing.T, name string) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	// The real fs_write stores under the path after SmartJoin+Clean, which
	// on Windows yields BACKSLASHES, while the fixture keys on the SLASHED
	// spelling. Try the raw name, then the Clean-ed (native separators)
	// form, then the Clean+ToSlash form, and return whichever key exists.
	for _, key := range []string{name, filepath.Clean(name), filepath.ToSlash(filepath.Clean(name))} {
		if v, ok := d.files[key]; ok {
			return v
		}
	}
	return ""
}

func (d *shutdownGatedDisk) List(context.Context, tools.ListRequest) (tools.ListResult, error) {
	return tools.ListResult{}, nil
}

func (d *shutdownGatedDisk) Find(context.Context, tools.FindRequest) (tools.FindResult, error) {
	return tools.FindResult{}, nil
}

func (d *shutdownGatedDisk) Search(context.Context, tools.SearchRequest) (tools.DiskSearchResult, error) {
	return tools.DiskSearchResult{}, nil
}

// shutdownAcceptanceApp is the app-level fixture: a REAL New() App (flag ON,
// so installBatchRuntime ran: real Runner budgets, real default executor,
// real HostStore + OS host lock) with a scripted openaicompat model that
// makes the model call fs_write once and then stop.
type shutdownAcceptanceApp struct {
	t          *testing.T
	app        *App
	sessionID  string
	dataDir    string
	targetPath string
	provider   *shutdownGatedDisk
	hostID     string
}

// newShutdownAcceptanceApp builds the fixture. handler is the scripted
// openaicompat SSE server. The model's fs_write targets
// <dataDir>/scoped/out.txt; the provider is seeded with the scoped dir so
// the real fs_write's ancestor check stops at the granted scope exactly like
// the established SDK provider fixture (sdk/sdk_disk_provider_test.go).
// forced selects the forced-shutdown data-dir strategy: see the comment at
// the dataDir assignment below.
func newShutdownAcceptanceApp(t *testing.T, forced bool, handler http.HandlerFunc) *shutdownAcceptanceApp {
	return newShutdownAcceptanceAppWithDecorator(t, forced, handler, nil)
}

func newShutdownAcceptanceAppWithDecorator(t *testing.T, forced bool, handler http.HandlerFunc, decorate func(jobbatch.Execution) jobbatch.Execution) *shutdownAcceptanceApp {
	t.Helper()
	// Install-scoped test decoration is restored immediately after New.
	if decorate != nil {
		previous := batchInstallDepsHook
		batchInstallDepsHook = func(deps *agent.BatchInstallDeps) { deps.DecorateExecution = decorate }
		defer func() { batchInstallDepsHook = previous }()
	}
	// The batch runtime flag must be on BEFORE New: the flagged install
	// (real Runner, real budgets, real host identity) happens inside New.
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	var dataDir string
	if forced {
		// The forced path RETAINS the DB writer by contract (that retention is
		// the behavior under test), so the data-dir file stays locked and
		// t.TempDir's RemoveAll cleanup would fail on Windows. Do NOT use
		// t.TempDir here and do NOT remove the directory: the forced shutdown
		// retains the host lock AND the writer — the caller must exit the
		// process (production semantics) — and the OS reclaims the temp dir.
		forcedDir, mkErr := os.MkdirTemp("", "shutdown-forced-*")
		require.NoError(t, mkErr)
		dataDir = forcedDir
	} else {
		// The graceful path releases the writer and the host lock, so the
		// standard t.TempDir cleanup works.
		dataDir = t.TempDir()
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat", Model: "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat", Model: "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	// Pair the refCounted pool handle (check_db_release_pairing.sh). Registered
	// BEFORE the Shutdown cleanup so LIFO runs it AFTER Shutdown; on the
	// graceful path Shutdown already dropped the pool entry (benign no-op),
	// and on the forced path (Shutdown skips Release) it closes the entry.
	t.Cleanup(func() { _ = db.Release(dataDir) })
	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() { application.Shutdown() })

	appCutoverWriter := application.batchCutoverWriter()
	agent.BindCoordinatorBatchCutoverFacts(application.AgentCoordinator,
		agent.NewBatchCutoverNodeFacts(session.NewBatchStore(appCutoverWriter), application.batchLiveReader, appCutoverWriter))

	require.NotNil(t, application.batchInstall, "flag on: the batch runtime is installed through the app install path")
	// REAL runner budgets, not zeros: a zero CommitTimeout/ControlTimeout/
	// RecordRetention would have been rejected by jobbatch.NewRunner during
	// the install above (the review's P1-4 fixture defect).
	require.NotZero(t, application.batchInstall.Runtime, "the runtime owns its runner")

	sess, err := application.Sessions.Create(context.Background(), "shutdown-acceptance")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	provider := newShutdownGatedDisk()
	scopedDir := filepath.Join(dataDir, "scoped")
	require.NoError(t, os.MkdirAll(scopedDir, 0o755))
	// Seed the scoped ancestor under BOTH spellings: the fs_* ancestor walk
	// may Stat either the native or the slashed absolute form.
	provider.seedDir(scopedDir)
	provider.seedDir(filepath.ToSlash(scopedDir))
	targetPath := filepath.Join(scopedDir, "out.txt")

	hostID := ""
	if application.batchHostStore() != nil {
		hostID = application.batchHostStore().HostID()
	}
	require.NotEmpty(t, hostID, "the flagged install registered the real host identity")
	return &shutdownAcceptanceApp{
		t: t, app: application, sessionID: sess.ID, dataDir: dataDir,
		// The production fs_* family hands SLASHED absolute paths to the
		// provider (the agent-layer fixtures key their fake maps on
		// filepath.ToSlash of the joined path and match) — the script's tool
		// input and the provider-map key use the same slashed spelling.
		targetPath: filepath.ToSlash(targetPath), provider: provider, hostID: hostID,
	}
}

// fsWriteScript is the scripted model: round 1 emits one REAL fs_write tool
// call for the target path; the follow-up round (recognized by the tool-call
// id in the request body) ends the turn with the final marker.
func fsWriteScript(targetPath func() string, marker string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"call_w"`) {
			admissionWriteSSE(w, []string{
				admissionSSEText("c2", marker),
				admissionSSEStop("c2", "stop"),
			})
			return
		}
		input, err := json.Marshal(map[string]any{
			"items": []map[string]any{{"path": targetPath(), "content": "shutdown-acceptance-write"}},
		})
		if err != nil {
			panic(err)
		}
		admissionWriteSSE(w, []string{
			admissionSSEToolCall("c1", "call_w", "fs_write", string(input)),
			admissionSSEStop("c1", "tool_calls"),
		})
	}
}

func (h *shutdownAcceptanceApp) executeRun() (*RunResult, error) {
	h.t.Helper()
	return h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "write through the provider",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sessionID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{
				Dir: "scoped",
				Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpRead},
			}},
			DiskProvider: h.provider,
		},
	})
}

func TestShutdownGracefulReleasesRealWriterAndHostLock(t *testing.T) {
	var h *shutdownAcceptanceApp
	h = newShutdownAcceptanceApp(t, false, fsWriteScript(func() string { return h.targetPath }, "SHUTDOWN_GRACEFUL_OK"))

	// The provider-backed leaf runs through the REAL default executor: the
	// write lands on the custom provider and the turn completes.
	res, err := h.executeRun()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, "SHUTDOWN_GRACEFUL_OK", res.FinalText)
	require.Equal(t, "shutdown-acceptance-write", h.provider.readFile(h.t, h.targetPath),
		"the REAL executor routed the fs_write through the custom provider")

	// While the App is alive the host lock is genuinely held: a real
	// kernel-level probe (shared, non-acquiring) reports the holder.
	status := probeHostInChild(t, h.dataDir, h.hostID)
	require.Equal(t, session.HostStatusAlive, status, "before shutdown the OS host lock is genuinely held")

	originalDB := h.app.DB()
	result := h.app.ShutdownWithResult()
	require.False(t, result.Forced, "all work settled before shutdown: graceful")
	require.Empty(t, result.CleanupErrors, "cleanup errors: %v", result.CleanupErrors)

	// The original writer was released, not merely supplemented by a new one.
	err = originalDB.PingContext(context.Background())
	require.Error(t, err, "the pre-shutdown application DB handle must be closed")
	require.True(t, errors.Is(err, sql.ErrConnDone) || strings.Contains(strings.ToLower(err.Error()), "closed"),
		"old handle must report a closed-DB error, got %v", err)
	freshConn, err := db.Connect(context.Background(), h.dataDir)
	require.NoError(t, err, "the writer was released: the same dataDir reconnects cleanly")
	require.NotSame(t, originalDB, freshConn, "reconnect must return a distinct DB handle")
	require.NoError(t, freshConn.PingContext(context.Background()),
		"the fresh handle answers a real ping after the graceful release")
	require.NoError(t, db.ReleaseConn(freshConn))

	// REAL host oracle: the OS lock is gone (kernel-attested, not a
	// callback string). ProbeHostLock is the self-guard-free primitive: it
	// acquires the lock to prove no other holder exists.
	status = probeHostInChild(t, h.dataDir, h.hostID)
	require.Equal(t, session.HostStatusDead, status,
		"graceful shutdown released the real host lock")
	require.Zero(t, h.provider.closeCount(), "Rush borrows the provider and never closes it")
}

func TestShutdownForcedPreservesHostLockWriterAndProvider(t *testing.T) {
	var h *shutdownAcceptanceApp
	h = newShutdownAcceptanceApp(t, true, fsWriteScript(func() string { return h.targetPath }, "SHUTDOWN_FORCED_OK"))
	// The forced path PINS the OS lock until process exit (CloseKeepLock).
	// Lock FILES can be released, so still drop the pinned handle in cleanup;
	// the data-dir file itself stays locked (retained writer) and is
	// intentionally left behind — see the forced dataDir branch above.
	t.Cleanup(func() { session.ReleaseRetainedHostLocksForTest(h.hostID) })
	h.provider.arm() // block INSIDE the provider WriteFile

	runDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(runDone)
		_, runErr = h.executeRun()
	}()
	select {
	case <-h.provider.enteredCh():
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the provider WriteFile never entered the gate through the real executor")
	}

	// The provider-backed work is durably live in the REAL store while
	// blocked inside the provider call: the full tree — the batch root row
	// (parent NULL) and its task child — is still running.
	var nonterminal, roots int
	require.NoError(t, h.app.DB().QueryRowContext(context.Background(),
		`SELECT
			count(*) FILTER (WHERE parent_id IS NULL),
			count(*)
		 FROM batch_nodes WHERE owner_session_id = ?1 AND state IN ('pending', 'starting', 'running', 'cancelling')`,
		h.sessionID).Scan(&roots, &nonterminal))
	// State+shape is the durable live-tree oracle; kind spelling is an
	// internal projection detail.
	require.Equal(t, 2, nonterminal, "the batch root and its single task child are live in the real store")
	require.Equal(t, 1, roots, "the batch root row is live in the real store")

	// PUBLIC shutdown while physical I/O is in flight: the drain cannot
	// finish (the provider ignores cancellation), so the shutdown reports
	// Forced and RETAINS the host lock, the writer and the manager's owners.
	// Snapshot the manager's synchronized diagnostics while the execution is
	// blocked in physical I/O; forced shutdown must not bulk-release owners.
	ownersBefore, constructionsBefore, _ := h.app.batchInstall.DiskBindings.OwnerDiagnostics()
	require.Positive(t, ownersBefore)
	result := h.app.ShutdownWithResult()
	require.True(t, result.Forced, "work still draining past the bound: forced")
	ownersAfter, constructionsAfter, _ := h.app.batchInstall.DiskBindings.OwnerDiagnostics()
	require.Equal(t, ownersBefore, ownersAfter, "forced shutdown retains manager owners")
	require.Equal(t, constructionsBefore, constructionsAfter, "forced shutdown retains constructions")

	// REAL host oracle: the OS lock is STILL HELD after the forced shutdown
	// (kernel-attested — CloseKeepLock pinned it).
	status := probeHostInChild(t, h.dataDir, h.hostID)
	require.Equal(t, session.HostStatusAlive, status,
		"forced shutdown pins the real host lock (the lock row still has a live holder)")
	// Writer NOT released: the App's own DB handle still answers.
	require.NoError(t, h.app.DB().PingContext(context.Background()),
		"the forced path must not release the DB writer")
	require.Zero(t, h.provider.closeCount(),
		"provider Close is NEVER called, not even on the forced path")
	// The node tree is STILL running: nothing settled beneath the drain
	// (state+shape is the durable live-tree oracle, kind spelling is an
	// internal projection detail): batch root parent NULL + its single
	// child, same shape as above.
	require.NoError(t, h.app.DB().QueryRowContext(context.Background(),
		`SELECT
			count(*) FILTER (WHERE parent_id IS NULL),
			count(*)
		 FROM batch_nodes WHERE owner_session_id = ?1 AND state IN ('pending', 'starting', 'running', 'cancelling')`,
		h.sessionID).Scan(&roots, &nonterminal))
	require.Equal(t, 2, nonterminal, "the forced shutdown preserved the live root and its single child")
	require.Equal(t, 1, roots, "the forced shutdown preserved the live batch root")

	// Housekeeping: release the physical write; the run unwinds and the
	// engine goroutines settle so nothing outlives the test.
	h.provider.letGo()
	select {
	case <-runDone:
		// The run returns once the physical write completes (its own context
		// was cancelled by the shutdown; either outcome is the unwind).
		_ = runErr
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the blocked run never unwound after the gate opened")
	}
}

func probeHostInChild(t *testing.T, dataDir, hostID string) session.HostLockStatus {
	t.Helper()
	// The bounded context is the mechanism that bounds the helper: if the
	// subprocess probe hangs, CombinedOutput returns when the context fires.
	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(probeCtx, os.Args[0], "-test.run=^TestShutdownHostProbeHelper$")
	cmd.Env = append(os.Environ(), "RUSH_SHUTDOWN_HOST_PROBE=1", "RUSH_SHUTDOWN_DATA_DIR="+dataDir, "RUSH_SHUTDOWN_HOST_ID="+hostID)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "subprocess host probe: %s", output)
	// The child is a real `go test` binary: after the helper's own status
	// line the framework prints its bookkeeping (e.g. "PASS"). Scan the
	// lines for the helper's status token instead of requiring the WHOLE
	// output to be the status.
	for _, line := range strings.Split(string(output), "\n") {
		switch strings.TrimSpace(line) {
		case "alive":
			return session.HostStatusAlive
		case "dead":
			return session.HostStatusDead
		}
	}
	t.Fatalf("unexpected subprocess probe output: %q", output)
	return session.HostStatusUnknown
}

func TestShutdownBlockedPhysicalDisposePreservesResources(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	disposeDone := make(chan struct{})
	decorate := func(execution jobbatch.Execution) jobbatch.Execution {
		disposer, hasDisposer := execution.(jobbatch.Disposer)
		output, hasOutput := execution.(jobbatch.OutputReader)
		require.True(t, hasDisposer, "default generic execution exposes physical Dispose")
		require.True(t, hasOutput, "default generic execution exposes OutputReader")
		return &shutdownDisposeGate{Execution: execution, entered: entered, release: release, disposeDone: disposeDone, disposer: disposer, output: output}
	}
	var h *shutdownAcceptanceApp
	h = newShutdownAcceptanceAppWithDecorator(t, true,
		fsWriteScript(func() string { return h.targetPath }, "SHUTDOWN_DISPOSE_OK"), decorate)
	t.Cleanup(func() {
		releaseGate()
		select {
		case <-disposeDone:
		case <-time.After(10 * time.Second):
			t.Errorf("physical Dispose did not finish after gate release")
		}
		session.ReleaseRetainedHostLocksForTest(h.hostID)
	})
	res, err := h.executeRun()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "shutdown-acceptance-write", h.provider.readFile(t, h.targetPath), "real fs_write completed before disposal")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("default execution did not enter physical Dispose")
	}
	ownersBefore, constructionsBefore, _ := h.app.batchInstall.DiskBindings.OwnerDiagnostics()
	require.Positive(t, ownersBefore)
	result := h.app.ShutdownWithResult()
	require.True(t, result.Forced, "shutdown must bound-wait while physical Dispose is blocked")
	ownersAfter, constructionsAfter, _ := h.app.batchInstall.DiskBindings.OwnerDiagnostics()
	require.Equal(t, ownersBefore, ownersAfter, "forced shutdown retains owners through blocked Dispose")
	require.Equal(t, constructionsBefore, constructionsAfter, "forced shutdown retains constructions")
	require.NoError(t, h.app.DB().PingContext(context.Background()), "forced shutdown retains writer")
	require.Equal(t, session.HostStatusAlive, probeHostInChild(t, h.dataDir, h.hostID))
	require.Zero(t, h.provider.closeCount())
	releaseGate()
	select {
	case <-disposeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("physical Dispose did not finish after gate release")
	}
}

type shutdownDisposeGate struct {
	jobbatch.Execution
	entered     chan<- struct{}
	release     <-chan struct{}
	disposeDone chan struct{}
	disposer    jobbatch.Disposer
	output      jobbatch.OutputReader
}

func (e *shutdownDisposeGate) Stop(ctx context.Context) error { return e.Execution.Stop(ctx) }
func (e *shutdownDisposeGate) Dispose(ctx context.Context) error {
	defer close(e.disposeDone)
	select {
	case e.entered <- struct{}{}:
	default:
	}
	<-e.release
	if e.disposer != nil {
		return e.disposer.Dispose(ctx)
	}
	return nil
}

func (e *shutdownDisposeGate) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return e.output.ReadOutput(ctx, cursor)
}

func TestShutdownHostProbeHelper(t *testing.T) {
	if os.Getenv("RUSH_SHUTDOWN_HOST_PROBE") != "1" {
		t.Skip("subprocess helper")
	}
	status, err := session.ProbeHostShared(os.Getenv("RUSH_SHUTDOWN_DATA_DIR"), os.Getenv("RUSH_SHUTDOWN_HOST_ID"))
	require.NoError(t, err)
	fmt.Fprintln(os.Stdout, status.String())
}
