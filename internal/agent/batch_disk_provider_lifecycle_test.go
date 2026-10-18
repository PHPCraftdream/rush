package agent

// R12-H2 chunk 3a: substitute-lifecycle tests rewritten to drive REAL
// production operations. Every test installs the real runtime via
// InstallBatchRuntime with deps.Executor == nil (the real default
// batchExecutor is built — batch_exec.go), a real *jobbatch.Runner, the real
// session.BatchStore attach/done seams, and drives invocations through
// batchRuntime.Invoke (singleflight → attach probe → submit → delivery).
// The pinned policy leaf is a capture leaf registered in a real
// policyLeafRegistry under a real BatchLeafRegistryStore identity; the leaf
// writes through the CallOptions DiskProvider resolved by the REAL executor
// owner path (batch_exec.go Start: the Execution owner key acquired against
// the App capability manager, hasToolset against the pinned construction,
// binding Lookup fail-closed).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers (all local to this file).
// ---------------------------------------------------------------------------

// lifecycleFileInfo is the minimal fs.FileInfo the lifecycle fake returns.
type lifecycleFileInfo struct {
	name  string
	isDir bool
}

func (fi lifecycleFileInfo) Name() string       { return fi.name }
func (fi lifecycleFileInfo) Size() int64        { return 0 }
func (fi lifecycleFileInfo) Mode() fs.FileMode  { return 0o644 }
func (fi lifecycleFileInfo) ModTime() time.Time { return time.Time{} }
func (fi lifecycleFileInfo) IsDir() bool        { return fi.isDir }
func (fi lifecycleFileInfo) Sys() any           { return nil }

// lifecycleFakeDisk is a counting DiskProvider whose writes are observable.
type lifecycleFakeDisk struct {
	mu     sync.Mutex
	files  map[string]string
	dirs   map[string]bool
	calls  int
	writes int
}

var _ tools.DiskProvider = (*lifecycleFakeDisk)(nil)

func newLifecycleFakeDisk() *lifecycleFakeDisk {
	return &lifecycleFakeDisk{files: make(map[string]string), dirs: make(map[string]bool)}
}

func (d *lifecycleFakeDisk) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *lifecycleFakeDisk) writeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writes
}

func (d *lifecycleFakeDisk) has(name string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.files[name]
	return v, ok
}

func (d *lifecycleFakeDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	d.mu.Lock()
	d.calls++
	_, hasFile := d.files[name]
	isDir := d.dirs[name]
	d.mu.Unlock()
	if hasFile {
		return lifecycleFileInfo{name: lifecycleBaseName(name)}, nil
	}
	if isDir {
		return lifecycleFileInfo{name: lifecycleBaseName(name), isDir: true}, nil
	}
	return nil, fs.ErrNotExist
}

func (d *lifecycleFakeDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	return name, nil
}

func (d *lifecycleFakeDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	d.mu.Lock()
	d.calls++
	v, ok := d.files[name]
	d.mu.Unlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(v)), nil
}

func (d *lifecycleFakeDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	d.mu.Lock()
	d.calls++
	v, ok := d.files[name]
	d.mu.Unlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(v), nil
}

func (d *lifecycleFakeDisk) MkdirAll(_ context.Context, name string, _ fs.FileMode) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	for {
		d.dirs[name] = true
		parent := lifecycleDir(name)
		if parent == name {
			return nil
		}
		name = parent
	}
}

func (d *lifecycleFakeDisk) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.writes++
	d.files[name] = string(data)
	return nil
}

func (d *lifecycleFakeDisk) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	delete(d.files, name)
	d.mu.Unlock()
	return nil
}

func (d *lifecycleFakeDisk) List(_ context.Context, _ tools.ListRequest) (tools.ListResult, error) {
	return tools.ListResult{}, nil
}

func (d *lifecycleFakeDisk) Find(_ context.Context, _ tools.FindRequest) (tools.FindResult, error) {
	return tools.FindResult{}, nil
}

func (d *lifecycleFakeDisk) Search(_ context.Context, _ tools.SearchRequest) (tools.DiskSearchResult, error) {
	return tools.DiskSearchResult{}, nil
}

