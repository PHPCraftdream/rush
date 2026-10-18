package agent

// R12-H2 chunk D shared acceptance fixtures (design §3 preamble rules).
// ONE file for the helpers shared by the new H2a acceptance tests:
//
//   - h2aGatingDisk: a counting fake DiskProvider whose WriteFile blocks
//     INSIDE the provider I/O on a test gate (and ignores context
//     cancellation, like a physical write that Stop cannot preempt) and
//     carries a Close hook — the borrowed provider's Close is NEVER called
//     by Rush, and the hook makes a violation observable.
//   - newH2aGatedInstall: one real install (real SQLite store, real
//     Runner, Executor nil → the REAL default batchExecutor) whose pinned
//     fs_write capture leaf writes through the gated provider.
//   - h2aGatedInvocation / h2aProbe / h2aTaskStates: the invocation/probe
//     and durable-state readers the acceptance tests share.
//
// Every test using these fixtures drives the NAMED production entry
// points (batchRuntime.Invoke / RunnerHandle.Submit/Stop / the installed
// model controls) and states the production mutation it catches.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// h2aGatingDisk is a lifecycleFakeDisk whose WriteFile can be gated. The
// gate models physical provider I/O that has already started and cannot be
// preempted: while armed, WriteFile ignores ctx cancellation and returns
// only when the gate opens. closeCount records Close hook invocations
// (Rush borrows the provider and never closes it).
type h2aGatingDisk struct {
	lifecycleFakeDisk

	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
	closers int
}

var _ tools.DiskProvider = (*h2aGatingDisk)(nil)

func newH2aGatingDisk() *h2aGatingDisk {
	return &h2aGatingDisk{lifecycleFakeDisk: *newLifecycleFakeDisk()}
}

func (d *h2aGatingDisk) arm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gate == nil {
		d.gate = make(chan struct{})
		d.entered = make(chan struct{}, 1)
	}
}

// enteredCh is signaled by WriteFile AFTER taking the gate and BEFORE the
// blocking wait: an observer of this channel knows the provider I/O is
// physically in flight (inside the provider call), not merely that the leaf
// started. Review P1-7e: the attach-probe boundary must be observed from
// INSIDE the provider call.
func (d *h2aGatingDisk) enteredCh() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entered
}

func (d *h2aGatingDisk) letGo() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gate != nil {
		close(d.gate)
		d.gate = nil
	}
}

// gated reports whether the gate is currently armed; read without blocking.
func (d *h2aGatingDisk) gated() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gate != nil
}

func (d *h2aGatingDisk) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closers
}

// Close is the borrowed provider's Close hook: production code must never
// call it (the App owns the provider; Rush only borrows it).
func (d *h2aGatingDisk) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closers++
	return nil
}

func (d *h2aGatingDisk) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	d.mu.Lock()
	gate, entered := d.gate, d.entered
	d.mu.Unlock()
	if gate != nil {
		// Signal "inside the provider call" first, THEN block: physical I/O
		// in flight; only the gate releases it. Context cancellation (Stop)
		// deliberately does not.
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		<-gate
	}
	return d.lifecycleFakeDisk.WriteFile(ctx, name, data, perm)
}

// h2aGatedInstall is one real install whose single pinned leaf (fs_write)
// writes through the gated provider at the EXACT absolute targetPath,
// preseeded with sentinel content on the real disk.
type h2aGatedInstall struct {
	t                   *testing.T
	store               *session.BatchStore
	conn                *sql.DB
	install             *BatchInstall
	leaf                *lifecycleCaptureLeaf
	fake                *h2aGatingDisk
	registries          *BatchLeafRegistryStore
	bindingID           string
	registryID          string
	fingerprint         string
	targetPath          string
	releaseConstruction func()
	sessionID           string
}

func newH2aGatedInstall(t *testing.T) *h2aGatedInstall {
	return newH2aGatedInstallWithLeafWrap(t, nil)
}

