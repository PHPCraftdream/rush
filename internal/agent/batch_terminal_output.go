package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

const (
	batchOutputTTL               = batchmeta.OutputTTL
	batchOutputTaskLimit         = batchmeta.OutputTaskLimit
	batchOutputCursorClamp       = batchmeta.OutputCursorClamp
	batchOutputCursorArgvRestart = batchmeta.OutputCursorArgvRestart
)

type batchOutputCursorContract = batchmeta.OutputCursorContract

// batchOutputCache keeps existing executor and control callers source-compatible.
type batchOutputCache struct {
	storage *batchmeta.OutputCache
	// retain, when set before use, replaces the default storage sink.
	retain func(id, root jobbatch.ID, chunk jobbatch.OutputChunk, contract batchOutputCursorContract)
}

func newBatchOutputCache() *batchOutputCache {
	return &batchOutputCache{storage: batchmeta.NewOutputCache()}
}

func (c *batchOutputCache) leaf() *batchmeta.OutputCache {
	if c == nil {
		return nil
	}
	return c.storage
}

func (c *batchOutputCache) putFor(id, root jobbatch.ID, chunk jobbatch.OutputChunk) {
	c.putForContract(id, root, chunk, batchOutputCursorClamp)
}

func (c *batchOutputCache) putForContract(id, root jobbatch.ID, chunk jobbatch.OutputChunk, contract batchOutputCursorContract) {
	if storage := c.leaf(); storage != nil {
		if c.retain != nil {
			c.retain(id, root, chunk, contract)
			return
		}
		storage.PutForContract(id, root, chunk, contract)
	}
}

func (c *batchOutputCache) readWithMetadata(id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, uint64, int, bool, bool) {
	return c.leaf().ReadWithMetadata(id, cursor)
}

type batchArgvOutputReader struct{ process batchArgvHandle }

func (r batchArgvOutputReader) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return batchmeta.NewArgvOutputReader(r.process).ReadOutput(ctx, cursor)
}

func batchOutputRoot(id jobbatch.ID) jobbatch.ID { return batchmeta.OutputRoot(id) }

func batchOutputCapture(cache *batchOutputCache, id, root jobbatch.ID, reader jobbatch.OutputReader, contract batchOutputCursorContract) error {
	if cache.leaf() == nil {
		return nil
	}
	return batchmeta.CaptureOutput(cache.putForContract, id, root, reader, contract)
}
