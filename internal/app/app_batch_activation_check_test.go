// Batch activation acceptance. After the H4 atomic activation every
// RUSH_BATCH_RUNTIME value selects the batch runtime: unset, empty,
// false-like ("0", "false", "off") and unparseable values included. This
// test is the shadow workflow's activation anchor
// (.github/workflows/job-batches-shadow.yml, shadow-aw-acceptance job),
// which fails if the test is missing, renamed, skipped, or not passing.
package app

import (
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBatchRuntimeSelectionAllValuesSelectBatch(t *testing.T) {
	prev, had := os.LookupEnv("RUSH_BATCH_RUNTIME")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("RUSH_BATCH_RUNTIME", prev)
		} else {
			_ = os.Unsetenv("RUSH_BATCH_RUNTIME")
		}
	})
	// Deliberately not t.Parallel: the test mutates the process environment,
	// which must not race other parallel tests that read it.
	values := []string{"", "0", "f", "false", "FALSE", "False", "off", "OFF", "garbage", "1", "t", "true", "TRUE", "True", "on", "2"}
	for _, value := range values {
		require.NoError(t, os.Setenv("RUSH_BATCH_RUNTIME", value))
		require.True(t, config.BatchRuntimeEnabled(), "RUSH_BATCH_RUNTIME=%q must select the batch runtime", value)
	}
	require.NoError(t, os.Unsetenv("RUSH_BATCH_RUNTIME"))
	require.True(t, config.BatchRuntimeEnabled(), "an unset RUSH_BATCH_RUNTIME must select the batch runtime")
}
