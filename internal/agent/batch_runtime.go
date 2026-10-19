package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/google/uuid"
)

// batchRunnerHandle is the Runner seam of the ingress. It is an interface, not
// *jobbatch.Runner, so tests inject a fake and the agent package never
// imports a Runner constructor (session may import jobbatch; agent never
// imports session). R11F adapts *jobbatch.Runner onto it.
type batchRunnerHandle interface {
	// Submit starts one root; the returned ID is the caller-supplied root.
	Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error)
	// Stop requests cancellation of one root (sync caller cancellation).
	Stop(ctx context.Context, id jobbatch.ID) error
}

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

// batchAttachProbe is one live-attach lookup.
type batchAttachProbe struct {
	Owner, Host, RunnerID string
	ToolCallID            string
	InputHash             string
}

// batchAttachStore is the R11B seam: attach to an existing live root for
// (owner, tool call id) under the owner-host and runner-incarnation fences,
// matching the input hash. Attached=false means no live root exists and the
// caller submits; a hash or fence mismatch is an error, never a resubmission.
// R11F implements it over session.BatchStore.ReserveIdentity.
type batchAttachStore interface {
	ProbeLive(ctx context.Context, p batchAttachProbe) (jobbatch.ID, bool, error)
}

// batchRuntimeConfig configures one dormant ingress runtime. Every field is
// required except Runner and Delivery, which may be bound later; a missing
// required field fails construction instead of failing later at Invoke.
type batchRuntimeConfig struct {
	// Host and RunnerID are the owning host and Runner incarnation written
	// into attach probes and reservations.
	Host, RunnerID string
	// Profile is the R11A closed capacity profile.
	Profile *BatchProfile
	// Resolver resolves MCP admission capabilities (R11A); may be nil, in
	// which case any MCP leaf is rejected as unresolved.
	Resolver batchMCPCapabilityResolver
	// Leaves resolves a pinned policy-leaf registry by REGISTRY identity,
	// never by session. Wired into the executor's batchLeafEnv by R11F.
	Leaves func(registryID string) (*policyLeafRegistry, error)
	// Attach is the R11B live-attach store (required).
	Attach batchAttachStore
	// Delivery is the R11D seam; nil keeps the runtime dormant (Invoke
	// returns the typed not-wired error).
	Delivery batchDelivery
	// Runner is the engine handle; nil until R11F binds it.
	Runner batchRunnerHandle
	// DiskBindings supplies validated process-bound providers to the executor.
	DiskBindings *DiskProviderRegistry
	// Ingress is the App-owned BatchWorkIngress carried into every leaf env
	// (R12-H2 P1-4) so the agent family driver resolves pinned leaves
	// through the invocation's own authority instead of the process-global
	// binding. Optional; bound after construction via setIngress (safe:
	// called before the runtime ever Invokes, and guarded by the mutex).
	Ingress       BatchWorkIngress
	RootLeases    *batchRootLeaseStore
	RegistryCount func() int
	// MaxDepth and MaxNodes are the admission budgets; zero selects the
	// defaults below.
	MaxDepth, MaxNodes int
}

// Default admission budgets for direct one-item invocations (a task root has
// depth 1 and node count 1; the budgets exist to bound future explicit specs).
const (
	batchDefaultMaxDepth = 8
	batchDefaultMaxNodes = 256
	// batchCallWaiterBuffer bounds the waiter-join signal channel of one
	// singleflight entry; joins beyond it are dropped (they are advisory,
	// for tests).
	batchCallWaiterBuffer = 256
)

// batchCallEntry is the in-process singleflight record of one (owner, call)
// pair. Concurrent matching retries share the first execution; a different
// input hash for the same call is refused.
type batchCallEntry struct {
	hash   string
	done   chan struct{}
	joined chan struct{} // One non-blocking send per singleflight waiter.
	resp   fantasy.ToolResponse
	err    error
}

// batchRuntime is the dormant R11C ingress core. Construction has NO side
// effects: no goroutine, no executor, no global registration; R11F owns every
// wiring. It is inert until something calls Invoke.
type batchRuntime struct {
	cfg batchRuntimeConfig

	mu    sync.Mutex
	calls map[string]*batchCallEntry
}

func newBatchRuntime(cfg batchRuntimeConfig) (*batchRuntime, error) {
	if cfg.Host == "" || cfg.RunnerID == "" {
		return nil, fmt.Errorf("%w: host and runner id", errBatchAttachNotWired)
	}
	if cfg.Profile == nil || cfg.Profile.caps == nil {
		return nil, fmt.Errorf("%w", errBatchProfileNotWired)
	}
	if cfg.Leaves == nil {
		return nil, fmt.Errorf("%w", errBatchLeavesNotWired)
	}
	if cfg.Attach == nil {
		return nil, fmt.Errorf("%w", errBatchAttachNotWired)
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = batchDefaultMaxDepth
	}
	if cfg.MaxNodes <= 0 {
		cfg.MaxNodes = batchDefaultMaxNodes
	}
	return &batchRuntime{cfg: cfg, calls: make(map[string]*batchCallEntry)}, nil
}

// bindRunner attaches the engine handle (R11F). Safe to call once.
func (r *batchRuntime) bindRunner(runner batchRunnerHandle) error {
	if runner == nil {
		return fmt.Errorf("%w: nil runner", errBatchRunnerNotWired)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.Runner != nil {
		return errors.New("batch runtime: runner already bound")
	}
	r.cfg.Runner = runner
	return nil
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
// the payload's pinned registry identity (batchLeafLookupID), never by
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

func batchRootID() jobbatch.ID { return jobbatch.ID(uuid.NewString()) }

// finishCall removes the in-flight entry — singleflight deduplicates only
// live executions; a retry after completion is a fresh durable call (design
// I5) and responses are never cached for the process lifetime — then
// publishes the outcome. Waiters keep their entry pointer and still receive
// the outcome.
func (r *batchRuntime) finishCall(key string, e *batchCallEntry, resp fantasy.ToolResponse, err error) {
	r.mu.Lock()
	if r.calls[key] == e {
		delete(r.calls, key)
	}
	r.mu.Unlock()
	e.resp, e.err = resp, err
	close(e.done)
}

// waitCall blocks for a shared entry's outcome; caller cancellation breaks
// the wait without touching the executing retry.
func waitCall(ctx context.Context, e *batchCallEntry) (fantasy.ToolResponse, error) {
	select {
	case <-e.done:
		return e.resp, e.err
	case <-ctx.Done():
		return fantasy.ToolResponse{}, ctx.Err()
	}
}

// inFlightCalls returns the number of live singleflight entries.
func (r *batchRuntime) inFlightCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// entryFor returns the in-flight entry of one call identity, or nil.
func (r *batchRuntime) entryFor(sessionID, callID string) *batchCallEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[sessionID+"\x00"+callID]
}
