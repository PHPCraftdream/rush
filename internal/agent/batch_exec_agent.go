package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

type batchAgentRunRequest = state.RunRequest[*batchLeafPolicyContext, BatchWorkIngress]

type (
	batchAgentRunFactory = state.RunFactory[batchAgentRunRequest]

	batchAgentDescendantReader = state.DescendantReader[batchAgentRunRequest]
	batchAgentDeps             = state.Deps[batchAgentRunRequest]
)

// batchAgentFamily implements the engine executor contract for the agent
// leaf kind. It is inert until registered with a batchExecutor by R11.
type batchAgentFamily struct{ deps batchAgentDeps }

func newBatchAgentFamily(deps batchAgentDeps) batchLeafFamily {
	return batchAgentFamily{deps: deps}
}

var _ batchLeafFamily = batchAgentFamily{}

// callIngress resolves the env's lazy ingress nil-safely: non-agent-family
// envs may carry no Ingress at all.
func callIngress(env batchLeafEnv) BatchWorkIngress {
	if env.Ingress == nil {
		return nil
	}
	return env.Ingress()
}

func (f batchAgentFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if invalid, _ := ctx.Value(batchdispatch.AgentDepthInvalidContextKey{}).(bool); invalid {
		return nil, errors.New("batch agent depth must be between 0 and 32")
	}
	if p.AgentDepth != nil && (*p.AgentDepth < 0 || *p.AgentDepth > MaxAgentDepthForResume) {
		return nil, errors.New("batch agent depth must be between 0 and 32")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	messageID := ""
	if p.Policy != nil {
		messageID = p.Policy.MessageID
	}
	req := batchAgentRunRequest{
		NodeID: l.ID, Token: l.Token, SessionID: p.SessionID,
		ToolCallID: p.ToolCallID, Input: p.Input,
		RegistryID: p.RegistryID, MessageID: messageID,
		DiskBindingID: func() string {
			if p.Policy != nil {
				return p.Policy.DiskBindingID
			}
			return ""
		}(),
		Policy: p.Policy, SelectedRoleContract: cloneRoleContractForCall(p.SelectedRoleContract), Tool: p.Tool, CallCredentials: p.CallCredentials,
		QuestionRoute: p.QuestionRoute,
		AgentDepth: func() int {
			if p.AgentDepth != nil {
				return *p.AgentDepth
			}
			return DefaultAgentDepth
		}(),
	}
	req.SetHandoff(pctClaimCapturedHandoffViaIngress(env, p))
	req.Ingress = callIngress(env)
	e := &batchAgentExecution{
		cache: env.OutputCache, nodeID: l.ID, rootID: batchmeta.OutputRoot(l.ID),
		deps:          f.deps,
		req:           req,
		pctHandoff:    req.Handoff(),
		scope:         newBatchAgentScope(),
		driverDone:    make(chan struct{}),
		watcherDone:   make(chan struct{}),
		finished:      make(chan struct{}),
		questionRoute: req.QuestionRoute,
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = toolwrap.WithRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.Policy != nil && p.Policy.DiskBindingID != "" && p.diskProvider == nil {
		cancel()
		if e.pctHandoff != nil {
			e.pctHandoff.Release()
		}
		return nil, ErrDiskProviderUnavailable
	}
	// R12-H2 P1-4: carry the App-owned binding registry into the driver so
	// WithCallOptions/diskBinding and any re-registration resolve
	// through the SAME instance (never a process global). env.DiskBindings
	// is injected by the runtime's LeafEnv.
	runCtx = withDiskProviderRegistry(runCtx, env.DiskBindings)
	if p.diskProvider != nil && p.Policy != nil {
		runCtx = WithCallOptions(runCtx, boundCallOptions(p.diskProvider, p.Policy.DiskBindingID, env.DiskBindings))
		// R12-H2 P1-4: stamp the provider the way generic leaves do
		// (batchLeafRunContext), so batchDiskProviderFrom(runCtx) works in
		// the driver. NOT batchLeafRunContext — it would re-stamp
		// session/message, which the agent family sets below.
		runCtx = context.WithValue(runCtx, batchdispatch.DiskProviderContextKey{}, p.diskProvider)
	}
	runCtx = context.WithValue(runCtx, tools.SessionIDContextKey, p.SessionID)
	runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, messageID)
	runCtx = state.WithBridgeInvocation(runCtx, state.BridgeInvocation{
		RegistryID: p.RegistryID, MessageID: messageID,
		AgentDepth: func() int {
			if p.AgentDepth != nil {
				return *p.AgentDepth
			}
			return DefaultAgentDepth
		}(), AgentDepthSet: true,
	})
	depthCap := DefaultAgentDepth
	if p.AgentDepth != nil {
		depthCap = *p.AgentDepth
	}
	runCtx, err := WithAgentDepth(runCtx, depthCap)
	if err != nil {
		cancel()
		return nil, err
	}
	if e.pctHandoff != nil {
		// pct: carry this executor's handoff into the child agent run context.
		runCtx = toolwrap.WithDelegationHandoff(runCtx, e.pctHandoff)
	}
	turn, err := e.launch(runCtx)
	if err != nil {
		cancel()
		if e.pctHandoff != nil {
			e.pctHandoff.Release()
		}
		return nil, err
	}
	e.mu.Lock()
	e.turn = turn
	e.mu.Unlock()
	state.StartExecutionWorkers(&e.internal, e.driverDone, turn, func() { e.watch(runCtx, turn, report) })
	return e, nil
}

func (e *batchAgentExecution) snapshotExecutionRequest() batchAgentRunRequest {
	return state.SnapshotRequest(&e.mu, &e.req)
}

func (e *batchAgentExecution) bindChildSession(ctx context.Context, id string) error {
	return state.BindChildSession(ctx, &e.mu, &e.req, e.deps.Bind, e.rootID, e.nodeID, id)
}

func (e *batchAgentExecution) recordExecutionNoChild() {
	state.MarkNoChildSelected(&e.mu, &e.req)
}

func (e *batchAgentExecution) launch(runCtx context.Context) (childagent.Turn, error) {
	return state.Launch(runCtx, e.deps.Runs, e.snapshotExecutionRequest, childagent.Host{
		Ask: e.askBatchQuestion, BindChildSession: e.bindChildSession, NoChildSelected: e.recordExecutionNoChild,
	}, func(err error) error { return batchdispatch.NewStartError("agent_start_panic", err) })
}

type batchAgentExecution struct {
	cache  *batchOutputCache
	nodeID jobbatch.ID
	rootID jobbatch.ID
	deps   batchAgentDeps
	req    batchAgentRunRequest
	scope  *batchAgentScope
	cancel context.CancelFunc

	mu            sync.Mutex
	turn          childagent.Turn
	lifecycle     state.ExecutionLifecycle
	pctHandoff    *toolwrap.PctDelegationHandoff
	driverDone    chan struct{} // Driver goroutine exit: late kill barrier.
	watcherDone   chan struct{}
	finished      chan struct{}  // Closed once, when the scope result is fixed.
	internal      sync.WaitGroup // Adapter goroutines; zero again after Dispose.
	questionRoute bool
}

var (
	_ jobbatch.Execution    = (*batchAgentExecution)(nil)
	_ jobbatch.OutputReader = (*batchAgentExecution)(nil)
	_ jobbatch.Messenger    = (*batchAgentExecution)(nil)
	_ jobbatch.Disposer     = (*batchAgentExecution)(nil)
)

// heldDepth reports the depth of the currently held question, if any.
func (e *batchAgentExecution) heldDepth() (int, bool) {
	return e.scope.HeldDepth()
}

// ask implements the host question channel of one incarnation.
func (e *batchAgentExecution) askBatchQuestion(ctx context.Context, depth int, question string) (json.RawMessage, error) {
	// Runtime guard, ahead of hold AND publish: a no-route incarnation
	// (stale wrapper, rebuilt payload without the field, or a forged
	// ask call) can never park the scope or emit a child_question
	// notice, so there is no eternal await and no fake success.
	if !e.questionRoute {
		return nil, batchinst.ErrBatchQuestionNoRoute
	}
	h, err := e.scope.hold(depth)
	if err != nil {
		return nil, err
	}
	if err := e.publishQuestion(ctx, depth, question); err != nil {
		// Nothing was published for this question, so there is no published
		// notice to resolve — discard WITHOUT resolution.
		e.scope.discardHold(h)
		return nil, err
	}
	select {
	case answer := <-h.Answer():
		return answer, nil
	case <-ctx.Done():
		resolved := h.Sequence() // stamped by publishQuestion before the wait
		resolveErr := e.resolvePublishedQuestions(ctx, resolved)
		if resolveErr != nil {
			// Один повтор после transient SQL failure; затем завершаем turn ошибкой.
			resolveErr = e.resolvePublishedQuestions(ctx, resolved)
		}
		e.scope.discardHold(h)
		return nil, errors.Join(ctx.Err(), resolveErr)
	}
}

const batchQuestionPersistenceTimeout = 5 * time.Second

func (e *batchAgentExecution) publishQuestion(ctx context.Context, depth int, question string) error {
	if e.deps.QuestionPublisher == nil {
		return errors.New("batch question: publisher unavailable")
	}
	req := e.snapshotExecutionRequest()
	return e.scope.PublishQuestion(func(sequence uint64) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchQuestionPersistenceTimeout)
		defer cancel()
		return e.deps.QuestionPublisher.PublishChildQuestion(ctx, childagent.QuestionNotice{Owner: req.SessionID, ChildSessionID: req.ChildSessionID, NodeID: string(req.NodeID), Token: req.Token, Depth: depth, Sequence: sequence, Question: question})
	})
}

