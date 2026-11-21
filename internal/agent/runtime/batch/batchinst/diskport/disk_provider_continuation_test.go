package diskport

import (
	"context"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

func TestPrepareContinuationUsesSourceAndRetainsExactOwner(t *testing.T) {
	source := &session.SessionExecutionRequirement{BindingID: "source"}
	var instance string
	var releases int
	p, err := PrepareContinuation(context.Background(), "session", source,
		func(context.Context, string) (*session.SessionExecutionRequirement, error) {
			t.Fatal("source must bypass stored fence")
			return nil, nil
		},
		func(binding string) (tools.DiskProvider, error) {
			if binding != "source" {
				t.Fatalf("looked up %q", binding)
			}
			return nil, nil
		},
		BindingOwners{
			Acquire: func(key, binding string) error {
				if key == "" || binding != "source" {
					t.Fatalf("acquired (%q, %q)", key, binding)
				}
				instance = key
				return nil
			},
			Release: func(key, binding string) {
				if key != instance || binding != "source" {
					t.Fatalf("released (%q, %q), want (%q, source)", key, binding, instance)
				}
				releases++
			},
		}, errors.New("unavailable"))
	if err != nil || p == nil {
		t.Fatalf("prepare = %v, %v", p, err)
	}
	call := agentcall.SessionAgentCall{}
	p.ApplyTo(&call)
	if opts := call.CallOptions; opts == nil || opts.BuildScope == nil || opts.Requirement == source || opts.Requirement == nil || opts.DiskBindingID != "source" || opts.DiskBinding.BindingID() != "source" {
		t.Fatalf("incorrect continuation options: %+v", opts)
	}
	p.Release()
	p.Release()
	if releases != 1 {
		t.Fatalf("released %d times, want 1", releases)
	}
}

func TestPrepareContinuationUnreadableFenceFailsClosed(t *testing.T) {
	unavailable := errors.New("unavailable")
	p, err := PrepareContinuation(context.Background(), "session", nil,
		func(context.Context, string) (*session.SessionExecutionRequirement, error) {
			return nil, errors.New("read failed")
		}, nil, BindingOwners{}, unavailable)
	if p != nil || !errors.Is(err, unavailable) {
		t.Fatalf("prepare = %v, %v", p, err)
	}
}
