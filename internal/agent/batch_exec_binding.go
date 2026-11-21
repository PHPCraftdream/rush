package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

func newBatchBindingExecution(execution jobbatch.Execution, release func()) jobbatch.Execution {
	return batchexec.WithBindingLease(execution, release)
}
