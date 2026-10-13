package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
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
	return "reg-1", nil
}

func (f *fakeBatchIngress) ResolveLeaf(registryID, name string) (fantasy.AgentTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reg == nil || registryID != "reg-1" {
		return nil, errors.New("unknown registry")
	}
	return f.reg.resolve(name)
}

func (f *fakeBatchIngress) Registry(registryID string) (*BatchLeafRegistry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reg == nil || registryID != "reg-1" {
		return nil, errors.New("unknown registry")
	}
	return f.reg, nil
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
// family is armed through exactly ONE authority; the D1 control set is not
// batch-wrapped; no async/stall wrapper is installed; hooks run exactly once.
func TestBatchWorkInventoryPerFamilyFlagOn(t *testing.T) {
	fake := &fakeBatchIngress{}
	bind := &batchCoordinatorBinding{ingress: fake}
	work := []string{
		"bash", "run_command", "agent", "agentic_fetch",
		"mcp_files__read", "view", "edit", "write", "glob", "grep", "ls",
		"fs_read", "fs_write", "fetch", "download",
	}
	controls := []string{
		"ask_question", "await_tasks", "batch_run", "batch_add",
		"batch_status", "batch_output", "batch_stop", "wakein", "wakeon", "loop",
		"wake_list", "wake_cancel", "inspect_agent", "inject_agent", "stop_agent",
	}
	list := batchTestTools(append(append([]string{}, work...), controls...)...)

	built, err := newBatchWorkToolBuild(bind, list)
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
		if _, ok := seen[name].(*asyncTool); ok {
			t.Fatalf("%s: old async wrapper installed on the flag-on path", name)
		}
		if _, ok := seen[name].(*stallDetacherTool); ok {
			t.Fatalf("%s: old stall wrapper installed on the flag-on path", name)
		}
	}
	for _, name := range controls {
		if _, ok := seen[name].(*batchIngressTool); ok {
			t.Fatalf("D1 control %s must NOT be batch-wrapped", name)
		}
	}

	// Leaf registry layers: no execution wrappers survive pinning.
	for name, entry := range fake.reg.entries {
		for _, layer := range entry.layers {
			if layer == "async" || layer == "stall" {
				t.Fatalf("leaf %s retains execution layer %q", name, layer)
			}
		}
	}

	// Per family: exactly one authority crossing and exactly one leaf
	// execution per call.
	for _, name := range work {
		leaf := seen[name].(*batchIngressTool)
		before := fake.invokes
		resp, err := leaf.Run(batchCallCtx("sess-1"), fantasy.ToolCall{ID: "call-" + name, Name: name, Input: `{"a":1}`})
		if err != nil {
			t.Fatalf("%s: Run: %v", name, err)
		}
		if resp.Content != "ok" {
			t.Fatalf("%s: unexpected response %q", name, resp.Content)
		}
		if fake.invokes != before+1 {
			t.Fatalf("%s: %d authority crossings for one call", name, fake.invokes-before)
		}
		if fake.origin != "coordinator" {
			t.Fatalf("%s: origin %q", name, fake.origin)
		}
		if fake.inv.SessionID != "sess-1" || fake.inv.MessageID != "msg-1" {
			t.Fatalf("%s: invocation identity not bound from the run context: %+v", name, fake.inv)
		}
		if fake.inv.RegistryID != "reg-1" || fake.inv.Policy.RegistryID != "reg-1" {
			t.Fatalf("%s: pinned registry identity not carried: %+v", name, fake.inv.Policy)
		}
		if fake.inv.Policy.RestrictedToolset == "" {
			t.Fatalf("%s: restricted toolset fingerprint missing", name)
		}
		if fake.leafRunCount(name) != 1 {
			t.Fatalf("%s: leaf executed %d times, want exactly 1", name, fake.leafRunCount(name))
		}
	}

	// Delivery choice per family: bash keeps its auto-background window;
	// every other family is sync with escape never.
	for name, want := range map[string]struct{ escape, window string }{
		"bash":            {batchEscapeAuto, inlineWindow.String()},
		"run_command":     {batchEscapeNever, ""},
		"agent":           {batchEscapeNever, ""},
		"agentic_fetch":   {batchEscapeNever, ""},
		"mcp_files__read": {batchEscapeNever, ""},
		"view":            {batchEscapeNever, ""},
	} {
		armed := seen[name].(*batchIngressTool)
		if armed.inv.BackgroundEscape != want.escape {
			t.Fatalf("%s: escape %q want %q", name, armed.inv.BackgroundEscape, want.escape)
		}
		if armed.inv.Delivery.Mode != batchDeliverySync {
			t.Fatalf("%s: mode %q want sync", name, armed.inv.Delivery.Mode)
		}
		if armed.inv.Delivery.Window != want.window {
			t.Fatalf("%s: window %q want %q", name, armed.inv.Delivery.Window, want.window)
		}
		if armed.inv.WaiterRank != batchGroupRanks[batchGroupIO] {
			t.Fatalf("%s: waiter rank %d", name, armed.inv.WaiterRank)
		}
	}
}

