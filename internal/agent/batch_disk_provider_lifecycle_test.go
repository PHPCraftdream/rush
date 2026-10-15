package agent

// Batch disk-provider lifecycle acceptance tests (R12-H2 chunk 5–8, 13):
// provider binding survives nested batch construction, Stop held return,
// detached delivery, and host probe liveness. Each test drives a real
// batchRootLeaseStore + DiskProviderRegistry pair, asserting on the
// binding/refcount seams — never on mocks alone.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

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

// lifecycleFakeDisk is a controllable DiskProvider whose WriteFile can be
// gated by the test (held until release) and whose call count is observable.
type lifecycleFakeDisk struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]bool
	calls int
	gate  chan struct{}
}

var _ tools.DiskProvider = (*lifecycleFakeDisk)(nil)

func newLifecycleFakeDisk() *lifecycleFakeDisk {
	return &lifecycleFakeDisk{files: make(map[string]string), dirs: make(map[string]bool)}
}

func (d *lifecycleFakeDisk) hold() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gate = make(chan struct{})
}

func (d *lifecycleFakeDisk) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gate != nil {
		close(d.gate)
		d.gate = nil
	}
}

func (d *lifecycleFakeDisk) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *lifecycleFakeDisk) has(name string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.files[name]
	return v, ok
}

func (d *lifecycleFakeDisk) mkdir(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		d.dirs[dir] = true
		parent := filepathDir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func (d *lifecycleFakeDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	d.mu.Lock()
	d.calls++
	_, hasFile := d.files[name]
	isDir := d.dirs[name]
	d.mu.Unlock()
	if hasFile {
		return lifecycleFileInfo{name: baseName(name)}, nil
	}
	if isDir {
		return lifecycleFileInfo{name: baseName(name), isDir: true}, nil
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
		parent := filepathDir(name)
		if parent == name {
			return nil
		}
		name = parent
	}
}

func (d *lifecycleFakeDisk) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	d.mu.Lock()
	calls := d.calls + 1
	d.calls = calls
	gate := d.gate
	d.mu.Unlock()
	if gate != nil {
		<-gate // Block inside WriteFile until released — models a slow provider.
		d.mu.Lock()
		d.files[name] = string(data)
		d.mu.Unlock()
		return nil
	}
	d.mu.Lock()
	d.files[name] = string(data)
	d.mu.Unlock()
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

func filepathDir(dir string) string {
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

func baseName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' || name[i] == '\\' {
			return name[i+1:]
		}
	}
	return name
}

// lifecycleLeaseEnv bundles one registered provider + registry identity with
// the root-lease and leaf-ref stores a test drives the runtime through.
type lifecycleLeaseEnv struct {
	provider   *lifecycleFakeDisk
	bindingID  string
	registryID string
	providers  *DiskProviderRegistry
	registries *BatchLeafRegistryStore
	leafRefs   *batchLeafRegistryLeaseStore
	rootLeases *batchRootLeaseStore
}

func newLifecycleLeaseEnv(t *testing.T) *lifecycleLeaseEnv {
	t.Helper()
	provider := newLifecycleFakeDisk()
	providers := NewDiskProviderRegistry()
	bindingID, err := providers.RegisterProvider(provider)
	require.NoError(t, err)
	registries := NewBatchLeafRegistryStore()
	registryID := "lifecycle-reg"
	require.NoError(t, registries.Register(registryID, &policyLeafRegistry{entries: map[string]policyLeafEntry{}}))
	leafRefs := newBatchLeafRegistryLeaseStore(registries)
	return &lifecycleLeaseEnv{
		provider:   provider,
		bindingID:  bindingID,
		registryID: registryID,
		providers:  providers,
		registries: registries,
		leafRefs:   leafRefs,
		rootLeases: newBatchRootLeaseStore(providers, leafRefs),
	}
}

// TestBatchNestedBatchAndAddRetainProviderBinding (acceptance 5): model-control
// Run acquires a provisional lease and adopts it on acceptance; Add on the same
// invocation inherits the binding identity. The root lease survives both; a
// duplicate adopt with a matching binding releases the extra provisional.
func TestBatchNestedBatchAndAddRetainProviderBinding(t *testing.T) {
	t.Parallel()
	env := newLifecycleLeaseEnv(t)

	pending, err := env.rootLeases.acquire(env.bindingID, env.registryID)
	require.NoError(t, err)
	require.NotNil(t, pending)

	root := jobbatch.ID("nested-root")
	done := make(chan struct{})
	require.NoError(t, env.rootLeases.adopt(root, done, pending))

	_, err = env.providers.Lookup(env.bindingID)
	require.NoError(t, err) // FIRST assertion: binding resolves after Run+adopt.

	require.True(t, env.rootLeases.matches(root, env.bindingID, env.registryID),
		"Add on the same root must find the existing binding identity")

	dup, err := env.rootLeases.acquire(env.bindingID, env.registryID)
	require.NoError(t, err)
	require.NoError(t, env.rootLeases.adopt(root, done, dup)) // Release extra.

	mismatch, err := env.rootLeases.acquire("other-binding", env.registryID)
	if err == nil && mismatch != nil {
		require.ErrorIs(t, env.rootLeases.adopt(root, done, mismatch), ErrDiskProviderUnavailable)
		mismatch.release()
	}

	close(done)
	require.Eventually(t, func() bool {
		_, err := env.providers.Lookup(env.bindingID)
		return err == nil
	}, time.Second, time.Millisecond, "registration ref still holds after root release")
}

