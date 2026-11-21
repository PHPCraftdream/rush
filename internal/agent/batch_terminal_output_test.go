package agent

import (
	"math"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

func TestBatchOutputCacheSuffixAndCursorContinuation(t *testing.T) {
	cache := newBatchOutputCache()
	data := []byte(strings.Repeat("x", batchOutputTaskLimit+97))
	cache.putFor("t", "r", jobbatch.OutputChunk{Data: data, Next: uint64(len(data)), EOF: true})
	chunk, _, _, _, ok := cache.readWithMetadata("t", 0)
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
// (internal/shell/background_execution_output.go:48-81) for a finished
// stream with an empty drop window: next = oldest + len(content);
// start = cursor clamped into [oldest, next]; data is read in absolute
// cursor space, data[start-oldest : next-oldest]; Next = next,
// Oldest = oldest, Truncated = cursor < oldest, EOF = done.
type fakeShellReader struct {
	content []byte
	oldest  uint64
}

func (r fakeShellReader) read(cursor uint64) jobbatch.OutputChunk {
	next := r.oldest + uint64(len(r.content))
	start := cursor
	if start < r.oldest {
		start = r.oldest
	}
	if start > next {
		start = next
	}
	return jobbatch.OutputChunk{
		Data:      append([]byte(nil), r.content[start-r.oldest:next-r.oldest]...),
		Next:      next,
		Oldest:    r.oldest,
		EOF:       true,
		Truncated: cursor < r.oldest,
	}
}

// fakeArgvReader reproduces the argv live contract
// (batch_exec_argv.go:168-186 restart of cursors above MaxInt64, then
// tools/run_command_leaf.go:153-186): cursors above MaxInt64 reset to zero,
// then cursors above the retained total (oldest + len(content)) reset to
// zero — both resets happen BEFORE Truncated is computed;
// start = max(cursor, oldest); data = content[start-oldest : total-oldest];
// Next = total, EOF = true, Truncated = reset-cursor < oldest.
type fakeArgvReader struct {
	content []byte
	oldest  uint64
}

func (r fakeArgvReader) read(cursor uint64) jobbatch.OutputChunk {
	if cursor > math.MaxInt64 {
		cursor = 0
	}
	total := r.oldest + uint64(len(r.content))
	if cursor > total {
		cursor = 0
	}
	start := max(cursor, r.oldest)
	return jobbatch.OutputChunk{
		Data:      append([]byte(nil), r.content[start-r.oldest:total-r.oldest]...),
		Next:      total,
		Oldest:    r.oldest,
		EOF:       true,
		Truncated: cursor < r.oldest,
	}
}

func TestBatchOutputCacheCursorContractPerFamily(t *testing.T) {
	content := []byte("abcdefgh")
	total := uint64(len(content))
	cursors := []uint64{0, 3, total, total + 5, math.MaxInt64, uint64(1) << 63, math.MaxUint64 - 1, ^uint64(0)}
	cache := newBatchOutputCache()
	chunk := jobbatch.OutputChunk{Data: append([]byte(nil), content...), Next: total, EOF: true}
	cache.putFor("clamp", "root", chunk)
	cache.putFor("agent", "root", chunk)
	cache.putFor("shell", "root", chunk)
	cache.putForContract("argv", "root", chunk, batchOutputCursorArgvRestart)
	families := []struct {
		id   jobbatch.ID
		live func(uint64) jobbatch.OutputChunk
	}{
		{id: "clamp", live: fakeClampReader{content: content}.read},
		{id: "agent", live: fakeClampReader{content: content}.read},
		{id: "shell", live: fakeShellReader{content: content}.read},
		{id: "argv", live: fakeArgvReader{content: content}.read},
	}
	for _, fam := range families {
		for _, cursor := range cursors {
			live := fam.live(cursor)
			cached, _, _, _, ok := cache.readWithMetadata(fam.id, cursor)
			if !ok {
				t.Fatalf("%s: cache miss at cursor %d", fam.id, cursor)
			}
			if string(live.Data) != string(cached.Data) || live.Next != cached.Next || live.EOF != cached.EOF || live.Oldest != cached.Oldest || live.Truncated != cached.Truncated {
				t.Fatalf("%s: cursor %d live=(%q,%d,%v,%d,%v) cached=(%q,%d,%v,%d,%v)", fam.id, cursor, live.Data, live.Next, live.EOF, live.Oldest, live.Truncated, cached.Data, cached.Next, cached.EOF, cached.Oldest, cached.Truncated)
			}
		}
	}

	// Truncated stream: a dropped prefix (oldest 3, next 8) must read
	// identically live and cached for the clamp shell shape and the restart
	// argv shape, including Truncated.
	//
	// Expected live tuples (Data, Next, EOF, Oldest, Truncated) per cursor
	// {0, 3, 5, 8, 13}, derived from the production readers for content
	// "defgh", oldest = 3, next = total = 8:
	//
	// shell-clamp (internal/shell/background_execution_output.go:48-81,
	// finished stream): start = cursor clamped into [3, 8];
	// data = data[start-3 : 5]; Truncated = cursor < 3:
	//	{0}:   ("defgh", 8, true, 3, true)
	//	{3}:   ("defgh", 8, true, 3, false)
	//	{5}:   ("fgh",   8, true, 3, false)
	//	{8}:   ("",     8, true, 3, false)
	//	{13}:  ("",     8, true, 3, false)  // start clamped to 8
	//
	// argv-restart (batch_exec_argv.go:168-186 cursor > MaxInt64 -> 0, then
	// tools/run_command_leaf.go:153-186 cursor > total -> 0, both BEFORE
	// Truncated; start = max(cursor, 3); data = data[start-3 : 5]):
	//	{0}:   ("defgh", 8, true, 3, true)
	//	{3}:   ("defgh", 8, true, 3, false)
	//	{5}:   ("fgh",   8, true, 3, false)
	//	{8}:   ("",     8, true, 3, false)
	//	{13}:  ("defgh", 8, true, 3, true)  // 13 > 8 resets to 0, 0 < 3
	truncData := []byte("defgh")
	truncChunk := jobbatch.OutputChunk{Data: append([]byte(nil), truncData...), Oldest: 3, Next: 8, EOF: true}
	cache.putFor("shell-trunc", "root", truncChunk)
	cache.putForContract("argv-trunc", "root", truncChunk, batchOutputCursorArgvRestart)
	truncCursors := []uint64{0, 3, 5, 8, 13}
	expectedShellTrunc := []jobbatch.OutputChunk{
		{Data: []byte("defgh"), Next: 8, EOF: true, Oldest: 3, Truncated: true},
		{Data: []byte("defgh"), Next: 8, EOF: true, Oldest: 3},
		{Data: []byte("fgh"), Next: 8, EOF: true, Oldest: 3},
		{Data: []byte(nil), Next: 8, EOF: true, Oldest: 3},
		{Data: []byte(nil), Next: 8, EOF: true, Oldest: 3},
	}
	expectedArgvTrunc := []jobbatch.OutputChunk{
		{Data: []byte("defgh"), Next: 8, EOF: true, Oldest: 3, Truncated: true},
		{Data: []byte("defgh"), Next: 8, EOF: true, Oldest: 3},
		{Data: []byte("fgh"), Next: 8, EOF: true, Oldest: 3},
		{Data: []byte(nil), Next: 8, EOF: true, Oldest: 3},
		{Data: []byte("defgh"), Next: 8, EOF: true, Oldest: 3, Truncated: true},
	}
	truncFamilies := []struct {
		id       jobbatch.ID
		live     func(uint64) jobbatch.OutputChunk
		expected []jobbatch.OutputChunk
	}{
		{id: "shell-trunc", live: fakeShellReader{content: truncData, oldest: 3}.read, expected: expectedShellTrunc},
		{id: "argv-trunc", live: fakeArgvReader{content: truncData, oldest: 3}.read, expected: expectedArgvTrunc},
	}
	for _, fam := range truncFamilies {
		for i, cursor := range truncCursors {
			live := fam.live(cursor)
			want := fam.expected[i]
			if string(live.Data) != string(want.Data) || live.Next != want.Next || live.EOF != want.EOF || live.Oldest != want.Oldest || live.Truncated != want.Truncated {
				t.Fatalf("%s: cursor %d live=(%q,%d,%v,%d,%v) want=(%q,%d,%v,%d,%v)", fam.id, cursor, live.Data, live.Next, live.EOF, live.Oldest, live.Truncated, want.Data, want.Next, want.EOF, want.Oldest, want.Truncated)
			}
			cached, _, _, _, ok := cache.readWithMetadata(fam.id, cursor)
			if !ok {
				t.Fatalf("%s: cache miss at cursor %d", fam.id, cursor)
			}
			if string(live.Data) != string(cached.Data) || live.Next != cached.Next || live.EOF != cached.EOF || live.Oldest != cached.Oldest || live.Truncated != cached.Truncated {
				t.Fatalf("%s: cursor %d live=(%q,%d,%v,%d,%v) cached=(%q,%d,%v,%d,%v)", fam.id, cursor, live.Data, live.Next, live.EOF, live.Oldest, live.Truncated, cached.Data, cached.Next, cached.EOF, cached.Oldest, cached.Truncated)
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
