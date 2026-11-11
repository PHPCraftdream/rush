package agent

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
)

func SupervisionNoticeStoreForTest(c Coordinator) *session.SessionNoticeStore {
	coord, ok := c.(*coordinator)
	if !ok || coord == nil {
		return nil
	}
	return coord.sessionNoticeStore()
}

type SupervisionNoticeWakeForTest struct {
	SessionID   string
	IsDrain     bool
	Kind        string
	Wake        int64
	NoticeCount int
	ReadErr     bool
}

type SupervisionNoticeProbeForTest struct {
	coord            *coordinator
	owner            string
	Wakes            chan SupervisionNoticeWakeForTest
	runs             atomic.Int32
	expectedRuns     atomic.Int32
	completed        chan struct{}
	driverGeneration uint64
}

type probeAgent struct {
	*mockSessionAgent
	busy bool
}

func (a *probeAgent) IsSessionBusy(string) bool { return a.busy }

func NewSupervisionNoticeProbeForTest(c Coordinator, owner string, conn *sql.DB, busy bool) (*SupervisionNoticeProbeForTest, error) {
	coord, ok := c.(*coordinator)
	if !ok || coord == nil {
		return nil, errors.New("supervision notice probe requires coordinator")
	}
	if owner == "" {
		return nil, errors.New("supervision notice probe requires owner")
	}
	if conn == nil {
		return nil, errors.New("supervision notice probe requires database")
	}
	if coord.supervision == nil {
		return nil, errors.New("supervision notice probe requires initialized supervision registry")
	}
	if coord.subAgentDrivers == nil {
		return nil, errors.New("supervision notice probe requires initialized sub-agent driver registry")
	}
	if coord.sessionNoticeStore() == nil {
		return nil, errors.New("supervision notice probe requires initialized session notice store")
	}
	coord.supervision.mu.Lock()
	byRootInitialized := coord.supervision.byRoot != nil
	coord.supervision.mu.Unlock()
	if !byRootInitialized {
		return nil, errors.New("supervision notice probe requires initialized supervision state map")
	}
	probe := &SupervisionNoticeProbeForTest{
		coord:     coord,
		owner:     owner,
		Wakes:     make(chan SupervisionNoticeWakeForTest, 16),
		completed: make(chan struct{}, 16),
	}
	agent := &probeAgent{mockSessionAgent: &mockSessionAgent{}, busy: busy}
	agent.runFunc = func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		probe.runs.Add(1)
		defer func() { probe.completed <- struct{}{} }()

		observation := SupervisionNoticeWakeForTest{SessionID: call.SessionID, IsDrain: call.IsDrain}
		queryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := conn.QueryContext(queryCtx, "SELECT kind, wake FROM session_notices WHERE owner=? ORDER BY id", call.SessionID)
		if err != nil {
			observation.ReadErr = true
		} else {
			func() {
				defer func() {
					if rows.Close() != nil {
						observation.ReadErr = true
					}
				}()
				for rows.Next() {
					var kind string
					var wake int64
					if err := rows.Scan(&kind, &wake); err != nil {
						observation.ReadErr = true
						break
					}
					if observation.NoticeCount == 0 {
						observation.Kind = kind
						observation.Wake = wake
					}
					observation.NoticeCount++
				}
				if rows.Err() != nil {
					observation.ReadErr = true
				}
			}()
		}
		probe.Wakes <- observation
		return nil, nil
	}
	probe.driverGeneration = coord.subAgentDrivers.register(owner, subAgentDriver{agent: agent, call: SessionAgentCall{SessionID: owner}})

	coord.supervision.mu.Lock()
	coord.supervision.byRoot[owner] = &supervisionState{
		rootSessionID: owner,
		cfg: SupervisionConfig{
			Enabled:       true,
			Interval:      time.Hour,
			MaxInterval:   time.Hour,
			MaxNoProgress: 1,
		},
		generation: 1,
		interval:   time.Hour,
	}
	coord.supervision.mu.Unlock()
	return probe, nil
}

func (p *SupervisionNoticeProbeForTest) Fire(generation uint64) {
	if p == nil || p.coord == nil {
		return
	}
	_, beforeTicks, _ := p.State()
	p.coord.handleSupervisionDeadline(p.owner, generation)
	_, afterTicks, _ := p.State()
	if afterTicks > beforeTicks && p.coord.sessionNoticeStore() != nil {
		p.expectedRuns.Add(1)
	}
}

func (p *SupervisionNoticeProbeForTest) State() (exists bool, tickCount int, paused bool) {
	if p == nil || p.coord == nil || p.coord.supervision == nil {
		return false, 0, false
	}
	p.coord.supervision.mu.Lock()
	defer p.coord.supervision.mu.Unlock()
	state, exists := p.coord.supervision.byRoot[p.owner]
	if !exists {
		return false, 0, false
	}
	return true, state.tickCount, state.paused
}

func (p *SupervisionNoticeProbeForTest) Runs() int32 {
	if p == nil {
		return 0
	}
	return p.runs.Load()
}

func (p *SupervisionNoticeProbeForTest) Wait() bool {
	if p == nil {
		return true
	}
	target := p.expectedRuns.Load()
	if target == 0 {
		return p.Runs() == 0
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for completed := int32(0); completed < target; completed++ {
		select {
		case <-p.completed:
		case <-timer.C:
			return false
		}
	}
	return p.Runs() == target
}

func (p *SupervisionNoticeProbeForTest) Close() {
	if p == nil || p.coord == nil || p.coord.subAgentDrivers == nil {
		return
	}
	p.coord.subAgentDrivers.releaseIfCurrent(p.owner, p.driverGeneration)
}
