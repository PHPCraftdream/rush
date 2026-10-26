package jobbatch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitSubprocess(t *testing.T) {
	if os.Getenv("JOBBATCH_COMMIT_HELPER") == "1" {
		ctx := runtimeTestContext(t)
		stuck := make(chan struct{})
		entered := make(chan struct{}, 1)
		r, e, c, _ := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
			if tr.Event.Type == EventAdd {
				entered <- struct{}{}
				<-stuck
			}
			return nil
		})
		runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
		stopped := make(chan struct{}, 1)
		a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
		go func() { _, _ = r.Add(ctx, AddRequest{Parent: "r", Items: []Spec{runtimeTestTask()}}) }()
		runtimeTestReceive(t, ctx, entered)
		go func() { _ = r.Stop(ctx, "r") }()
		runtimeTestReceive(t, ctx, stopped)
		for range 4 {
			runtimeTestReceive(t, ctx, c.armed)
		}
		c.advance(time.Second)
		runtimeTestReceive(t, ctx, r.Faulted())
		a.report(Result{State: StateCancelled})
		os.Exit(0)
	}
	t.Parallel()
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, exe, "-test.run=^TestRuntimeCommitSubprocess$")
	cmd.Env = append(os.Environ(), "JOBBATCH_COMMIT_HELPER=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
