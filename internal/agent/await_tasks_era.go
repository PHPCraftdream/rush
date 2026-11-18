package agent

import (
	"context"
)

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
