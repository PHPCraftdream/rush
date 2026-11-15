package lab

// scenarioPhysicalState copies fixture observations, not Runtime internals or
// error text. It cannot release gates or authorize calls.
func (c *ControlledExecutor) scenarioPhysicalState() []ScenarioPhysicalState {
	c.mu.Lock()
	defer c.mu.Unlock()
	states := make([]ScenarioPhysicalState, 0, len(c.order))
	for _, a := range c.order {
		states = append(states, ScenarioPhysicalState{
			Node: a.launch.ID, Token: a.launch.Token, StopActive: a.stopActive,
			Reads: a.reads, DisposeEntered: a.dispose != nil,
			DisposeReturned: a.phase == controlledDisposed,
		})
	}
	return states
}