func lifecycleDir(dir string) string {
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			if i == 0 {
				return dir[:1]
			}
			return dir[:i]
		}
	}
	return dir
}

func lifecycleBaseName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' || name[i] == '\\' {
			return name[i+1:]
		}
	}
	return name
}

// lifecycleCaptureLeaf is the pinned policy leaf registered in the test's
// policyLeafRegistry. On Run it reads CallOptions from the executor-built
// run context (callOptionsFrom), then writes through
// CallOptions.DiskProvider — the provider the REAL executor acquired under
// the invocation's binding lease. When armed, Run blocks on the test gate
// BEFORE writing, deliberately IGNORING ctx cancellation: it models a
// provider whose physical I/O return cannot be preempted by Stop.
type lifecycleCaptureLeaf struct {
	mu          sync.Mutex
	gate        chan struct{}
	runs        int
	gateEntries int
	inFlight    chan struct{}
}

var _ fantasy.AgentTool = (*lifecycleCaptureLeaf)(nil)

func newLifecycleCaptureLeaf() *lifecycleCaptureLeaf {
	return &lifecycleCaptureLeaf{inFlight: make(chan struct{}, 8)}
}

func (l *lifecycleCaptureLeaf) arm() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gate = make(chan struct{})
}

func (l *lifecycleCaptureLeaf) letGo() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gate != nil {
		close(l.gate)
		l.gate = nil
	}
}

func (l *lifecycleCaptureLeaf) runCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runs
}

// gateEntryCount reports how many Run invocations reached the provider gate
// (cumulative). Nothing leaves the gate until letGo, so this is also the
// current in-flight count while gated.
func (l *lifecycleCaptureLeaf) gateEntryCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gateEntries
}

func (l *lifecycleCaptureLeaf) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "fs_write", Description: "capture leaf writing through the bound disk provider"}
}

func (l *lifecycleCaptureLeaf) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (l *lifecycleCaptureLeaf) SetProviderOptions(fantasy.ProviderOptions) {}

func (l *lifecycleCaptureLeaf) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	l.mu.Lock()
	l.runs++
	gate := l.gate
	inFlight := l.inFlight
	if gate != nil {
		l.gateEntries++
	}
	l.mu.Unlock()
	select {
	case inFlight <- struct{}{}:
	default:
	}
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.DiskProvider == nil {
		resp := fantasy.NewTextResponse("lifecycle leaf: no bound disk provider in the run context")
		resp.IsError = true
		return resp, errors.New("lifecycle leaf: no bound disk provider")
	}
	if gate != nil {
		// Slow physical return: the provider ignores cancellation until the
		// write physically completes. Only the gate releases it.
		<-gate
	}
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Input), &input); err != nil || input.Path == "" {
		return fantasy.NewTextResponse("lifecycle leaf: bad input"), err
	}
	if err := opts.DiskProvider.WriteFile(ctx, input.Path, []byte("lifecycle-data"), 0o644); err != nil {
		resp := fantasy.NewTextResponse("lifecycle leaf: write failed: " + err.Error())
		resp.IsError = true
		return resp, err
	}
	return fantasy.NewTextResponse("wrote " + input.Path), nil
}

// lifecycleInstall is one real install harness: real Runner (Executor nil →
// real default batchExecutor), real store/attach/done seams, a custom fake
// provider registered in the install's own DiskProviderRegistry, and the
// capture leaf pinned in a real policyLeafRegistry identity.
type lifecycleInstall struct {
	t           *testing.T
	store       *session.BatchStore
	conn        *sql.DB
	install     *BatchInstall
	leaf        *lifecycleCaptureLeaf
	fake        *lifecycleFakeDisk
	registries  *BatchLeafRegistryStore
	bindingID   string
	registryID  string
	fingerprint string
	realDir     string
	// targetPath is the EXACT absolute OS path the leaf writes to; the real
	// disk is preseeded with sentinel content there, so the untouched-disk
	// assertion inspects the actual target, never an unrelated temp dir.
	targetPath          string
	releaseConstruction func()
}

const (
	lifecycleSessionID = "lifecycle-session"
	lifecycleRegistry  = "lifecycle-reg"
	lifecycleCallID    = "call-1"
	lifecycleSentinel  = "lifecycle-sentinel-do-not-touch"
)

