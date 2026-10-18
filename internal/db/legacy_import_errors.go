package db

import "errors"

// ErrLegacyImportLiveHosts is returned when an old host lock is alive or unknown.
var ErrLegacyImportLiveHosts = errors.New("legacy import refused while an old Rush host is alive or unknown")

// LegacyImportLiveHostsError carries the host probe detail and a typed cause.
type LegacyImportLiveHostsError struct{ Cause error }

func (e *LegacyImportLiveHostsError) Error() string {
	return "legacy import refused: " + e.Cause.Error()
}
func (e *LegacyImportLiveHostsError) Unwrap() error { return ErrLegacyImportLiveHosts }
