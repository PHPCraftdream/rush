package agent

import (
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchCutoverReactionStore returns the node-backed BatchStore behind c's
// bound flag-ON fact source, for the two WRITE twins the reaction paths own:
// the real-step reaction marker (BatchReactionMarkReactedWithMessageUpdate,
// persistStepFinish/persistFailureFinish) and Stop's wake-zero pass
// (BatchReactionSetWakeZeroForOwners, stopTree). nil means "keep the legacy
// async_jobs twin byte-for-byte": the flag is off, nothing is bound to this
// coordinator, or the bound source is not the node-backed one (the
// conservative unknown source has no store). Fact READS keep flowing through
// the BatchCutoverFacts seam; only these two writes are unwrapped here,
// because their legacy twins are called from turnStream/coordinator, not
// behind the fact-source interface.
func batchCutoverReactionStore(c *coordinator) *session.BatchStore {
	if !batchFlagEnabled() || c == nil {
		return nil
	}
	v, ok := coordinatorBatchCutoverFactsBindings.Load(c)
	if !ok {
		return nil
	}
	nbf, ok := v.(nodeBatchCutoverFacts)
	if !ok || nbf.store == nil {
		return nil
	}
	return nbf.store
}
