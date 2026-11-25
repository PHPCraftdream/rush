package batchmeta

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/stretchr/testify/require"
)

type captureOutputReaderFunc func(context.Context, uint64) (jobbatch.OutputChunk, error)

func (f captureOutputReaderFunc) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return f(ctx, cursor)
}

func TestCaptureOutput(t *testing.T) {
	readErr := errors.New("read failed")
	chunk := jobbatch.OutputChunk{Data: []byte("done"), Oldest: 2, Next: 6, EOF: true, Truncated: true}
	for _, tc := range []struct {
		name                   string
		noReader, noRecorder   bool
		readErr                error
		wantReads, wantRecords int
	}{
		{name: "success", wantReads: 1, wantRecords: 1},
		{name: "read error", readErr: readErr, wantReads: 1},
		{name: "nil reader", noReader: true},
		{name: "nil recorder", noRecorder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, records := 0, 0
			var reader jobbatch.OutputReader = captureOutputReaderFunc(func(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
				reads++
				if ctx != context.Background() || cursor != 0 {
					t.Fatalf("read context = %v, cursor = %d", ctx, cursor)
				}
				return chunk, tc.readErr
			})
			record := func(id, root jobbatch.ID, got jobbatch.OutputChunk, contract OutputCursorContract) {
				records++
				if reads != 1 || id != "root.task" || root != "root" || !reflect.DeepEqual(got, chunk) || contract != OutputCursorArgvRestart {
					t.Fatalf("record after %d reads: (%q, %q, %+v, %v)", reads, id, root, got, contract)
				}
			}
			if tc.noReader {
				reader = nil
			}
			if tc.noRecorder {
				record = nil
			}
			if err := CaptureOutput(record, "root.task", "root", reader, OutputCursorArgvRestart); err != tc.readErr {
				t.Fatalf("error = %v, want %v", err, tc.readErr)
			}
			if reads != tc.wantReads || records != tc.wantRecords {
				t.Fatalf("reads = %d, records = %d; want %d, %d", reads, records, tc.wantReads, tc.wantRecords)
			}
		})
	}
}

func TestBatchOutputCacheTTLBoundaryAndCopy(t *testing.T) {
	cache := NewOutputCache()
	now := time.Unix(100, 0)
	cache.now = func() time.Time { return now }
	input := []byte("done")
	cache.PutFor("task", "root", jobbatch.OutputChunk{Data: input, Next: 4, EOF: true})
	if entry := cache.entries["task"]; entry.taskID != "task" || entry.rootID != "root" {
		t.Fatalf("stored identity: task %q, root %q", entry.taskID, entry.rootID)
	}
	input[0] = 'X'
	chunk, _, _, _, ok := cache.ReadWithMetadata("task", 0)
	if !ok || string(chunk.Data) != "done" {
		t.Fatalf("copy %q", chunk.Data)
	}
	chunk.Data[0] = 'Y'
	if next, _, _, _, _ := cache.ReadWithMetadata("task", 0); string(next.Data) != "done" {
		t.Fatal("reader mutated cache")
	}
	now = now.Add(OutputTTL - time.Nanosecond)
	if _, _, _, _, ok := cache.ReadWithMetadata("task", 0); !ok {
		t.Fatal("expired early")
	}
	now = now.Add(time.Nanosecond)
	if _, _, _, _, ok := cache.ReadWithMetadata("task", 0); ok {
		t.Fatal("survived exact boundary")
	}
}

func TestBatchOutputCacheEvictsOldestFinished(t *testing.T) {
	cache := NewOutputCache()
	payload := make([]byte, OutputTaskLimit)
	clock := time.Unix(200, 0)
	cache.now = func() time.Time { return clock }
	firstID, secondID := jobbatch.ID(fmt.Sprintf("task-%06d", 0)), jobbatch.ID(fmt.Sprintf("task-%06d", 1))
	cache.PutFor(firstID, "root", jobbatch.OutputChunk{Data: payload, Next: uint64(len(payload)), EOF: true})
	clock = time.Unix(100, 0)
	cache.PutFor(secondID, "root", jobbatch.OutputChunk{Data: payload, Next: uint64(len(payload)), EOF: true})
	for i := 2; i < 33; i++ {
		clock = time.Unix(int64(201+i), 0)
		id := jobbatch.ID(fmt.Sprintf("task-%06d", i))
		cache.PutFor(id, "root", jobbatch.OutputChunk{Data: payload, Next: uint64(len(payload)), EOF: true})
	}
	clock = time.Unix(300, 0)
	if cache.bytes != outputAppLimit {
		t.Fatalf("retained bytes %d", cache.bytes)
	}
	if _, _, _, _, ok := cache.ReadWithMetadata(firstID, 0); !ok {
		t.Fatal("oldest insertion was incorrectly evicted")
	}
	if _, _, _, _, ok := cache.ReadWithMetadata(secondID, 0); ok {
		t.Fatal("oldest finish was retained")
	}
}

