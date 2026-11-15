package shell

import (
	"context"
	"errors"
	"os/exec"
	"sync"
)

type retainedStopKey struct{}

// retainedStops serializes startup registration, cancellation intent and physical
// controls. Successful per-process acknowledgments are cached; failures retry.
// A control deadline bounds slot acquisition, never a dispatched physical call.
type retainedStops struct {
	mu        sync.Mutex
	entries   map[*retainedProcess]struct{}
	cancelled bool
	cancel    context.CancelFunc
	output    *retainedOutput
}

type retainedProcess struct {
	mu       sync.Mutex
	kill     func() error
	finished bool
	acked    bool
}

func (p *retainedProcess) stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.acked {
		return nil
	}
	err := p.kill()
	if err == nil {
		p.acked = true
	}
	return err
}

func (s *retainedStops) stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = true
	if s.cancel != nil {
		s.cancel()
	}
	var errs []error
	for p := range s.entries {
		errs = append(errs, p.stop())
	}
	return errors.Join(errs...)
}

// discardOutput drops the retained merged output buffer. The nil checks live
// here so BackgroundExecution never reaches the output field directly, which
// keeps this file movable to its own package.
func (s *retainedStops) discardOutput() {
	if s == nil || s.output == nil {
		return
	}
	s.output.mu.Lock()
	s.output.data = nil
	s.output.mu.Unlock()
}

// runRetainedProcess fences Start with registration. Stop cannot acknowledge an
// empty snapshot while a started process is not yet registered. Cancellation
// before startup rejects the spawn. The context callback is always joined.
func runRetainedProcess(ctx context.Context, cmd *exec.Cmd, s *retainedStops, kill func() error, register func(int)) error {
	s.mu.Lock()
	if s.cancelled || ctx.Err() != nil {
		s.mu.Unlock()
		return context.Canceled
	}
	err := cmd.Start()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	p := &retainedProcess{kill: kill}
	s.entries[p] = struct{}{}
	if register != nil {
		register(cmd.Process.Pid)
	}
	s.mu.Unlock()
	return waitRetainedRegistered(ctx, cmd, s, p)
}

func waitRetainedRegistered(ctx context.Context, cmd *exec.Cmd, s *retainedStops, p *retainedProcess) error {
	callbackDone := make(chan struct{})
	stopCallback := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = s.stop()
	})
	err := cmd.Wait()
	p.mu.Lock()
	p.finished = true
	p.mu.Unlock()
	if !stopCallback() {
		<-callbackDone
	}
	s.mu.Lock()
	delete(s.entries, p)
	s.mu.Unlock()
	return err
}

// retainedLaunchRoute adapts the retained launch path to shexec's
// ProcessGroupExecHandler hook: the retained stop types stay in this
// package, so the moved exec handlers call back through this route
// instead of referencing them directly.
func retainedLaunchRoute(ctx context.Context, cmd *exec.Cmd, kill func() error, register func(int)) (bool, error) {
	stops, ok := ctx.Value(retainedStopKey{}).(*retainedStops)
	if !ok {
		return false, nil
	}
	return true, runRetainedProcess(ctx, cmd, stops, kill, register)
}
