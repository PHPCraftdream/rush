package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
)

// batchDriverHints preserves the root method names while batchlife owns the
// ledger. The receiver conversions share the same ledger without copying it.
type batchDriverHints batchlife.DriverHints

func newBatchDriverHints() *batchDriverHints { return (*batchDriverHints)(batchlife.NewDriverHints()) }

func (h *batchDriverHints) bumpHint(owner string) {
	(*batchlife.DriverHints)(h).BumpHint(owner)
}

func (h *batchDriverHints) hintSeqOf(owner string) uint64 {
	return (*batchlife.DriverHints)(h).HintSeqOf(owner)
}

func (h *batchDriverHints) waitForHint(ctx context.Context, owner string, since uint64) bool {
	return (*batchlife.DriverHints)(h).WaitForHint(ctx, owner, since)
}

func (h *batchDriverHints) claimExternalDriver(owner string) {
	(*batchlife.DriverHints)(h).ClaimExternalDriver(owner)
}

func (h *batchDriverHints) releaseExternalDriver(owner string) {
	(*batchlife.DriverHints)(h).ReleaseExternalDriver(owner)
}

func (h *batchDriverHints) isExternalDriver(owner string) bool {
	return (*batchlife.DriverHints)(h).IsExternalDriver(owner)
}

func (h *batchDriverHints) idleCandidates() []string {
	return (*batchlife.DriverHints)(h).IdleCandidates()
}

func (h *batchDriverHints) dropIfIdle(id string) bool {
	return (*batchlife.DriverHints)(h).DropIfIdle(id)
}

func (h *batchDriverHints) subscribe(owner string) (<-chan struct{}, func()) {
	return (*batchlife.DriverHints)(h).Subscribe(owner)
}
