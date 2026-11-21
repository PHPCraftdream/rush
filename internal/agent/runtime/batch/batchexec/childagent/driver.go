package childagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Host controls the child lifecycle of one incarnation.
type Host struct {
	Ask              func(context.Context, int, string) (json.RawMessage, error)
	BindChildSession func(context.Context, string) error
	NoChildSelected  func()
}

// TrackChild makes unbound turn exit observable without overriding a successful bind.
func TrackChild(host Host, bound bool) Host {
	var childBound atomic.Bool
	childBound.Store(bound)
	var once sync.Once
	tracked := host
	tracked.NoChildSelected = func() {
		if !childBound.Load() && host.NoChildSelected != nil {
			once.Do(host.NoChildSelected)
		}
	}
	if host.BindChildSession != nil {
		tracked.BindChildSession = func(ctx context.Context, id string) error {
			if err := host.BindChildSession(ctx, id); err != nil {
				return err
			}
			childBound.Store(true)
			return nil
		}
	}
	return tracked
}

type TurnResult struct {
	Response fantasy.ToolResponse
	Declared jobbatch.Outcome
}

type Turn interface {
	Done() <-chan struct{}
	// Exit includes cleanup and can lag Done.
	Exit() <-chan struct{}
	Result() TurnResult
	Err() error
	Stop(context.Context) error
}

// RunTool starts a prepared leaf. Done publishes the result; Exit also fences
// build-scope and credential release.
func RunTool(runCtx context.Context, cancel context.CancelFunc, leaf fantasy.AgentTool, call fantasy.ToolCall, markNoChild, closeScope, releaseCredentials func()) Turn {
	turn := &toolTurn{cancel: cancel, done: make(chan struct{}), exit: make(chan struct{})}
	go func() {
		defer close(turn.exit)
		defer releaseCredentials()
		defer closeScope()
		defer close(turn.done)
		// Отказы leaf до runSubAgent тоже должны закрывать scope без ребёнка.
		defer markNoChild()
		resp, runErr := func() (resp fantasy.ToolResponse, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("batch agent leaf panic: %v", r)
				}
			}()
			return leaf.Run(runCtx, call)
		}()
		turn.mu.Lock()
		turn.resp, turn.err = resp, runErr
		turn.mu.Unlock()
		cancel()
	}()
	return turn
}

// toolTurn is one launched incarnation.
type toolTurn struct {
	cancel      context.CancelFunc
	done, exit  chan struct{}
	mu          sync.Mutex
	resp        fantasy.ToolResponse
	err         error
	stopPending bool
}

func (t *toolTurn) Done() <-chan struct{} { return t.done }
func (t *toolTurn) Exit() <-chan struct{} { return t.exit }

func (t *toolTurn) Result() TurnResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TurnResult{Response: t.resp}
}

func (t *toolTurn) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Stop acknowledges the stop request by cancelling the run context; the
// scope watcher still settles only on confirmed closure.
func (t *toolTurn) Stop(context.Context) error {
	t.mu.Lock()
	first := !t.stopPending
	t.stopPending = true
	t.mu.Unlock()
	if first {
		t.cancel()
	}
	return nil
}
