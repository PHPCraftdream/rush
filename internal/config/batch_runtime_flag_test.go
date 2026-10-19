// R11F round-2 (P2 m4-flag-parse): the shared RUSH_BATCH_RUNTIME parser must
// treat an unparseable value as OFF (unset/unparseable means off), and only
// the exact boolean forms flip it on. Compile-only in this worktree; CI runs
// it and it kills the `err`-dropping mutation of BatchRuntimeEnabled.
package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchRuntimeEnabledGarbageMeansOff(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "sometimes")
	require.False(t, BatchRuntimeEnabled(), "unparseable RUSH_BATCH_RUNTIME means OFF")
	t.Setenv("RUSH_BATCH_RUNTIME", "")
	require.False(t, BatchRuntimeEnabled(), "empty RUSH_BATCH_RUNTIME means OFF")
	t.Setenv("RUSH_BATCH_RUNTIME", "0")
	require.False(t, BatchRuntimeEnabled(), "explicit zero means OFF")
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	require.True(t, BatchRuntimeEnabled(), "exact boolean true means ON")
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	require.True(t, BatchRuntimeEnabled(), "exact boolean one means ON")
}