// watch waits for turn completion, then confirmed scope closure, and reports exactly once.
func (e *batchAgentExecution) watch(ctx context.Context, turn childagent.Turn, report func(jobbatch.Result)) {
	defer close(e.watcherDone)
	read := func(ctx context.Context, req batchAgentRunRequest) (bool, bool) {
		return e.deps.Descendants.DescendantsLive(ctx, req)
	}
	if !state.WaitForClosure(ctx, turn.Done(), e.scope.state, e.deps.Poll, e.snapshotExecutionRequest, read) {
		return
	}
	// Package S (jb/psc): settle with the child's refreshed final outcome
	// for this delegation, never the captured initial (pre-reaction)
	// answer.
	res := pscRefreshedTurnResult(e.deps.ChildFacts, e.snapshotExecutionRequest(), turn.Result(), turn.Err())
	e.finish(state.ScopeResult(res.Response, res.Declared, turn.Err(), e.scope.stop()), report)
}

// finish fixes the terminal result once, retains the output and reports.
func (e *batchAgentExecution) finish(result jobbatch.Result, report func(jobbatch.Result)) {
	var retain func(jobbatch.OutputChunk)
	if e.cache != nil {
		retain = func(chunk jobbatch.OutputChunk) { e.cache.putFor(e.nodeID, e.rootID, chunk) }
	}
	e.lifecycle.Finish(&e.mu, e.finished, result, retain, report)
}

