package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type finalizationQueuePumpProbe struct {
	service *service
	calls   int
}

func (p *finalizationQueuePumpProbe) Run(ctx context.Context, data SessionAgentCallData) (*any, error) {
	snapshot, err := p.service.RestoreAgentFinalizationQueue(ctx, RunQueueRowIDFrom(ctx), data)
	if err != nil {
		return nil, err
	}
	if snapshot.Requirement() == nil {
		return nil, ErrAgentFinalizationChanged
	}
	p.calls++
	return nil, nil
}

func TestFinalizationQueuePumpTransportsClaimTransactionProof(t *testing.T) {
	fixture := newFinalizationQueueFixture(t)
	fixture.enqueue(t, "queued")
	entry := fixture.lease(t, "pump")
	probe := &finalizationQueuePumpProbe{service: fixture.service}
	pump := NewRunQueuePump(RunQueuePumpConfig{Sessions: fixture.service, Coordinator: probe, PumpInstanceID: "pump"})
	defer pump.cancel()
	release, admission, admitted := pump.admitSession(fixture.data.SessionID)
	require.True(t, admitted)
	defer release(admissionOutcome{})
	_, _, err := pump.executeEntrySync(fixture.ctx, entry, admission)
	require.NoError(t, err)
	require.Equal(t, 1, probe.calls)
	require.Zero(t, commitHookCount(t, fixture.env.conn, `SELECT count(*) FROM session_run_queue WHERE id='queued'`))
	require.EqualValues(t, 1, commitHookCount(t, fixture.env.conn, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='queued'`))
}