// newLifecycleInstall builds one real install. The registry identity carries
// ONLY the capture leaf; the restricted-toolset fingerprint is derived with
// the production batchToolsetFingerprint over the registered tool list, so
// hasToolset at batch_exec.go:121 is satisfied without bypassing it.
func newLifecycleInstall(t *testing.T) *lifecycleInstall {
	t.Helper()
	store, conn, _ := newTestBatchStore(t)
	leaf := newLifecycleCaptureLeaf()
	fingerprint := batchToolsetFingerprint([]fantasy.AgentTool{leaf})
	deps := installTestDeps(t, store, conn, "unused")
	// Executor nil: newBatchInstall builds the REAL default batchExecutor
	// (generic/MCP/shell/argv/agent families) — the production Start path
	// that acquires the binding and registry leases.
	deps.Executor = nil
	deps.DonePoll = time.Millisecond
	deps.DiskBindings = NewDiskProviderRegistry()
	fake := newLifecycleFakeDisk()
	binding, err := deps.DiskBindings.RegisterProvider(fake)
	require.NoError(t, err)
	registries := deps.Registries
	require.NoError(t, registries.Register(lifecycleRegistry, &policyLeafRegistry{
		entries: map[string]policyLeafEntry{"fs_write": {layers: []string{"tool"}, tool: leaf}},
	}))
	// R12-H2 §1.2: the executor's Execution owner (and the root lease) name
	// the pinned registry's construction record. Hold its Construction owner
	// exactly as the trusted call state's build scope does; tests release it
	// explicitly before asserting deletion.
	releaseConstruction := pinTestConstruction(t, deps.DiskBindings, lifecycleRegistry)
	install, err := InstallBatchRuntime(context.Background(), deps)
	require.NoError(t, err)
	t.Cleanup(func() {
		if install.Runner != nil {
			require.NoError(t, install.Runner.Close(context.Background()))
		}
	})
	// Registered AFTER the Close cleanup so LIFO runs it BEFORE Close: on an
	// assertion failure the leaf can still be parked in its gate and Close
	// must never wait behind a gated execution. letGo is idempotent.
	t.Cleanup(func() { leaf.letGo() })
	// The EXACT absolute target the leaf writes to, preseeded with sentinel
	// content on the real disk.
	realDir := t.TempDir()
	targetPath := filepath.ToSlash(filepath.Join(realDir, "out.txt"))
	require.NoError(t, os.WriteFile(filepath.FromSlash(targetPath), []byte(lifecycleSentinel), 0o644))
	return &lifecycleInstall{
		t: t, store: store, conn: conn, install: install, leaf: leaf, fake: fake,
		registries: registries, bindingID: binding, registryID: lifecycleRegistry,
		fingerprint: fingerprint, realDir: realDir, targetPath: targetPath,
		releaseConstruction: releaseConstruction,
	}
}

// newSecondRuntime builds a second REAL install over the SAME durable store
// with its own Runner, registries and root-lease store (a fresh process
// incarnation): its singleflight map and root-lease entries are empty, so
// its Invoke reaches invokeFresh's durable attach probe against the live
// root started by the first install.
func newSecondRuntime(t *testing.T, first *lifecycleInstall) *BatchInstall {
	t.Helper()
	deps := installTestDeps(t, first.store, first.conn, "unused")
	deps.Executor = nil
	deps.DonePoll = time.Millisecond
	deps.DiskBindings = NewDiskProviderRegistry()
	install, err := InstallBatchRuntime(context.Background(), deps)
	require.NoError(t, err)
	// Cleanup tolerates ONLY the proven identity-collision artifact of this
	// fixture: the second runner reuses the FIRST runtime's (Host="host",
	// RunnerID="runner-1") because BatchStore.BatchProbeLive fences the
	// durable attach strictly by identical Host AND RunnerID
	// (internal/session/batch_adapters.go: live.RunnerID != p.RunnerID →
	// ErrBatchRunnerMismatch), so a fresh RunnerID would make the foreign
	// attach MISS the live root instead of reaching RootLeases.matches. On
	// Close the runner applies EventHostClosing
	// (internal/jobbatch/runner.go:232) whose commit runs the session hook
	// (internal/session/batch_commit.go batchCommitProject), which always
	// upserts the (host, runner) checkpoint row
	// (internal/db/batch_commit.sql.go UpdateBatchRunnerCheckpoint: ON
	// CONFLICT (host_id, runner_id) ... WHERE excluded.sequence >
	// batch_runner_checkpoints.sequence). The FIRST runtime already wrote a
	// higher sequence under the same (host, runner-1) key, so this Close's
	// checkpoint upsert affects 0 rows → ErrBatchProjectionConflict
	// (internal/session/batch_store.go Project/batchRequireRow) → the hook
	// errors → the runner commits reason "error" → Close returns
	// ErrCommitFailed. This asserts nothing about production behavior: a
	// real second incarnation never shares (Host, RunnerID) with a live one.
	t.Cleanup(func() {
		if install.Runner != nil {
			if err := install.Runner.Close(context.Background()); err != nil {
				require.ErrorContains(t, err, "commit failed")
			}
		}
	})
	return install
}

