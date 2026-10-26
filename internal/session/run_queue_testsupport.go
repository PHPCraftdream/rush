package session

// LiveRunQueueDispatchesForTest reports the number of worker slots currently
// held by dispatched run-queue workers. Tests must first observe Stop's
// false barrier; this count supplements that barrier and is not itself a join.
func (p *RunQueuePump) LiveRunQueueDispatchesForTest() int {
	return len(p.execSem)
}
