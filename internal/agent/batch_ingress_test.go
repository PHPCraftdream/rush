package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// newTestLeaf is a minimal fantasy.AgentTool standing in for a pinned leaf.
type testLeaf struct {
	name string
}

func newTestLeaf(name string) fantasy.AgentTool { return &testLeaf{name: name} }

func (l *testLeaf) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: l.name}
}

func (l *testLeaf) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, nil
}

func (l *testLeaf) ProviderOptions() fantasy.ProviderOptions { return nil }

func (l *testLeaf) SetProviderOptions(fantasy.ProviderOptions) {}

func newTestCall(id, input string) fantasy.ToolCall {
	return fantasy.ToolCall{ID: id, Name: "view", Input: input}
}

func mustCanonical(t *testing.T, input string) json.RawMessage {
	t.Helper()
	raw, err := batchCanonicalInput(input)
	require.NoError(t, err)
	return raw
}

func decodeLeafPayload(spec jobbatch.Spec) (batchLeafPayload, error) {
	if spec.Task == nil {
		return batchLeafPayload{}, errors.New("test spec: not a task")
	}
	var p batchLeafPayload
	err := json.Unmarshal(spec.Task.Payload, &p)
	return p, err
}

// fakeBatchRunner records Submit calls; Stop is accepted silently. An
// optional gate blocks Submit until released, to hold a first flight in
// flight deterministically.
type fakeBatchRunner struct {
	mu      sync.Mutex
	reqs    []jobbatch.SubmitRequest
	calls   int
	err     error
	started chan struct{}
	gate    chan struct{}
}

func newFakeRunner(err error) *fakeBatchRunner {
	return &fakeBatchRunner{err: err, started: make(chan struct{}, 64)}
}

func (f *fakeBatchRunner) Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.calls++
	err := f.err
	f.mu.Unlock()
	f.started <- struct{}{}
	if f.gate != nil {
		<-f.gate
	}
	if err != nil {
		return "", err
	}
	return req.Root, nil
}

func (f *fakeBatchRunner) Stop(context.Context, jobbatch.ID) error { return nil }

func (f *fakeBatchRunner) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBatchRunner) submitCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBatchRunner) submitSpecs() []jobbatch.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	specs := make([]jobbatch.Spec, len(f.reqs))
	for i, req := range f.reqs {
		specs[i] = req.Spec
	}
	return specs
}

// fakeDelivery is the narrow R11D stand-in: one canned response, or a
// numbered response per delivery when numbered is set.
type fakeDelivery struct {
	mu       sync.Mutex
	calls    int
	resp     fantasy.ToolResponse
	roots    []jobbatch.ID
	numbered bool
}

func newFakeDelivery(resp fantasy.ToolResponse) *fakeDelivery {
	return &fakeDelivery{resp: resp}
}

func (d *fakeDelivery) DeliverBatch(_ context.Context, root jobbatch.ID, _ BatchInvocation) (fantasy.ToolResponse, error) {
	d.mu.Lock()
	d.calls++
	n := d.calls
	numbered := d.numbered
	d.roots = append(d.roots, root)
	d.mu.Unlock()
	if numbered {
		return fantasy.ToolResponse{Content: fmt.Sprintf("resp-%d", n)}, nil
	}
	return d.resp, nil
}

func (d *fakeDelivery) deliverCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *fakeDelivery) deliveredRoots() []jobbatch.ID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]jobbatch.ID(nil), d.roots...)
}

// fakeAttach is the in-memory R11B stand-in with owner-host and
// runner-incarnation fences and input-hash matching.
type fakeAttach struct {
	mu     sync.Mutex
	live   map[string]attachLive
	probes int
}

type attachLive struct {
	root       jobbatch.ID
	hash, host string
	runner     string
}

func newFakeAttach() *fakeAttach {
	return &fakeAttach{live: make(map[string]attachLive)}
}

func attachKey(owner, callID string) string { return owner + "\x00" + callID }

