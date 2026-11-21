// Package-level output caching for batch leaves.
//
// Cursor contracts are per family and the cached read reproduces the live
// reader of the family that captured the entry: generic, agent and shell use
// the clamp contract (cursor is clamped to the retained end; over-end cursors
// return an empty page at EOF); argv uses the restart contract (cursors above
// MaxInt64 or above the retained end restart from offset 0, as
// tools.ArgvProcess.ReadOutput does live).
package batchmeta

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

const (
	OutputTTL       = 10 * time.Minute
	OutputTaskLimit = 1 << 20
	outputAppLimit  = 32 << 20
)

type batchCachedOutput struct {
	data           []byte
	taskID, rootID string
	oldest, total  uint64
	finished       time.Time
	truncated      bool
	argvContract   bool
}

// OutputCursorContract names the live cursor contract a family uses.
type OutputCursorContract uint8

const (
	OutputCursorClamp OutputCursorContract = iota
	OutputCursorArgvRestart
)

// OutputCache stores detached output bytes per App; expiry is lazy and
// never creates a cache-lifecycle goroutine.
type OutputCache struct {
	mu         sync.Mutex
	entries    map[jobbatch.ID]batchCachedOutput
	bytes      int
	maxEntries int
	now        func() time.Time
}

func NewOutputCache() *OutputCache {
	return &OutputCache{entries: make(map[jobbatch.ID]batchCachedOutput), maxEntries: 1024, now: time.Now}
}

func (c *OutputCache) put(id jobbatch.ID, chunk jobbatch.OutputChunk) { c.PutFor(id, id, chunk) }

func (c *OutputCache) PutFor(id, root jobbatch.ID, chunk jobbatch.OutputChunk) {
	c.PutForContract(id, root, chunk, OutputCursorClamp)
}

// PutForContract records chunk under the family's cursor contract so the
// cached read normalizes cursors exactly like the live reader does.
func (c *OutputCache) PutForContract(id, root jobbatch.ID, chunk jobbatch.OutputChunk, contract OutputCursorContract) {
	if c == nil {
		return
	}
	data, oldest, total := chunk.Data, chunk.Oldest, chunk.Next
	if total < oldest+uint64(len(data)) {
		total = oldest + uint64(len(data))
	}
	truncated := chunk.Truncated
	if uint64(len(data)) > OutputTaskLimit {
		drop := uint64(len(data)) - OutputTaskLimit
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
	c.entries[id] = batchCachedOutput{data: copyData, taskID: string(id), rootID: string(root), oldest: oldest, total: total, finished: now, truncated: truncated, argvContract: contract == OutputCursorArgvRestart}
	c.bytes += len(copyData)
	c.evictLocked()
}

func (c *OutputCache) evictLocked() {
	for c.bytes > outputAppLimit || len(c.entries) > c.maxEntries {
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

func (c *OutputCache) expireLocked(now time.Time) {
	for id, e := range c.entries {
		if !now.Before(e.finished.Add(OutputTTL)) {
			c.bytes -= len(e.data)
			delete(c.entries, id)
		}
	}
	c.evictLocked()
}

func (c *OutputCache) ReadWithMetadata(id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, uint64, int, bool, bool) {
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
	argvRestart := false
	if e.argvContract {
		if start > math.MaxInt64 {
			start, argvRestart = 0, true
		}
		if start > e.total {
			start, argvRestart = 0, true
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
	truncated := e.truncated || cursor < e.oldest
	if argvRestart {
		// tools.ArgvProcess.ReadOutput normalizes an out-of-range cursor to
		// zero BEFORE computing Truncated (cursor < oldest); mirror that so a
		// restarted cached read stays identical to the live one.
		truncated = e.truncated || 0 < e.oldest
	}
	chunk := jobbatch.OutputChunk{Data: data, Next: start + uint64(len(data)), Oldest: e.oldest, EOF: true, Truncated: truncated}
	return chunk, e.total, len(e.data), truncated, true
}

type argvOutputProcess interface {
	ReadOutput(context.Context, int64) (jobbatch.OutputChunk, error)
}

// NewArgvOutputReader adapts an argv process to the batch output interface.
func NewArgvOutputReader(process argvOutputProcess) argvOutputReader {
	return argvOutputReader{process: process}
}

type argvOutputReader struct{ process argvOutputProcess }

func (r argvOutputReader) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	if cursor > math.MaxInt64 {
		cursor = math.MaxInt64
	}
	return r.process.ReadOutput(ctx, int64(cursor))
}

// CaptureOutput reads from cursor zero and records the chunk only on success.
// A nil recorder or reader skips capture.
func CaptureOutput(record func(id, root jobbatch.ID, chunk jobbatch.OutputChunk, contract OutputCursorContract), id, root jobbatch.ID, reader jobbatch.OutputReader, contract OutputCursorContract) error {
	if record == nil || reader == nil {
		return nil
	}
	chunk, err := reader.ReadOutput(context.Background(), 0)
	if err != nil {
		return err
	}
	record(id, root, chunk, contract)
	return nil
}

func OutputRoot(id jobbatch.ID) jobbatch.ID {
	root, _, _ := strings.Cut(string(id), ".")
	return jobbatch.ID(root)
}
