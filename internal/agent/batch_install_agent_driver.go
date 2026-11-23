package agent

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// R11F (M3): coordinator seams of the R7c agent executor family. The M2
// install left batchAgentDeps zero; the flagged install wires
// CoordinatorBatchAgentDeps, whose factory resolves the ENCLOSED
// invocation's pinned "agent" leaf (hooks/restricted retained) through the
// one ingress and runs it as the incarnation's turn loop.

// errBatchCoordinatorNotBound: the flag-on wiring (BindCoordinatorBatchRuntime)
// has not run — the agent family refuses instead of dispatching raw.
var errBatchCoordinatorNotBound = errors.New("batch agent family: no coordinator batch binding (flag-on wiring missing)")

// batchCoordinatorAgentRuns is the R7c run factory over the process binding.
type batchCoordinatorAgentRuns struct{}

// StartAgentTurn launches one incarnation: the pinned delegation leaf runs
// to completion on its own goroutine. R11F round-2 (P1-3): dispatch is by
// the RECORDED tool name (req.Tool) — an "agentic_fetch" payload resolves
// its own pinned leaf and names the call agentic_fetch; an empty name fails
// closed.
func (batchCoordinatorAgentRuns) StartAgentTurn(ctx context.Context, req batchAgentRunRequest, host childagent.Host) (childagent.Turn, error) {
	// R12-H2 P1-4: resolve the pinned leaf through the App-owned ingress
	// carried on the request (the invocation's own authority). The
	// process-global binding is only a fallback for flag-off-adjacent
	// fixtures — NEVER for provider-bound work: a bound incarnation with no
	// request ingress fails closed instead of touching process state.
	ingress := req.Ingress
	if ingress == nil {
		if req.DiskBindingID != "" {
			return nil, ErrDiskProviderUnavailable
		}
		if bind := installwire.ProcessBinding(processBatchBinding); bind != nil {
			ingress = bind.ingress
		}
	}
	if ingress == nil {
		return nil, errBatchCoordinatorNotBound
	}
	if req.RegistryID == "" {
		return nil, fmt.Errorf("%w: batch agent incarnation", errBatchMissingRegistryID)
	}
	if req.Tool == "" {
		// P1-3: fail closed rather than silently dispatching "agent".
		return nil, fmt.Errorf("%w: batch agent incarnation without a recorded tool name", batchinst.ErrBatchUnknownTool)
	}
	leaf, err := ingress.ResolveLeaf(req.RegistryID, req.Tool)
	if err != nil {
		return nil, err
	}
	if req.CallCredentials {
		credentialLeaf, ok := leaf.(interface {
			credentialEscrow() *batchCredentialEscrow
		})
		if !ok {
			return nil, batchadapter.ErrCredentialEscrowMissing
		}
		creds, releaseCredentials, err := credentialLeaf.credentialEscrow().consume(batchCredentialKey{
			registryID: req.RegistryID, sessionID: req.SessionID, messageID: req.MessageID,
			callID: req.ToolCallID, tool: req.Tool,
		}, string(req.NodeID))
		if err != nil {
			return nil, err
		}
		return startBatchAgentTurn(ctx, req, host, leaf, creds, releaseCredentials)
	}
	return startBatchAgentTurn(ctx, req, host, leaf, nil, func() {})
}

func startBatchAgentTurn(ctx context.Context, req batchAgentRunRequest, host childagent.Host, leaf fantasy.AgentTool, creds *CredentialSet, releaseCredentials func()) (childagent.Turn, error) {
	if req.DiskBindingID != "" && batchdispatch.DiskProviderFrom(ctx) == nil {
		releaseCredentials()
		return nil, ErrDiskProviderUnavailable
	}
	if req.AgentDepth < 0 || req.AgentDepth > 32 {
		return nil, errors.New("batch agent depth must be between 0 and 32")
	}
	depth := state.QuestionDepth(ctx) + 1
	if depth <= 0 {
		depth = 1
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if req.Policy != nil && req.Policy.RoleContract != nil {
		runCtx = WithCallRoleContract(runCtx, req.Policy.RoleContract)
	}
	if req.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, req.SelectedRoleContract)
	}
	if req.CallCredentials {
		runCtx = withCallCredentials(runCtx, creds)
	}
	runCtx = state.WithQuestionContext(runCtx, depth)
	// The question marker travels on EVERY batch incarnation's run context
	// (both values), so the per-call child toolset shaping can tell a
	// no-route incarnation from a non-batch one. The held-answer bridge
	// itself is armed ONLY for route-ful incarnations: an unattended
	// batch child never gets a working-looking ask.
	runCtx = batchmeta.WithBatchQuestionRoute(runCtx, req.QuestionRoute)
	if req.QuestionRoute {
		runCtx = withBatchAgentQuestionBinding(runCtx, batchAgentQuestionBinding{Ask: host.Ask})
	}
	tracked := childagent.TrackChild(host, req.ChildSessionID != "")
	lifecycle := state.Lifecycle{NoChildSelected: tracked.NoChildSelected, BindChildSession: tracked.BindChildSession}
	runCtx = state.WithLifecycle(runCtx, lifecycle)
	runCtx = context.WithValue(runCtx, tools.SessionIDContextKey, req.SessionID)
	runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, req.MessageID)
	// P1-4: stamp the TRUSTED enclosing invocation so the CLI bridge binds
	// to THIS invocation's pinned registry (never the latest one).
	runCtx = state.WithBridgeInvocation(runCtx, state.BridgeInvocation{RegistryID: req.RegistryID, MessageID: req.MessageID, AgentDepth: req.AgentDepth, AgentDepthSet: true})
	callOptions, err := WithAgentDepth(runCtx, req.AgentDepth)
	if err != nil {
		cancel()
		releaseCredentials()
		return nil, err
	}
	runCtx = callOptions
	var childScope *buildResourceScope
	var provider tools.DiskProvider
	if req.DiskBindingID != "" {
		provider = batchdispatch.DiskProviderFrom(ctx)
		if provider == nil {
			cancel()
			releaseCredentials()
			return nil, ErrDiskProviderUnavailable
		}
		co := boundCallOptions(provider, req.DiskBindingID, batchDiskRegistryFromContext(ctx))
		co.MaxAgentDepth = req.AgentDepth
		co.AgentDepthSet = true
		co.BuildScope = newBuildResourceScope()
		childScope = co.BuildScope
		runCtx = WithCallOptions(runCtx, co)
	}
	call := fantasy.ToolCall{ID: req.ToolCallID, Name: req.Tool, Input: string(req.Input)}
	return childagent.RunTool(runCtx, cancel, leaf, call, lifecycle.NoChildSelected, childScope.Close, releaseCredentials), nil
}

// CoordinatorBatchAgentDeps builds the flag-on agent-family seams for the
// M1 install (BatchInstallDeps.AgentDeps). Before the process binding runs
// the family fails closed with errBatchCoordinatorNotBound.
// CoordinatorBatchAgentDeps builds the flag-on agent-family seams WITHOUT
// the store-backed descendant reader: DescendantsLive then reports an
// INCOMPLETE read (false, false) — the scope stays open, never an optimistic
// closure. Flagged installs use CoordinatorBatchAgentDepsWithLive (P1-5b).
func CoordinatorBatchAgentDeps() BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{},
	}
}
