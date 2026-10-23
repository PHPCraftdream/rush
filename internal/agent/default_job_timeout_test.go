// Opt-in default background-job timeout (item 5): default termination of
// bash/run_command jobs that pass no explicit timeout, its precedence and
// its blast radius.
//
// Revert-check — the one surviving test catches exactly one production
// line; revert it and the test must fail:
//   - TestDefaultJobTimeout_CallOptionsWinOverConfig:
//     default_job_timeout.go's CallOptions-first precedence over config
//     Options (the arming half of the old map was deleted with the wrapper
//     and the ledger deadline machinery).
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// The deleted helpers defaultTimeoutLedger/barrier/terminalOf belonged to the
// deleted ledger timeout tests (see below): the workLedger fixture, the
// durable async_jobs store and the jobOf/terminal-state readbacks are gone.
// The batch world arms per-job timeouts in the jobbatch launch spec (the tool
// layer); only the resolution precedence below survives here.

// TestDefaultJobTimeout_BashNoExplicitTimeoutEndsTimedOut,
// TestDefaultJobTimeout_ExplicitTimeoutBeatsDefault,
// TestDefaultJobTimeout_NoDefaultArmsNoDeadline and
// TestDefaultJobTimeout_DelegationUntouchedByDefault were DELETEd as
// obsolete contracts: their premise was the deleted asyncTool wrapper arming
// per-job terminate_and_wake deadlines on the workLedger (job.deadline /
// job.timeoutKind / phaseTimedOut + the timeout service's kill path). In the
// batch world the per-job timeout is carried in the jobbatch launch spec by
// the tool layer (agent/tools), and only the RESOLUTION precedence survives
// here (the test below).

// TestDefaultJobTimeout_CallOptionsWinOverConfig: the run's CallOptions
// (`--job-timeout`) wins over config's
// background_job_default_timeout_seconds, which wins over "off".
func TestDefaultJobTimeout_CallOptionsWinOverConfig(t *testing.T) {
	// Not parallel: isolateAllGlobalConfigPaths uses t.Setenv.
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfg.Config().Options.BackgroundJobDefaultTimeoutSeconds = 30
	coord := &coordinator{cfg: cfg}

	plain := context.Background()
	require.Equal(t, 30*time.Second, coord.resolveBackgroundJobDefaultTimeout(plain),
		"config alone must supply the default")

	perCall := WithCallOptions(plain, &CallOptions{BackgroundJobDefaultTimeout: 2 * time.Second})
	require.Equal(t, 2*time.Second, coord.resolveBackgroundJobDefaultTimeout(perCall),
		"CallOptions must win over config Options")
}
