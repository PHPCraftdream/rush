// Opt-in default deadline for background bash/run_command jobs that pass no
// explicit timeout: resolved from config + per-call CallOptions, mirroring
// supervision.go's resolveSupervisionConfig precedence.
package agent

import (
	"context"
	"time"
)

// resolveBackgroundJobDefaultTimeout returns the effective default
// terminate_and_wake deadline for a background bash/run_command call: the
// run's CallOptions override (`rush run --job-timeout`) when positive,
// otherwise config's background_job_default_timeout_seconds, otherwise zero
// (off). The 5-second timeoutSecondsFloor does NOT apply here: this is an
// operator-configured value, not a model-supplied one.
func (c *coordinator) resolveBackgroundJobDefaultTimeout(ctx context.Context) time.Duration {
	if callOpts := callOptionsFrom(ctx); callOpts != nil && callOpts.BackgroundJobDefaultTimeout > 0 {
		return callOpts.BackgroundJobDefaultTimeout
	}
	if c != nil && c.cfg != nil {
		if opts := c.cfg.Config().Options; opts != nil && opts.BackgroundJobDefaultTimeoutSeconds > 0 {
			return time.Duration(opts.BackgroundJobDefaultTimeoutSeconds) * time.Second
		}
	}
	return 0
}
