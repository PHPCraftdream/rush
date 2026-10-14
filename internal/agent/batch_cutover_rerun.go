// Exact-incarnation rerun stop routing (R12-F, cutover acceptance 3): the
// flag-on counterpart of Coordinator.StopRerunJobs (coordinator_rerun.go).
// The durable reconciliation is session.BatchStore.BatchRerunReconcile's
// single transaction (R10c); this runs only AFTER it committed, from the
// Voided set it returned. Every entry carries its exact root_id, so stops
// are keyed by (owner, root_id) — never widened to a tool_call_id: a
// provider-reused call id can never cancel a NEW incarnation, and a late
// completion of a stopped old incarnation can only commit against its own
// (void-delivery or retired) root rows, never into the new one (I5).
package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

// errBatchRetiredRoot is the typed delivery fence error: a root whose durable
// row says delivery='void' was retired by a rerun, so its delivery must never
// elect started (or completion) history. Distinguishable via errors.Is.
var errBatchRetiredRoot = errors.New("batch delivery: root incarnation was retired by a rerun")

// batchRetiredFenceRegistry is the DURABLE retired-root delivery fence
// (R12-F fixup P1, resynchronized): one store per (owner, rootID) so
// deliveries always consult the RIGHT database — never another App's — and
// concurrent reruns never race the map. A per-delivery parameter is
// impossible: batchDeliveryImpl is built in batch_install_adapters.go
// outside any coordinator's ownership and carries no coordinator reference,
// so the delivery path cannot receive the store through its deps. Keyed by
// owner\x00rootID under an RWMutex; the flag-off world never marks anything,
// so an empty registry fails open and every existing behavior is unchanged.
// batch_delivery.go consults it in deliverDetached BEFORE ElectStarted and
// in electCompletion BEFORE the inline retry loop.
type batchRetiredFenceRegistry struct {
	mu sync.RWMutex
	m  map[string]*session.BatchStore
}

// batchRetiredRoots is the package-level registry instance: a synchronized
// map keyed by owner+root is the correctly scoped form of the former
// process-global hook (which one App's rerun overwrote for every other
// App in the process).
var batchRetiredRoots = &batchRetiredFenceRegistry{m: map[string]*session.BatchStore{}}

// mark records the owning batch store for a retired root. Idempotent per
// (owner, rootID); a nil store is ignored so a fence entry never points at
// an unreadable database.
func (r *batchRetiredFenceRegistry) mark(owner, rootID string, store *session.BatchStore) {
	if owner == "" || rootID == "" || store == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[string]*session.BatchStore)
	}
	r.m[owner+"\x00"+rootID] = store
}

// check reports whether the exact (owner, rootID) root was retired. Absent:
// (false, nil) — the flag-off / not-yet-stopped world fails open. Present:
// read the row in the OWNING store as a sanity gate — a read error is not a
// retirement (fail open, e.g. a dropped database); a readable row is retired
// because mark only ever records roots the committed rerun's Voided set
// named. delivery == "void" (the void arm's settled shape) is simply one
// readable case; the live arm retires by stop routing only, and its row
// stays delivery='none' forever (the schema CHECK forbids a void write on an
// unselected root), so membership — not the row's delivery — is the fence.
func (r *batchRetiredFenceRegistry) check(ctx context.Context, owner, rootID string) (bool, error) {
	if owner == "" || rootID == "" {
		return false, nil
	}
	r.mu.RLock()
	store, ok := r.m[owner+"\x00"+rootID]
	r.mu.RUnlock()
	if !ok || store == nil {
		return false, nil
	}
	if _, err := store.Read(ctx, owner, rootID); err != nil {
		return false, nil
	}
	return true, nil
}

// BatchRerunStopper is the flag-on rerun stop surface. It is an OPTIONAL
// interface: the rerun handler type-asserts it when the batch runtime flag is
// on, so coordinator fakes stay untouched and the flag-off path keeps its
// VoidedAsyncJob contract byte-for-byte.
type BatchRerunStopper interface {
	BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode)
}

