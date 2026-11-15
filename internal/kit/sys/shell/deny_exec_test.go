package shell

import (
	"context"
	"errors"
	"testing"
)

func TestRegisteredPolicyMiddlewareSentinel(t *testing.T) {
	old := denyExecPolicy
	denyExecMu.Lock()
	denyExecPolicy = nil
	denyExecMu.Unlock()
	RegisterCommandPolicy(func(args []string) error {
		if len(args) > 0 && args[0] == "deny-sentinel" {
			return context.DeadlineExceeded
		}
		return nil
	})
	t.Cleanup(func() { denyExecMu.Lock(); denyExecPolicy = old; denyExecMu.Unlock() })
	called := false
	handler := ciOnlyPolicyHandler()(func(context.Context, []string) error { called = true; return nil })
	if err := handler(context.Background(), []string{"deny-sentinel"}); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("denied command reached fake handler or changed sentinel: %v", err)
	}
	called = false
	if err := handler(context.Background(), []string{"allow-sentinel"}); err != nil || !called {
		t.Fatalf("allowed command did not reach fake handler: %v", err)
	}
	called = false
	if err := handler(context.Background(), []string{"jq", "--help"}); err != nil || !called {
		t.Fatalf("middleware allow path failed: %v", err)
	}
}

func TestDisabledRegisteredPolicyPassesThrough(t *testing.T) {
	old := denyExecPolicy
	denyExecMu.Lock()
	denyExecPolicy = nil
	denyExecMu.Unlock()
	t.Cleanup(func() { denyExecMu.Lock(); denyExecPolicy = old; denyExecMu.Unlock() })
	called := false
	err := ciOnlyPolicyHandler()(func(context.Context, []string) error { called = true; return context.DeadlineExceeded })(context.Background(), []string{"go", "test"})
	if !errors.Is(err, context.DeadlineExceeded) || !called {
		t.Fatalf("disabled policy changed fake handler error: %v", err)
	}
}

func TestStandardHandlersPolicyBlocksExpandedCommand(t *testing.T) {
	old := denyExecPolicy
	denyExecMu.Lock()
	denyExecPolicy = func(args []string) error {
		if len(args) > 1 && args[0] == "go" && args[1] == "test" {
			return context.DeadlineExceeded
		}
		return nil
	}
	denyExecMu.Unlock()
	t.Cleanup(func() { denyExecMu.Lock(); denyExecPolicy = old; denyExecMu.Unlock() })
	spawned := false
	testInnermostHandler = func(context.Context, []string) error { spawned = true; return nil }
	t.Cleanup(func() { testInnermostHandler = nil })
	err := Run(t.Context(), RunOptions{Command: "cmd=go; $cmd test ./...", Cwd: t.TempDir()})
	if !errors.Is(err, context.DeadlineExceeded) || spawned {
		t.Fatalf("expanded denied command reached fake spawner: err=%v spawned=%v", err, spawned)
	}
}
