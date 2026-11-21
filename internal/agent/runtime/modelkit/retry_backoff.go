package modelkit

import "time"

// StreamStallRetryBaseBackoff is the first retry's backoff (10s in
// production). A var, not a const, so tests can shrink it instead of
// sleeping through the real delay.
var StreamStallRetryBaseBackoff = 10 * time.Second
