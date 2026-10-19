package agent

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestBatchOutputCacheTTLBoundaryAndCopy(t *testing.T) {
	cache := newBatchOutputCache()
	now := time.Unix(100, 0)
	cache.now = func() time.Time { return now }
	input := []byte("done")
	cache.putFor("task", "root", jobbatch.OutputChunk{Data: input, Next: 4, EOF: true})
	input[0] = 'X'
	chunk, ok := cache.read("task", 0)
	if !ok || string(chunk.Data) != "done" {
		t.Fatalf("copy %q", chunk.Data)
	}
	chunk.Data[0] = 'Y'
	if next, _ := cache.read("task", 0); string(next.Data) != "done" {
		t.Fatal("reader mutated cache")
	}
	now = now.Add(batchOutputTTL - time.Nanosecond)
	if _, ok := cache.read("task", 0); !ok {
		t.Fatal("expired early")
	}
	now = now.Add(time.Nanosecond)
	if _, ok := cache.read("task", 0); ok {
		t.Fatal("survived exact boundary")
	}
}

func TestBatchOutputCacheSuffixAndCursorContinuation(t *testing.T) {
	cache := newBatchOutputCache()
	data := []byte(strings.Repeat("x", batchOutputTaskLimit+97))
	cache.putFor("t", "r", jobbatch.OutputChunk{Data: data, Next: uint64(len(data)), EOF: true})
	chunk, ok := cache.read("t", 0)
	if !ok || len(chunk.Data) != batchOutputTaskLimit || chunk.Oldest != 97 || !chunk.Truncated {
		t.Fatalf("suffix %#v", chunk)
	}
	cursor := uint64(97)
	page, _, retained, truncated, found := cache.readWithMetadata("t", cursor)
	if !found || !truncated || retained != batchOutputTaskLimit {
		t.Fatalf("first page metadata: found=%v truncated=%v retained=%d", found, truncated, retained)
	}
	total := uint64(len(data))
	first := outputResponse(page, total, retained, truncated).(map[string]any)
	if len(first["data"].(string)) != batchModelOutputLimit || first["next_cursor"] != uint64(97+batchModelOutputLimit) || first["total_bytes"] != uint64(len(data)) || first["retained_bytes"] != batchOutputTaskLimit || first["eof"] != false {
		t.Fatalf("first page response %#v", first)
	}
	cursor = first["next_cursor"].(uint64)
	page, _, retained, truncated, found = cache.readWithMetadata("t", cursor)
	if !found || retained != batchOutputTaskLimit {
		t.Fatalf("second page metadata: found=%v retained=%d", found, retained)
	}
	second := outputResponse(page, total, retained, truncated).(map[string]any)
	if len(second["data"].(string)) != batchModelOutputLimit || second["next_cursor"] != uint64(97+2*batchModelOutputLimit) || second["total_bytes"] != uint64(len(data)) || second["retained_bytes"] != batchOutputTaskLimit {
		t.Fatalf("second page response %#v", second)
	}
	cursor = second["next_cursor"].(uint64)
	for cursor < uint64(len(data)) {
		page, _, retained, truncated, found = cache.readWithMetadata("t", cursor)
		if !found {
			t.Fatalf("cache entry vanished at cursor %d", cursor)
		}
		resp := outputResponse(page, total, retained, truncated).(map[string]any)
		chunk := resp["data"].(string)
		if len(chunk) == 0 {
			t.Fatalf("empty page %d", cursor)
		}
		if len(chunk) > batchModelOutputLimit {
			t.Fatalf("response page exceeds model limit: %d", len(chunk))
		}
		cursor = resp["next_cursor"].(uint64)
	}
}

