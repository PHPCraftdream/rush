package agent

import (
	"context"
	"encoding/json"
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
	id, err := c.PollExternalInterrupt(t.Context(), sid)
	require.NoError(t, err)
	require.Empty(t, id, "an unclaimed session must not consume an interrupt")
	c.driverHints.claimExternalDriver(sid)
	c.currentAgent = busyExternalInterruptAgent{current}
	id, err = c.PollExternalInterrupt(t.Context(), sid)
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
	fired, err := c.handleInterruptTickObserved(t.Context(), sid, func(id string) { observed = id; trace.addf("delivered message=%s", id) })
	require.NoError(t, err)
	require.True(t, fired)
	require.Equal(t, source.MessageID, observed)
	rows, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var call session.SessionAgentCallData
	require.NoError(t, json.Unmarshal([]byte(rows[0].CallData), &call))
	require.Equal(t, observed, call.ExistingMessageID)
	fired, err = c.handleInterruptTickObserved(context.Background(), sid, func(string) { t.Error("source was consumed twice") })
	require.NoError(t, err)
	require.False(t, fired)
}
