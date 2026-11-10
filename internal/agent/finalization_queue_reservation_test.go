package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type reservationBarrierAgent struct {
	SessionAgent
	before  func()
	after   func()
	capture func(context.Context, context.CancelFunc)
}

func (a *reservationBarrierAgent) ReserveExclusive(ctx context.Context, owner string) (context.Context, uint64, context.CancelFunc, bool) {
	if a.before != nil {
		a.before()
	}
	hold, epoch, cancel, ok := a.SessionAgent.ReserveExclusive(ctx, owner)
	if ok && a.capture != nil {
		a.capture(hold, cancel)
	}
	if ok && a.after != nil {
		a.after()
	}
	return hold, epoch, cancel, ok
}

type reservationQueueProbe struct {
	coordinator *coordinator
	builds      atomic.Int64
	runs        atomic.Int64
	releases    atomic.Int64
	afterBuild  func(context.Context)
	execute     func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	panicRun    bool
	observed    any
	lastErr     error
}

func (p *reservationQueueProbe) rebuild(ctx context.Context, rowID string, data session.SessionAgentCallData) (SessionAgentCall, error) {
	p.builds.Add(1)
	snapshot, err := p.coordinator.restoreFinalizationQueuedCall(ctx, rowID, data)
	if err != nil {
		return SessionAgentCall{}, err
	}
	options := &CallOptions{}
	EnsureCallBuildScope(options)
	options.buildScope.register(func() { p.releases.Add(1) })
	if p.afterBuild != nil {
		p.afterBuild(ctx)
	}
	return SessionAgentCall{
		SessionID: data.SessionID, LogicalCallID: data.LogicalCallID, Prompt: data.Prompt,
		AutoResumed: data.AutoResumed, MaxCost: data.MaxCost, MaxTokens: data.MaxTokens,
		Tools: []fantasy.AgentTool{}, finalizationAuthorities: snapshot, CallOptions: options,
	}, nil
}

func (p *reservationQueueProbe) run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	p.runs.Add(1)
	if p.panicRun {
		panic("reservation phase panic")
	}
	if _, err := p.coordinator.finalizationQueueDriverForRun(ctx, call); err != nil {
		return nil, &ErrCallAlreadyAttempted{Err: err}
	}
	if p.execute != nil {
		return p.execute(ctx, call)
	}
	return nil, &ErrCallAlreadyAttempted{Err: errors.New("unexpected provider dispatch")}
}

func (p *reservationQueueProbe) Run(ctx context.Context, data session.SessionAgentCallData) (result *any, err error) {
	defer func() {
		if value := recover(); value != nil {
			if !p.panicRun || value != "reservation phase panic" {
				panic(value)
			}
			p.observed = value
			err = &ErrCallAlreadyAttempted{Err: errors.New("reservation phase panic")}
		}
		p.lastErr = err
	}()
	_, err = p.coordinator.runFinalizationQueuedCall(ctx, session.RunQueueRowIDFrom(ctx), data, p.rebuild, p.run)
	return nil, err
}

func reservationQueueCall(t *testing.T, fixture finalizationDriverFixture, owner session.AgentFinalizationIdentity, snapshot session.AgentFinalizationContext) session.SessionAgentCallData {
	t.Helper()
	ctx, err := snapshot.Restore(context.Background())
	require.NoError(t, err)
	data := session.SessionAgentCallData{SessionID: owner.SessionID, LogicalCallID: owner.LogicalCallID, Prompt: "compose settled results", AutoResumed: true, FinalizationRequirement: snapshot.Requirement()}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, fixture.api.(session.Service).EnqueueRunQueueEntry(ctx, "reserved", owner.SessionID, raw))
	return data
}

func reservationConsumed(t *testing.T, fixture finalizationDriverFixture) int64 {
	t.Helper()
	var consumed int64
	require.NoError(t, fixture.conn.QueryRowContext(fixture.ctx, `SELECT consumed FROM session_agent_finalization_continuations WHERE row_id='reserved'`).Scan(&consumed))
	return consumed
}

func TestFinalizationReservationLeaseChangeAndStopBeforeConsume(t *testing.T) {
	for _, change := range []string{"expiry", "replacement", "cancel", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			fixture := newFinalizationDriverFixture(t)
			sessions := fixture.api.(session.Service)
			parent, err := fixture.api.BindAgentFinalization(fixture.ctx, fixture.parent)
			require.NoError(t, err)
			trusted, err := session.WithAgentFinalizationContext(context.Background(), parent)
			require.NoError(t, err)
			data := reservationQueueCall(t, fixture, fixture.parent, session.CaptureAgentFinalizationContext(trusted))
			real := NewSessionAgent(SessionAgentOptions{})
			driver := &reservationBarrierAgent{SessionAgent: real}
			coordinator := &coordinator{sessions: sessions, currentAgent: driver}
			probe := &reservationQueueProbe{coordinator: coordinator}
			var barrierErr error
			switch change {
			case "expiry":
				driver.before = func() {
					_, barrierErr = fixture.conn.ExecContext(fixture.ctx, `UPDATE session_run_queue SET lease_expires_at=? WHERE id='reserved'`, time.Now().Unix()-1)
				}
			case "replacement":
				driver.before = func() {
					barrierErr = sessions.NackRunQueueEntryNoAttemptPenalty(fixture.ctx, "reserved", "old-pump", "not started")
					if barrierErr == nil {
						_, barrierErr = sessions.LeaseRunQueueEntry(fixture.ctx, data.SessionID, "new-pump", time.Minute)
					}
				}
			case "cancel":
				driver.after = func() { real.Cancel(data.SessionID) }
			case "shutdown":
				driver.after = func() { real.CancelAll() }
			}
			pump := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: sessions, Coordinator: probe, PumpInstanceID: "old-pump"})
			defer pump.Stop()
			ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
			defer cancel()
			result, err := pump.DrainSessionNow(ctx, data.SessionID)
			switch change {
			case "cancel", "shutdown":
				require.Error(t, err)
				require.ErrorIs(t, probe.lastErr, context.Canceled)
			default:
				require.NoError(t, err)
				require.Equal(t, session.DrainNoWork, result)
				require.ErrorIs(t, probe.lastErr, session.ErrCallQueuedNotExecuted)
				require.ErrorIs(t, probe.lastErr, session.ErrAgentFinalizationLeaseChanged)
			}
			require.NoError(t, barrierErr)
			require.Zero(t, reservationConsumed(t, fixture))
			require.Zero(t, probe.builds.Load())
			require.Zero(t, probe.runs.Load())
			require.False(t, real.IsSessionBusy(data.SessionID))
			if change == "replacement" {
				var leasedBy string
				require.NoError(t, fixture.conn.QueryRowContext(fixture.ctx, `SELECT leased_by FROM session_run_queue WHERE id='reserved'`).Scan(&leasedBy))
				require.Equal(t, "new-pump", leasedBy, "the old attempt cannot delete the competing lease")
			}
		})
	}
}

