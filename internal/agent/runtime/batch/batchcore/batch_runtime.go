package batchcore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/google/uuid"
)

// AttachProbe is one live-attach lookup.
type AttachProbe struct {
	Owner, Host, RunnerID string
	ToolCallID            string
	InputHash             string
	SubmittedMessageID    string
	SubmittedRegistryID   string
}

// AttachStore is the R11B seam: attach to an existing live root for
// (owner, tool call id) under the owner-host and runner-incarnation fences,
// matching the input hash. Attached=false means no live root exists and the
// caller submits; a hash or fence mismatch is an error, never a resubmission.
// R11F implements it over session.BatchStore.ReserveIdentity.
type AttachStore interface {
	ProbeLive(ctx context.Context, p AttachProbe) (jobbatch.ID, bool, error)
}

// RunnerHandle is the narrow engine seam used by the ingress runtime.
type RunnerHandle interface {
	Submit(context.Context, jobbatch.SubmitRequest) (jobbatch.ID, error)
	Stop(context.Context, jobbatch.ID) error
}

// Default admission budgets for direct calls and explicit batch specs.
const (
	DefaultMaxDepth = 8
	DefaultMaxNodes = 256
)

// RuntimeConfig stores the dormant runtime wiring. Type parameters keep the
// caller's profile and compatibility seams typed without importing the caller.
type RuntimeConfig[Profile, Delivery, WorkIngress, DiskBindings, RootLeases any] struct {
	Host, RunnerID string
	Profile        Profile
	Resolver       batchinst.MCPCapabilityResolver
	// Leaves looks up a pinned registry by registry identity, never by session.
	Leaves        func(registryID string) (*toolwrap.PolicyLeafRegistry, error)
	Attach        AttachStore
	Delivery      Delivery
	Runner        RunnerHandle
	Ingress       WorkIngress
	DiskBindings  DiskBindings
	RootLeases    RootLeases
	RegistryCount func() int
	// Non-positive budgets select the defaults.
	MaxDepth, MaxNodes int
}

// InitIngress validates wiring before applying defaults and allocating the
// dormant core. The caller supplies its profile predicate and error identities.
func (cfg *RuntimeConfig[P, D, I, B, L]) InitIngress(profileReady func(P) bool, attachErr, profileErr, leavesErr error) (*Ingress, error) {
	if cfg.Host == "" || cfg.RunnerID == "" {
		return nil, fmt.Errorf("%w: host and runner id", attachErr)
	}
	if !profileReady(cfg.Profile) {
		return nil, fmt.Errorf("%w", profileErr)
	}
	if cfg.Leaves == nil {
		return nil, fmt.Errorf("%w", leavesErr)
	}
	if cfg.Attach == nil {
		return nil, fmt.Errorf("%w", attachErr)
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = DefaultMaxDepth
	}
	if cfg.MaxNodes <= 0 {
		cfg.MaxNodes = DefaultMaxNodes
	}
	return NewIngress(), nil
}

// BindRunner binds once under the same mutex the caller uses to snapshot its
// runtime wiring. A nil handle is refused before taking that mutex.
func (cfg *RuntimeConfig[P, D, I, B, L]) BindRunner(mu *sync.Mutex, runner RunnerHandle, notWired error) error {
	if runner == nil {
		return fmt.Errorf("%w: nil runner", notWired)
	}
	mu.Lock()
	defer mu.Unlock()
	if cfg.Runner != nil {
		return errors.New("batch runtime: runner already bound")
	}
	cfg.Runner = runner
	return nil
}

const (
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

// Ingress owns in-flight deduplication; completed calls are never cached.
type Ingress struct {
	mu    sync.Mutex
	calls map[string]*batchCallEntry
}

func NewIngress() *Ingress { return &Ingress{calls: make(map[string]*batchCallEntry)} }

func RootID() jobbatch.ID { return jobbatch.ID(uuid.NewString()) }

// finishCall removes the in-flight entry — singleflight deduplicates only
// live executions; a retry after completion is a fresh durable call (design
// I5) and responses are never cached for the process lifetime — then
// publishes the outcome. Waiters keep their entry pointer and still receive
// the outcome.
func (r *Ingress) finishCall(key string, e *batchCallEntry, resp fantasy.ToolResponse, err error) {
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
func (r *Ingress) InFlightCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// Joined returns the advisory waiter signal for one in-flight call, or nil.
func (r *Ingress) Joined(sessionID, callID string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.calls[sessionID+"\x00"+callID]; e != nil {
		return e.joined
	}
	return nil
}