// Stop cancels the run context (the addressed subtree at depth), records the
// stop intent and returns the turn's acknowledgement. It never settles.
func (e *batchAgentExecution) Stop(ctx context.Context) error {
	e.mu.Lock()
	first, err := e.lifecycle.BeginStopLocked()
	if err != nil {
		e.mu.Unlock()
		return err
	}
	turn := e.turn
	e.mu.Unlock()
	if first {
		e.scope.beginStop()
		e.cancel()
	}
	if turn == nil {
		return nil
	}
	return turn.Stop(ctx)
}

// Резолюция должна зафиксироваться до принятия ответа в held scope.
func (e *batchAgentExecution) resolvePublishedQuestions(ctx context.Context, sequence uint64) error {
	return state.ResolvePublishedQuestions(ctx, e.deps.QuestionResolver, e.snapshotExecutionRequest, batchQuestionPersistenceTimeout, sequence)
}

// Inject delivers an answer or message into this incarnation. It is rejected
// deterministically when the node is stopping, finished or holds no matching
// question, so a message is never lost silently nor delivered to a new
// incarnation.
func (e *batchAgentExecution) Inject(ctx context.Context, msg json.RawMessage) error {
	disposed, finished, stopped := e.lifecycle.Status(&e.mu)
	if disposed {
		return jobbatch.ErrReleased
	}
	if finished {
		return ErrBatchAgentStaleGeneration
	}
	if stopped {
		return ErrBatchAgentStopped
	}
	var m batchinst.AgentInjectMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return fmt.Errorf("%w: %v", ErrBatchAgentNotHeld, err)
	}
	return e.scope.deliverWithCommit(m, func() error {
		return e.resolvePublishedQuestions(ctx, m.Sequence)
	})
}

// ReadOutput returns the retained result summary as one terminal chunk.
func (e *batchAgentExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return e.lifecycle.ReadOutput(ctx, &e.mu, e.finished, cursor)
}

// Dispose waits for the late kill (driver exit) and the watcher, then
// releases generation state. It deliberately does NOT release the
// execution owner: the interface-preserving physical-Dispose wrapper
// (batch_exec_binding.go) is the ONLY execution-owner releaser, and only
// after this disposal — including the child-driver exit drain — succeeded.
func (e *batchAgentExecution) Dispose(ctx context.Context) error {
	return e.lifecycle.Dispose(ctx, &e.mu, e.finished, e.driverDone, e.watcherDone, e.pctHandoff)
}