// errFakeFence marks an owner-host or runner-incarnation fence violation in
// the fake store; the ingress surfaces it unwrapped in the attach error.
var errFakeFence = errors.New("fake attach: fenced live root")

func (a *fakeAttach) ProbeLive(_ context.Context, p batchAttachProbe) (jobbatch.ID, bool, error) {
	a.mu.Lock()
	a.probes++
	live, ok := a.live[attachKey(p.Owner, p.ToolCallID)]
	a.mu.Unlock()
	if !ok {
		return "", false, nil
	}
	if live.hash != p.InputHash {
		return "", false, errBatchInvocationMismatch
	}
	if live.host != p.Host || live.runner != p.RunnerID {
		return "", false, errFakeFence
	}
	return live.root, true, nil
}

func (a *fakeAttach) seed(owner, callID, hash string, root jobbatch.ID, host, runner string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live[attachKey(owner, callID)] = attachLive{root: root, hash: hash, host: host, runner: runner}
}

func (a *fakeAttach) probeCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probes
}

// gateAttach blocks every ProbeLive until released or the caller's context
// is done, to hold a first flight inside the attach probe deterministically.
type gateAttach struct {
	enter   chan struct{}
	release chan struct{}
}

func (a *gateAttach) ProbeLive(ctx context.Context, _ batchAttachProbe) (jobbatch.ID, bool, error) {
	a.enter <- struct{}{}
	select {
	case <-a.release:
		return "", false, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

// TestInvokeConcurrentMatchingRetriesExecuteOnce: N goroutines race the same
// (owner session, tool call id) with the same input; exactly one execution
// happens and every retry observes the same delivered response. The first
// flight is held in flight until all retries have joined it, so no retry can
// arrive after completion and legitimately re-execute.
func TestInvokeConcurrentMatchingRetriesExecuteOnce(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	release := make(chan struct{})
	runner.gate = release
	delivery := newFakeDelivery(fantasy.ToolResponse{Content: "done"})
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = delivery
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)

	const retries = 8
	inv := testInvocation()
	start := make(chan struct{})
	responses := make(chan fantasy.ToolResponse, retries)
	errs := make(chan error, retries)
	for i := 0; i < retries; i++ {
		go func() {
			<-start
			resp, err := rt.Invoke(context.Background(), inv, newTestLeaf("view"),
				newTestCall("call-1", `{"a":1}`))
			responses <- resp
			errs <- err
		}()
	}
	close(start)
	<-runner.started // The first flight is submitted and gated.
	joined := rt.entryFor(inv.SessionID, "call-1").joined
	for i := 0; i < retries-1; i++ {
		<-joined // Every retry joined the in-flight entry.
	}
	close(release)
	for i := 0; i < retries; i++ {
		require.NoError(t, <-errs)
		resp := <-responses
		require.Equal(t, "done", resp.Content)
	}
	require.Equal(t, 1, runner.submitCalls(), "matching retries execute exactly once")
	require.Equal(t, 1, delivery.deliverCalls())
	require.Zero(t, rt.inFlightCalls(), "completed calls leave the singleflight map")
	specs := runner.submitSpecs()
	require.Len(t, specs, 1)
	require.NotNil(t, specs[0].Task)
	require.Equal(t, batchKindGeneric, specs[0].Task.Kind)
}

// TestInvokeMismatchedHashFails: the same call id with a different input is
// refused while the first flight is IN FLIGHT and never re-executed. After
// completion the entry is gone, so a different-input retry takes the fresh
// durable path (refused only when the store reports a live root).
func TestInvokeMismatchedHashFails(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	release := make(chan struct{})
	runner.gate = release
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	first := make(chan error, 1)
	go func() {
		_, err := rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"),
			newTestCall("call-1", `{"a":1}`))
		first <- err
	}()
	<-runner.started // The first flight is in flight.
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"),
		newTestCall("call-1", `{"a":2}`))
	require.ErrorIs(t, err, errBatchInvocationMismatch)
	require.Equal(t, 1, runner.submitCalls())
	close(release)
	require.NoError(t, <-first)
	require.Zero(t, rt.inFlightCalls())
}

