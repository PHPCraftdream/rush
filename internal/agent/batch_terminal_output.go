// Package-level output caching for batch leaves.
//
// Cursor contracts are per family and the cached read reproduces the live
// reader of the family that captured the entry: generic, agent and shell use
// the clamp contract (cursor is clamped to the retained end; over-end cursors
// return an empty page at EOF); argv uses the restart contract (cursors above
// MaxInt64 or above the retained end restart from offset 0, as
// tools.ArgvProcess.ReadOutput does live).
package agent

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

const (
	batchOutputTTL       = 10 * time.Minute
	batchOutputTaskLimit = 1 << 20
	batchOutputAppLimit  = 32 << 20
)

type batchCachedOutput struct {
	data           []byte
	taskID, rootID string
	oldest, total  uint64
	finished       time.Time
	truncated      bool
	argvContract   bool
}

// batchOutputCursorContract names the live cursor contract a family uses.
type batchOutputCursorContract uint8

const (
	batchOutputCursorClamp batchOutputCursorContract = iota
	batchOutputCursorArgvRestart
)

// batchOutputCache stores detached output bytes per App; expiry is lazy and
// never creates a cache-lifecycle goroutine.
type batchOutputCache struct {
	mu         sync.Mutex
	entries    map[jobbatch.ID]batchCachedOutput
	bytes      int
	maxEntries int
	now        func() time.Time
}

func newBatchOutputCache() *batchOutputCache {
	return &batchOutputCache{entries: make(map[jobbatch.ID]batchCachedOutput), maxEntries: 1024, now: time.Now}
}

func (c *batchOutputCache) put(id jobbatch.ID, chunk jobbatch.OutputChunk) { c.putFor(id, id, chunk) }

func (c *batchOutputCache) putFor(id, root jobbatch.ID, chunk jobbatch.OutputChunk) {
	c.putForContract(id, root, chunk, batchOutputCursorClamp)
}

// putForContract records chunk under the family's cursor contract so the
// cached read normalizes cursors exactly like the live reader does.
func (c *batchOutputCache) putForContract(id, root jobbatch.ID, chunk jobbatch.OutputChunk, contract batchOutputCursorContract) {
	if c == nil {
		return
	}
	data, oldest, total := chunk.Data, chunk.Oldest, chunk.Next
	if total < oldest+uint64(len(data)) {
		total = oldest + uint64(len(data))
	}
	truncated := chunk.Truncated
	if uint64(len(data)) > batchOutputTaskLimit {
		drop := uint64(len(data)) - batchOutputTaskLimit
		data = data[drop:]
		oldest += drop
		truncated = true
	}
	if oldest+uint64(len(data)) > total {
		total = oldest + uint64(len(data))
	}
	copyData := append([]byte(nil), data...)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.expireLocked(now)
	if prev, ok := c.entries[id]; ok {
		c.bytes -= len(prev.data)
	}
	c.entries[id] = batchCachedOutput{data: copyData, taskID: string(id), rootID: string(root), oldest: oldest, total: total, finished: now, truncated: truncated, argvContract: contract == batchOutputCursorArgvRestart}
	c.bytes += len(copyData)
	c.evictLocked()
}

func (c *batchOutputCache) evictLocked() {
	for c.bytes > batchOutputAppLimit || len(c.entries) > c.maxEntries {
		var victim jobbatch.ID
		var oldest time.Time
		first := true
		for id, entry := range c.entries {
			if first || entry.finished.Before(oldest) {
				victim, oldest, first = id, entry.finished, false
			}
		}
		if first {
			return
		}
		c.bytes -= len(c.entries[victim].data)
		delete(c.entries, victim)
	}
}

func (c *batchOutputCache) expireLocked(now time.Time) {
	for id, e := range c.entries {
		if !now.Before(e.finished.Add(batchOutputTTL)) {
			c.bytes -= len(e.data)
			delete(c.entries, id)
		}
	}
	c.evictLocked()
}

func (c *batchOutputCache) readWithMetadata(id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, uint64, int, bool, bool) {
	if c == nil {
		return jobbatch.OutputChunk{}, 0, 0, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(c.now())
	e, ok := c.entries[id]
	if !ok {
		return jobbatch.OutputChunk{}, 0, 0, false, false
	}
	start := cursor
	if e.argvContract {
		if start > math.MaxInt64 {
			start = 0
		}
		if start > e.total {
			start = 0
		}
	}
	if start < e.oldest {
		start = e.oldest
	}
	end := e.oldest + uint64(len(e.data))
	if start > e.total {
		start = e.total
	}
	if start > end {
		start = end
	}
	data := append([]byte(nil), e.data[start-e.oldest:end-e.oldest]...)
	chunk := jobbatch.OutputChunk{Data: data, Next: start + uint64(len(data)), Oldest: e.oldest, EOF: true, Truncated: e.truncated || cursor < e.oldest}
	return chunk, e.total, len(e.data), e.truncated || cursor < e.oldest, true
}

func (c *batchOutputCache) read(id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, bool) {
	chunk, _, _, _, ok := c.readWithMetadata(id, cursor)
	return chunk, ok
}

type batchArgvOutputReader struct{ process batchArgvHandle }

func (r batchArgvOutputReader) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	if cursor > math.MaxInt64 {
		cursor = math.MaxInt64
	}
	return r.process.ReadOutput(ctx, int64(cursor))
}

func batchOutputRoot(id jobbatch.ID) jobbatch.ID {
	root, _, _ := strings.Cut(string(id), ".")
	return jobbatch.ID(root)
}

func batchOutputCapture(cache *batchOutputCache, id, root jobbatch.ID, reader jobbatch.OutputReader, contract batchOutputCursorContract) error {
	if cache == nil || reader == nil {
		return nil
	}
	chunk, err := reader.ReadOutput(context.Background(), 0)
	if err != nil {
		return err
	}
	cache.putForContract(id, root, chunk, contract)
	return nil
}
