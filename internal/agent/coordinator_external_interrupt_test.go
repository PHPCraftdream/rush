package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type busyExternalInterruptAgent struct{ SessionAgent }

func (busyExternalInterruptAgent) IsSessionBusy(string) bool { return true }

func TestExternalInterruptRequiresDriverAndIdleRoot(t *testing.T) {
	c, current, env, sid := newInterruptDrainFixture(t, func(id string) SessionAgentCall { return SessionAgentCall{SessionID: id} })
	trace := batchAgentTraceFor(t)
	before, err := env.sessions.PeekInterruptInject(t.Context(), sid)
	require.NoError(t, err)
	require.NotNil(t, before)
	id, err := c.PollExternalInterrupt(t.Context(), sid, nil)
	require.NoError(t, err)
	require.Empty(t, id, "an unclaimed session must not consume an interrupt")
	c.driverHints.claimExternalDriver(sid)
	c.currentAgent = busyExternalInterruptAgent{current}
	id, err = c.PollExternalInterrupt(t.Context(), sid, nil)
	require.NoError(t, err)
	require.Empty(t, id, "an active turn retains its own interrupt ticker")
	still, err := env.sessions.PeekInterruptInject(t.Context(), sid)
	require.NoError(t, err)
	require.Equal(t, before.ID, still.ID)
	trace.addf("unclaimed and busy paths retained inject=%s", before.ID)
}

func TestInterruptObserverNamesCommittedMessage(t *testing.T) {
	c, _, env, sid := newInterruptDrainFixture(t, func(id string) SessionAgentCall { return SessionAgentCall{SessionID: id} })
	trace := batchAgentTraceFor(t)
	source, err := env.sessions.PeekInterruptInject(t.Context(), sid)
	require.NoError(t, err)
	require.NotNil(t, source)
	var observed string
	fired, err := c.handleInterruptTickObserved(t.Context(), sid, func(id string) { observed = id; trace.addf("delivered message=%s", id) }, nil)
	require.NoError(t, err)
	require.True(t, fired)
	require.Equal(t, source.MessageID, observed)
	rows, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var call session.SessionAgentCallData
	require.NoError(t, json.Unmarshal([]byte(rows[0].CallData), &call))
	require.Equal(t, observed, call.ExistingMessageID)
	fired, err = c.handleInterruptTickObserved(context.Background(), sid, func(string) { t.Error("source was consumed twice") }, nil)
	require.NoError(t, err)
	require.False(t, fired)
}

func TestExternalInterruptPreflightOnlyForPendingRow(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_interrupt", true: "pending_interrupt"}[pending], func(t *testing.T) {
			c, _, env, sid := newInterruptDrainFixture(t, func(id string) SessionAgentCall { return SessionAgentCall{SessionID: id} })
			trace := batchAgentTraceFor(t)
			c.driverHints.claimExternalDriver(sid)
			row, err := env.sessions.PeekInterruptInject(t.Context(), sid)
			require.NoError(t, err)
			require.NotNil(t, row)
			if !pending {
				require.NoError(t, env.sessions.DeleteInterruptInject(t.Context(), row.ID))
			}
			stopped := errors.New("operator stopped the run")
			calls := 0
			id, err := c.PollExternalInterrupt(t.Context(), sid, func() error { calls++; return stopped })
			trace.addf("pending=%v preflight_calls=%d message=%s error=%v", pending, calls, id, err)
			require.Empty(t, id)
			if pending {
				require.ErrorIs(t, err, stopped)
				require.Equal(t, 1, calls)
				restored, readErr := env.sessions.PeekInterruptInject(t.Context(), sid)
				require.NoError(t, readErr)
				require.NotNil(t, restored)
				require.Equal(t, row.ID, restored.ID, "stop must precede atomic consumption")
			} else {
				require.NoError(t, err)
				require.Zero(t, calls, "an ordinary scope check must not reinterpret an already-handled Drain cancellation")
			}
			queued, readErr := env.sessions.ListPendingRunQueueEntries(t.Context())
			require.NoError(t, readErr)
			require.Empty(t, queued)
		})
	}
}
