package app

import "time"

// reviewerPassTimeout bounds one automatic reviewer pass on its own wall
// clock. It is a hang-guard, NOT a cap on how much the reviewer may check:
// the review turn reads as much as it needs to be confident in its verdict,
// and it already runs under the run's own context (--timeout, the 6h
// default cap). This ceiling only stops a reviewer that never finishes from
// holding the process open forever; a reviewer that legitimately needs the
// full hour is not interrupted.
const reviewerPassTimeout = 60 * time.Minute
