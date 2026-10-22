package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// fakeBatchIngress records every ingress crossing so the inventory tests can
// assert ONE authority, ONE pin and ONE execution per call.
type fakeBatchIngress struct {
	mu       sync.Mutex
	pins     int
	invokes  int
	origin   string
	inv      BatchInvocation
	reg      *policyLeafRegistry
	leafRuns map[string]int
	regs     map[string]*BatchLeafRegistry
}

func (f *fakeBatchIngress) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	f.mu.Lock()
	f.invokes++
	f.origin = inv.Origin
	f.inv = inv
	if f.leafRuns == nil {
		f.leafRuns = make(map[string]int)
	}
	f.leafRuns[call.Name]++
	f.mu.Unlock()
	return leaf.Run(ctx, call)
}

func (f *fakeBatchIngress) leafRunCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leafRuns[name]
}

func (f *fakeBatchIngress) PinLeafRegistry(registry *BatchLeafRegistry) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins++
	f.reg = registry
	// Unique id per pin, like the production ingress: a build that pins more
	// than once (nested sub-agent toolsets) must not collide in the disk
	// capability manager's construction records. The first pin stays "reg-1".
	if f.regs == nil {
		f.regs = make(map[string]*BatchLeafRegistry)
	}
	id := fmt.Sprintf("reg-%d", f.pins)
	f.regs[id] = registry
	return id, nil
}

func (f *fakeBatchIngress) ResolveLeaf(registryID, name string) (fantasy.AgentTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reg, ok := f.regs[registryID]
	if !ok {
		return nil, errors.New("unknown registry")
	}
	return reg.resolve(name)
}

func (f *fakeBatchIngress) Registry(registryID string) (*BatchLeafRegistry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reg, ok := f.regs[registryID]
	if !ok {
		return nil, errors.New("unknown registry")
	}
	return reg, nil
}

// countingTool is a bare leaf that counts its own executions.
type countingTool struct {
	name string
	mu   sync.Mutex
	runs int
}

func (t *countingTool) Info() fantasy.ToolInfo                     { return fantasy.ToolInfo{Name: t.name} }
func (t *countingTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *countingTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (t *countingTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.mu.Lock()
	t.runs++
	t.mu.Unlock()
	return fantasy.NewTextResponse("ok"), nil
}

func (t *countingTool) runCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs
}

func batchTestTools(names ...string) []fantasy.AgentTool {
	list := make([]fantasy.AgentTool, 0, len(names))
	for _, name := range names {
		list = append(list, &countingTool{name: name})
	}
	return list
}

func batchCallCtx(sessionID string) context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, sessionID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	return ctx
}

