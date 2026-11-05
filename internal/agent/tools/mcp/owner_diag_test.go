package mcp

import (
	"bytes"
	"context"
	"fmt"
	"runtime/pprof"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The owner lifecycle tests race Close, renewals and init barriers against one
// another, and each has failed once on a CI runner with only an assertion
// message ("Close coordinator must cancel the owner lifecycle", a session left
// in the registry). logOwnerOnFailure prints the owner's flags and counters,
// the shared registry and the stacks of the goroutines inside the owner, taken
// at the instant the test failed, before the deferred cleanups unwind.

// describeOwnerForTest renders o and the shared registry. It never blocks: the
// lifecycle lock is taken with a short deadline, because the failure may be a
// Close or renewal path stuck while holding it.
func describeOwnerForTest(o *Owner, names ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== mcp owner state ===\n")
	fmt.Fprintf(&b, "closeRequested=%v lifecycleCtx.Err()=%v\n", o.closeRequested.Load(), o.lifecycleCtx.Err())
	closed := "no"
	select {
	case <-o.closeDone:
		closed = "yes"
	default:
	}
	fmt.Fprintf(&b, "closeDone closed=%s\n", closed)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if lifecycleMu.LockContext(ctx, false) {
		fmt.Fprintf(&b, "locked view: current=%v closing=%v standalone=%v implicit=%v generation=%d initCount=%d fullInitCount=%d trackedSessions=%d closeErr=%v standaloneOwners=%d\n",
			o.isCurrentLocked(), o.closing, o.standalone, o.implicit, o.generation, o.initCount, o.fullInitCount, len(o.trackedSessions), o.closeErr, len(standaloneOwners))
		lifecycleMu.RUnlock()
	} else {
		b.WriteString("lifecycleMu not acquired within 2s: a Close, renewal or init path is holding it\n")
	}

	var registered []string
	for name := range sessions.Seq2() {
		registered = append(registered, name)
	}
	sort.Strings(registered)
	fmt.Fprintf(&b, "registry sessions: %v\n", registered)
	for _, name := range names {
		info, ok := GetState(name)
		fmt.Fprintf(&b, "state[%s]: present=%v %+v\n", name, ok, info)
	}

	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var keep []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "mcp.(*Owner)") || strings.Contains(g, "contextlock.") {
			keep = append(keep, g)
		}
	}
	dump := strings.Join(keep, "\n\n")
	if len(dump) > 12000 {
		dump = dump[:12000] + "\n...(truncated)"
	}
	fmt.Fprintf(&b, "goroutines inside the owner: %d\n%s\n", len(keep), dump)
	return b.String()
}

// logOwnerOnFailure registers the dump to run first on cleanup, only when the
// test has failed.
func logOwnerOnFailure(t *testing.T, o *Owner, names ...string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(describeOwnerForTest(o, names...))
		}
	})
}

func TestDescribeOwnerForTestReportsFlagsRegistryAndStacks(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(context.Background()) })

	report := describeOwnerForTest(owner, "not-there")
	require.Contains(t, report, "closeRequested=false")
	require.Contains(t, report, "lifecycleCtx.Err()=<nil>")
	require.Contains(t, report, "closeDone closed=no")
	require.Contains(t, report, "locked view: current=true closing=false")
	require.Contains(t, report, "registry sessions:")
	require.Contains(t, report, "state[not-there]: present=false")
	require.Contains(t, report, "goroutines inside the owner:")

	require.NoError(t, owner.Close(context.Background()))
	after := describeOwnerForTest(owner)
	require.Contains(t, after, "closeRequested=true")
	require.Contains(t, after, "closeDone closed=yes")
}
