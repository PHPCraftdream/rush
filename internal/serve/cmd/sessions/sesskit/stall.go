package sesskit

import "time"

// StallPulseThreshold is the heartbeat age past which a live-holder session
// is reported as STALLED (shared by the root `sessions why` stall header and
// the sesslocks `sessions locks` STALLED marker).
const StallPulseThreshold = 10 * time.Minute
