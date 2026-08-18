// Output-shaping tests for the grep tool: multiple matches per file, column
// (charNum) reporting, and the bounded heap that keeps the newest K matches.

package tools

import (
	"container/heap"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMultipleMatchesPerFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// "world" appears on lines 1 and 3, but not line 2. Both grep
	// implementations must report every matching line, not just the first.
	content := "Hello world.\nHello.\nHello world.\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "file.txt"), []byte(content), 0o644))

	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(t.Context(), pattern, path, include)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			matches, _, err := searchWithRipgrep(t.Context(), pattern, path, include, 100)
			return matches, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("world", tempDir, "")
			require.NoError(t, err)
			require.Len(t, matches, 2, "should report both matching lines")

			lines := make([]int, len(matches))
			for i, match := range matches {
				lines[i] = match.lineNum
				require.Equal(t, 7, match.charNum)
				require.Equal(t, "Hello world.", match.lineText)
			}
			require.ElementsMatch(t, []int{1, 3}, lines)
		})
	}
}

func TestColumnMatch(t *testing.T) {
	t.Parallel()

	// Test both implementations
	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(t.Context(), pattern, path, include)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			matches, _, err := searchWithRipgrep(t.Context(), pattern, path, include, 100)
			return matches, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("THIS", "./testdata/", "")
			require.NoError(t, err)
			require.Len(t, matches, 1)
			match := matches[0]
			require.Equal(t, 2, match.lineNum)
			require.Equal(t, 14, match.charNum)
			require.Equal(t, "I wanna grep THIS particular word", match.lineText)
			require.Equal(t, "testdata/grep.txt", filepath.ToSlash(filepath.Clean(match.path)))
		})
	}
}

// TestBoundedMatchHeapRetainsNewest verifies the bounded heap keeps exactly the
// K newest matches by modTime (ties broken by earliest insertion order) when
// more than K matches are fed in.
func TestBoundedMatchHeapRetainsNewest(t *testing.T) {
	t.Parallel()
	h := &boundedMatchHeap{}
	limit := 10
	base := time.Now()

	for i := 0; i < 100; i++ {
		gm := grepMatch{
			path:    fmt.Sprintf("file_%d.txt", i),
			modTime: base.Add(time.Duration(i) * time.Minute),
			seq:     int64(i + 1),
		}
		if h.Len() < limit {
			heap.Push(h, gm)
		} else if !evictFirst(gm, (*h)[0]) {
			(*h)[0] = gm
			heap.Fix(h, 0)
		}
	}

	require.Equal(t, limit, h.Len(), "heap must never exceed limit")

	matches := []grepMatch(*h)
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].modTime.After(matches[j].modTime)
		}
		return matches[i].seq < matches[j].seq
	})

	// Should be the 10 newest (indices 90-99), sorted newest-first.
	for i, m := range matches {
		expected := 99 - i
		require.Equal(t, fmt.Sprintf("file_%d.txt", expected), m.path,
			"position %d should be file_%d", i, expected)
	}
}

// TestBoundedMatchHeapStableTiebreak verifies that when multiple matches share
// the same modTime, earlier-inserted ones (smaller seq = earlier line) survive
// eviction over later ones within the same modTime group.
func TestBoundedMatchHeapStableTiebreak(t *testing.T) {
	t.Parallel()
	h := &boundedMatchHeap{}
	limit := 2
	mt := time.Now()

	// 3 matches, same modTime, increasing seq (line order).
	for i := 0; i < 3; i++ {
		gm := grepMatch{
			path:    fmt.Sprintf("f%d", i),
			modTime: mt,
			seq:     int64(i + 1),
		}
		if h.Len() < limit {
			heap.Push(h, gm)
		} else if !evictFirst(gm, (*h)[0]) {
			(*h)[0] = gm
			heap.Fix(h, 0)
		}
	}

	require.Equal(t, limit, h.Len())
	// Should keep seq 1 and 2 (earliest), NOT seq 3.
	seqs := map[int64]bool{}
	for _, m := range *h {
		seqs[m.seq] = true
	}
	require.True(t, seqs[1], "seq=1 must survive (earliest line)")
	require.True(t, seqs[2], "seq=2 must survive")
	require.False(t, seqs[3], "seq=3 must be evicted (latest line, same modTime)")
}