// TestUnboundCoordinatorKeepsLegacyAssembly is acceptance item (a): with no
// binding the assembly takes the historical branch — async wrapper inside
// hooks/restrictions, stall-detach outermost, registry leaves stripped of
// every execution wrapper.
func TestUnboundCoordinatorKeepsLegacyAssembly(t *testing.T) {
	c := &coordinator{asyncJobs: newWorkLedger(nil)}
	c.asyncJobs.inlineWindow = inlineWindow
	list := batchTestTools("bash", "run_command", "view", "ask_question")

	built, err := c.finishToolAssembly(list, nil, false)
	if err != nil {
		t.Fatalf("finishToolAssembly: %v", err)
	}
	byName := map[string]fantasy.AgentTool{}
	for _, tool := range built.tools {
		byName[tool.Info().Name] = tool
	}
	for _, name := range []string{"bash", "run_command", "view", "ask_question"} {
		if _, ok := byName[name].(*stallDetacherTool); !ok {
			t.Fatalf("%s: outermost wrapper is not the stall detacher (%T)", name, byName[name])
		}
	}
	if entry, ok := built.leaves.entries["bash"]; ok {
		if !batchLayersContain(entry.layers, "async") {
			t.Fatalf("bash leaf layers %v missing async", entry.layers)
		}
	}
	for name, entry := range built.leaves.entries {
		if !batchLayersContain(entry.layers, "stall") {
			t.Fatalf("%s: leaf layers %v missing stall", name, entry.layers)
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

// TestNoRawAgentFamilyFactory is acceptance item (d): the flagged install
// cannot reach the zero-dep (raw) agent family, and the zero-dep shape is
// the fail-closed default.
func TestNoRawAgentFamilyFactory(t *testing.T) {
	wired := newBatchExecutorFamilies(CoordinatorBatchAgentDeps())
	family, ok := wired[batchKindAgent].(batchAgentFamily)
	if !ok {
		t.Fatalf("agent family type %T", wired[batchKindAgent])
	}
	if family.deps.Runs == nil || family.deps.Descendants == nil {
		t.Fatalf("flagged agent family seams are zero: %+v", family.deps)
	}
	zero := newBatchExecutorFamilies(batchAgentDeps{})
	zeroFamily, ok := zero[batchKindAgent].(batchAgentFamily)
	if !ok {
		t.Fatalf("zero agent family type %T", zero[batchKindAgent])
	}
	if zeroFamily.deps.Runs != nil || zeroFamily.deps.Descendants != nil {
		t.Fatalf("zero agent family deps must stay zero: %+v", zeroFamily.deps)
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
		Input: []byte(`{"prompt":"hi"}`), RegistryID: "reg-1", MessageID: "msg-1",
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
	batchLeafKindOverride = func(tool string) (string, bool) {
		if strings.HasPrefix(tool, "mcp_") {
			return batchKindMCP, true
		}
		return "", false
	}
	defer func() { batchLeafKindOverride = nil }()

	if got := batchLeafKind("mcp_files__read"); got != batchKindMCP {
		t.Fatalf("mcp kind %q", got)
	}
	if got := batchLeafKind("view"); got != batchKindGeneric {
		t.Fatalf("generic kind %q", got)
	}

	bridge := coordinatorBridgeFallback(context.Background())
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
// their pinned leaves through the same ingress when the batch runtime is
// bound (process binding), with the agentic_fetch origin.
func TestPrivateFetchArmsThroughIngress(t *testing.T) {
	fake := &fakeBatchIngress{}
	restore := processBatchBinding.Swap(&batchCoordinatorBinding{ingress: fake})
	defer processBatchBinding.Store(restore)

	list := batchTestTools("view", "grep")
	built := buildPrivateFetchTools(context.Background(), list, nil)
	for i, tool := range built.tools {
		if _, ok := tool.(*privateFetchIngressTool); !ok {
			t.Fatalf("private tool %d not ingress-armed (%T)", i, tool)
		}
	}
	resp, err := built.tools[0].Run(batchCallCtx("sess-1"), fantasy.ToolCall{ID: "c1", Name: "view", Input: `{}`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Content != "ok" || fake.invokes != 1 {
		t.Fatalf("response %q crossings %d", resp.Content, fake.invokes)
	}
	if fake.origin != "agentic_fetch" {
		t.Fatalf("origin %q", fake.origin)
	}
}
