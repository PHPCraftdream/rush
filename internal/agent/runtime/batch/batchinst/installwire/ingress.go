// Package installwire owns batch installation policy and state; coordinator
// and runtime wiring enter through typed operation ports.
package installwire

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/google/uuid"
)

type LeafRegistry = toolwrap.PolicyLeafRegistry

// RegistryIndex is the pin/lookup subset used by ingress.
type RegistryIndex interface {
	Register(string, *LeafRegistry) error
	Resolve(string) (*LeafRegistry, error)
}

type Invoke[I any] func(context.Context, I, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error)

// Ingress keeps the invoke operation and disk context local to this install.
type Ingress[I any, D any] struct {
	Run              Invoke[I]
	Registries       RegistryIndex
	DiskBindings     D
	WithDiskBindings func(context.Context, D) context.Context
}

var ErrIngressDeps = errors.New("batch work ingress: runtime and pinned registry store are required")

// NewIngress validates the required dependencies before publishing the ingress.
// configure, when supplied, updates the runtime's own disk bindings first.
func NewIngress[I any, D any](run Invoke[I], registries RegistryIndex, bindings *D, configure func(*D), withBindings func(context.Context, *D) context.Context) (Ingress[I, *D], error) {
	if run == nil || registries == nil || bindings == nil {
		return Ingress[I, *D]{}, ErrIngressDeps
	}
	if configure != nil {
		configure(bindings)
	}
	return Ingress[I, *D]{Run: run, Registries: registries, DiskBindings: bindings, WithDiskBindings: withBindings}, nil
}

func (i Ingress[I, D]) Invoke(ctx context.Context, inv I, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return i.Run(i.WithDiskBindings(ctx, i.DiskBindings), inv, leaf, call)
}

func (i Ingress[I, D]) PinLeafRegistry(registry *LeafRegistry) (string, error) {
	if registry == nil {
		return "", errors.New("batch work ingress: nil leaf registry")
	}
	id := uuid.NewString()
	// Index registration is not ownership. The caller takes Construction
	// ownership and registers its release with the call/build scope.
	if err := i.Registries.Register(id, registry); err != nil {
		return "", err
	}
	return id, nil
}

func (i Ingress[I, D]) ResolveLeaf(id, name string) (fantasy.AgentTool, error) {
	registry, err := i.Registry(id)
	if err != nil {
		return nil, err
	}
	return registry.Resolve(name)
}

func (i Ingress[I, D]) Registry(id string) (*LeafRegistry, error) {
	return i.Registries.Resolve(id)
}

// RegistryForIngress exposes bindings only for the install's native ingress.
func RegistryForIngress[D any, I interface{ InstallDiskBindings() D }](ingress any) (zero D) {
	if i, ok := ingress.(I); ok {
		return i.InstallDiskBindings()
	}
	return zero
}

// Bindings owns the per-coordinator index and the process's first-wins slot.
// The coordinator supplies its key and binding types, not its state or methods.
type Bindings[K comparable, B any] struct {
	byCoordinator sync.Map
	process       atomic.Pointer[B]
}

func (b *Bindings[K, B]) Store(k K, v *B) { b.byCoordinator.Store(k, v) }
func (b *Bindings[K, B]) Load(k K) (*B, bool) {
	v, ok := b.byCoordinator.Load(k)
	if !ok {
		return nil, false
	}
	return v.(*B), true
}
func (b *Bindings[K, B]) ProcessSlot() *atomic.Pointer[B] { return &b.process }

func BindCoordinatorRuntime[C, I any](c C, ingress I, bind func(C, I) error) error {
	return bind(c, ingress)
}

func CoordinatorIngress[B, I any](binding *B, ingress func(*B) I) (zero I) {
	if binding != nil {
		return ingress(binding)
	}
	return zero
}

func ProcessBinding[B any](slot *atomic.Pointer[B]) *B { return slot.Load() }

func BindRuntimeIngress[I any](set func(I), ingress I) {
	if set != nil {
		set(ingress)
	}
}

func PinnedInvoke[I, W any](ingress W, tpl I, invoke func(context.Context, W, I, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error)) func(context.Context, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return func(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return invoke(ctx, ingress, tpl, leaf, call)
	}
}