// lifecycleInvocation builds the BatchInvocation + ToolCall pair an Invoke
// drives, plus the exact spec/hash the production ingress derives
// (batchOneItemSpec + batchInvocationHash) so tests can probe the durable
// attach identity of the call.
func lifecycleInvocation(t *testing.T, env *lifecycleInstall, bindingID, callID, deliveryMode string) (BatchInvocation, fantasy.ToolCall, jobbatch.Spec, string) {
	t.Helper()
	call := fantasy.ToolCall{ID: callID, Name: "fs_write", Input: `{"path":"` + env.targetPath + `"}`}
	inv := BatchInvocation{
		SessionID: lifecycleSessionID, MessageID: "m-1", RegistryID: env.registryID,
		Origin: "coordinator", CallID: callID,
		Delivery: batchDeliveryChoice{Mode: deliveryMode},
		Policy: batchLeafPolicyContext{
			RegistryID: env.registryID, RestrictedToolset: env.fingerprint,
			CustomDisk: "lifecycle", DiskProvider: true, DiskBindingID: bindingID,
		},
	}
	// Hash ONLY the normalized invocation: Validate canonicalises the
	// delivery mode, escape policy and identity values once; the runtime
	// hashes the same normalized form (batch_invocation.go).
	require.NoError(t, inv.Validate())
	input, err := batchCanonicalInput(call.Input)
	require.NoError(t, err)
	spec, err := batchOneItemSpec(inv, call, input)
	require.NoError(t, err)
	hash := batchInvocationHash(spec)
	return inv, call, spec, hash
}

// lifecycleProbe builds the live-attach probe for one call identity.
func lifecycleProbe(hash, callID string) batchAttachProbe {
	return batchAttachProbe{
		Owner: lifecycleSessionID, Host: "host", RunnerID: "runner-1",
		ToolCallID: callID, InputHash: hash,
	}
}

// lifecycleTaskNodeState reads the durable task-node state of a root.
func lifecycleTaskNodeState(t *testing.T, store *session.BatchStore, root jobbatch.ID) string {
	t.Helper()
	view, err := store.Read(context.Background(), lifecycleSessionID, string(root))
	require.NoError(t, err)
	require.Len(t, view.Nodes, 2, "one-item root: batch root + task child")
	return view.Nodes[1].State
}

// ---------------------------------------------------------------------------
// Test 1 (items 1/5): the REAL executor path writes through the CUSTOM
// provider, holds the binding + registry leases while the root is live, and
// releases both once the root settles and its execution disposes.
// ---------------------------------------------------------------------------

