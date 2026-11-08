package agent

import "context"

// ExternalInterruptSource обслуживает interrupt между ходами CLI-драйвера.
type ExternalInterruptSource interface {
	PollExternalInterrupt(context.Context, string) (string, error)
}

func (c *coordinator) PollExternalInterrupt(ctx context.Context, sessionID string) (string, error) {
	if !c.driverHints.isExternalDriver(sessionID) || c.IsSessionBusy(sessionID) {
		return "", nil
	}
	tickCtx, cancel := context.WithTimeout(ctx, interruptTickOperationTimeout)
	defer cancel()
	var messageID string
	// Существующий atomic consume/enqueue сохраняет generation и policy fences.
	fired, err := c.handleInterruptTickObserved(tickCtx, sessionID, func(id string) { messageID = id })
	if fired {
		c.clearSleepAll(sessionID)
		c.driverHints.bumpHint(sessionID)
	}
	return messageID, err
}
