package cmdtest

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// WaitOrDump waits for ch or the deadline. On deadline it dumps ALL goroutine
// stacks into the test log — so a hang becomes a fast red test with a full
// stack in the -v output instead of a package-level -timeout with a lost
// stack (task #1148) — and returns an error naming what was awaited.
// Copy of waitOrDump (testwait_test.go); lane K3 deletes the original.
func WaitOrDump(t *testing.T, ch <-chan struct{}, d time.Duration, what string) error {
	t.Helper()
	select {
	case <-ch:
		return nil
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Logf("goroutine dump after %s waiting for %s:\n%s", d, what, buf[:n])
		return fmt.Errorf("timed out after %s waiting for %s", d, what)
	}
}

// ResetCommandContexts walks the command tree rooted at root and clears the
// context Cobra cached on each command during an earlier Execute, so the
// next Execute inherits the root command's context. Generalized from
// resetRunCmdContext (run_custom_role_dispatch_test.go); lane K3 deletes
// the original.
func ResetCommandContexts(root *cobra.Command) {
	root.SetContext(nil) //nolint:staticcheck // nil is the pristine state Cobra checks before inheriting the parent context.
	for _, c := range root.Commands() {
		ResetCommandContexts(c)
	}
}
