package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Invoke is the ONE ingress API (decomposition row C): validate the
// invocation fail-closed, refuse D1 control tools as generic targets, admit
// the one-item spec through the R11A validator, attach to a live root for a
// matching retry (exactly-once execution), otherwise Submit, then hand the
// started root to the R11D delivery seam. Hooks, restrictions and permissions
// run once, inside the pinned policy leaf the executor resolves; the old
// wrapper path is never taken.
//
// A nil delivery seam or runner handle is a typed not-wired error returned
// BEFORE any store probe or engine call: the dormant runtime never falls back
// to the legacy async path.
func (r *batchRuntime) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if err := inv.Validate(); err != nil {
		return fantasy.ToolResponse{}, err
	}
	if call.ID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchMissingCallID)
	}
	if leaf == nil {
		return fantasy.ToolResponse{}, fmt.Errorf("%w: %q", errBatchNilLeaf, call.Name)
	}
	// D1: control-plane tools execute their pinned policy leaf directly;
	// they are never generic batch targets.
	if batchControlTarget(call.Name) {
		return fantasy.ToolResponse{}, fmt.Errorf("%w: %q", errBatchControlPlaneLeaf, call.Name)
	}
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, err
	}
	input, err := batchCanonicalInput(call.Input)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	spec, err := batchOneItemSpec(inv, call, input)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	hash := batchInvocationHash(spec)

	// Singleflight covers only IN-FLIGHT executions: finishCall removes the
	// entry before publishing, so a retry arriving after completion takes
	// the fresh durable path (design I5: a reused call id after a history
	// rerun is a new execution, never a cached replay). A waiter whose own
	// context is still alive but that received the first caller's
	// cancellation re-runs the fresh path as the new first caller; the loop
	// bound only guards against a pathological store that keeps returning
	// foreign cancellation errors.
	for attempts := 0; ; attempts++ {
		resp, err, retry := r.invokeOnce(ctx, inv, spec, hash, call.ID)
		if !retry || attempts >= 4 {
			return resp, err
		}
	}
}

// invokeOnce runs one singleflight step for a prepared invocation: join an
// in-flight execution or become its first caller.
func (r *batchRuntime) invokeOnce(ctx context.Context, inv BatchInvocation, spec jobbatch.Spec, hash, callID string) (fantasy.ToolResponse, error, bool) {
	r.mu.Lock()
	if r.cfg.Delivery == nil {
		r.mu.Unlock()
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchDeliveryNotWired), false
	}
	if r.cfg.Runner == nil {
		r.mu.Unlock()
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchRunnerNotWired), false
	}
	key := inv.SessionID + "\x00" + callID
	if e, ok := r.calls[key]; ok {
		r.mu.Unlock()
		if e.hash != hash {
			return fantasy.ToolResponse{}, fmt.Errorf("%w: call %q", errBatchInvocationMismatch, callID), false
		}
		select {
		case e.joined <- struct{}{}:
		default:
		}
		resp, err := waitCall(ctx, e)
		// Waiter poisoning: the published outcome is the FIRST caller's
		// cancellation, not this waiter's (its own ctx is still alive) and
		// the entry is already removed, so re-run the fresh path.
		if ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return fantasy.ToolResponse{}, nil, true
		}
		return resp, err, false
	}
	e := &batchCallEntry{hash: hash, done: make(chan struct{}), joined: make(chan struct{}, batchCallWaiterBuffer)}
	r.calls[key] = e
	r.mu.Unlock()

	resp, err := r.invokeFresh(ctx, inv, spec, hash, callID)
	r.finishCall(key, e, resp, err)
	return resp, err, false
}

// invokeFresh runs the first execution of one call identity: durable attach
// probe, admission, Submit, delivery.
func (r *batchRuntime) invokeFresh(ctx context.Context, inv BatchInvocation, spec jobbatch.Spec, hash, callID string) (fantasy.ToolResponse, error) {
	probe := batchAttachProbe{
		Owner: inv.SessionID, Host: r.cfg.Host, RunnerID: r.cfg.RunnerID,
		ToolCallID: callID, InputHash: hash,
	}
	root, attached, err := r.cfg.Attach.ProbeLive(ctx, probe)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("batch attach: %w", err)
	}
	if !attached {
		root, err = r.submit(ctx, inv, spec, hash, callID)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
	}
	return r.cfg.Delivery.DeliverBatch(ctx, root, inv)
}

// submit admits and starts the one-item root. Admission (ValidateSubmit) runs
// strictly before the Runner is called.
func (r *batchRuntime) submit(ctx context.Context, inv BatchInvocation, spec jobbatch.Spec, hash, callID string) (jobbatch.ID, error) {
	root := batchRootID()
	waiter := batchRankContext{Rank: inv.WaiterRank, Root: root}
	if err := ValidateSubmit(r.cfg.Profile, waiter, nil, spec, r.cfg.MaxDepth, r.cfg.MaxNodes, r.cfg.Resolver); err != nil {
		return "", fmt.Errorf("%w: %w", errBatchSubmitBeforeInvalid, err)
	}
	id, err := r.cfg.Runner.Submit(ctx, jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: callID})
	if err != nil {
		return "", fmt.Errorf("batch submit: %w", err)
	}
	if id != root {
		return "", fmt.Errorf("batch submit: runner root %q differs from requested %q", id, root)
	}
	_ = hash // The commit hook reserves (root, callID, hash) atomically in R11B.
	return root, nil
}

// batchOneItemSpec builds the direct-call spec: OneItem, NotifyAll, kind
// derived from the tool family, payload carrying the invocation's immutable
// policy transport and pinned registry identity. Explicit specs and Notify
// selection are represented by the batch model controls, not this direct-call
// adapter.
func batchOneItemSpec(inv BatchInvocation, call fantasy.ToolCall, input json.RawMessage) (jobbatch.Spec, error) {
	payload := batchLeafPayload{
		Tool:       call.Name,
		ToolCallID: call.ID,
		SessionID:  inv.SessionID,
		Input:      input,
		RegistryID: inv.RegistryID,
		Policy:     &inv.Policy,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return jobbatch.Spec{}, fmt.Errorf("batch payload: %w", err)
	}
	spec := jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind:    batchLeafKind(call.Name),
		Payload: raw,
	}}
	// Direct task specs are normalized by batchRunnerAdapter, the engine seam
	// that enforces a batch root; keep this spec canonical for its durable hash.
	return spec, nil
}

// batchLeafKind maps the tool family onto the executor routing kind. MCP
// leaves must declare their kind explicitly (R7a wiring); nothing is inferred
// from a name prefix.
func batchLeafKind(tool string) string {
	switch tool {
	case "bash":
		return batchKindShell
	case "run_command":
		return batchKindArgv
	case "agent", "agentic_fetch":
		return batchKindAgent
	}
	// R11F (M3) flag-on hook: MCP leaves registered by the binding resolve
	// through the MCP kind; nil (flag-off) keeps the generic default.
	if batchLeafKindOverride != nil {
		if kind, ok := batchLeafKindOverride(tool); ok {
			return kind
		}
	}
	return batchKindGeneric
}

// batchLeafKindOverride is set only by the flag-on coordinator binding.
var batchLeafKindOverride func(tool string) (kind string, ok bool)
