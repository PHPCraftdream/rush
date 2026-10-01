package tools

import "fmt"

// JobDeliveryStatusError is returned by JobShellResolver.ResolveJobShellID
// when jobID is no longer in the ledger's in-memory map but its durable
// async_jobs row answers what happened to the result: it is still waiting to
// be pulled (Delivered=false -- B1: the job finished and deliverLocked
// dropped it from the map the moment its terminal state committed, while the
// row stays delivery='pending' until the turn boundary pulls it, so "already
// delivered" would be a lie), or it was already pulled and delivered
// (Delivered=true). A typed error (not a string match) so job_output's
// routing decision does not depend on exact wording, mirroring
// RunCommandJobError/DelegationJobError.
type JobDeliveryStatusError struct {
	JobID     string
	Delivered bool
}

func (e *JobDeliveryStatusError) Error() string {
	if e.Delivered {
		return fmt.Sprintf("job %s was already delivered", e.JobID)
	}
	return fmt.Sprintf("job %s finished; its result is delivered as the next message -- end your turn now (no tool call) to receive it", e.JobID)
}