func TestBatchOutputCacheEvictsOldestFinished(t *testing.T) {
	cache := newBatchOutputCache()
	payload := make([]byte, batchOutputTaskLimit)
	clock := time.Unix(200, 0)
	cache.now = func() time.Time { return clock }
	firstID, secondID := jobbatch.ID(fmt.Sprintf("task-%06d", 0)), jobbatch.ID(fmt.Sprintf("task-%06d", 1))
	cache.putFor(firstID, "root", jobbatch.OutputChunk{Data: payload, Next: uint64(len(payload)), EOF: true})
	clock = time.Unix(100, 0)
	cache.putFor(secondID, "root", jobbatch.OutputChunk{Data: payload, Next: uint64(len(payload)), EOF: true})
	for i := 2; i < 33; i++ {
		clock = time.Unix(int64(201+i), 0)
		id := jobbatch.ID(fmt.Sprintf("task-%06d", i))
		cache.putFor(id, "root", jobbatch.OutputChunk{Data: payload, Next: uint64(len(payload)), EOF: true})
	}
	clock = time.Unix(300, 0)
	if cache.bytes != batchOutputAppLimit {
		t.Fatalf("retained bytes %d", cache.bytes)
	}
	if _, ok := cache.read(firstID, 0); !ok {
		t.Fatal("oldest insertion was incorrectly evicted")
	}
	if _, ok := cache.read(secondID, 0); ok {
		t.Fatal("oldest finish was retained")
	}
}

func TestBatchOutputCacheReaderCopySurvivesConcurrentExpiry(t *testing.T) {
	cache := newBatchOutputCache()
	var seconds atomic.Int64
	seconds.Store(100)
	cache.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	cache.putFor("reader", "root", jobbatch.OutputChunk{Data: []byte("snapshot"), Next: 8})
	ready, release := make(chan jobbatch.OutputChunk, 1), make(chan struct{})
	result := make(chan string, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		chunk, _ := cache.read("reader", 0)
		ready <- chunk
		<-release
		result <- string(chunk.Data)
	}()
	held := <-ready
	seconds.Store(100 + int64(batchOutputTTL/time.Second))
	if _, ok := cache.read("reader", 0); ok {
		t.Fatal("expired entry retained")
	}
	close(release)
	if got := <-result; got != "snapshot" || string(held.Data) != "snapshot" {
		t.Fatalf("reader copy changed: %q", got)
	}
}

func TestBatchOutputCacheReaderCopySurvivesConcurrentEviction(t *testing.T) {
	cache := newBatchOutputCache()
	cache.maxEntries = 1
	var seconds atomic.Int64
	seconds.Store(1000)
	cache.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	cache.putFor("old", "root", jobbatch.OutputChunk{Data: []byte("old-bytes"), Next: 9})
	ready, release := make(chan struct{}), make(chan struct{})
	result := make(chan string, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		chunk, _ := cache.read("old", 0)
		close(ready)
		<-release
		result <- string(chunk.Data)
	}()
	<-ready
	seconds.Store(1001)
	cache.putFor("new", "root", jobbatch.OutputChunk{Data: []byte("new"), Next: 3})
	if _, ok := cache.read("old", 0); ok {
		t.Fatal("old entry retained after eviction")
	}
	close(release)
	if got := <-result; got != "old-bytes" {
		t.Fatalf("reader copy changed: %q", got)
	}
}

// fakeClampReader reproduces the generic/agent live contract
// (batch_exec_generic.go ReadOutput): cursor = min(cursor, len(content)),
// data = content[cursor:], Next = total, Oldest = 0, EOF = true.
type fakeClampReader struct{ content []byte }

func (r fakeClampReader) read(cursor uint64) jobbatch.OutputChunk {
	total := uint64(len(r.content))
	if cursor > total {
		cursor = total
	}
	return jobbatch.OutputChunk{Data: append([]byte(nil), r.content[cursor:]...), Next: total, Oldest: 0, EOF: true}
}

// fakeShellReader reproduces the shell live contract
// (internal/shell BackgroundExecution.ReadOutput) for a finished stream with
// an empty drop window: start = max(cursor, oldest) clamped to next,
// Next = stream next, Truncated = cursor < oldest, EOF = done.
type fakeShellReader struct{ content []byte }

func (r fakeShellReader) read(cursor uint64) jobbatch.OutputChunk {
	oldest, next := uint64(0), uint64(len(r.content))
	start := cursor
	if start < oldest {
		start = oldest
	}
	if start > next {
		start = next
	}
	return jobbatch.OutputChunk{Data: append([]byte(nil), r.content[start-oldest:]...), Next: next, Oldest: oldest, EOF: true, Truncated: cursor < oldest}
}

