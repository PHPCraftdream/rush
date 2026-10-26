package tools

import (
	"context"
	"errors"
	"os/exec"
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// ArgvLeaf opts a guarded run_command call into retained process ownership.
// OnStart runs after Start succeeds and before Wait begins. It must return
// promptly; it may publish the handle, but must not wait for Done itself.
type (
	ArgvLeaf           struct{ OnStart func(*ArgvProcess) }
	argvLeafContextKey struct{}
)

// WithArgvLeaf selects the lifetime seam without bypassing tool policy.
func WithArgvLeaf(ctx context.Context, leaf *ArgvLeaf) context.Context {
	return context.WithValue(ctx, argvLeafContextKey{}, leaf)
}

func argvLeafFromContext(ctx context.Context) *ArgvLeaf {
	leaf, _ := ctx.Value(argvLeafContextKey{}).(*ArgvLeaf)
	return leaf
}

var (
	ErrArgvNotDone  = errors.New("argv process has not completed")
	ErrArgvDisposed = errors.New("argv process disposed")
	ErrArgvBusy     = errors.New("argv process has an active physical stop")
)

// ArgvOutcome is sourced exclusively from the real, drained exec.Cmd.Wait.
// ExitCode is -1 when the platform supplies no numeric exit status.
type ArgvOutcome struct {
	WaitErr  error
	ExitCode int
}

// ArgvProcess retains completion and output independently of tool return.
type ArgvProcess struct {
	mu       sync.Mutex
	buf      *runCommandOutputBuffer
	done     chan struct{}
	stopSlot chan struct{}
	kill     func() error
	outcome  ArgvOutcome
	disposed bool
}

func newArgvProcess(cmd *exec.Cmd, buf *runCommandOutputBuffer) *ArgvProcess {
	p := &ArgvProcess{buf: buf, done: make(chan struct{}), stopSlot: make(chan struct{}, 1), kill: cmd.Cancel}
	p.stopSlot <- struct{}{}
	buf.mu.Lock()
	buf.changed = make(chan struct{})
	buf.mu.Unlock()
	return p
}

func runArgvLeaf(ctx context.Context, leaf *ArgvLeaf, cmd *exec.Cmd, buf *runCommandOutputBuffer) (err error) {
	p := newArgvProcess(cmd, buf)
	// A callback panic cannot strand the successfully started process.
	defer func() {
		if v := recover(); v != nil {
			_ = p.Stop(context.Background())
			p.finish(cmd.Wait(), cmd)
			panic(v)
		}
	}()
	if leaf.OnStart != nil {
		leaf.OnStart(p)
	}
	go func() { p.finish(cmd.Wait(), cmd) }()
	select {
	case <-p.done:
	case <-ctx.Done():
		go func() { _ = p.Stop(context.Background()) }()
		<-p.done
	}
	outcome, _ := p.Outcome()
	return outcome.WaitErr
}

func (p *ArgvProcess) finish(err error, cmd *exec.Cmd) {
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	p.mu.Lock()
	p.outcome = ArgvOutcome{WaitErr: err, ExitCode: code}
	close(p.done)
	p.mu.Unlock()
}

// Done closes only after real Wait, including stdout/stderr drain.
func (p *ArgvProcess) Done() <-chan struct{} { return p.done }

// Outcome refuses to infer an exit result before completion.
func (p *ArgvProcess) Outcome() (ArgvOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return p.outcome, nil
	default:
		return ArgvOutcome{}, ErrArgvNotDone
	}
}

// Stop serializes physical tree kills. A caller deadline bounds its wait,
// never releases the slot of a kill still physically executing.
func (p *ArgvProcess) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stopSlot:
	}
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		p.stopSlot <- struct{}{}
		return ErrArgvDisposed
	}
	select {
	case <-p.done:
		p.mu.Unlock()
		p.stopSlot <- struct{}{}
		return nil
	default:
	}
	p.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		defer func() { p.stopSlot <- struct{}{} }()
		result <- p.kill()
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReadOutput blocks at the live head and returns caller-owned retained bytes.
// Invalid signed cursors normalize to zero, as on the existing live buffer.
func (p *ArgvProcess) ReadOutput(ctx context.Context, cursor int64) (jobbatch.OutputChunk, error) {
	for {
		p.mu.Lock()
		if p.disposed {
			p.mu.Unlock()
			return jobbatch.OutputChunk{}, ErrArgvDisposed
		}
		b := p.buf
		b.mu.Lock()
		if cursor < 0 || cursor > b.total {
			cursor = 0
		}
		oldest := b.availableFromLocked()
		start := max(cursor, oldest)
		chunk := jobbatch.OutputChunk{Data: append([]byte(nil), b.data[start-oldest:]...), Next: uint64(b.total), Oldest: uint64(oldest), Truncated: cursor < oldest}
		changed := b.changed
		select {
		case <-p.done:
			chunk.EOF = true
		default:
		}
		b.mu.Unlock()
		p.mu.Unlock()
		if len(chunk.Data) > 0 || chunk.EOF {
			return chunk, nil
		}
		select {
		case <-ctx.Done():
			return jobbatch.OutputChunk{}, ctx.Err()
		case <-changed:
		case <-p.done:
		}
	}
}

// Dispose gives up output once, only after completion and physical Stop return.
func (p *ArgvProcess) Dispose(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.disposed {
		p.mu.Unlock()
		return ErrArgvDisposed
	}
	select {
	case <-p.done:
	default:
		p.mu.Unlock()
		return ErrArgvNotDone
	}
	p.mu.Unlock()
	// Stop takes mu after acquiring the slot; never wait with mu held.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stopSlot:
	}
	defer func() { p.stopSlot <- struct{}{} }()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.disposed {
		return ErrArgvDisposed
	}
	p.disposed = true
	p.buf.mu.Lock()
	p.buf.data = nil
	p.buf.mu.Unlock()
	return nil
}
