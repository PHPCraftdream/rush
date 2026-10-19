package sdk

import "github.com/PHPCraftdream/rush/internal/agent"

// ErrDiskProviderUnavailable is the stable re-export of the sentinel
// returned when batch work requires a process-bound disk binding that is no
// longer available (released, drained, or lost to a restart). Aliased in
// sdk (not documented as agent.*) for the same internal/-visibility reason
// as ErrSessionBusy / ErrDiskProviderNotDurable: an external consumer can
// only compile errors.Is against the sdk aliases. It never falls back to
// OS disk. Kept in its own file because sdk/sdk.go is capped at 1000 lines.
var ErrDiskProviderUnavailable = agent.ErrDiskProviderUnavailable
