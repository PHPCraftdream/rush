package agent

import (
	"context"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type batchBoundRunnerHandle interface {
	SubmitBound(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error)
}

type batchReleaseHandle interface {
	Release(context.Context, jobbatch.ID) error
}

type batchModelRunner interface {
	Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error)
	Add(ctx context.Context, req jobbatch.AddRequest) ([]jobbatch.ID, error)
	Snapshot(ctx context.Context, id jobbatch.ID) (jobbatch.BatchView, error)
	View(ctx context.Context, id jobbatch.ID) (jobbatch.NodeView, error)
	Output(ctx context.Context, id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, error)
	Stop(ctx context.Context, id jobbatch.ID) error
}

// batchDelivery is the narrow R11D seam: deliver this started root per the
// invocation's mode/window and return its tool response. A nil seam is a
// typed not-wired error, never a silent fallback to the old path.
type batchDelivery interface {
	DeliverBatch(ctx context.Context, root jobbatch.ID, inv BatchInvocation) (fantasy.ToolResponse, error)
}

// batchRuntimeConfig specializes the core wiring for root compatibility types.
type batchRuntimeConfig = batchcore.RuntimeConfig[*BatchProfile, batchDelivery, BatchWorkIngress, *DiskProviderRegistry, *batchRootLeaseStore]

// batchRuntime is the dormant R11C ingress core. Construction has NO side
// effects: no goroutine, no executor, no global registration; R11F owns every
// wiring. It is inert until something calls Invoke.
type batchRuntime struct {
	cfg batchRuntimeConfig

	mu   sync.Mutex
	core *batchcore.Ingress
}

func newBatchRuntime(cfg batchRuntimeConfig) (*batchRuntime, error) {
	core, err := cfg.InitIngress(func(profile *BatchProfile) bool {
		return profile != nil && profile.caps != nil
	}, errBatchAttachNotWired, errBatchProfileNotWired, errBatchLeavesNotWired)
	if err != nil {
		return nil, err
	}
	return &batchRuntime{cfg: cfg, core: core}, nil
}

// bindRunner attaches the engine handle (R11F). Safe to call once.
func (r *batchRuntime) bindRunner(runner batchcore.RunnerHandle) error {
	return r.cfg.BindRunner(&r.mu, runner, errBatchRunnerNotWired)
}

// setIngress binds the App-owned ingress (R12-H2 P1-4). Mutex-guarded, so
// it is safe even after Start, though production wiring calls it during
// install, before the runtime ever Invokes.
func (r *batchRuntime) setIngress(ingress BatchWorkIngress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.Ingress = ingress
}

// ingress reads the App-owned ingress under the same mutex as setIngress,
// so a LeafEnv built before BindRuntimeIngress still sees the binding
// lazily at invocation time.
func (r *batchRuntime) ingress() BatchWorkIngress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.Ingress
}

// LeafEnv builds the executor environment whose leaf resolution is keyed by
// the payload's pinned registry identity (batchdispatch.LeafLookupID), never by
// session, closing the section-3 collision gap.
func (r *batchRuntime) LeafEnv() batchLeafEnv {
	return batchLeafEnv{Leaves: r.cfg.Leaves, DiskBindings: r.cfg.DiskBindings, Ingress: r.ingress}
}

func (r *batchRuntime) RegistryCount() int {
	if r == nil {
		return 0
	}
	if r.cfg.RegistryCount == nil {
		return -1
	}
	return r.cfg.RegistryCount()
}

func batchRootID() jobbatch.ID { return batchcore.RootID() }

func (r *batchRuntime) inFlightCalls() int { return r.core.InFlightCalls() }

// Test compatibility: the channel remains owned by the in-flight core.
type batchCallEntryView struct{ joined <-chan struct{} }

func (r *batchRuntime) entryFor(sessionID, callID string) *batchCallEntryView {
	joined := r.core.Joined(sessionID, callID)
	if joined == nil {
		return nil
	}
	return &batchCallEntryView{joined: joined}
}