// Production path exercised: InstallBatchRuntime (Executor nil → real
// batchExecutor) → batchRuntime.Invoke → invokeFresh → submit (SubmitBound,
// root-lease acquire/adopt) → jobbatch engine → batchExecutor.Start
// (Execution owner key + hasToolset + fail-closed binding Lookup) →
// batchGenericFamily.start → batchGenericExecution.run → leaf Run →
// CallOptions.DiskProvider.WriteFile.
func TestBatchGenericLeafWritesThroughCustomProvider(t *testing.T) {
	t.Parallel()
	// setup: real install, capture leaf armed so the root stays live.
	env := newLifecycleInstall(t)
	env.leaf.arm()
	inv, call, _, hash := lifecycleInvocation(t, env, env.bindingID, lifecycleCallID, batchDeliverySync)

	respCh := make(chan fantasy.ToolResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := env.install.Runtime.Invoke(context.Background(), inv, env.leaf, call)
		respCh <- resp
		errCh <- err
	}()
	select {
	case <-env.leaf.inFlight:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "leaf never reached its gate through the real executor")
	}

	// FIRST real assertion: mid-run, the executor's Execution owner keeps the
	// custom provider resolvable and the pinned registry identity pinned.
	// The live root attaches through the same durable attach store the
	// ingress probe uses; its disposal is automatic (adopted Done watcher),
	// so the fixture never Releases it.
	_, attached, err := env.install.Attach.ProbeLive(context.Background(), lifecycleProbe(hash, lifecycleCallID))
	require.NoError(t, err)
	require.True(t, attached, "live root attaches during execution")
	_, err = env.install.DiskBindings.Lookup(env.bindingID)
	require.NoError(t, err, "binding must stay resolvable while the root is live (Execution owner held)")
	_, err = env.registries.Resolve(env.registryID)
	require.NoError(t, err, "pinned registry must stay resolvable while the root is live")

	// production mutation caught: if the executor skipped its Execution
	// owner/binding Lookup (or released at Start), the fake would never be
	// handed to the leaf and the owner would drop before dispose.
	env.leaf.letGo()
	var resp fantasy.ToolResponse
	require.Eventually(t, func() bool {
		select {
		case resp = <-respCh:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "sync Invoke delivers the leaf envelope")
	require.NoError(t, func() error {
		err := <-errCh
		root2, att2, e2 := env.install.Attach.ProbeLive(context.Background(), lifecycleProbe(hash, lifecycleCallID))
		t.Logf("DEBUG resp=%q isErr=%v err=%v root=%q att=%v perr=%v", resp.Content, resp.IsError, err, root2, att2, e2)
		if att2 {
			t.Logf("DEBUG nodeState=%q", lifecycleTaskNodeState(t, env.store, root2))
		}
		return err
	}())
	require.False(t, resp.IsError, "leaf response: %s", resp.Content)
	require.Equal(t, "wrote "+env.targetPath, resp.Content)

	// the write went through the CUSTOM provider; the EXACT absolute OS
	// target (preseeded with sentinel content) is untouched.
	got, ok := env.fake.has(env.targetPath)
	require.True(t, ok, "custom provider must hold the written file")
	require.Equal(t, "lifecycle-data", got)
	sentinel, err := os.ReadFile(filepath.FromSlash(env.targetPath))
	require.NoError(t, err)
	require.Equal(t, lifecycleSentinel, string(sentinel),
		"the exact OS target the leaf wrote to must be untouched by any OS fallback")
	require.Equal(t, 1, env.leaf.runCount(), "exactly one leaf execution")

	// the settled root no longer attaches (durable fence): BatchProbeLive
	// reports ("", false, nil) when no live row exists.
	root, attached, err := env.install.Attach.ProbeLive(context.Background(), lifecycleProbe(hash, lifecycleCallID))
	require.NoError(t, err)
	require.False(t, attached, "settled root no longer attaches")
	require.Empty(t, root, "no live row: BatchProbeLive returns the empty root id")

	// after settle + execution dispose the owners are gone: release THIS
	// fixture's own Construction owner too, then the binding's owner set is
	// empty (binding deleted) and no owner names the registry (construction
	// record deleted, identity index dropped).
	//
	// REAL execution-owner release trigger (R12-H2 §1.2): jobbatch disposes
	// a settled root's execution when the root is RELEASED — the adopted
	// Done watcher (batchRootLeaseStore.adopt) calls the Runner's Release
	// AUTOMATICALLY once Done closes, so the fixture must NOT Release the
	// adopted root itself: it only releases its own Construction owner and
	// observes the automatic disposal drop the Execution/binding owners.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding must become unavailable after dispose")
	require.Eventually(t, func() bool {
		_, err := env.registries.Resolve(env.registryID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "pinned registry must be released after dispose")
}

// ---------------------------------------------------------------------------
// Test 2 (item 6): real Runner.Stop acknowledges cancellation, but the node
// does NOT settle until the provider's physical write returns.
// ---------------------------------------------------------------------------

// Production path exercised: batchRunnerAdapter.Stop → *jobbatch.Runner.Stop
// → engine cancellation → batchGenericExecution.Stop (cancel run context);
// the gated capture leaf ignores the cancellation and settles only after the
// physical WriteFile returns.
func TestBatchStopHoldsUntilProviderPhysicallyReturns(t *testing.T) {
	t.Parallel()
	env := newLifecycleInstall(t)
	env.leaf.arm()
	inv, call, _, hash := lifecycleInvocation(t, env, env.bindingID, lifecycleCallID, batchDeliverySync)

	respCh := make(chan fantasy.ToolResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := env.install.Runtime.Invoke(context.Background(), inv, env.leaf, call)
		respCh <- resp
		errCh <- err
	}()
	select {
	case <-env.leaf.inFlight:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "leaf never reached its gate")
	}

	// The live root is found through the durable attach store (the same
	// fences the ingress probe uses): this is how the test learns the
	// runner-minted root id.
	root, attached, err := env.install.Attach.ProbeLive(context.Background(), lifecycleProbe(hash, lifecycleCallID))
	require.NoError(t, err)
	require.True(t, attached, "live root attaches during execution")

	// FIRST real assertion: Stop returns an ack while the provider is still
	// physically blocked.
	require.NoError(t, env.install.RunnerHandle.Stop(context.Background(), root))

	// production mutation caught: if Stop forced settlement before the
	// physical return, the node would already be terminal here.
	state := lifecycleTaskNodeState(t, env.store, root)
	require.False(t, jobbatch.State(state).Terminal(),
		"node must stay live after the Stop ack while the provider is blocked (state %q)", state)
	require.Zero(t, env.fake.writeCount(), "no write may have landed while the provider is blocked")
	select {
	case resp := <-respCh:
		require.FailNow(t, "no response may be delivered before the physical return", "%s", resp.Content)
	default:
	}

	// Physical return: the write lands, THEN the node settles and the
	// single response is delivered.
	env.leaf.letGo()
	var resp fantasy.ToolResponse
	require.Eventually(t, func() bool {
		select {
		case resp = <-respCh:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "the one response is delivered after the physical return")
	require.NoError(t, <-errCh, "sync Invoke survives a Stop that the provider outlives")
	require.Equal(t, "wrote "+env.targetPath, resp.Content)
	require.False(t, resp.IsError,
		"the physical return still delivers the leaf's own response envelope (the durable NODE state is a separate surface: the engine deterministically projects a stopped tree as cancelled — engine.go EventSettled reapplies causeState(CauseStop) over the physical report)")
	require.Equal(t, 1, env.leaf.runCount(), "exactly one leaf execution despite Stop")
	_, ok := env.fake.has(env.targetPath)
	require.True(t, ok, "the physical write landed through the custom provider")
	require.Eventually(t, func() bool {
		return jobbatch.State(lifecycleTaskNodeState(t, env.store, root)).Terminal()
	}, 5*time.Second, time.Millisecond, "the node settles only after the provider returns")
}

// ---------------------------------------------------------------------------
// Test 3 (item 7): detached delivery returns while the execution is live;
// the binding lease is retained until the root settles and disposes.
// ---------------------------------------------------------------------------

// Production path exercised: batchRuntime.Invoke with Delivery.Mode detached
// → deliverDetached (ElectStarted + started response, no wait); the engine
// keeps executing; the executor's Execution owner survives delivery return
// and drops only on the wrapped execution's Dispose after settlement.
func TestBatchDetachedDeliveryRetainsLeaseUntilDispose(t *testing.T) {
	t.Parallel()
	env := newLifecycleInstall(t)
	env.leaf.arm()
	inv, call, _, hash := lifecycleInvocation(t, env, env.bindingID, lifecycleCallID, batchDeliveryDetached)

	resp, err := env.install.Runtime.Invoke(context.Background(), inv, env.leaf, call)
	require.NoError(t, err)
	// FIRST real assertion: detached delivery returned the started response
	// while the execution is still live behind the gate.
	require.True(t, strings.HasPrefix(resp.Content, "batch root ") && strings.HasSuffix(resp.Content, batchStartedResponseSuffix),
		"detached delivery must return the started response, got %q", resp.Content)

	// production mutation caught: if detached delivery disposed the leases
	// at return (or the executor released at Start), the binding would stop
	// resolving while the execution is still running.
	root, attached, err := env.install.Attach.ProbeLive(context.Background(), lifecycleProbe(hash, lifecycleCallID))
	require.NoError(t, err)
	require.True(t, attached, "the detached root stays live and attachable")
	require.False(t, jobbatch.State(lifecycleTaskNodeState(t, env.store, root)).Terminal(),
		"the task node is still running when detached delivery returns")
	_, err = env.install.DiskBindings.Lookup(env.bindingID)
	require.NoError(t, err, "the binding is retained while the detached execution runs")

	// Completion arrives through the real engine; only after settle + dispose
	// does the lease drop. No hand-closed channels anywhere.
	env.leaf.letGo()
	require.Eventually(t, func() bool {
		return jobbatch.State(lifecycleTaskNodeState(t, env.store, root)).Terminal()
	}, 5*time.Second, time.Millisecond, "the detached root settles after the leaf completes")
	require.Equal(t, 1, env.leaf.runCount())
	_, ok := env.fake.has(env.targetPath)
	require.True(t, ok, "the detached execution wrote through the custom provider")
	// REAL execution-owner release trigger (mirrors the sync sibling above):
	// jobbatch disposes a settled root's execution when the root is RELEASED
	// and the adopted Done watcher (batchRootLeaseStore.adopt) calls the
	// Runner's Release AUTOMATICALLY once Done closes — no manual
	// Runner.Release for the adopted root. `root` was captured mid-run
	// through the durable attach store before the settle.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding becomes unavailable after settle + dispose")
	require.Eventually(t, func() bool {
		_, err := env.registries.Resolve(env.registryID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "pinned registry released after dispose")
}

// ---------------------------------------------------------------------------
// Test 4 (item 4): the durable attach identity rejects a different disk
// binding on a live root, and a same-binding concurrent retry joins the
// in-flight execution without a second leaf run.
// ---------------------------------------------------------------------------

// Production path exercised: batchRuntime.Invoke → invokeOnce (singleflight
// join on session+call identity) and → invokeFresh (durable ProbeLive attach
// → RootLeases.matches(binding, registry) → ErrDiskProviderUnavailable).
//
// In-process note: a second Invoke on the SAME runtime while the first call
// is in flight cannot reach invokeFresh's attach-mismatch branch — the
// in-process singleflight entry (session, callID) intercepts it by design
// (a different payload hash is rejected as errBatchInvocationMismatch). The
// live-attach-mismatch branch is the cross-process retry, so this test runs
// the retry through a SECOND real runtime (fresh Runner, registries and
// root-lease store) over the SAME durable store — exactly the restart
// scenario the branch exists for. The same-binding concurrent retry uses the
// owning runtime and joins singleflight.
func TestBatchAttachIdentityRejectsDifferentBinding(t *testing.T) {
	t.Parallel()
	env := newLifecycleInstall(t)
	env.leaf.arm()
	inv, call, _, hash := lifecycleInvocation(t, env, env.bindingID, lifecycleCallID, batchDeliverySync)

	// Live root #1 through the real ingress on the owning runtime.
	resp1Ch := make(chan fantasy.ToolResponse, 1)
	err1Ch := make(chan error, 1)
	go func() {
		resp, err := env.install.Runtime.Invoke(context.Background(), inv, env.leaf, call)
		resp1Ch <- resp
		err1Ch <- err
	}()
	select {
	case <-env.leaf.inFlight:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "leaf never reached its gate")
	}
	root, attached, err := env.install.Attach.ProbeLive(context.Background(), lifecycleProbe(hash, lifecycleCallID))
	require.NoError(t, err)
	require.True(t, attached, "the live root is attachable")

	// (a) Same-runtime concurrent retry on the SAME live call identity with
	// a DIFFERENT registered binding (the payload carries the other binding
	// id, so the spec hash differs): the in-process singleflight entry for
	// (session, callID) still lives, and a different hash is a typed
	// mismatch — no second submit, no second leaf run.
	otherBinding := lifecycleOtherBindingID(t, env)
	mismatchInv, mismatchCall, _, mismatchHash := lifecycleInvocation(t, env, otherBinding, lifecycleCallID, batchDeliverySync)
	require.NotEqual(t, hash, mismatchHash, "a different binding id must change the spec hash")
	mismatchDone := make(chan error, 1)
	go func() {
		_, err := env.install.Runtime.Invoke(context.Background(), mismatchInv, env.leaf, mismatchCall)
		mismatchDone <- err
	}()
	select {
	case err := <-mismatchDone:
		require.ErrorIs(t, err, errBatchInvocationMismatch,
			"a live call identity with a different payload hash is a typed mismatch")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "mismatched retry must fail fast")
	}
	require.Equal(t, 1, env.leaf.runCount(), "no second leaf run for the mismatched retry")
	require.Zero(t, env.fake.writeCount(), "no write landed from the mismatched retry")

	// (b) Cross-incarnation live-attach mismatch: a SECOND real runtime over
	// the same store attaches to the SAME live root (same call id + same
	// spec hash), but its root-lease store holds no entry for the root, so
	// invokeFresh's attached && !matches check refuses the binding.
	second := newSecondRuntime(t, env)
	retryInv, retryCall, _, retryHash := lifecycleInvocation(t, env, env.bindingID, lifecycleCallID, batchDeliverySync)
	require.Equal(t, hash, retryHash, "same binding + input reproduce the attach hash")
	foreignDone := make(chan error, 1)
	go func() {
		_, err := second.Runtime.Invoke(context.Background(), retryInv, env.leaf, retryCall)
		foreignDone <- err
	}()
	select {
	case err := <-foreignDone:
		// FIRST real assertion on the attach path: typed provider refusal.
		require.ErrorIs(t, err, ErrDiskProviderUnavailable,
			"a live root bound to another incarnation's lease rejects a foreign attach")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "foreign attach must be rejected promptly")
	}
	require.Zero(t, env.fake.writeCount(), "no second write on the custom provider after the failed attach")
	require.Equal(t, 1, env.leaf.runCount(), "the foreign attach never executed a leaf")

	// (c) Positive: a same-binding concurrent retry on the OWNING runtime
	// joins the singleflight execution and receives the same response
	// without an extra leaf run.
	retryRespCh := make(chan fantasy.ToolResponse, 1)
	retryErrCh := make(chan error, 1)
	go func() {
		resp, err := env.install.Runtime.Invoke(context.Background(), retryInv, env.leaf, retryCall)
		retryRespCh <- resp
		retryErrCh <- err
	}()

	// Release the physical write; both waiters get the one response.
	env.leaf.letGo()
	var resp1, respRetry fantasy.ToolResponse
	require.Eventually(t, func() bool {
		select {
		case resp1 = <-resp1Ch:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "first caller delivered")
	require.Eventually(t, func() bool {
		select {
		case respRetry = <-retryRespCh:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "same-binding joiner delivered")
	require.Equal(t, resp1, respRetry, "the joiner receives the first caller's exact envelope")
	require.Equal(t, "wrote "+env.targetPath, resp1.Content)
	require.Equal(t, 1, env.leaf.runCount(), "singleflight: still exactly one leaf execution")
	require.Equal(t, 1, env.fake.writeCount(), "exactly one physical write")
	require.Eventually(t, func() bool {
		return jobbatch.State(lifecycleTaskNodeState(t, env.store, root)).Terminal()
	}, 5*time.Second, time.Millisecond, "the root settles after the physical return")
	require.NoError(t, <-err1Ch)
	require.NoError(t, <-retryErrCh)
}

// lifecycleOtherBindingID registers a second custom provider on the install
// registry and returns its binding id (a separately registered binding).
func lifecycleOtherBindingID(t *testing.T, env *lifecycleInstall) string {
	t.Helper()
	id, err := env.install.DiskBindings.RegisterProvider(newLifecycleFakeDisk())
	require.NoError(t, err)
	return id
}