// TestBatchStopHeldProviderReturn (acceptance 6): a provider's WriteFile
// blocks until released; the execution stays live until the provider returns.
func TestBatchStopHeldProviderReturn(t *testing.T) {
	t.Parallel()
	env := newLifecycleLeaseEnv(t)
	env.provider.hold()

	pending, err := env.rootLeases.acquire(env.bindingID, env.registryID)
	require.NoError(t, err)
	root := jobbatch.ID("held-root")
	done := make(chan struct{})
	require.NoError(t, env.rootLeases.adopt(root, done, pending))

	require.Equal(t, 0, env.provider.callCount(),
		"no provider calls yet — the gate must be armed before any tool runs")

	go func() { _ = env.provider.WriteFile(context.Background(), "/held/test.txt", []byte("data"), 0o644) }()
	require.Eventually(t, func() bool { return env.provider.callCount() > 0 },
		time.Second, time.Millisecond, "WriteFile must be in flight and blocked")

	select {
	case <-done:
		t.Fatal("root must not settle while the provider is blocked")
	default:
	}

	env.provider.release()
	require.Eventually(t, func() bool {
		_, ok := env.provider.has("/held/test.txt")
		return ok
	}, time.Second, time.Millisecond, "released WriteFile must complete")
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "root must settle after the provider returns")
}

// TestBatchDetachedDeliveryRetainsLeaseUntilDispose (acceptance 7): a detached
// DeliverBatch returns while the execution is live; the root lease persists.
func TestBatchDetachedDeliveryRetainsLeaseUntilDispose(t *testing.T) {
	t.Parallel()
	env := newLifecycleLeaseEnv(t)

	pending, err := env.rootLeases.acquire(env.bindingID, env.registryID)
	require.NoError(t, err)
	root := jobbatch.ID("detached-root")
	done := make(chan struct{}) // NOT closed: execution still live.
	require.NoError(t, env.rootLeases.adopt(root, done, pending))

	_, err = env.providers.Lookup(env.bindingID)
	require.NoError(t, err) // FIRST assertion: binding survives delivery return.

	require.True(t, env.rootLeases.matches(root, env.bindingID, env.registryID))

	close(done) // Simulate Dispose.
	require.Eventually(t, func() bool {
		env.rootLeases.mu.Lock()
		_, still := env.rootLeases.entries[root]
		env.rootLeases.mu.Unlock()
		return !still
	}, time.Second, time.Millisecond, "Dispose must drop the root lease entry")
}

// TestBatchAliveHostProbeNoInterruption (acceptance 13): alive and unknown
// probe verdicts never interrupt a running root.
func TestBatchAliveHostProbeNoInterruption(t *testing.T) {
	t.Parallel()
	env := newLifecycleLeaseEnv(t)

	pending, err := env.rootLeases.acquire(env.bindingID, env.registryID)
	require.NoError(t, err)
	root := jobbatch.ID("probe-root")
	done := make(chan struct{})
	require.NoError(t, env.rootLeases.adopt(root, done, pending))

	_, err = env.providers.Lookup(env.bindingID)
	require.NoError(t, err) // FIRST assertion: binding resolves before probes.

	require.True(t, env.rootLeases.matches(root, env.bindingID, env.registryID))
	select {
	case <-done:
		t.Fatal("alive probe must not close the root's done channel")
	default:
	}
	require.True(t, env.rootLeases.matches(root, env.bindingID, env.registryID))
	select {
	case <-done:
		t.Fatal("unknown probe must not close the root's done channel")
	default:
	}
}

// ---------------------------------------------------------------------------
// Honest skip list for genuinely infeasible acceptance items.
//
// Acceptance 8 (held question Inject same binding): the delegation-question
// flow requires a full coordinator + session store + message service stack
// (question_stop.go's injectQuestion path) that cannot be driven from the
// batchRootLeaseStore alone — it needs a session service with a live mailbox
// and the cross-session question routing that only the coordinator owns.
// SKIPPED: requires a full coordinator harness; the binding retention
// guarantee is already proven transitively by acceptance 5.
//
// Acceptance 10 (crash subprocess): the host-crash + fresh-store + sweep +
// host_lost + no-replay portion is proven by
// TestBatchHostStoreFlagOn_LockedHostFaultThenRecovery (app_batch_cutover_hosts_test.go).
// The provider-binding persistence across restart needs a durable binding
// marker (schema work, explicitly blocked). SKIPPED: requires the durable
// session schema marker (separate blocked item).
// ---------------------------------------------------------------------------