// pscRefreshedTurnResult uses the child's final outcome at confirmed closure.
func pscRefreshedTurnResult(facts state.BatchChildReactionFacts, req batchAgentRunRequest, res childagent.TurnResult, turnErr error) childagent.TurnResult {
	if response, ok := state.RefreshTurnResponse(facts, req, res.Response, turnErr); ok {
		return childagent.TurnResult{Response: response}
	}
	return res
}

// pctClaimCapturedHandoffViaIngress defers the batch-inst-only callIngress
// registry resolution into the toolwrap claim: callIngress/Registry/resolve
// run only when toolwrap.ClaimCapturedHandoff invokes the resolver, and the
// unwrapping stays the exact pctCapturedToolFrom traversal in package agent.
func pctClaimCapturedHandoffViaIngress(env batchLeafEnv, p batchLeafPayload) *toolwrap.PctDelegationHandoff {
	return toolwrap.ClaimCapturedHandoff(func() (toolwrap.CapturedClaimer, error) {
		ingress := callIngress(env)
		if ingress == nil {
			return nil, nil
		}
		registry, err := ingress.Registry(p.RegistryID)
		if err != nil {
			return nil, nil
		}
		leaf, err := registry.Resolve(p.Tool)
		if err != nil {
			return nil, nil
		}
		captured := childagent.CapturedAgentToolFrom(leaf)
		if captured == nil {
			// explicit nil: a typed-nil *pctCapturedAgentTool must not become
			// a non-nil CapturedClaimer interface value.
			return nil, nil
		}
		return captured, nil
	}, p.SessionID, p.ToolCallID)
}

/*
batch_exec_agent_doc.go — R7c agent scope adapter: behavior mapping and
assumed contracts. The adapter is the LIVE delegation authority: a batch
executor runs it through the registered batchAgentFamily (the deleted
asyncTool path is gone).

The adapter is one engine leaf for a delegation (agent / agentic_fetch). Its
driver is an injected run factory (one agent turn loop), its scope is
batchAgentScope, and the live-descendant fact comes from an injected reader
over the R10a live-root seam. No global state; everything is injected.

Old agent behavior -> new counterpart:

| Old behavior (async agent path) | New counterpart |
| --- | --- |
| Agent tool wraps a nested Run in a goroutine; tool response is not termination (the deleted asyncTool path) | Run factory launches one turn loop (batchAgentRunFactory); report comes only from the scope watcher, never from the tool response. |
| Finalization parks results; release checks child work, shells, holds (the deleted ledger delegation release) | Scope closes only when the turn ended AND no question is held AND descendants are confirmed terminal; report after closure, exactly once. |
| Live child work discovered via lag-tolerant scans (the deleted ledger) | Injected batchAgentDescendantReader over R10a; incomplete or unknown reads look live (conservative-read rule) and keep the scope open. |
| Held question: the driver is resumed via the held-answer bridge; ask_question returns a question-shaped response | Route-ful only: a held question exists ONLY for incarnations whose durable payload carries question_route (served API intake or a parent-owned orchestrator delegation). Any depth holds via batchAgentHost.Ask; the ask_question tool returns a successful question-shaped response; the node stays live; the answer arrives by Inject and is routed to exactly the addressed depth and published question sequence. An unattended standalone batch (`rush batch run`) stamps no route, advertises no ask_question, and rejects any stale or forged ask before a hold or a child_question notice can exist. The capability travels the direct-call adapter too (invokeBatchWithCallContext stamps it like the model control) and is re-armed onto wake-driven turns from the driver call template (batchmeta.ArmBatchQuestionTurnContext in runTurn); a durable row never carries it — durable queue producers refuse capability-bearing calls fail-closed (ErrBatchQuestionCapabilityNotDurable), so nothing to re-derive exists, and a batch child's restart re-runs the durable payload. |
| Inject via inject_agent into a child session (AgentControl) | Execution.Inject parses batchAgentInjectMessage{depth,sequence,text}; rejection is deterministic (stopped, stale generation, no matching depth/sequence) — never a silent loss, never delivery to a new incarnation. |
| Stop/run cancel commit "stopped" before physical termination | Removed per plan correction 3: Stop records intent, cancels the run context (the addressed subtree at depth) and returns the turn acknowledgement; only confirmed closure settles (Cancelled). Siblings are separate executions and untouched. |
| Stop ack / timeout racing settlement | Stop acknowledgement never settles; batchAgentScopeResult reports Cancelled only from confirmed closure facts. |
| Generation-bound policy inheritance, driver registration before execution (coordinator_subagents) | Per-incarnation state (question channel, injected messages, watchers) lives in one batchAgentExecution; Dispose waits for the late kill (driver Exit) and releases it; a finished incarnation rejects Inject as stale. |
| Stream watchdog panic containment around nested Run | Run factory panics are contained at Start (agent_start_panic) and classification happens outside the factory goroutine. |
| Denied/failed nested runs surface as prose | A run error is Failed agent_run_error, an error response is Failed agent_error, with the envelope retained as Details and the summary readable via ReadOutput. |

Contracts assumed from the engine (internal/jobbatch):

- Start returns a non-nil error only when no execution exists; report states
  the execution stopped and is called exactly once (executor.go).
- Stop may overlap ReadOutput, Inject and report, never another Stop.
- Messenger, OutputReader and Disposer are optional capabilities; Dispose is
  called once after termination is reported and the handle is given up, and
  is the release barrier for generation state.
- Launch.ID/Token identify the admission attempt; Payload carries
  batchLeafPayload; the pinned policy leaf resolves the tool (R1) — the
  agent family does not resolve leaves, the run factory does.
- Result.State must be Reportable; codes follow the Runner machine-code rule
  (agent_start_panic, agent_run_error, agent_error are new nonreserved
  codes, consistent with the sibling adapters' tool_run_error/tool_error).

Open decisions (smallest choice made, per plan row R7c):

- task_outcome (design 9.3): D7 says it is not introduced now. The adapter
  carries an optional Declared outcome on the turn result that may only
  reclassify a clean closure to a Reportable state, is scoped to this
  incarnation and can never overwrite a terminal node. Whether any tool
  surface will set it remains undecided upstream; nothing sets it today.
- Descendant reads have no push event in the R10a seam, so the watcher polls
  at batchAgentDeps.Poll (default 10 ms) plus change signals; a push seam
  would remove the poll and is deferred.
*/

