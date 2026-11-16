package session_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/run/sessiontest"
)

func TestPumpFailureReportCarriesTimelineAndPumpState(t *testing.T) {
	tl := sessiontest.NewTestTimeline()
	tl.Stepf(func() { time.Sleep(3 * time.Millisecond) }, "fixture setup")
	tl.Addf("pump started")
	sessiontest.RegisterPumpDiag(t, tl.String)

	report := sessiontest.PumpFailureReport(t)
	for _, want := range []string{"printed only because the test failed", "timeline:", "fixture setup took", "pump started", "pump goroutines:"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// The pump tests' database fixtures must use the production pragmas; with the
// driver defaults every commit fsyncs several times and a stalled CI disk turns
// fixture setup into tens of seconds.
func TestPumpFixtureDatabaseUsesProductionPragmas(t *testing.T) {
	_, _, sqlDB := setupTestSessionWithDB(t, "pragma-pin")
	var journal string
	if err := sqlDB.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var synchronous int
	if err := sqlDB.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
}