// TestBatchWorkInventoryPerFamilyFlagOn is acceptance item (b): every work
// family is armed through the batch ingress and NO async/stall wrapper is
// installed; the D1 control set is not batch-wrapped. The per-call
// single-authority / single-root / zero-async-ledger / one-envelope proofs
// against the REAL ingress live in batch_install_inventory_test.go
// (TestBatchWorkInventoryRealFamilies) — a fake ingress here would prove
// nothing about the installed system.
func TestBatchWorkInventoryPerFamilyFlagOn(t *testing.T) {
	fake := &fakeBatchIngress{}
	// R12-H2 §1.2: the build takes the pinned registry's Construction owner
	// in the app-owned capability manager, so the binding must carry one.
	bind := &batchCoordinatorBinding{ingress: fake, diskBindings: NewDiskProviderRegistry()}
	work := []string{
		"bash", "run_command", "agent", "agentic_fetch",
		"mcp_files__read", "view", "edit", "write", "glob", "grep", "ls",
		"fs_read", "fs_write", "fetch", "download",
		// R11F round-2 completeness hunk: todos MUTATES session state, so it
		// is a WORK tool and must be armed through the ONE authority.
		"todos",
	}
	controls := []string{
		"ask_question", "await_tasks", "batch_run", "batch_add",
		"batch_status", "batch_output", "batch_stop", "wakein", "wakeon", "loop",
		"wake_list", "wake_cancel", "inspect_agent", "inject_agent", "stop_agent",
	}
	list := batchTestTools(append(append([]string{}, work...), controls...)...)

	built, err := newBatchWorkToolBuild(bind, list, newBuildResourceScope(), false)
	if err != nil {
		t.Fatalf("newBatchWorkToolBuild: %v", err)
	}

	// Exactly ONE pinned registry for the whole build.
	if fake.pins != 1 {
		t.Fatalf("registry pinned %d times, want 1", fake.pins)
	}

	seen := map[string]fantasy.AgentTool{}
	for _, tool := range built.tools {
		seen[tool.Info().Name] = tool
	}
	if len(seen) != len(list) {
		t.Fatalf("tool count changed: got %d want %d", len(seen), len(list))
	}
	for _, name := range work {
		armed, ok := seen[name].(*batchIngressTool)
		if !ok {
			t.Fatalf("%s: not armed through the batch ingress (%T)", name, seen[name])
		}
		if armed.registry != fake.reg {
			t.Fatalf("%s: armed against a foreign registry", name)
		}
		if _, ok := seen[name].(*batchIngressTool); !ok {
			t.Fatalf("%s: the batch ingress must be the ONLY arming wrapper (%T)", name, seen[name])
		}
	}
	for _, name := range controls {
		if _, ok := seen[name].(*batchIngressTool); ok {
			t.Fatalf("D1 control %s must NOT be batch-wrapped", name)
		}
	}

	// Delivery choice per family: bash keeps its auto-background window;
	// agent and agentic_fetch are DETACHED (a delegation returns the batch
	// started text in-turn; a sync wait deadlocks a parent whose child is
	// gated or asks a question); every other family is sync with escape
	// never. (Per-call authority, root and ledger counts:
	// batch_install_inventory_test.go.)
	for name, want := range map[string]struct{ escape, window, mode string }{
		"bash":            {batchEscapeAuto, inlineWindow.String(), batchDeliverySync},
		"run_command":     {batchEscapeNever, "", batchDeliverySync},
		"agent":           {batchEscapeNever, "", batchDeliveryDetached},
		"agentic_fetch":   {batchEscapeNever, "", batchDeliveryDetached},
		"mcp_files__read": {batchEscapeNever, "", batchDeliverySync},
		"view":            {batchEscapeNever, "", batchDeliverySync},
	} {
		armed := seen[name].(*batchIngressTool)
		if armed.inv.BackgroundEscape != want.escape {
			t.Fatalf("%s: escape %q want %q", name, armed.inv.BackgroundEscape, want.escape)
		}
		if armed.inv.Delivery.Mode != want.mode {
			t.Fatalf("%s: mode %q want %q", name, armed.inv.Delivery.Mode, want.mode)
		}
		if armed.inv.Delivery.Window != want.window {
			t.Fatalf("%s: window %q want %q", name, armed.inv.Delivery.Window, want.window)
		}
		if armed.inv.WaiterRank != batchGroupRanks[batchGroupIO] {
			t.Fatalf("%s: waiter rank %d", name, armed.inv.WaiterRank)
		}
	}
}

// TestUnboundCoordinatorArmsNoBatchTools is acceptance item (a), batch
// edition: with no binding the assembly arms NOTHING through the batch
// ingress; the policy layers are hooks/restricted only (the deleted async
// and stall-detach wrapper layers are gone with their authorities).
func TestUnboundCoordinatorArmsNoBatchTools(t *testing.T) {
	c := newBatchRuntimeCoordinator()
	list := batchTestTools("bash", "run_command", "view", "ask_question")

	built, err := c.finishToolAssembly(context.Background(), list, nil, false, nil)
	if err != nil {
		t.Fatalf("finishToolAssembly: %v", err)
	}
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range built.tools {
		byName[tool.Info().Name] = tool
	}
	if entry, ok := built.leaves.entries["bash"]; ok {
		if batchLayersContain(entry.layers, "async") || batchLayersContain(entry.layers, "stall") {
			t.Fatalf("bash leaf layers %v carry a deleted wrapper layer", entry.layers)
		}
	}
	for _, tool := range built.tools {
		if _, ok := tool.(*batchIngressTool); ok {
			t.Fatalf("unbound coordinator produced a batch-armed tool")
		}
	}
}

func batchLayersContain(layers []string, want string) bool {
	for _, layer := range layers {
		if layer == want {
			return true
		}
	}
	return false
}

