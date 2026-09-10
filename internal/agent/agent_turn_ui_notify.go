package agent

import (
	"context"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

// turnUINotifier decouples the token arrival rate from the UI render rate
// during a turn: it holds at most one pending snapshot (latest-value
// semantics) and a ticker goroutine drains it at ~20fps, publishing through
// a.messages.Notify. Split out of agent_turn.go's runTurn when the
// 1000-line limit landed. Owns latestMsgCh outright; only READS the two
// things it shares with the rest of runTurn -- sessionLock and the current
// assistant message -- never owns them.
type turnUINotifier struct {
	a      *sessionAgent
	genCtx context.Context

	sessionLock *sync.Mutex
	assistant   **message.Message

	latestMsgCh chan message.Message
}

func newTurnUINotifier(a *sessionAgent, genCtx context.Context, sessionLock *sync.Mutex, assistant **message.Message) *turnUINotifier {
	return &turnUINotifier{
		a:           a,
		genCtx:      genCtx,
		sessionLock: sessionLock,
		assistant:   assistant,
		latestMsgCh: make(chan message.Message, 1),
	}
}

// start launches the draining ticker goroutine. Unlike turnCheckpointWriter
// and peakHoursWatcher this has no configuration to be absent under -- a
// turn always wants its streaming visible -- so there is no nil-check fast
// path, and the goroutine exits only when genCtx is done.
func (n *turnUINotifier) start() {
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-n.genCtx.Done():
				// Flush any final pending snapshot before exiting.
				select {
				case msg := <-n.latestMsgCh:
					n.a.messages.Notify(msg)
				default:
				}
				return
			case <-ticker.C:
				select {
				case msg := <-n.latestMsgCh:
					n.a.messages.Notify(msg)
				default:
				}
			}
		}
	}()
}

// notify enqueues the latest assistant snapshot for the ticker goroutine.
// It never blocks: if the channel already has a pending snapshot, the old
// one is discarded and replaced with the newest state.
func (n *turnUINotifier) notify() error {
	n.sessionLock.Lock()
	if *n.assistant == nil {
		n.sessionLock.Unlock()
		return nil
	}
	msg := (*n.assistant).Clone()
	n.sessionLock.Unlock()
	select {
	case n.latestMsgCh <- msg:
	default:
		// Channel full — discard stale snapshot and enqueue fresh one.
		select {
		case <-n.latestMsgCh:
		default:
		}
		select {
		case n.latestMsgCh <- msg:
		default:
		}
	}
	return nil
}

// drainPending discards any snapshot still queued for the ticker goroutine.
// runTurn calls this right before persisting the assistant message's final
// state, so the ticker cannot publish a stale snapshot racing behind the
// authoritative write.
func (n *turnUINotifier) drainPending() {
	select {
	case <-n.latestMsgCh:
	default:
	}
}
