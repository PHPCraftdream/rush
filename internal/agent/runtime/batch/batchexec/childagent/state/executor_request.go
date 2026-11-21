package state

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	core "github.com/PHPCraftdream/rush/internal/session/service"
)

// RunRequest identifies one incarnation attempt of the leaf.
type RunRequest[Policy, Ingress any] struct {
	NodeID     jobbatch.ID
	Token      uint64
	SessionID  string
	ToolCallID string
	Input      json.RawMessage
	// R11F (M3): the invocation's pinned registry identity (leaf resolution
	// through the ingress) and enclosing message id (child-session creation).
	RegistryID           string
	MessageID            string
	DiskBindingID        string
	Policy               Policy
	SelectedRoleContract *core.RoleContract
	pctHandoff           *toolwrap.PctDelegationHandoff // pct: owned by the executor through confirmed disposal.
	CallCredentials      bool
	// QuestionRoute is carried verbatim from the durable payload; false
	// means the incarnation must never hold or publish questions.
	QuestionRoute bool
	AgentDepth    int
	// R11F round-2 (P1-3): the RECORDED tool name of the delegation leaf.
	// Dispatch resolves the pinned leaf by this name and names the call
	// with it, so an "agentic_fetch" payload reaches its OWN leaf, never
	// the "agent" leaf. Empty fails closed in the driver.
	Tool            string
	ChildSessionID  string
	NoChildSelected bool
	// R12-H2 P1-4: the App-owned ingress carried in the leaf env, so the
	// driver resolves the pinned leaf through THIS invocation's authority —
	// never the process-global binding. Nil keeps the driver's flag-off
	// fallback (tests), except on the provider path where it fails closed.
	Ingress Ingress
}

// SetHandoff records the executor-owned captured handoff.
func (r *RunRequest[P, I]) SetHandoff(h *toolwrap.PctDelegationHandoff) { r.pctHandoff = h }

// Handoff returns the captured handoff without transferring ownership.
func (r RunRequest[P, I]) Handoff() *toolwrap.PctDelegationHandoff { return r.pctHandoff }

// RunFactory launches the turn loop for one incarnation.
type RunFactory[Request any] interface {
	StartAgentTurn(context.Context, Request, childagent.Host) (childagent.Turn, error)
}

// QuestionResolver closes durable notices after an answer or cancellation.
type QuestionResolver interface {
	ResolveChildQuestions(ctx context.Context, owner, node, child string, token, sequence int64) error
}

// DescendantReader reports liveness and whether the read was complete.
// An incomplete read keeps the scope open.
type DescendantReader[Request any] interface {
	DescendantsLive(ctx context.Context, req Request) (live bool, complete bool)
}

// ChildBinder durably links a child before it can run.
type ChildBinder = func(ctx context.Context, owner, root, node string, token uint64, child string) error

// Deps contains the injected ports for an agent incarnation.
type Deps[Request any] struct {
	Runs        RunFactory[Request]
	Descendants DescendantReader[Request]
	// Zero means a single read after the turn ends.
	Poll              time.Duration
	QuestionPublisher childagent.QuestionPublisher
	// Nil keeps published notices pending (test and flag-off installs).
	QuestionResolver QuestionResolver
	// Commit-hook identity for the injected Bind closure.
	Host, RunnerID string
	// Nil updates only the in-memory request.
	Bind ChildBinder
	// Nil disables completion-reaction gating and final-outcome refresh.
	ChildFacts BatchChildReactionFacts
}

// SnapshotRequest copies the request header under the execution lock. Its
// borrowed fields retain their original identities.
func SnapshotRequest[Request any](mu *sync.Mutex, req *Request) Request {
	mu.Lock()
	defer mu.Unlock()
	return *req
}

// BindChildSession updates the request only after the durable write succeeds.
func BindChildSession[P, I any](ctx context.Context, mu *sync.Mutex, req *RunRequest[P, I], bind ChildBinder, root, node jobbatch.ID, id string) error {
	if bind != nil {
		if err := bind(ctx, req.SessionID, string(root), string(node), req.Token, id); err != nil {
			return fmt.Errorf("batch bind child session: %w", err)
		}
	}
	mu.Lock()
	req.ChildSessionID = id
	mu.Unlock()
	return nil
}

// MarkNoChildSelected records that the turn did not select a child.
func MarkNoChildSelected[P, I any](mu *sync.Mutex, req *RunRequest[P, I]) {
	mu.Lock()
	req.NoChildSelected = true
	mu.Unlock()
}

// ResolvePublishedQuestions closes the durable notices published for this incarnation after an answer or cancellation.
func ResolvePublishedQuestions[P, I any](ctx context.Context, resolver QuestionResolver, snapshot func() RunRequest[P, I], timeout time.Duration, sequence uint64) error {
	if resolver == nil {
		return nil
	}
	req := snapshot()
	if req.SessionID == "" || req.NodeID == "" || req.ChildSessionID == "" || req.Token == 0 || sequence == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := resolver.ResolveChildQuestions(ctx, req.SessionID, string(req.NodeID), req.ChildSessionID, int64(req.Token), int64(sequence)); err != nil {
		slog.Warn("Batch question resolution failed", "node", string(req.NodeID), "err", err)
		return err
	}
	return nil
}

// Launch invokes the factory with panic containment. The caller supplies the
// engine-specific panic classification and the incarnation's host callbacks.
func Launch[Request any](ctx context.Context, runs RunFactory[Request], snapshot func() Request, host childagent.Host, panicError func(error) error) (turn childagent.Turn, err error) {
	defer func() {
		if r := recover(); r != nil {
			turn, err = nil, panicError(fmt.Errorf("%v", r))
		}
	}()
	return runs.StartAgentTurn(ctx, snapshot(), host)
}