// newH2aGatedInstallWithLeafWrap is newH2aGatedInstall with an optional wrap
// applied to the pinned leaf BEFORE registration and fingerprinting — the
// hook/restriction-counting layers wrap the CAPTURE leaf, so the pinned
// registry entries themselves carry the counters (once per executed leaf).
// The capture leaf stays the innermost tool, so env.leaf's own counters and
// gate keep working.
func newH2aGatedInstallWithLeafWrap(t *testing.T, wrap func(fantasy.AgentTool) fantasy.AgentTool) *h2aGatedInstall {
	t.Helper()
	store, conn, _ := newTestBatchStore(t)
	capture := newLifecycleCaptureLeaf()
	leaf := fantasy.AgentTool(capture)
	if wrap != nil {
		leaf = wrap(leaf)
	}
	fingerprint := batchToolsetFingerprint([]fantasy.AgentTool{leaf})
	deps := installTestDeps(t, store, conn, "unused")
	deps.Executor = nil // the REAL default batchExecutor
	deps.DonePoll = time.Millisecond
	deps.DiskBindings = NewDiskProviderRegistry()
	fake := newH2aGatingDisk()
	binding, err := deps.DiskBindings.RegisterProvider(fake)
	require.NoError(t, err)
	registries := deps.Registries
	require.NoError(t, registries.Register(h2aRegistry, &policyLeafRegistry{
		entries: map[string]policyLeafEntry{"fs_write": {layers: []string{"tool"}, tool: leaf}},
	}))
	releaseConstruction := pinTestConstruction(t, deps.DiskBindings, h2aRegistry)
	install, err := InstallBatchRuntime(context.Background(), deps)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, install.Runner.Close(context.Background()))
	})
	realDir := t.TempDir()
	targetPath := filepath.ToSlash(filepath.Join(realDir, "out.txt"))
	require.NoError(t, os.WriteFile(filepath.FromSlash(targetPath), []byte(h2aSentinel), 0o644))
	return &h2aGatedInstall{
		t: t, store: store, conn: conn, install: install, leaf: capture, fake: fake,
		registries: registries, bindingID: binding, registryID: h2aRegistry,
		fingerprint: fingerprint, targetPath: targetPath,
		releaseConstruction: releaseConstruction,
		sessionID:           "h2a-session-" + filepath.Base(realDir),
	}
}

// h2aGatedInvocation builds a BatchInvocation + ToolCall pair for the env's
// leaf and returns the spec/hash of the VALIDATED (normalized) invocation —
// hash only after Validate normalizes the delivery/escape defaults.
func h2aGatedInvocation(t *testing.T, env *h2aGatedInstall, bindingID, callID, deliveryMode string) (BatchInvocation, fantasy.ToolCall, jobbatch.Spec, string) {
	t.Helper()
	call := fantasy.ToolCall{ID: callID, Name: "fs_write", Input: `{"path":"` + env.targetPath + `"}`}
	inv := BatchInvocation{
		SessionID: env.sessionID, MessageID: "m-1", RegistryID: env.registryID,
		Origin: "coordinator", CallID: callID,
		Delivery: batchDeliveryChoice{Mode: deliveryMode},
		Policy: batchLeafPolicyContext{
			RegistryID: env.registryID, RestrictedToolset: env.fingerprint,
			CustomDisk: "h2a", DiskProvider: true, DiskBindingID: bindingID,
		},
	}
	require.NoError(t, inv.Validate())
	input, err := batchCanonicalInput(call.Input)
	require.NoError(t, err)
	spec, err := batchOneItemSpec(inv, call, input)
	require.NoError(t, err)
	return inv, call, spec, batchInvocationHash(spec)
}

func h2aProbe(env *h2aGatedInstall, hash, callID string) batchAttachProbe {
	return batchAttachProbe{
		Owner: env.sessionID, Host: "host", RunnerID: "runner-1",
		ToolCallID: callID, InputHash: hash,
	}
}

// h2aTaskStates reads every durable node state of a root.
func h2aTaskStates(t *testing.T, store *session.BatchStore, owner string, root jobbatch.ID) []string {
	t.Helper()
	view, err := store.Read(context.Background(), owner, string(root))
	require.NoError(t, err)
	states := make([]string, 0, len(view.Nodes))
	for _, n := range view.Nodes {
		states = append(states, n.State)
	}
	return states
}

func h2aAnyNonTerminal(states []string) bool {
	for _, s := range states {
		if !jobbatch.State(s).Terminal() {
			return true
		}
	}
	return false
}

// h2aLeafInput is the fs_write input for the capture leaf.
func h2aLeafInput(path string) string {
	raw, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// h2a runtime constants shared by the chunk-D acceptance tests.
const (
	h2aRegistry = "h2a-acceptance-reg"
	h2aSentinel = "h2a-sentinel-do-not-touch"
)
