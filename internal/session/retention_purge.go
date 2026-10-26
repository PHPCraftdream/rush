// Retention-purge constants (R12-H1 extraction): `sessions gc
// --jobs-older-than` support and the fixed retention age survive the H4
// session old-store deletion. batch_retention.go applies the same age to the
// batch_nodes/session_notices retention pass (batch retention is the
// selected execution authority); the async_jobs-specific purge helpers of
// the old store were deleted with it.
package session

import "time"

// AsyncDataRetentionAge bounds how long a terminal, delivered-or-voided
// session_notices/batch row (and an empty dead host's lock file) survives
// before the retention pass reaps it: the 60s pass of the web process and of
// a `rush run` loop (ClaimExternalDriver starts the ticker; the loop also
// runs it once at start). Fixed, not a config option -- the doc's own tests
// only require "old rows are purged, recent ones are kept", not a tunable
// knob.
const AsyncDataRetentionAge = 7 * 24 * time.Hour
