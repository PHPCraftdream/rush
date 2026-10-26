package db

import "errors"

// ErrLegacyImportLiveHosts is returned when an old host lock is alive or unknown.
var ErrLegacyImportLiveHosts = errors.New("legacy import refused while an old Rush host is alive or unknown")

// ErrLegacyImportFutureVersion is returned when the legacy store records
// migration versions the importer-owned legacy chain does not know (the file
// was written by a newer or dev lineage), because importing it would silently
// drop unknown columns and tables.
var ErrLegacyImportFutureVersion = errors.New("legacy store contains migration versions the importer does not know")

// ErrLegacyImportUnknownTable is returned when the legacy store contains a
// table the import policy does not classify.
var ErrLegacyImportUnknownTable = errors.New("legacy store contains a table the import policy does not classify")

// ErrLegacyImportUnknownColumns is returned when the legacy store has columns
// the new store does not have.
var ErrLegacyImportUnknownColumns = errors.New("legacy store has columns the new store does not have")

// LegacyImportLiveHostsError carries the host probe detail and a typed cause.
type LegacyImportLiveHostsError struct{ Cause error }

func (e *LegacyImportLiveHostsError) Error() string {
	return "legacy import refused: " + e.Cause.Error()
}
func (e *LegacyImportLiveHostsError) Unwrap() error { return ErrLegacyImportLiveHosts }
