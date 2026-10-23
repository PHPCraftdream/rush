// R12-H4: the batch runtime is unconditional, so EVERY RUSH_BATCH_RUNTIME
// value - unset, empty, boolean-off forms, garbage, boolean-on forms -
// selects batch. This kills any resurrection of a parse-based selector
// (the old OFF-for-unset/garbage behavior) and pins the flag-erasure
// contract lane R12-I inherits.
package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchRuntimeEnabledAllValuesSelectBatch(t *testing.T) {
	for _, value := range []string{"", "0", "false", "off", "sometimes", "1", "true"} {
		t.Setenv("RUSH_BATCH_RUNTIME", value)
		require.True(t, BatchRuntimeEnabled(),
			"RUSH_BATCH_RUNTIME=%q must select the batch runtime", value)
	}
}

func TestBatchRuntimeEnabledWhenGenuinelyUnset(t *testing.T) {
	// t.Setenv registers the restore of the inherited state; the
	// Unsetenv then makes the variable truly absent for this test.
	t.Setenv("RUSH_BATCH_RUNTIME", "")
	require.NoError(t, os.Unsetenv("RUSH_BATCH_RUNTIME"))
	_, present := os.LookupEnv("RUSH_BATCH_RUNTIME")
	require.False(t, present, "fixture must leave the variable absent")
	require.True(t, BatchRuntimeEnabled(), "unset RUSH_BATCH_RUNTIME selects batch")
}