// TestInvokeAttachesLiveRoot: a seeded live root matching the hash attaches
// and never submits; a fenced or mismatching live root fails closed.
func TestInvokeAttachesLiveRoot(t *testing.T) {
	t.Parallel()
	inv := testInvocation()
	call := newTestCall("call-1", `{"a":1}`)
	spec, err := batchOneItemSpec(inv, call, mustCanonical(t, `{"a":1}`))
	require.NoError(t, err)
	hash := batchInvocationHash(spec)

	runner := newFakeRunner(nil)
	delivery := newFakeDelivery(fantasy.ToolResponse{Content: "attached"})
	attach := newFakeAttach()
	cfg := testRuntimeConfig()
	cfg.Attach = attach
	cfg.Runner = runner
	cfg.Delivery = delivery
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)

	attach.seed(inv.SessionID, call.ID, hash, "root-live", cfg.Host, cfg.RunnerID)
	resp, err := rt.Invoke(context.Background(), inv, newTestLeaf("view"), call)
	require.NoError(t, err)
	require.Equal(t, "attached", resp.Content)
	require.Zero(t, runner.submitCalls(), "an attached retry never submits")
	require.Equal(t, []jobbatch.ID{"root-live"}, delivery.deliveredRoots())

	// A different input hash on the same live call fails closed.
	attach.seed(inv.SessionID, call.ID, "deadbeef", "root-live", cfg.Host, cfg.RunnerID)
	_, err = rt.Invoke(context.Background(), inv, newTestLeaf("view"), call)
	require.ErrorIs(t, err, errBatchInvocationMismatch)
}

// TestInvokeRefusesControlToolLeaves is the table over the full D1 set:
// control-plane tools execute their pinned policy leaf directly and are never
// generic batch targets.
func TestInvokeRefusesControlToolLeaves(t *testing.T) {
	t.Parallel()
	controlTools := []string{
		"await_tasks",
		"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop",
		"ask_question",
		"wakein", "wakeon", "loop", "wake_list", "wake_cancel",
		"inspect_agent", "inject_agent", "stop_agent",
	}
	runner := newFakeRunner(nil)
	delivery := newFakeDelivery(fantasy.ToolResponse{})
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = delivery
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	for _, name := range controlTools {
		call := newTestCall("call-1", `{}`)
		call.Name = name
		leaf := newTestLeaf(name)
		resp, err := rt.Invoke(context.Background(), testInvocation(), leaf, call)
		require.ErrorIs(t, err, errBatchControlPlaneLeaf, "tool %q", name)
		// The refusal must come from the ingress D1 gate, not later from
		// admission: the tool never reaches spec validation.
		require.False(t, errors.Is(err, errBatchSubmitBeforeInvalid), "tool %q", name)
		require.Zero(t, resp)
	}
	require.Zero(t, runner.submitCalls(), "no control tool reached the runner")
	require.Zero(t, delivery.deliverCalls(), "no control tool reached delivery")
}

func TestInvokeMissingCallIDFails(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"), newTestCall("", `{}`))
	require.ErrorIs(t, err, errBatchMissingCallID)
	require.Zero(t, runner.submitCalls())
}

func TestInvokeNilLeafFails(t *testing.T) {
	t.Parallel()
	cfg := testRuntimeConfig()
	cfg.Runner = newFakeRunner(nil)
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), nil, newTestCall("call-1", `{}`))
	require.ErrorIs(t, err, errBatchNilLeaf)
}

func TestInvokeSubmitErrorSurfaces(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(errors.New("engine down"))
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"),
		newTestCall("call-1", `{"a":1}`))
	require.ErrorContains(t, err, "engine down")
}

