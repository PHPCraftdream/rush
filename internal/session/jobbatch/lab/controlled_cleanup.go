package lab

// releaseCleanupReads unblocks only physical held reads after cancellation of
// abandoned async Output. Reports and Stop/Dispose returns remain script-owned;
// teardown must not invent successful controls or change dispose_pending facts.
func (c *ControlledExecutor) releaseCleanupReads() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.order {
		if g := a.read; g != nil && g.done != nil && !g.released {
			g.result, g.released = GateOK, true
			close(g.done)
		}
	}
	c.signal()
}
