package childagent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/stretchr/testify/require"
)

func TestScopeSharedPublicationSlots(t *testing.T) {
	s := NewScope()
	h, err := s.Hold(2)
	require.NoError(t, err)
	require.Same(t, h, s.held)
	require.Equal(t, 1, cap(h.answer))
	require.NoError(t, s.WaitChange(t.Context()))
	s.TurnEnded()
	s.Descendants(false, false)
	require.False(t, s.ClosedNow())
	s.Descendants(false, true)
	require.False(t, s.ClosedNow(), "a held question keeps the scope open")

	msg := batchinst.AgentInjectMessage{Depth: 2, Text: json.RawMessage(`"ok"`)}
	require.ErrorIs(t, s.Deliver(msg), batchinst.ErrBatchAgentNotHeld)
	require.Same(t, h, s.held, "a zero-sequence answer must retain the original hold")
	depth, ok := s.HeldDepth()
	require.True(t, ok)
	require.Equal(t, 2, depth)
	require.NoError(t, s.PublishQuestion(func(sequence uint64) error {
		unlocked := s.mu.TryLock()
		if unlocked {
			s.mu.Unlock()
		}
		require.False(t, unlocked, "publication must hold the same scope mutex as answer acceptance")
		require.EqualValues(t, 1, sequence)
		require.Equal(t, sequence, s.held.sequence)
		return nil
	}))
	msg.Sequence = 1
	commitErr := errors.New("commit failed")
	require.ErrorIs(t, s.DeliverWithCommit(msg, func() error { return commitErr }), commitErr)
	require.Same(t, h, s.held)
	require.False(t, h.resolved)
	require.Empty(t, h.answer)
	require.NoError(t, s.Deliver(msg))
	require.Equal(t, msg.Text, <-h.answer)
	require.True(t, h.resolved)
	require.Nil(t, s.held)
	require.True(t, s.ClosedNow())
	require.True(t, s.MarkClosed())
	require.True(t, s.closed)
	require.False(t, s.MarkClosed())
	s.BeginStop()
	require.True(t, s.stopped)
	require.True(t, s.Stop())
}
