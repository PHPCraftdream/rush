package installwire

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
)

type testLeaf struct {
	fantasy.AgentTool
	name string
}

func (t testLeaf) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: t.name} }

type testEscrow struct{ n int }

func TestIngressPinsCloneAndPropagatesErrors(t *testing.T) {
	newEscrow := func() *testEscrow { return &testEscrow{} }
	store := NewRegistryStore(newEscrow)
	registry := toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{testLeaf{name: "agent"}, testLeaf{name: "bash"}}).Leaves
	index := testIndex{RegistryStore: &store, register: func(id string, registry *LeafRegistry) error {
		return store.Register(id, registry, newEscrow, func(tool fantasy.AgentTool, escrow *testEscrow) fantasy.AgentTool {
			escrow.n++
			return tool
		})
	}}
	disk := 1
	type key struct{}
	invokeErr := errors.New("invoke")
	run := func(ctx context.Context, inv string, _ fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		if ctx.Value(key{}) != &disk || inv != "inv" || call.ID != "call" {
			t.Fatal("invoke lost install or call identity")
		}
		return fantasy.ToolResponse{}, invokeErr
	}
	withBindings := func(ctx context.Context, value *int) context.Context { return context.WithValue(ctx, key{}, value) }
	configured := false
	ingress, err := NewIngress(run, index, &disk, func(b *int) { configured = b == &disk }, withBindings)
	if err != nil || !configured {
		t.Fatalf("construction: %v; configured=%v", err, configured)
	}
	if _, err := NewIngress(run, index, nil, func(*int) { t.Fatal("invalid install configured") }, withBindings); err != ErrIngressDeps {
		t.Fatalf("constructor error = %v", err)
	}
	id, err := ingress.PinLeafRegistry(registry)
	if err != nil || id == "" || store.Count() != 1 {
		t.Fatalf("pin: id=%q err=%v count=%d", id, err, store.Count())
	}
	if got, err := ingress.Registry(id); err != nil || got == registry {
		t.Fatalf("pin must resolve its own copy: %v", err)
	}
	if err := index.Register(id, registry); err == nil {
		t.Fatal("duplicate pin replaced existing entry")
	}
	leaf, err := ingress.ResolveLeaf(id, "bash")
	if err != nil || leaf.Info().Name != "bash" {
		t.Fatalf("resolve: %v", err)
	}
	if got := store.CredentialEscrow(newEscrow).n; got != 1 {
		t.Fatalf("wrapped %d leaves; want only agent", got)
	}
	if _, err := ingress.Invoke(context.Background(), "inv", leaf, fantasy.ToolCall{ID: "call"}); err != invokeErr {
		t.Fatalf("invoke error = %v", err)
	}
	store.Delete(id)
	if _, err := ingress.Registry(id); err == nil {
		t.Fatal("deleted registry still resolves")
	}
}

type testIndex struct {
	*RegistryStore[testEscrow]
	register func(string, *LeafRegistry) error
}

func (i testIndex) Register(id string, r *LeafRegistry) error { return i.register(id, r) }

func TestBindingsFirstWinsAndCoordinatorIsolation(t *testing.T) {
	var bindings Bindings[string, int]
	first, second := 1, 2
	bindings.Store("first", &first)
	bindings.Store("second", &second)
	if !bindings.ProcessSlot().CompareAndSwap(nil, &first) || bindings.ProcessSlot().CompareAndSwap(nil, &second) {
		t.Fatal("process binding is not first-wins")
	}
	if got, ok := bindings.Load("second"); !ok || got != &second || ProcessBinding(bindings.ProcessSlot()) != &first {
		t.Fatal("coordinator binding leaked across installs")
	}
	bindings.byCoordinator.Delete("second")
	if _, ok := bindings.Load("second"); ok {
		t.Fatal("deleted coordinator binding still resolves")
	}
}

func TestWindowAndRefusalPorts(t *testing.T) {
	if InlineWindow != 3*time.Second || SetInlineWindowForTest(nil, false) {
		t.Fatal("inline window default or absent-seam behavior changed")
	}
	var on bool
	set := func(v bool) { on = v }
	if !SetInlineWindowForTest(set, true) || !on || !SetInlineWindowForTest(set, false) || on {
		t.Fatal("inline window setting not forwarded")
	}
	refusal := errors.New("pin failed")
	tool := NewRefusedTool(testLeaf{name: "bash"}, refusal)
	if _, err := tool.Run(context.Background(), fantasy.ToolCall{}); err != refusal {
		t.Fatalf("refusal error = %v", err)
	}
}
