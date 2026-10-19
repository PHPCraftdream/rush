package agent

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// Goroutine-leak checks compare goroutine IDENTITIES, never the process-wide
// runtime.NumGoroutine(): the count also moves with unrelated goroutines of
// earlier tests and of the runtime finishing (CI saw before=52 after=53 with
// nothing leaked). A goroutine that did not exist in the "before" snapshot,
// whose stack names the code under test, and that is still alive after a
// bounded wait is a leak; transient goroutines of the code under test simply
// exit within the wait.

// allGoroutineStacks returns every live goroutine's stack keyed by its id.
func allGoroutineStacks() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[string]string{}
	for _, block := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(block, "\n")
		fields := strings.Fields(header) // "goroutine 123 [state]:"
		if len(fields) >= 2 && fields[0] == "goroutine" {
			out[fields[1]] = block
		}
	}
	return out
}

// goroutineIDSnapshot records the ids of the goroutines alive now.
func goroutineIDSnapshot() map[string]struct{} {
	ids := map[string]struct{}{}
	for id := range allGoroutineStacks() {
		ids[id] = struct{}{}
	}
	return ids
}

// requireNoNewGoroutinesNaming fails unless, within wait, no goroutine absent
// from before has a stack containing any of the given substrings.
func requireNoNewGoroutinesNaming(t *testing.T, before map[string]struct{}, wait time.Duration, msg string, needles ...string) {
	t.Helper()
	var leaked []string
	for deadline := time.Now().Add(wait); ; {
		leaked = leaked[:0]
		for id, stack := range allGoroutineStacks() {
			if _, existed := before[id]; existed {
				continue
			}
			for _, needle := range needles {
				if strings.Contains(stack, needle) {
					leaked = append(leaked, stack)
					break
				}
			}
		}
		if len(leaked) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s; new goroutines:\n%s", msg, strings.Join(leaked, "\n\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