func TestFinalizationReservationPostConsumeFailureReleasesEraAndBuildScope(t *testing.T) {
	for _, failure := range []string{"panic", "cancel", "driver_replaced"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newFinalizationDriverFixture(t)
			sessions := fixture.api.(session.Service)
			data := reservationQueueCall(t, fixture, fixture.child, fixture.capability)
			driver := NewSessionAgent(SessionAgentOptions{})
			root := NewSessionAgent(SessionAgentOptions{})
			coordinator := &coordinator{sessions: sessions, currentAgent: root, subAgentDrivers: newSubAgentDriverRegistry()}
			coordinator.subAgentDrivers.register(data.SessionID, subAgentDriver{
				agent: driver, call: SessionAgentCall{SessionID: data.SessionID, LogicalCallID: data.LogicalCallID}, finalizationAuthorities: fixture.capability,
			})
			probe := &reservationQueueProbe{coordinator: coordinator}
			switch failure {
			case "panic":
				probe.panicRun = true
			case "cancel":
				probe.afterBuild = func(context.Context) { driver.Cancel(data.SessionID) }
			case "driver_replaced":
				probe.afterBuild = func(context.Context) {
					coordinator.subAgentDrivers.register(data.SessionID, subAgentDriver{
						agent: driver, call: SessionAgentCall{SessionID: data.SessionID, LogicalCallID: data.LogicalCallID}, finalizationAuthorities: fixture.capability,
					})
				}
			}
			pump := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: sessions, Coordinator: probe, PumpInstanceID: "pump"})
			defer pump.Stop()
			ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
			defer cancel()
			_, err := pump.DrainSessionNow(ctx, data.SessionID)
			require.Error(t, err)
			require.EqualValues(t, 1, reservationConsumed(t, fixture))
			require.EqualValues(t, 1, probe.builds.Load(), "consumed recovery never rebuilds a second model")
			require.EqualValues(t, 1, probe.releases.Load())
			require.False(t, driver.IsSessionBusy(data.SessionID))
			require.False(t, root.IsSessionBusy(data.SessionID))
			switch failure {
			case "panic":
				require.Equal(t, "reservation phase panic", probe.observed)
				require.EqualValues(t, 1, probe.runs.Load())
			case "driver_replaced":
				require.ErrorIs(t, probe.lastErr, session.ErrAgentFinalizationDriverUnavailable)
				require.EqualValues(t, 1, probe.runs.Load())
			case "cancel":
				require.ErrorIs(t, probe.lastErr, context.Canceled)
				require.Zero(t, probe.runs.Load())
			}
			state, err := session.NewAgentFinalizationStore(fixture.conn).Snapshot(ctx, fixture.child)
			require.NoError(t, err)
			require.Equal(t, "executing", state.Phase)
		})
	}
}

func TestFinalizationReservationMissingChildDriverDoesNotBurnProofOrUseRoot(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	sessions := fixture.api.(session.Service)
	data := reservationQueueCall(t, fixture, fixture.child, fixture.capability)
	root := NewSessionAgent(SessionAgentOptions{})
	coordinator := &coordinator{sessions: sessions, currentAgent: root, subAgentDrivers: newSubAgentDriverRegistry()}
	probe := &reservationQueueProbe{coordinator: coordinator}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{Sessions: sessions, Coordinator: probe, PumpInstanceID: "pump"})
	defer pump.Stop()
	ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
	defer cancel()
	_, err := pump.DrainSessionNow(ctx, data.SessionID)
	require.Error(t, err)
	require.ErrorIs(t, probe.lastErr, session.ErrAgentFinalizationDriverUnavailable)
	require.Zero(t, reservationConsumed(t, fixture))
	require.Zero(t, probe.builds.Load())
	require.Zero(t, probe.runs.Load())
	require.False(t, root.IsSessionBusy(data.SessionID))
	var code string
	require.NoError(t, fixture.conn.QueryRowContext(ctx, `SELECT failure_code FROM session_agent_finalization_continuations WHERE row_id='reserved'`).Scan(&code))
	require.Equal(t, "continuation_driver_unavailable", code)
}

func TestFinalizationReservationCancelledEntryDoesNotAccessStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	coordinator := &coordinator{}
	_, err := coordinator.RunFinalizationQueuedCall(ctx, "untrusted", session.SessionAgentCallData{})
	require.ErrorIs(t, err, context.Canceled)
}
