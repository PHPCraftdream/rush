package session_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

func pumpQueueSnapshot(svc session.Service, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	began := time.Now()
	e, err := svc.GetRunQueueEntry(ctx, id)
	if err != nil {
		return fmt.Sprintf("queue snapshot: read error=%v elapsed=%s\n", err, time.Since(began))
	}
	if e == nil {
		return "queue snapshot: row absent\n"
	}
	return fmt.Sprintf("queue snapshot: status=%s attempts=%d leasedBy=%q leasedAt=%d leaseExpiresAt=%d terminal=%v\n",
		e.Status, e.Attempts, e.LeasedBy, e.LeasedAt, e.LeaseExpiresAt, e.TerminalFailure)
}

func registerPumpQueueReport(t *testing.T, tl *testTimeline, svc session.Service, id string) func(context.Context) (*session.RunQueueEntry, error) {
	registerPumpDiag(t, func() string { return pumpQueueSnapshot(svc, id) })
	return func(ctx context.Context) (*session.RunQueueEntry, error) {
		began := time.Now()
		e, err := svc.GetRunQueueEntry(ctx, id)
		state := "absent"
		if e != nil {
			state = fmt.Sprintf("status=%s attempts=%d leaseExpiresAt=%d", e.Status, e.Attempts, e.LeaseExpiresAt)
		}
		if err != nil {
			tl.observef("queue read error", "err=%v ctx=%v elapsed=%s", err, ctx.Err(), time.Since(began))
		}
		tl.observef("queue read", "%s err=%v ctx=%v elapsed=%s", state, err, ctx.Err(), time.Since(began))
		return e, err
	}
}

type pumpReportSink struct {
	failed bool
	logs   []any
}

func (s *pumpReportSink) Failed() bool    { return s.failed }
func (s *pumpReportSink) Log(args ...any) { s.logs = append(s.logs, args...) }

func TestPumpReportOnlyOnFailure(t *testing.T) {
	tl := newTestTimeline()
	tl.addf("fixture setup started")
	tl.observef("queue read", "first observation")
	tl.observef("queue read", "last read error")
	sink := &pumpReportSink{}
	calls := 0
	render := func() string { calls++; return tl.String() }
	tl.report(sink, render)
	if calls != 0 || len(sink.logs) != 0 {
		t.Fatal("successful test emitted or collected diagnostics")
	}
	sink.failed = true
	tl.report(sink, render)
	tl.report(sink, render)
	if calls != 1 || len(sink.logs) != 1 {
		t.Fatalf("failure report calls=%d logs=%d, want one", calls, len(sink.logs))
	}
	report := fmt.Sprint(sink.logs...)
	for _, want := range []string{"fixture setup started", "last read error"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(report, "first observation") {
		t.Fatal("polling history was not bounded")
	}
}

type queueReportService struct {
	session.Service
	entry *session.RunQueueEntry
	err   error
}

func (s queueReportService) GetRunQueueEntry(context.Context, string) (*session.RunQueueEntry, error) {
	return s.entry, s.err
}

func TestPumpQueueReportRetainsReadErrors(t *testing.T) {
	tl := newTestTimeline()
	registerPumpDiag(t, tl.String)
	svc := &queueReportService{err: errors.New("transient probe error")}
	getEntry := registerPumpQueueReport(t, tl, svc, "probe")
	_, _ = getEntry(t.Context())
	svc.err = nil
	svc.entry = &session.RunQueueEntry{Status: "pending"}
	_, _ = getEntry(t.Context())
	report := pumpFailureReport(t)
	for _, want := range []string{"queue read error:", "transient probe error", "queue snapshot: status=pending attempts=0"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q: %s", want, report)
		}
	}
}

func TestPumpQueueSnapshotBeforeFirstLeaseAndOnReadError(t *testing.T) {
	report := pumpQueueSnapshot(queueReportService{entry: &session.RunQueueEntry{Status: "pending", Attempts: 0}}, "probe")
	if !strings.Contains(report, "status=pending attempts=0") {
		t.Fatalf("pending row missing: %s", report)
	}
	report = pumpQueueSnapshot(queueReportService{err: errors.New("diagnostic read failed")}, "probe")
	if !strings.Contains(report, "read error=diagnostic read failed") {
		t.Fatalf("read error missing: %s", report)
	}
}
