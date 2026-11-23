package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
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
		runCtx = withRoleToolContract(runCtx, p.Policy.RoleContract)
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