func TestBatchOutputCacheReaderCopySurvivesConcurrentExpiry(t *testing.T) {
	cache := NewOutputCache()
	var seconds atomic.Int64
	seconds.Store(100)
	cache.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	cache.PutFor("reader", "root", jobbatch.OutputChunk{Data: []byte("snapshot"), Next: 8})
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
		chunk, _, _, _, _ := cache.ReadWithMetadata("reader", 0)
		ready <- chunk
		<-release
		result <- string(chunk.Data)
	}()
	held := <-ready
	seconds.Store(100 + int64(OutputTTL/time.Second))
	if _, _, _, _, ok := cache.ReadWithMetadata("reader", 0); ok {
		t.Fatal("expired entry retained")
	}
	close(release)
	if got := <-result; got != "snapshot" || string(held.Data) != "snapshot" {
		t.Fatalf("reader copy changed: %q", got)
	}
}

func TestBatchOutputCacheReaderCopySurvivesConcurrentEviction(t *testing.T) {
	cache := NewOutputCache()
	cache.maxEntries = 1
	var seconds atomic.Int64
	seconds.Store(1000)
	cache.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	cache.PutFor("old", "root", jobbatch.OutputChunk{Data: []byte("old-bytes"), Next: 9})
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
		chunk, _, _, _, _ := cache.ReadWithMetadata("old", 0)
		close(ready)
		<-release
		result <- string(chunk.Data)
	}()
	<-ready
	seconds.Store(1001)
	cache.PutFor("new", "root", jobbatch.OutputChunk{Data: []byte("new"), Next: 3})
	if _, _, _, _, ok := cache.ReadWithMetadata("old", 0); ok {
		t.Fatal("old entry retained after eviction")
	}
	close(release)
	if got := <-result; got != "old-bytes" {
		t.Fatalf("reader copy changed: %q", got)
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

func TestFormatAsyncCompletionChildSessionIDEveryBranch(t *testing.T) {
	const suffix = "\n\nchild_session_id=child-123 (resume with agent(resume_session_id=child-123); inject_agent addresses a running child)"
	for _, tc := range []struct {
		name       string
		completion batchdispatch.AsyncCompletion
		want       string
	}{
		{name: "finished", want: "Async job call-1 () finished.\n\noutput"},
		{name: "failed", completion: batchdispatch.AsyncCompletion{IsError: true}, want: "Async job call-1 () failed.\n\noutput"},
		{name: "timed out", completion: batchdispatch.AsyncCompletion{TimedOut: true}, want: "Async job call-1 () timed out after 0s and was stopped. Partial output:\n\noutput"},
		{name: "stopped", completion: batchdispatch.AsyncCompletion{Stopped: true}, want: "Async job call-1 () was stopped (batch_stop). Partial output before the stop:\n\noutput"},
		{name: "cancelled", completion: batchdispatch.AsyncCompletion{Cancelled: true}, want: "Async job call-1 () was cancelled (session stopped). Partial output:\n\noutput"},
		{name: "interrupted", completion: batchdispatch.AsyncCompletion{Interrupted: true}, want: "Async job call-1 () was interrupted: its host process ended before the job finished.\n\noutput"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.completion.ToolCallID = "call-1"
			tc.completion.Content = "output"
			require.Equal(t, tc.want, FormatAsyncCompletion(tc.completion))
			tc.completion.ChildSessionID = "child-123"
			require.Equal(t, tc.want+suffix, FormatAsyncCompletion(tc.completion))
		})
	}
}