// batchRerunStopWorthy reports whether a voided incarnation's executor can
// still be stopped: the live states. A terminal incarnation is already
// settled; a stop would only chase dead rows.
func batchRerunStopWorthy(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// batchRerunRuntime unwraps the coordinator's bound flag-on runtime into the
// engine handle, owning host and batch store the stop needs. The binding is
// per-coordinator (BindCoordinatorBatchRuntime, batch_install_ingress.go);
// nil means not bound — the caller and its log treat that as "retire by
// delivery fencing only".
func batchRerunRuntime(c *coordinator) (runner batchRunnerHandle, host string, store *session.BatchStore) {
	bind := coordinatorBatchBindingFor(c)
	if bind == nil {
		return nil, "", nil
	}
	wi, ok := bind.ingress.(*batchWorkIngress)
	if !ok || wi == nil || wi.rt == nil {
		return nil, "", nil
	}
	if attach, ok := wi.rt.cfg.Attach.(batchAttachAdapter); ok {
		store = attach.store
	}
	return wi.rt.cfg.Runner, wi.rt.cfg.Host, store
}

// batchRerunFence records the incarnations a committed rerun retired, keyed
// by owner and BOTH identities: the exact root_id set and the (reusable)
// tool_call_id map. Incarnation identity is the (tool_call_id, root_id) PAIR:
// an event whose exact root_id is retired is stale; an event that carries only
// the tool_call_id (no incarnation identity) naming a retired call is stale
// conservatively. An event naming the SAME tool_call_id under a DIFFERENT,
// non-retired root_id is the NEW incarnation's own business — never stale —
// which is what keeps two reused tool-call IDs distinct.
type batchRerunFence struct {
	mu      sync.Mutex
	retired map[string]string   // owner\x00toolCallID -> retired root_id
	roots   map[string]struct{} // owner\x00rootID set
}

// batchRerunFenceCap bounds one coordinator's fence maps: many reruns on a
// long-lived coordinator would otherwise grow them without bound. On
// overflow BOTH maps reset before the insert — worst case a very old retired
// root loses its in-memory fence entry, which is acceptable: the durable
// delivery fence (batchRetiredRoots.check over delivery='void' rows, fixup
// P1) is
// the authority, this map only accelerates the coordinator-local checks.
const batchRerunFenceCap = 1024

func (f *batchRerunFence) retire(owner, toolCallID, rootID string) {
	if owner == "" || rootID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retired == nil {
		f.retired = make(map[string]string)
		f.roots = make(map[string]struct{})
	}
	if len(f.roots) >= batchRerunFenceCap {
		f.roots = make(map[string]struct{})
		f.retired = make(map[string]string)
	}
	if _, seen := f.roots[owner+"\x00"+rootID]; !seen {
		f.roots[owner+"\x00"+rootID] = struct{}{}
		f.retired[owner+"\x00"+toolCallID] = rootID
	}
}

func (f *batchRerunFence) stale(owner, toolCallID, rootID string) bool {
	if owner == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if rootID != "" {
		_, ok := f.roots[owner+"\x00"+rootID]
		return ok
	}
	_, ok := f.retired[owner+"\x00"+toolCallID]
	return ok
}

func (f *batchRerunFence) retiredRoot(owner, rootID string) bool {
	if owner == "" || rootID == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.roots[owner+"\x00"+rootID]
	return ok
}

// batchRerunFences maps a coordinator onto its retirement fence. Package-level
// like coordinatorBatchBindings: bare coordinator fixtures (tests, lazy
// construction) never need a constructor change.
var batchRerunFences sync.Map

func batchRerunFenceFor(c *coordinator) *batchRerunFence {
	if c == nil {
		return nil
	}
	if f, ok := batchRerunFences.Load(c); ok {
		return f.(*batchRerunFence)
	}
	f := &batchRerunFence{}
	actual, _ := batchRerunFences.LoadOrStore(c, f)
	return actual.(*batchRerunFence)
}

// BatchRerunStopVoided implements the flag-on post-commit stop: iterate the
// committed reconciliation's Voided set, fence every entry as retired, stop
// exactly the local live engine roots it names, and stop every voided
// delegation's child tree (same shape as the flag-off StopRerunJobs).
// Best effort and idempotent: the rows are already void-delivery or routed
// for retirement, so a stop that misses (foreign host, engine gone, already
// retired) only means the executor's completion commits void instead of
// being stopped early.
func (c *coordinator) BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode) {
	if c == nil || len(voided) == 0 {
		return
	}
	fence := batchRerunFenceFor(c)
	runner, host, store := batchRerunRuntime(c)
	if runner == nil || host == "" {
		slog.Warn("coordinator.BatchRerunStopVoided: batch runtime not bound; voided incarnations retire by delivery fencing only",
			"session_id", sessionID, "voided", len(voided))
	}
	for i := range voided {
		v := voided[i]
		// Fence (even an unstopped incarnation is retired — a late
		// completion for it is stale and must never reach a new incarnation),
		// then idempotence by exact root: a root an earlier pass already
		// retired is not stopped twice. Keyed by root_id, never tool_call_id.
		already := fence.retiredRoot(sessionID, v.RootID)
		fence.retire(sessionID, v.ToolCallID, v.RootID)
		// Mark the DURABLE delivery fence for this exact root FIRST — before
		// the stop, so even a failed stop is fenced: from here on the delivery
		// path refuses this (owner, rootID) in THIS owning store (never
		// another App's), because check reads the row, not memory.
		if store != nil {
			batchRetiredRoots.mark(sessionID, v.RootID, store)
		}
		if v.ChildSessionID != "" {
			c.stopTree(v.ChildSessionID)
		}
		if already {
			continue
		}
		if !batchRerunStopWorthy(v.State) {
			continue
		}
		if runner == nil {
			continue
		}
		if v.HostID != host {
			slog.Warn("coordinator.BatchRerunStopVoided: voided running incarnation lives on another host; its executor is not stopped, its completion commits void",
				"session_id", sessionID, "root_id", v.RootID, "tool_call_id", v.ToolCallID, "host_id", v.HostID)
			continue
		}
		// Exact-incarnation guard: re-read the row and stop only while THIS
		// root is still live, so a provider-reused tool_call_id on a NEW live
		// root can never widen this stop onto the new incarnation.
		if store != nil {
			snap, err := store.Read(ctx, sessionID, v.RootID)
			if err != nil {
				slog.Warn("coordinator.BatchRerunStopVoided: voided incarnation row unreadable; skipping the stop",
					"session_id", sessionID, "root_id", v.RootID, "err", err)
				continue
			}
			if !batchRerunStopWorthy(snap.Root.State) {
				continue
			}
		}
		out := cancelBatchCall(ctx, runner, jobbatch.ID(v.RootID))
		if out.Err != nil {
			slog.Warn("coordinator.BatchRerunStopVoided: stop request failed; the completion commits void",
				"session_id", sessionID, "root_id", v.RootID, "err", out.Err)
		}
	}
}
