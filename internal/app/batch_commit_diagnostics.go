package app

import (
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"modernc.org/sqlite"
)

type batchCommitDiagnostics struct {
	writer   *sql.DB
	baseline sql.DBStats
}

// snapshot never acquires a database connection or invokes an error's callbacks.
func (d batchCommitDiagnostics) snapshot(err error) []any {
	attrs := make([]any, 0, 32)
	classification := "unknown"
	if fault, ok := err.(*jobbatch.CommitFault); ok && fault != nil {
		details := fault.Details()
		attrs = append(attrs,
			"commit_kind", details.Kind, "commit_sequence", details.Sequence,
			"commit_event", details.Event, "commit_budget", details.Budget,
			"commit_elapsed", details.Elapsed, "commit_phase", details.Phase,
			"commit_phase_elapsed", details.PhaseElapsed)
		// Store SQL errors are returned directly. Do not walk arbitrary wrappers:
		// their As/Unwrap methods may block or execute host code on the fatal path.
		for _, cause := range fault.Unwrap() {
			if sqlErr, ok := cause.(*sqlite.Error); ok && sqlErr != nil {
				switch sqlErr.Code() & 0xff {
				case 5:
					classification = "sqlite_busy"
				case 6:
					classification = "sqlite_locked"
				}
			}
		}
	} else {
		attrs = append(attrs, "commit_kind", "unknown", "commit_phase", "unknown")
	}
	attrs = append(attrs, "commit_error_class", classification)
	if d.writer != nil {
		stats := d.writer.Stats()
		attrs = append(attrs,
			"pool_max_open", stats.MaxOpenConnections, "pool_open", stats.OpenConnections,
			"pool_in_use", stats.InUse, "pool_idle", stats.Idle,
			"pool_wait_count_since_runtime_start", stats.WaitCount-d.baseline.WaitCount,
			"pool_wait_duration_since_runtime_start", stats.WaitDuration-d.baseline.WaitDuration)
	}
	return attrs
}

func batchCommitSafeFaultText(err error) string {
	if fault, ok := err.(*jobbatch.CommitFault); ok && fault != nil {
		return fault.Error()
	}
	return "jobbatch: commit failed: unknown"
}