// TestNoRawAgentFamilyFactory is acceptance item (d), R11F round-2 P1-7
// item 5: a NO-BYPASS proof, not a non-nil-deps assertion. By inspection of
// the production (non _test) sources of this package: the literal zero-deps
// family constructor `batchAgentDeps{}` appears in NO production file, and
// every production call site of newBatchExecutorFamilies derives its
// argument from BatchInstallDeps.AgentDeps / CoordinatorBatchAgentDeps (the
// flagged wiring) — the raw zero-deps family exists only for the M1
// fail-closed default inside newBatchExecutorFamilies itself and in tests.
// The behavioral counterpart (the flagged install resolves the agent leaf
// through the pinned registry) is TestBatchWorkInventoryRealFamilies and
// TestBatchAgentRouterDispatchesByRecordedTask.
func TestNoRawAgentFamilyFactory(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			// Narrow to CODE: comment lines are prose, and the constructor's
			// own declaration line names the parameter, not the wiring.
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func ") {
				continue
			}
			if strings.Contains(trimmed, "batchAgentDeps{}") {
				t.Fatalf("%s: production code constructs a zero-deps (raw) batch agent family: %s", name, trimmed)
			}
			if !strings.Contains(trimmed, "newBatchExecutorFamilies(") {
				continue
			}
			if !strings.Contains(trimmed, "AgentDeps") && !strings.Contains(trimmed, "CoordinatorBatchAgentDeps") {
				t.Fatalf("%s: newBatchExecutorFamilies call site does not derive its deps from the flagged wiring: %s", name, trimmed)
			}
		}
	}
}

// TestAgentDriverRunsPinnedLeaf: the R7c factory resolves the pinned
// "agent" leaf through the ingress and runs it exactly once; stop
// acknowledges and the incarnation terminates.
func TestAgentDriverRunsPinnedLeaf(t *testing.T) {
	fake := &fakeBatchIngress{leafRuns: map[string]int{}}
	list := batchTestTools("agent")
	built := newPolicyToolBuild(list)
	if _, err := fake.PinLeafRegistry(built.leaves); err != nil {
		t.Fatalf("pin: %v", err)
	}
	restore := processBatchBinding.Swap(&batchCoordinatorBinding{ingress: fake})
	defer processBatchBinding.Store(restore)

	deps := CoordinatorBatchAgentDeps()
	turn, err := deps.Runs.StartAgentTurn(batchCallCtx("sess-1"), batchAgentRunRequest{
		NodeID: "n1", Token: 1, SessionID: "child-1", ToolCallID: "call-a",
		// Round-2 P1-3: the driver fails closed on an empty Tool — dispatch
		// is by the RECORDED tool name.
		Tool: AgentToolName, Input: []byte(`{"prompt":"hi"}`),
		RegistryID: "reg-1", MessageID: "msg-1",
	}, batchAgentHost{})
	if err != nil {
		t.Fatalf("StartAgentTurn: %v", err)
	}
	select {
	case <-turn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("agent turn never finished")
	}
	if err := turn.Err(); err != nil {
		t.Fatalf("turn err: %v", err)
	}
	if turn.Result().Response.Content != "ok" {
		t.Fatalf("unexpected result %q", turn.Result().Response.Content)
	}
	// The driver resolves the pinned leaf through the ingress and runs it
	// directly (the ONE authority already crossed at ingress time), so the
	// leaf itself must have executed exactly once.
	leaf := list[0].(*countingTool)
	if leaf.runCount() != 1 {
		t.Fatalf("pinned leaf ran %d times", leaf.runCount())
	}
	if err := turn.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-turn.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("driver goroutine never exited")
	}
}

// TestAgentDriverFailsClosedWithoutBinding: no process binding => typed
// refusal, never a raw dispatch.
func TestAgentDriverFailsClosedWithoutBinding(t *testing.T) {
	restore := processBatchBinding.Swap(nil)
	defer processBatchBinding.Store(restore)
	deps := CoordinatorBatchAgentDeps()
	if _, err := deps.Runs.StartAgentTurn(context.Background(), batchAgentRunRequest{RegistryID: "reg-1"}, batchAgentHost{}); !errors.Is(err, errBatchCoordinatorNotBound) {
		t.Fatalf("err %v, want errBatchCoordinatorNotBound", err)
	}
}

