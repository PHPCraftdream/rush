package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A batch node starts its child without an enclosing turn, so runSubAgent is the
// first entry point to touch params.Agent. It must wait for the asynchronous
// prompt/tool build first: a child run before that goes out with an empty
// system prompt (promptLen=0) and the model never sees its instructions.
func TestRunSubAgentWaitsForInFlightAgentBuilds(t *testing.T) {
	coord := &coordinator{}
	release := make(chan struct{})
	buildErr := errors.New("agent build failed")
	started := make(chan struct{})
	coord.readyWg.Go(func() error {
		close(started)
		<-release
		return buildErr
	})
	<-started

	type outcome struct {
		err      error
		panicked any
	}
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		defer func() {
			out.panicked = recover()
			done <- out
		}()
		_, out.err = coord.runSubAgent(t.Context(), subAgentParams{})
	}()

	select {
	case out := <-done:
		t.Fatalf("runSubAgent proceeded while the agent build was pending (err=%v panic=%v)", out.err, out.panicked)
	case <-time.After(200 * time.Millisecond): // a correct run blocks; waiting longer only delays a pass
	}
	close(release)
	out := <-done
	if out.panicked != nil {
		t.Fatal(fmt.Sprint("runSubAgent went on after the build drained: ", out.panicked))
	}
	if !errors.Is(out.err, buildErr) {
		t.Fatalf("runSubAgent error = %v, want the build error", out.err)
	}
}
