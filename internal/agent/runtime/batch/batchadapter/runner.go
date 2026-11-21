package batchadapter

import (
	"context"
	"errors"
	"sync"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Runner adapts the physical engine, with late binding for deferred installs.
type Runner struct {
	Engine             func() *jobbatch.Runner
	Stamp              func(context.Context, jobbatch.Spec) (jobbatch.Spec, error)
	Register           func(context.Context, string, []jobbatch.Spec) (func(), error)
	ReleaseCredentials func(string)
	State              *RunnerState
}

// RunnerState tracks in-process batches and CLI ownership under one lock.
// Its zero value supports lookups and recording a started batch.
type RunnerState struct {
	mu      sync.Mutex
	started map[jobbatch.ID]*jobbatch.Batch
	owners  map[jobbatch.ID]string
}

// NewRunnerState initializes both maps for an installed runner.
func NewRunnerState() RunnerState {
	return RunnerState{
		started: make(map[jobbatch.ID]*jobbatch.Batch),
		owners:  make(map[jobbatch.ID]string),
	}
}

// CLIState exposes the installed state to the root CLI wiring, which records
// the batch and its owner together under this lock. Call before publication.
func (s *RunnerState) CLIState() (*sync.Mutex, map[jobbatch.ID]*jobbatch.Batch, map[jobbatch.ID]string) {
	return &s.mu, s.started, s.owners
}

func (s *RunnerState) recordStarted(root jobbatch.ID, batch *jobbatch.Batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started == nil {
		s.started = make(map[jobbatch.ID]*jobbatch.Batch)
	}
	s.started[root] = batch
}

// BatchFor returns the in-process batch, or nil for a root not started here.
func (s *RunnerState) BatchFor(root jobbatch.ID) *jobbatch.Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started[root]
}

// rememberOwner records an owner. It requires an initialized owners map.
func (s *RunnerState) rememberOwner(root jobbatch.ID, owner string) {
	s.mu.Lock()
	s.owners[root] = owner
	s.mu.Unlock()
}

// OwnerFor returns the CLI owner, or an empty string if none is recorded.
func (s *RunnerState) OwnerFor(root jobbatch.ID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owners[root]
}

// Submit starts the caller-supplied root. The returned ID is always the
// requested root (ingress contract: runner root must equal requested root).
func (a *Runner) Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	root, _, err := a.SubmitBound(ctx, req)
	return root, err
}

func (a *Runner) SubmitBound(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error) {
	batch, err := a.submitBatch(ctx, req)
	if err != nil {
		return "", nil, err
	}
	return req.Root, batch, nil
}

func (a *Runner) submitBatch(ctx context.Context, req jobbatch.SubmitRequest) (*jobbatch.Batch, error) {
	// R11F P1-6: a deferred install binds the real Runner at Start. An
	// Invoke that races ahead of Start has no Runner and fails closed.
	engine := a.Engine()
	if engine == nil {
		return nil, errors.New("batch runner adapter: runner not constructed yet (Start did not run)")
	}
	var err error
	req.Spec, err = a.Stamp(ctx, req.Spec)
	if err != nil {
		return nil, err
	}
	if req.Spec.Task != nil {
		if req.Spec.Batch != nil {
			return nil, jobbatch.ErrInvalidSpec
		}
		req.Spec = jobbatch.OneItem(*req.Spec.Task)
	} else if req.Spec.Batch == nil {
		return nil, jobbatch.ErrInvalidSpec
	}
	release, err := a.Register(ctx, string(req.Root), []jobbatch.Spec{req.Spec})
	if err != nil {
		return nil, err
	}
	batch, err := engine.Submit(ctx, jobbatch.SubmitRequest{Root: req.Root, Spec: req.Spec, Ref: req.Ref})
	if err != nil {
		release()
		return nil, err
	}
	a.State.recordStarted(req.Root, batch)
	return batch, nil
}

// Stop requests cancellation of one root.
func (a *Runner) Stop(ctx context.Context, id jobbatch.ID) error {
	return a.Engine().Stop(ctx, id)
}

// Release disposes a settled root's retained execution (the batchReleaseHandle
// seam): the ingress/model control adopt this as the automatic post-Done
// disposal trigger, so the adapter must surface the real Runner's Release.
func (a *Runner) Release(ctx context.Context, id jobbatch.ID) error {
	err := a.Engine().Release(ctx, id)
	if err == nil {
		a.ReleaseCredentials(string(id))
	}
	return err
}

func (a *Runner) Add(ctx context.Context, req jobbatch.AddRequest) ([]jobbatch.ID, error) {
	engine := a.Engine()
	view, err := engine.View(ctx, req.Parent)
	if err != nil {
		return nil, err
	}
	root := string(view.Root)
	stamped := make([]jobbatch.Spec, len(req.Items))
	for i, spec := range req.Items {
		var err error
		stamped[i], err = a.Stamp(ctx, spec)
		if err != nil {
			return nil, err
		}
	}
	req.Items = stamped
	release, err := a.Register(ctx, root, req.Items)
	if err != nil {
		return nil, err
	}
	ids, err := engine.Add(ctx, req)
	if err != nil {
		release()
	}
	return ids, err
}

func (a *Runner) Snapshot(ctx context.Context, id jobbatch.ID) (jobbatch.BatchView, error) {
	return a.Engine().Snapshot(ctx, id)
}

func (a *Runner) View(ctx context.Context, id jobbatch.ID) (jobbatch.NodeView, error) {
	return a.Engine().View(ctx, id)
}

func (a *Runner) Output(ctx context.Context, id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, error) {
	return a.Engine().Output(ctx, id, cursor)
}