type batchGenericFamily struct{}

func newBatchGenericFamily() batchLeafFamily { return batchGenericFamily{} }

func (batchGenericFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batchControlTarget(p.Tool) {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, errors.New("control-plane target"))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil {
		if err := p.Policy.RoleContract.Validate(); err != nil {
			return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, err)
		}
		if !roleContractAllowsTool(p.Policy.RoleContract, p.Tool) {
			return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, fmt.Errorf("role tool denied: %s", p.Tool))
		}
	}
	if p.SelectedRoleContract != nil {
		if err := batchinst.ValidateSelectedRole(p.SelectedRoleContract, p.SelectedRoleContract.Name, func() *session.RoleContract {
			if p.Policy == nil {
				return nil
			}
			return p.Policy.RoleContract
		}()); err != nil {
			return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, err)
		}
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	if p.Policy == nil {
		runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, tools.GetMessageFromContext(ctx))
		runCtx = context.WithValue(runCtx, tools.SupportsImagesContextKey, tools.GetSupportsImagesFromContext(ctx))
		runCtx = context.WithValue(runCtx, tools.ModelNameContextKey, tools.GetModelNameFromContext(ctx))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = toolwrap.WithRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	runCtx = batchlife.WithBatchManagedCall(batchLeafRunContext(runCtx, p))
	// Restore trusted inherited and selected caps after any call-options
	// rebuild performed while decorating the leaf execution context.
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = WithCallRoleContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	// R12-H2 P1-4: batchLeafRunContext does NOT stamp the binding registry;
	// carry the App-owned registry so leaf-side binding resolution /
	// batchDiskRegistryFromContext resolve the SAME instance, never a
	// process global.
	runCtx = withDiskProviderRegistry(runCtx, env.DiskBindings)
	if p.diskProvider != nil && p.Policy != nil {
		runCtx = WithCallOptions(runCtx, boundCallOptions(p.diskProvider, p.Policy.DiskBindingID, env.DiskBindings))
	}
	e := batchexec.NewGeneric(cancel, batchExecutionOutput(env.OutputCache, l.ID, batchmeta.OutputRoot(l.ID), batchmeta.OutputCursorClamp))
	call := fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}
	go e.Run(runCtx, leaf, call, report)
	return e, nil
}

func batchControlTarget(name string) bool {
	switch name {
	case "await_tasks", "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop",
		"ask_question", "wakein", "wakeon", "loop", "wake_list", "wake_cancel",
		"inspect_agent", "inject_agent", "stop_agent":
		return true
	}
	return false
}

func batchExecutionOutput(cache *batchOutputCache, id, root jobbatch.ID, contract batchmeta.OutputCursorContract) batchexec.Output {
	output := batchexec.Output{Result: batchdispatch.ResultFromResponse}
	if cache != nil {
		output.Store = func(chunk jobbatch.OutputChunk) { cache.putForContract(id, root, chunk, contract) }
	}
	return output
}