// TestInvokeEvictsEntryAfterCompletion: the singleflight map holds only
// IN-FLIGHT entries — success and failure both leave it empty.
func TestInvokeEvictsEntryAfterCompletion(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(errors.New("engine down"))
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{Content: "done"})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"),
		newTestCall("call-1", `{"a":1}`))
	require.ErrorContains(t, err, "engine down")
	require.Zero(t, rt.inFlightCalls(), "a failed call leaves the singleflight map")
	runner.setErr(nil)
	resp, err := rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"),
		newTestCall("call-1", `{"a":1}`))
	require.NoError(t, err)
	require.Equal(t, "done", resp.Content)
	require.Zero(t, rt.inFlightCalls(), "a completed call leaves the singleflight map")
}

// TestInvokeReexecutesCompletedCall: a retry after completion is a new
// execution by design (I5) — it never replays the first response, and a
// different input does not hit the in-flight mismatch refusal.
func TestInvokeReexecutesCompletedCall(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	delivery := newFakeDelivery(fantasy.ToolResponse{})
	delivery.numbered = true
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = delivery
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	inv := testInvocation()
	resp, err := rt.Invoke(context.Background(), inv, newTestLeaf("view"),
		newTestCall("call-1", `{"a":1}`))
	require.NoError(t, err)
	require.Equal(t, "resp-1", resp.Content)
	// Same input: executed again, not replayed.
	resp, err = rt.Invoke(context.Background(), inv, newTestLeaf("view"),
		newTestCall("call-1", `{"a":1}`))
	require.NoError(t, err)
	require.Equal(t, "resp-2", resp.Content)
	// Different input after completion: a fresh execution, no mismatch.
	resp, err = rt.Invoke(context.Background(), inv, newTestLeaf("view"),
		newTestCall("call-1", `{"a":2}`))
	require.NoError(t, err)
	require.Equal(t, "resp-3", resp.Content)
	require.Equal(t, 3, runner.submitCalls())
	require.Zero(t, rt.inFlightCalls())
}

// TestInvokeFailedCallNotReplayed: a transient first-attempt failure is not
// cached; the retry reaches Submit again and surfaces the fresh attempt.
func TestInvokeFailedCallNotReplayed(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(errors.New("engine down"))
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{Content: "done"})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"),
		newTestCall("call-1", `{"a":1}`))
	require.ErrorContains(t, err, "engine down")
	runner.setErr(nil)
	resp, err := rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"),
		newTestCall("call-1", `{"a":1}`))
	require.NoError(t, err)
	require.Equal(t, "done", resp.Content)
	require.GreaterOrEqual(t, runner.submitCalls(), 2, "the retry reaches Submit again")
}

// TestInvokeWaiterNotPoisonedByFirstCancel: the first caller's cancellation
// must not be inherited by a concurrent waiter whose own ctx is alive; the
// waiter re-runs the fresh path and succeeds.
func TestInvokeWaiterNotPoisonedByFirstCancel(t *testing.T) {
	t.Parallel()
	attach := &gateAttach{enter: make(chan struct{}, 4), release: make(chan struct{})}
	runner := newFakeRunner(nil)
	cfg := testRuntimeConfig()
	cfg.Attach = attach
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{Content: "done"})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	inv := testInvocation()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	first := make(chan error, 1)
	go func() {
		_, err := rt.Invoke(ctxA, inv, newTestLeaf("view"),
			newTestCall("call-1", `{"a":1}`))
		first <- err
	}()
	<-attach.enter // The first flight is blocked in the attach probe.
	second := make(chan error, 1)
	resps := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, err := rt.Invoke(context.Background(), inv, newTestLeaf("view"),
			newTestCall("call-1", `{"a":1}`))
		second <- err
		resps <- resp
	}()
	joined := rt.entryFor(inv.SessionID, "call-1").joined
	<-joined // The second call is registered as a singleflight waiter.
	cancelA()
	require.ErrorIs(t, <-first, context.Canceled)
	close(attach.release)
	require.NoError(t, <-second)
	require.Equal(t, "done", (<-resps).Content)
	require.Equal(t, 1, runner.submitCalls(), "the waiter executes the fresh path once")
	require.Zero(t, rt.inFlightCalls())
}
