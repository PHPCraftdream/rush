package finalize

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// FinalizationAuthorityRegistry maps an owner session id to the in-process
// finalization authority bound at that owner's root-run start. The batch
// commit hook runs on runner goroutines without the submitting call's
// context, so this registry is the only way a per-run authority can admit a
// commit. The composition root builds ONE registry when
// options.finalization.enabled and shares it between the wiring and the
// batch commit identity. The zero value is not usable; build through
// NewFinalizationAuthorityRegistry. All methods are nil-receiver safe.
// (finalization_authority_registry.go:17 verbatim.)
type FinalizationAuthorityRegistry struct {
	mu      sync.Mutex
	byOwner map[string]*session.AgentFinalizationAuthority
}

// NewFinalizationAuthorityRegistry builds the shared authority registry
// (finalization_authority_registry.go:22 verbatim).
func NewFinalizationAuthorityRegistry() *FinalizationAuthorityRegistry {
	return &FinalizationAuthorityRegistry{byOwner: make(map[string]*session.AgentFinalizationAuthority)}
}

// Register stores the authority under owner and returns the unregister
// callback. The callback removes the entry only while it still holds THIS
// authority (pointer comparison), so a stale unregister can never drop a
// newer registration for the same owner.
// (finalization_authority_registry.go:30 verbatim.)
func (r *FinalizationAuthorityRegistry) Register(owner string, authority *session.AgentFinalizationAuthority) func() {
	if r == nil || owner == "" || authority == nil {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byOwner[owner] = authority
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.byOwner[owner] == authority {
				delete(r.byOwner, owner)
			}
		})
	}
}

// Lookup returns the registered authority for owner, or nil when none is
// registered (finalization_authority_registry.go:51 verbatim).
func (r *FinalizationAuthorityRegistry) Lookup(owner string) *session.AgentFinalizationAuthority {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byOwner[owner]
}

// FinalizationCancelLookup resolves an owner session id to the in-process
// finalization work that must stop. The coordinator-interrupt side knows only
// the session id; the process-local registry below is the other half, wired
// by W1 together with this hook.
// (finalization_cancel.go:9 verbatim.)
type FinalizationCancelLookup interface {
	// CancelFinalizationSession cancels every finalization execution currently
	// registered for sessionID. Unknown sessions are a no-op.
	CancelFinalizationSession(sessionID string)
}

// FinalizationCancelRegistry is the minimal in-process index from owner
// session id to the live finalization executions of that session. There is no
// package-level registry: a finalizationExecution otherwise lives only in the
// driver context, so a coordinator interrupt could not find it. W1 registers
// an execution right after a successful WithFinalizationExecution and
// unregisters it when the run's generation unwinds; the registry itself never
// owns execution lifetime. (finalization_cancel.go:22; type and Register /
// Unregister exported because the root finalization wiring calls them.)
type FinalizationCancelRegistry struct {
	mu    sync.Mutex
	byKey map[string]map[*FinalizationExecution]struct{}
}

// NewFinalizationCancelRegistry builds the empty registry
// (finalization_cancel.go:27; exported for the root wiring).
func NewFinalizationCancelRegistry() *FinalizationCancelRegistry {
	return &FinalizationCancelRegistry{byKey: make(map[string]map[*FinalizationExecution]struct{})}
}

// Register indexes one execution under its session id. Executions with no
// session, no gate, or a nil pointer are ignored: cancelling must never panic
// on a half-built run. (finalization_cancel.go:34, case-only export.)
func (r *FinalizationCancelRegistry) Register(execution *FinalizationExecution) {
	if r == nil || execution == nil || execution.SessionID == "" || execution.Gate == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	targets, ok := r.byKey[execution.SessionID]
	if !ok {
		targets = make(map[*FinalizationExecution]struct{})
		r.byKey[execution.SessionID] = targets
	}
	targets[execution] = struct{}{}
}

// Unregister removes exactly this execution (pointer identity) and leaves any
// other execution of the same session untouched. Registering again later is a
// fresh, independent entry. (finalization_cancel.go:51, case-only export.)
func (r *FinalizationCancelRegistry) Unregister(execution *FinalizationExecution) {
	if r == nil || execution == nil || execution.SessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	targets, ok := r.byKey[execution.SessionID]
	if !ok {
		return
	}
	delete(targets, execution)
	if len(targets) == 0 {
		delete(r.byKey, execution.SessionID)
	}
}

// cancelSession cancels every gate currently registered for sessionID. The
// gates are snapshotted under the registry lock and cancelled without it, so
// the call can never block on gate activity, and gate.cancel itself is
// idempotent and wakes waiters without waiting for them.
// (finalization_cancel.go:71 verbatim.)
func (r *FinalizationCancelRegistry) cancelSession(sessionID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	gates := make([]*gate.Gate, 0, len(r.byKey[sessionID]))
	for execution := range r.byKey[sessionID] {
		if execution != nil && execution.Gate != nil {
			gates = append(gates, execution.Gate)
		}
	}
	r.mu.Unlock()
	for _, g := range gates {
		g.Cancel()
	}
}

// CancelFinalizationSession satisfies FinalizationCancelLookup so the
// registry can be passed to NewFinalizationOnCancelHook directly.
// (finalization_cancel.go:90 verbatim.)
func (r *FinalizationCancelRegistry) CancelFinalizationSession(sessionID string) {
	r.cancelSession(sessionID)
}

// NewFinalizationOnCancelHook builds the coordinator-interrupt OnCancel hook
// (lane L6). The returned hook is the in-process half of finalization
// cancellation: it looks the session's live executions up in the registry W1
// maintains and cancels their gates, which forbids further mutations and
// seals and wakes any waitState waiter. The durable half (cancel_requested on
// the session row, refused SealTx and StoreCandidateTx) is already implemented
// in the session package and is deliberately not duplicated here.
//
// The hook is inert by construction: a nil lookup, unknown session ids and
// empty ids are no-ops, it never blocks and never panics, and it has no
// production call site until W1 installs it.
// (finalization_cancel.go:105 verbatim.)
func NewFinalizationOnCancelHook(lookup FinalizationCancelLookup) func(sessionID string) {
	return func(sessionID string) {
		if lookup == nil {
			return
		}
		lookup.CancelFinalizationSession(sessionID)
	}
}
