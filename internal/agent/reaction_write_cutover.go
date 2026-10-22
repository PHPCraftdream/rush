package agent

import (
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchCutoverReactionStore returns the node-backed BatchStore behind c's
// bound fact source, for the two WRITE paths the reaction sides own:
// the step/failure reaction marker (BatchReactionMarkReactedWithMessageUpdate,
// persistStepFinish/persistFailureFinish) and Stop's wake-zero pass
// (BatchReactionSetWakeZeroForOwners, stopTree). nil means "not bound to
// this coordinator, or the bound source is not the node-backed one (the
// conservative unknown source has no store)": the callers fall back to the
// plain message write so the finish still lands. Fact READS keep flowing
// through the BatchCutoverFacts seam.
func batchCutoverReactionStore(c *coordinator) *session.BatchStore {
	if c == nil {
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
