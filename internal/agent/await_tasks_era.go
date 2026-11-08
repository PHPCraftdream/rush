package agent

import (
	"context"
	"errors"
)

// Ненулевой размер гарантирует отдельную identity для каждой установки.
type awaitTasksEra struct{ marker byte }

func (a *arbiter) beginAwait(sid string, all bool) (*awaitTasksEra, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if s.suspended {
		return nil, ""
	}
	old := s.maxWaitWakeID
	era := &awaitTasksEra{}
	s.awaitEra, s.sleepAll, s.maxWaitWakeID = era, all, ""
	return era, old
}

func (a *arbiter) installAwait(sid string, era *awaitTasksEra, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil || era == nil || s.awaitEra != era || s.suspended {
		return false
	}
	s.maxWaitWakeID = id
	return true
}

func (a *arbiter) abortAwait(sid string, era *awaitTasksEra) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.bySession[sid]; s != nil && s.awaitEra == era {
		s.awaitEra, s.sleepAll, s.maxWaitWakeID = nil, false, ""
	}
}

var errAwaitSuperseded = errors.New("await_tasks was stopped or superseded; recheck current work before waiting again")

// create выполняет SQL вне mutex; результат принимается только той же era.
func (c *coordinator) armAwait(ctx context.Context, sid string, all bool, create func() (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	era, old := c.arb.beginAwait(sid, all)
	if era == nil {
		return "", errAwaitSuperseded
	}
	c.cancelAwaitTimer(sid, old)
	id := ""
	if create != nil {
		var err error
		id, err = create()
		if err != nil {
			c.arb.abortAwait(sid, era)
			c.cancelAwaitTimer(sid, id)
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		c.arb.abortAwait(sid, era)
		c.cancelAwaitTimer(sid, id)
		return "", err
	}
	if !c.arb.installAwait(sid, era, id) {
		c.cancelAwaitTimer(sid, id)
		return "", errAwaitSuperseded
	}
	return id, nil
}
