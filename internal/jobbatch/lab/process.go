package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/platform"
)

type processExecutor struct{ ceiling int }

type processStartError struct {
	code string
	err  error
}

func (e *processStartError) Error() string { return e.code }
func (e *processStartError) Code() string  { return e.code }
func (e *processStartError) Unwrap() error { return e.err }

// NewProcessExecutor constructs a direct-argv, isolated process executor.
func NewProcessExecutor(options ProcessOptions) (jobbatch.Executor, error) {
	if options.MaxRetainedBytes <= 0 {
		return nil, jobbatch.ErrInvalidSpec
	}
	if !processSupported() {
		return nil, jobbatch.ErrUnsupported
	}
	return &processExecutor{ceiling: options.MaxRetainedBytes}, nil
}

type processExecution struct {
	cmd         *exec.Cmd
	tree        *processTree
	mu          sync.Mutex
	data        []byte
	head        uint64
	cap         int
	changed     chan struct{}
	done        chan struct{}
	drained     chan struct{}
	reaped      bool
	leaderDone  chan struct{}
	stopped     bool
	eof         bool
	disposeOnce sync.Once
	disposeErr  error
}

func (p *processExecutor) Start(ctx context.Context, launch jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var payload Payload
	decoder := json.NewDecoder(bytes.NewReader(launch.Task.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, jobbatch.ErrInvalidSpec
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, jobbatch.ErrInvalidSpec
	}
	if payload.Kind != "process" {
		return nil, jobbatch.ErrInvalidSpec
	}
	if payload.Program == nil || *payload.Program == "" {
		return nil, &processStartError{code: "missing_program", err: exec.ErrNotFound}
	}
	capacity := p.ceiling
	if payload.MaxOutputBytes != nil {
		if *payload.MaxOutputBytes <= 0 {
			return nil, jobbatch.ErrInvalidSpec
		}
		if *payload.MaxOutputBytes < capacity {
			capacity = *payload.MaxOutputBytes
		}
	}
	var args []string
	if payload.Args != nil {
		args = *payload.Args
	}
	// Start context bounds setup, not the accepted process lifetime.
	cmd := platform.Command(context.Background(), *payload.Program, args...)
	if payload.Cwd != nil {
		cmd.Dir = *payload.Cwd
	}
	tree, err := processPrepare(cmd)
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = tree.dispose()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	cleanup := func() { _ = reader.Close(); _ = writer.Close(); _ = tree.dispose() }
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, &processStartError{code: "missing_program", err: err}
		}
		return nil, err
	}
	_ = writer.Close()
	if err := tree.attach(cmd); err != nil {
		_ = tree.kill()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		tree.awaitGone()
		cleanup()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = tree.kill()
		_ = cmd.Wait()
		tree.awaitGone()
		cleanup()
		return nil, err
	}
	execution := &processExecution{cmd: cmd, tree: tree, cap: capacity, changed: make(chan struct{}), done: make(chan struct{}), drained: make(chan struct{}), leaderDone: make(chan struct{})}
	go execution.readPipe(reader)
	go execution.wait(report)
	return execution, nil
}

func (p *processExecution) notify() { close(p.changed); p.changed = make(chan struct{}) }

func (p *processExecution) readPipe(reader *os.File) {
	defer close(p.drained)
	defer reader.Close()
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			p.mu.Lock()
			p.head += uint64(n)
			if n >= p.cap {
				p.data = append(p.data[:0], buffer[n-p.cap:n]...)
			} else {
				excess := len(p.data) + n - p.cap
				if excess > 0 {
					copy(p.data, p.data[excess:])
					p.data = p.data[:len(p.data)-excess]
				}
				p.data = append(p.data, buffer[:n]...)
			}
			p.notify()
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *processExecution) wait(report func(jobbatch.Result)) {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.reaped = true
	close(p.leaderDone)
	stopped := p.stopped
	p.mu.Unlock()
	p.tree.awaitGone()
	<-p.drained
	result := jobbatch.Result{State: jobbatch.StateCompleted}
	if stopped {
		result.State = jobbatch.StateCancelled
	} else if err != nil {
		result.State = jobbatch.StateFailed
		result.Code = fmt.Sprintf("exit_%d", p.cmd.ProcessState.ExitCode())
	}
	p.mu.Lock()
	p.eof = true
	p.notify()
	close(p.done)
	p.mu.Unlock()
	// A host callback cannot delay Start or termination publication.
	report(result)
}

func (p *processExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	for {
		p.mu.Lock()
		oldest := p.head - uint64(len(p.data))
		start := cursor
		if start < oldest {
			start = oldest
		}
		if start > p.head {
			start = p.head
		}
		if start < p.head || p.eof || cursor < oldest {
			chunk := jobbatch.OutputChunk{Data: append([]byte(nil), p.data[start-oldest:]...), Next: p.head, Oldest: oldest, EOF: p.eof, Truncated: cursor < oldest}
			p.mu.Unlock()
			return chunk, nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return jobbatch.OutputChunk{}, ctx.Err()
		}
	}
}

func (p *processExecution) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.eof {
		p.mu.Unlock()
		return nil
	}
	if !p.reaped && !processLeaderExited(p.cmd) {
		p.stopped = true
	}
	p.mu.Unlock()
	if err := p.tree.term(); err != nil {
		return err
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		_ = p.tree.kill()
		return ctx.Err()
	case <-timer.C:
	}
	if err := p.tree.kill(); err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *processExecution) Dispose(ctx context.Context) error {
	select {
	case <-p.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.disposeOnce.Do(func() { p.disposeErr = p.tree.dispose(); p.mu.Lock(); p.data = nil; p.mu.Unlock() })
	return p.disposeErr
}
