package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

type escrowTestTool struct {
	started chan context.Context
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (t *escrowTestTool) Info() fantasy.ToolInfo                     { return fantasy.ToolInfo{Name: "agent"} }
func (t *escrowTestTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *escrowTestTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (t *escrowTestTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	t.started <- ctx
	<-t.release
	return fantasy.NewTextResponse("done"), nil
}
func (t *escrowTestTool) count() int { t.mu.Lock(); defer t.mu.Unlock(); return t.calls }

type escrowTestIngress struct{ store *BatchLeafRegistryStore }

func (i escrowTestIngress) Invoke(context.Context, BatchInvocation, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, nil
}
func (i escrowTestIngress) PinLeafRegistry(*BatchLeafRegistry) (string, error) { return "", nil }
func (i escrowTestIngress) ResolveLeaf(registryID, name string) (fantasy.AgentTool, error) {
	registry, err := i.store.Resolve(registryID)
	if err != nil {
		return nil, err
	}
	return registry.resolve(name)
}

func (i escrowTestIngress) Registry(registryID string) (*BatchLeafRegistry, error) {
	return i.store.Resolve(registryID)
}

type escrowAgentExecutor struct {
	deps     batchAgentDeps
	ingress  BatchWorkIngress
	mu       sync.Mutex
	reqs     []batchAgentRunRequest
	payloads [][]byte
	turns    []batchAgentTurn
	wg       sync.WaitGroup
	launched chan struct{}
}

func (e *escrowAgentExecutor) Start(ctx context.Context, launch jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	var payload batchLeafPayload
	if err := json.Unmarshal(launch.Task.Payload, &payload); err != nil {
		return nil, err
	}
	messageID := ""
	if payload.Policy != nil {
		messageID = payload.Policy.MessageID
	}
	req := batchAgentRunRequest{
		NodeID: launch.ID, Token: launch.Token, SessionID: payload.SessionID,
		ToolCallID: payload.ToolCallID, Input: payload.Input, RegistryID: payload.RegistryID,
		MessageID: messageID, Tool: payload.Tool, CallCredentials: payload.CallCredentials,
		Ingress: e.ingress,
	}
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	e.payloads = append(e.payloads, append([]byte(nil), launch.Task.Payload...))
	e.mu.Unlock()
	turn, err := e.deps.Runs.StartAgentTurn(ctx, req, batchAgentHost{})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.turns = append(e.turns, turn)
	e.mu.Unlock()
	e.wg.Add(1)
	if e.launched != nil {
		e.launched <- struct{}{}
	}
	go func() {
		defer e.wg.Done()
		<-turn.Done()
		if err := turn.Err(); err != nil {
			report(jobbatch.Result{State: jobbatch.StateFailed, Code: "agent_failed"})
			return
		}
		report(jobbatch.Result{State: jobbatch.StateCompleted})
	}()
	return escrowTurnExecution{turn: turn}, nil
}

func (e *escrowAgentExecutor) snapshot() ([]batchAgentRunRequest, [][]byte, []batchAgentTurn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]batchAgentRunRequest(nil), e.reqs...),
		append([][]byte(nil), e.payloads...),
		append([]batchAgentTurn(nil), e.turns...)
}

type escrowTurnExecution struct{ turn batchAgentTurn }

func (e escrowTurnExecution) Stop(ctx context.Context) error { return e.turn.Stop(ctx) }
func (e escrowTurnExecution) Dispose(context.Context) error {
	select {
	case <-e.turn.Exit():
		return nil
	default:
		return errors.New("driver has not exited")
	}
}

func escrowAgentSpec(t *testing.T, callID string) jobbatch.Spec {
	t.Helper()
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: callID, SessionID: "session", RegistryID: "registry",
		CallCredentials: true,
		Input:           json.RawMessage(`{"prompt":"work"}`),
		Policy:          &batchLeafPolicyContext{MessageID: "message", RestrictedToolset: "agent"},
	})
	require.NoError(t, err)
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}}
}

func TestBatchCredentialDriverRejectsMissingEscrow(t *testing.T) {
	tool := &escrowTestTool{started: make(chan context.Context, 1), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-tool.release:
		default:
			close(tool.release)
		}
	})
	store := NewBatchLeafRegistryStore()
	registry := newPolicyToolBuild([]fantasy.AgentTool{tool}).leaves
	require.NoError(t, store.Register("registry", registry))
	req := batchAgentRunRequest{
		NodeID: "root.1", SessionID: "session", MessageID: "message", ToolCallID: "call",
		Tool: "agent", RegistryID: "registry", CallCredentials: true, Ingress: escrowTestIngress{store: store},
	}
	turn, err := (batchCoordinatorAgentRuns{}).StartAgentTurn(context.Background(), req, batchAgentHost{})
	if turn != nil {
		close(tool.release)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = turn.Stop(cleanupCtx)
		select {
		case <-turn.Exit():
		case <-cleanupCtx.Done():
			t.Error("agent driver failed to exit")
		}
	}
	require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
	require.EqualError(t, err, "batch credential escrow entry missing or already consumed")
	require.Zero(t, tool.count())
}

func TestBatchCredentialSubmitAddDriverLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := NewBatchLeafRegistryStore()
	tool := &escrowTestTool{started: make(chan context.Context, 3), release: make(chan struct{})}
	registry := newPolicyToolBuild([]fantasy.AgentTool{tool}).leaves
	require.NoError(t, store.Register("registry", registry))
	ingress := escrowTestIngress{store: store}
	executor := &escrowAgentExecutor{deps: CoordinatorBatchAgentDeps(), ingress: ingress, launched: make(chan struct{}, 2)}
	runner, err := jobbatch.NewRunner(context.Background(), jobbatch.RunnerConfig{
		Executor: executor, Limits: jobbatch.Limits{MaxConcurrent: 4, MaxDepth: 8, MaxNodes: 16},
		ControlTimeout: time.Second, RecordRetention: 16,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case <-tool.release:
		default:
			close(tool.release)
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Close(closeCtx)
		_, _, turns := executor.snapshot()
		for _, turn := range turns {
			select {
			case <-turn.Exit():
			case <-closeCtx.Done():
				t.Errorf("agent driver failed to exit during cleanup")
			}
		}
		executor.wg.Wait()
	})
	adapter := newBatchRunnerAdapter(runner)
	adapter.credentials = store.credentialEscrow()
	root := jobbatch.ID("root-credential-test")
	creds := escrowTestCreds("tenant-secret")
	ctx = withCallCredentials(ctx, creds)
	_, batch, err := adapter.SubmitBound(ctx, jobbatch.SubmitRequest{Root: root, Spec: escrowAgentSpec(t, "submit-call")})
	require.NoError(t, err)
	firstCtx := receiveEscrowStarted(t, ctx, tool.started)
	receiveEscrowLaunched(t, ctx, executor.launched)
	firstCreds := callCredentialsFrom(firstCtx)
	require.NotNil(t, firstCreds)
	require.Equal(t, "tenant-secret", firstCreds.Credentials[0].APIKey)
	creds.Credentials[0].APIKey = "mutated"
	require.Equal(t, "tenant-secret", firstCreds.Credentials[0].APIKey)
	addCreds := escrowTestCreds("second-tenant-secret")
	addCtx := withCallCredentials(context.Background(), addCreds)

	ids, err := adapter.Add(addCtx, jobbatch.AddRequest{Parent: root, Items: []jobbatch.Spec{escrowAgentSpec(t, "add-call")}})
	require.NoError(t, err)
	require.Len(t, ids, 1)
	secondCtx := receiveEscrowStarted(t, ctx, tool.started)
	receiveEscrowLaunched(t, ctx, executor.launched)
	require.Equal(t, "second-tenant-secret", callCredentialsFrom(secondCtx).Credentials[0].APIKey)
	requests, payloads, turns := executor.snapshot()
	require.Len(t, requests, 2)
	require.Len(t, turns, 2)
	require.Len(t, payloads, 2)
	for i, request := range requests {
		require.True(t, request.CallCredentials, "request %d carries the marker", i)
		require.Contains(t, string(payloads[i]), `"call_credentials":true`)
		require.NotContains(t, string(payloads[i]), "tenant-secret")
		require.NotContains(t, string(payloads[i]), "second-tenant-secret")
	}
	close(tool.release)
	for _, turn := range turns {
		select {
		case <-turn.Exit():
		case <-time.After(5 * time.Second):
			t.Fatal("agent driver did not exit")
		}
	}
	closeCtx, cancelClose := context.WithTimeout(ctx, 5*time.Second)
	defer cancelClose()
	_, err = batch.Wait(closeCtx)
	require.NoError(t, err)
	pendingKey := batchCredentialKey{registryID: "registry", sessionID: "session", messageID: "message", callID: "unstarted-call", tool: "agent"}
	require.NoError(t, store.credentialEscrow().register(pendingKey, string(root), escrowTestCreds("pending-secret")))
	require.NoError(t, adapter.Release(closeCtx, root))
	_, _, err = store.credentialEscrow().consume(pendingKey, string(root)+".1")
	require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
	for _, req := range requests {
		_, _, err := store.credentialEscrow().consume(batchCredentialKey{
			registryID: req.RegistryID, sessionID: req.SessionID, messageID: req.MessageID,
			callID: req.ToolCallID, tool: req.Tool,
		}, string(req.NodeID))
		require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
	}
	require.NoError(t, runner.Close(closeCtx))
	executor.wg.Wait()
}

func receiveEscrowLaunched(t *testing.T, ctx context.Context, launched <-chan struct{}) {
	t.Helper()
	select {
	case <-launched:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for agent driver launch: %v", ctx.Err())
	}
}

func receiveEscrowStarted(t *testing.T, ctx context.Context, started <-chan context.Context) context.Context {
	t.Helper()
	select {
	case got := <-started:
		return got
	case <-ctx.Done():
		t.Fatalf("timed out waiting for agent tool start: %v", ctx.Err())
		return nil
	}
}