// fakeArgvReader reproduces the argv live contract
// (batchArgvOutputReader + tools.ArgvProcess.ReadOutput on a finished
// process): cursors above MaxInt64 reset to zero, cursors below zero or above
// the retained total reset to zero, Next = total, EOF = true.
type fakeArgvReader struct{ content []byte }

func (r fakeArgvReader) read(cursor uint64) jobbatch.OutputChunk {
	if cursor > math.MaxInt64 {
		cursor = 0
	}
	total := uint64(len(r.content))
	if cursor > total {
		cursor = 0
	}
	return jobbatch.OutputChunk{Data: append([]byte(nil), r.content[cursor:]...), Next: total, Oldest: 0, EOF: true}
}

func TestBatchOutputCacheCursorContractPerFamily(t *testing.T) {
	content := []byte("abcdefgh")
	total := uint64(len(content))
	cursors := []uint64{0, 3, total, total + 5, math.MaxInt64, uint64(1) << 63}
	cache := newBatchOutputCache()
	chunk := jobbatch.OutputChunk{Data: append([]byte(nil), content...), Next: total, EOF: true}
	cache.putFor("clamp", "root", chunk)
	cache.putFor("shell", "root", chunk)
	cache.putForContract("argv", "root", chunk, batchOutputCursorArgvRestart)
	families := []struct {
		id   jobbatch.ID
		live func(uint64) jobbatch.OutputChunk
	}{
		{id: "clamp", live: fakeClampReader{content}.read},
		{id: "shell", live: fakeShellReader{content}.read},
		{id: "argv", live: fakeArgvReader{content}.read},
	}
	for _, fam := range families {
		for _, cursor := range cursors {
			live := fam.live(cursor)
			cached, ok := cache.read(fam.id, cursor)
			if !ok {
				t.Fatalf("%s: cache miss at cursor %d", fam.id, cursor)
			}
			if string(live.Data) != string(cached.Data) || live.Next != cached.Next || live.EOF != cached.EOF {
				t.Fatalf("%s: cursor %d live=(%q,%d,%v) cached=(%q,%d,%v)", fam.id, cursor, live.Data, live.Next, live.EOF, cached.Data, cached.Next, cached.EOF)
			}
		}
	}
}

func TestLiveBatchOutputRetainedBytesUsesWindow(t *testing.T) {
	chunk := jobbatch.OutputChunk{Data: []byte("0123456789"), Next: 300, Oldest: 100, EOF: true, Truncated: true}
	resp := liveBatchOutput(chunk).(map[string]any)
	if resp["total_bytes"] != uint64(300) {
		t.Fatalf("total_bytes %#v", resp["total_bytes"])
	}
	if resp["retained_bytes"] != 200 {
		t.Fatalf("retained_bytes %#v", resp["retained_bytes"])
	}
	if resp["oldest_cursor"] != uint64(100) || resp["next_cursor"] != uint64(300) || resp["eof"] != true || resp["truncated"] != true {
		t.Fatalf("chunk fields %#v", resp)
	}
	inverted := jobbatch.OutputChunk{Next: 5, Oldest: 9}
	if got := liveBatchOutput(inverted).(map[string]any)["retained_bytes"]; got != 0 {
		t.Fatalf("inverted retained_bytes %#v", got)
	}
}

func TestBatchOutputCacheMetadataOnly(t *testing.T) {
	typ := reflect.TypeOf(batchCachedOutput{})
	allowed := []reflect.Type{reflect.TypeOf([]byte{}), reflect.TypeOf(""), reflect.TypeOf(uint64(0)), reflect.TypeOf(false), reflect.TypeOf(time.Time{})}
	for i := 0; i < typ.NumField(); i++ {
		fieldType := typ.Field(i).Type
		found := false
		for _, candidate := range allowed {
			if fieldType == candidate {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("unexpected retained field %s %s", typ.Field(i).Name, fieldType)
		}
	}
}