// TestMCPKindRoutingAndBridgeFallback: the flag-on binding routes MCP names
// through the MCP executor kind and serves the CLI bridge from the latest
// pinned registry via the R7d pinnedInvoke.
func TestMCPKindRoutingAndBridgeFallback(t *testing.T) {
	fake := &fakeBatchIngress{}
	list := batchTestTools("view", "bash", "mcp_files__read")
	built := newPolicyToolBuild(list)
	bind := &batchCoordinatorBinding{ingress: fake}
	if _, err := bind.pin(built.leaves); err != nil {
		t.Fatalf("pin: %v", err)
	}
	restore := processBatchBinding.Swap(bind)
	defer processBatchBinding.Store(restore)
	setBatchLeafKindOverride(func(tool string) (string, bool) {
		if strings.HasPrefix(tool, "mcp_") {
			return batchKindMCP, true
		}
		return "", false
	})
	defer setBatchLeafKindOverride(nil)

	if got := batchLeafKind("mcp_files__read"); got != batchKindMCP {
		t.Fatalf("mcp kind %q", got)
	}
	if got := batchLeafKind("view"); got != batchKindGeneric {
		t.Fatalf("generic kind %q", got)
	}

	// P1-4: the fallback resolves a bridge ONLY from a STAMPED invocation —
	// stamp the context with the pinned registry the binding just pinned.
	stamped := withBatchBridgeInvocation(batchCallCtx("sess-1"), batchBridgeInvocation{RegistryID: "reg-1", MessageID: "msg-1"})
	bridge := coordinatorBridgeFallback(stamped)
	if bridge == nil {
		t.Fatal("bridge fallback nil with a live binding")
	}
	advertised := bridge.Tools()
	if len(advertised) == 0 {
		t.Fatal("bridge advertised nothing from the pinned registry")
	}
	invokes := fake.invokes
	resp, err := bridge.Call(batchCallCtx("sess-1"), fantasy.ToolCall{ID: "gen-1", Name: "view", Input: `{"path":"x"}`})
	if err != nil {
		t.Fatalf("bridge call: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("bridge response %q", resp.Content)
	}
	if fake.invokes != invokes+1 {
		t.Fatalf("bridge crossed the authority %d times for one call", fake.invokes-invokes)
	}
	if fake.origin != "cli_bridge" {
		t.Fatalf("bridge origin %q", fake.origin)
	}
}

// TestPrivateFetchArmsThroughIngress: agentic_fetch's private tools resolve
// their pinned leaves through the same ingress when the App-owned ingress is
// stamped into the trusted build context (R12-H2 §1.3: ONLY an explicit
// binding — the process-global fallback is gone), with the agentic_fetch
// origin.
func TestPrivateFetchArmsThroughIngress(t *testing.T) {
	// A REAL ingress over a REAL install (real runner, executor and
	// delivery): the arming path takes the pinned registry's Construction
	// owner in the App-owned capability manager, so a fake ingress no longer
	// suffices, and the run must cross the real Invoke end to end.
	env := newBatchInventoryEnv(t, "view")
	// Record the runner-minted root so this test can drive the REAL
	// execution-owner release trigger (Runner.Release) after the run.
	runner := &diskRefusalRunner{batchRunnerHandle: env.install.Runtime.cfg.Runner}
	env.install.Runtime.cfg.Runner = runner
	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	if err != nil {
		t.Fatalf("ingress: %v", err)
	}

	list := batchTestTools("view", "grep")
	ctx := withPrivateFetchBuildIngress(context.Background(), ingress)
	built := buildPrivateFetchTools(ctx, list, nil, newBuildResourceScope())
	for i, tool := range built.tools {
		if _, ok := tool.(*privateFetchIngressTool); !ok {
			t.Fatalf("private tool %d not ingress-armed (%T)", i, tool)
		}
	}
	resp, err := built.tools[0].Run(batchCallCtx(env.sessionID), fantasy.ToolCall{ID: "c1", Name: "view", Input: `{}`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("response %q, want the real ingress-delivered leaf envelope", resp.Content)
	}
	// The pin took its Construction owner in the App manager, and the run's
	// bound root holds Root+Execution owners naming the same registry, so
	// the owner set is non-empty while everything is live.
	caps := env.install.DiskBindings
	if caps.ownerCount() == 0 {
		t.Fatal("the pin must hold a Construction owner while the build scope is open")
	}
	// REAL execution-owner release trigger (R12-H2 §1.2): the run's root
	// settled (Batch.Done released the Root owner) and the adopted Done
	// watcher (batchRootLeaseStore.adopt) called the Runner's Release
	// AUTOMATICALLY — the fixture must NOT Release the adopted root itself;
	// the physical-Dispose wrapper already dropped the Execution owner.
	if n := len(runner.recordedRoots()); n != 1 {
		t.Fatalf("expected one bound root for the private-fetch run, got %d", n)
	}
	// The oracle is the CONSTRUCTION this build owns: after the graceful
	// residual drain removes the Construction owner and the execution
	// dispose drops the last owner naming the registry, the construction
	// record is deleted and the pinned registry identity is unresolvable.
	caps.releaseResidualConstructions()
	require.Eventually(t, func() bool {
		_, err := env.registries.Resolve(built.registryID)
		return err != nil
	}, 5*time.Second, time.Millisecond,
		"the construction record must die once no owner names its registry")
}

// TestPrivateFetchCustomProviderPassesExecutorAdmission: a CUSTOM disk
// provider carried through the trusted call state (BindCallDiskProvider) on
// the PRIVATE-FETCH route passes the executor's hasToolset admission
// (batch_exec.go:139-146) end to end, through the PRODUCTION wiring: the
// coordinator binding's ingress is stamped the way agenticFetchTool stamps
// it (agentic_fetch_tool.go), and armPrivateFetchIngress stamps the Policy
// template with the REAL batchToolsetFingerprint of the pinned build — NOT
// the registry id. Regression oracle: reverting the fingerprint to the
// registry id (or dropping the production ingress stamp in agenticFetchTool)
// makes the binding-carrying payload FAIL hasToolset, so the executor
// refuses admission and the provider-marked payload falls out of the batch
// authority with ErrDiskProviderUnavailable.
func TestPrivateFetchCustomProviderPassesExecutorAdmission(t *testing.T) {
	env := newBatchInventoryEnv(t, "view")
	// Count the bound submission so the run is proven to have crossed the
	// REAL Runner through SubmitBound (a root-owning bound run).
	runner := &diskRefusalRunner{batchRunnerHandle: env.install.Runtime.cfg.Runner}
	env.install.Runtime.cfg.Runner = runner
	inventoryBindProcess(t, env)
	// PRODUCTION wiring (agentic_fetch_tool.go): resolve the coordinator
	// binding's OWN ingress and stamp it into the private-fetch build
	// context under privateFetchIngressKey{}.
	ingress := processCoordinatorBatchBinding().ingress
	if ingress == nil {
		t.Fatal("no coordinator binding ingress after inventoryBindProcess")
	}
	// A non-comparable custom provider through the trusted call state is a
	// SUPPORTED provider: the binding handle is the opaque minted ID.
	opts, release := bindTestCallDiskProvider(t, env,
		bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"private-fetch"}})
	defer release()
	ctx := WithCallOptions(withPrivateFetchBuildIngress(batchCallCtx(env.sessionID), ingress), opts)

	built := buildPrivateFetchTools(ctx, batchTestTools("view"), nil, EnsureCallBuildScope(opts))
	for i, tool := range built.tools {
		if _, ok := tool.(*privateFetchIngressTool); !ok {
			t.Fatalf("private tool %d not ingress-armed (%T)", i, tool)
		}
	}
	resp, runErr := built.tools[0].Run(ctx, fantasy.ToolCall{ID: "c1", Name: "view", Input: `{}`})
	require.NoError(t, runErr, "the custom-provider private payload must pass executor admission")
	require.False(t, resp.IsError, "leaf response: %s", resp.Content)
	require.Positive(t, runner.submitsBound, "the binding-carrying payload submitted through SubmitBound")
	require.Positive(t, runner.submits, "the run reached the real Runner")
}

// withPrivateFetchBuildIngress stamps the App-owned ingress into the trusted
// build context — the ONLY resolution the private fetch build accepts
// (R12-H2 §1.3: the process-global fallback is eliminated).
func withPrivateFetchBuildIngress(ctx context.Context, ingress BatchWorkIngress) context.Context {
	return context.WithValue(ctx, privateFetchIngressKey{}, ingress)
}
