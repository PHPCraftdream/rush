// Pattern-handling tests for the grep tool: the compiled-regex cache,
// glob-to-regex conversion, and the cache-vs-compile benchmark.

package tools

import (
	"regexp"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegexCache(t *testing.T) {
	cache := newRegexCache()

	// Test basic caching
	pattern := "test.*pattern"
	regex1, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to compile regex: %v", err)
	}

	regex2, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to get cached regex: %v", err)
	}

	// Should be the same instance (cached)
	if regex1 != regex2 {
		t.Error("Expected cached regex to be the same instance")
	}

	// Test that it actually works
	if !regex1.MatchString("test123pattern") {
		t.Error("Regex should match test string")
	}
}

func TestRegexCacheCapacityAndEviction(t *testing.T) {
	cache := newRegexCache(2)
	first, err := cache.get("a")
	require.NoError(t, err)
	_, err = cache.get("b")
	require.NoError(t, err)
	_, err = cache.get("c")
	require.NoError(t, err)
	require.Len(t, cache.entries, 2)
	secondA, err := cache.get("a")
	require.NoError(t, err)
	require.NotSame(t, first, secondA, "the least-recently-used entry must be evicted")
}

func TestRegexCacheConcurrentSameKeyAndInvalidPattern(t *testing.T) {
	cache := newRegexCache(2)
	const callers = 32
	results := make([]*regexp.Regexp, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = cache.get("same")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range callers {
		require.NoError(t, errs[i])
		require.Same(t, results[0], results[i])
	}
	_, err := cache.get("[")
	require.Error(t, err)
	require.NotContains(t, cache.entries, "[")
}

func TestGlobToRegexCaching(t *testing.T) {
	// Test that globToRegex uses pre-compiled regex
	pattern1 := globToRegex("*.{js,ts}")

	// Should not panic and should work correctly
	regex1, err := regexp.Compile(pattern1)
	if err != nil {
		t.Fatalf("Failed to compile glob regex: %v", err)
	}

	if !regex1.MatchString("test.js") {
		t.Error("Glob regex should match .js files")
	}
	if !regex1.MatchString("test.ts") {
		t.Error("Glob regex should match .ts files")
	}
	if regex1.MatchString("test.go") {
		t.Error("Glob regex should not match .go files")
	}
}

// Benchmark to show performance improvement
func BenchmarkRegexCacheVsCompile(b *testing.B) {
	cache := newRegexCache()
	pattern := "test.*pattern.*[0-9]+"

	b.Run("WithCache", func(b *testing.B) {
		for b.Loop() {
			_, err := cache.get(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("WithoutCache", func(b *testing.B) {
		for b.Loop() {
			_, err := regexp.Compile(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
