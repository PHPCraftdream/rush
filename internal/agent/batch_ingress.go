package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
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
	spec, resp, err := batchcore.PrepareDirectCall(ctx, leaf, call, batchcore.DirectCallPreparation{
		Validate: inv.Validate, ControlTarget: batchControlTarget,
		CanonicalInput: batchCanonicalInput, StampCredentials: stampBatchCallCredentials,
		AgentDepth: &inv.AgentDepth, AgentDepthSet: &inv.AgentDepthSet,
		DefaultDepth: DefaultAgentDepth, MaxDepth: MaxAgentDepthForResume,
		MissingCallID: errBatchMissingCallID, NilLeaf: errBatchNilLeaf, ControlPlaneLeaf: errBatchControlPlaneLeaf,
		BuildSpec: func(input json.RawMessage) (jobbatch.Spec, error) {
			return batchOneItemSpec(inv, call, input)
		},
	})
	if spec.Task == nil {
		return resp, err
	}
	hash := batchInvocationHash(spec)

	r.mu.Lock()
	delivery, runner := r.cfg.Delivery, r.cfg.Runner
	r.mu.Unlock()
	if delivery == nil {
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchDeliveryNotWired)
	}
	if runner == nil {
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchRunnerNotWired)
	}
	probe := batchAttachProbe{
		Owner: inv.SessionID, Host: r.cfg.Host, RunnerID: r.cfg.RunnerID,
		ToolCallID: call.ID, InputHash: hash,
		SubmittedMessageID: inv.MessageID, SubmittedRegistryID: inv.RegistryID,
	}
	fresh := batchcore.FreshCall{
		Attach: r.cfg.Attach, LeaseUnavailable: ErrDiskProviderUnavailable,
		ReleaseHandoff: func(ctx context.Context) {
			if handoff := toolwrap.HandoffFrom(ctx); handoff != nil {
				handoff.Release()
			}
		},
		Submit: func(ctx context.Context) (jobbatch.ID, error) { return r.submit(ctx, runner, inv, spec, call.ID) },
		Deliver: func(ctx context.Context, root jobbatch.ID) (fantasy.ToolResponse, error) {
			return delivery.DeliverBatch(ctx, root, inv)
		},
	}
	if inv.Policy.DiskBindingID != "" {
		fresh.MatchLease = func(root jobbatch.ID) bool {
			return r.cfg.RootLeases != nil && r.cfg.RootLeases.Matches(root, inv.Policy.DiskBindingID, inv.RegistryID)
		}
	}
	return r.core.Invoke(ctx, probe, fresh, errBatchInvocationMismatch)
}

// submit adapts the root invocation and runtime wiring to batchcore.
func (r *batchRuntime) submit(ctx context.Context, runner batchRunnerHandle, inv BatchInvocation, spec jobbatch.Spec, callID string) (jobbatch.ID, error) {
	params := batchcore.SubmitParams{
		Owner: inv.SessionID, RegistryID: inv.RegistryID, DiskBindingID: inv.Policy.DiskBindingID,
		CallID: callID, Spec: spec, Runner: runner,
		Admit: func(root jobbatch.ID, spec jobbatch.Spec) error {
			waiter := batchRankContext{Rank: inv.WaiterRank, Root: root}
			return ValidateSubmit(r.cfg.Profile, waiter, nil, spec, r.cfg.MaxDepth, r.cfg.MaxNodes, r.cfg.Resolver)
		},
		InvalidAdmission: errBatchSubmitBeforeInvalid,
		LeaseUnavailable: ErrDiskProviderUnavailable,
		HasDiskBindings:  r.cfg.DiskBindings != nil,
		HandoffFrom:      toolwrap.HandoffFrom,
	}
	if leases := r.cfg.RootLeases; leases != nil {
		params.AcquireRoot = leases.acquire
		params.ReleaseRoot = leases.Release
		params.AdoptRoot = leases.Adopt
	}
	return batchcore.Submit(ctx, params)
}

// Keep the private fixture type and builder on the root adapter.
type batchToolInputError = batchcore.ToolInputError

func batchOneItemSpec(inv BatchInvocation, call fantasy.ToolCall, input json.RawMessage) (jobbatch.Spec, error) {
	return batchcore.OneItemSpec(call, input, inv.AgentDepth, func(role string) (*session.RoleContract, error) {
		cfg, err := configForBatchInvocation(inv)
		if err != nil {
			return nil, err
		}
		return ResolveRoleContract(cfg, role, inv.Policy.RoleContract)
	}, func(fields batchcore.DirectLeafFields) (json.RawMessage, error) {
		payload := batchLeafPayload{
			Tool: call.Name, ToolCallID: call.ID, SessionID: inv.SessionID,
			Input: input, RegistryID: inv.RegistryID, Policy: &inv.Policy,
			QuestionRoute: inv.QuestionRoute, SelectedRoleContract: fields.SelectedRoleContract,
			AgentDepth: fields.AgentDepth,
		}
		if fields.WakeOnlySeconds != nil {
			payload.Timeout = &batchLeafTimeoutSpec{Kind: batchTimeoutWakeOnly, Seconds: *fields.WakeOnlySeconds}
		}
		return json.Marshal(payload)
	}, lookupBatchLeafKindOverride)
}

func batchLeafKind(tool string) string {
	return batchcore.LeafKind(tool, lookupBatchLeafKindOverride)
}

func lookupBatchLeafKindOverride(tool string) (string, bool) {
	if override := batchLeafKindOverride.Load(); override != nil {
		return (*override)(tool)
	}
	return "", false
}

// batchLeafKindOverride is set only by the flag-on coordinator binding. It is
// atomic: every App bind stores it while invocations of other Apps (and
// parallel tests) read it.
var batchLeafKindOverride atomic.Pointer[func(tool string) (kind string, ok bool)]

// setBatchLeafKindOverride installs fn (nil clears the override).
func setBatchLeafKindOverride(fn func(tool string) (kind string, ok bool)) {
	if fn == nil {
		batchLeafKindOverride.Store(nil)
		return
	}
	batchLeafKindOverride.Store(&fn)
}
