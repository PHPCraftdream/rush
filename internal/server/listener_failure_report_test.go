package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

func listenerSnapshot(s *Server) string {
	counts := make(map[http.ConnState]int)
	s.connMu.Lock()
	for _, state := range s.connections {
		counts[state]++
	}
	s.connMu.Unlock()
	return fmt.Sprintf("connections: new=%d active=%d idle=%d hijacked=%d closed=%d\n",
		counts[http.StateNew], counts[http.StateActive], counts[http.StateIdle], counts[http.StateHijacked], counts[http.StateClosed])
}

func reportListenerFailure(t interface {
	Failed() bool
	Log(...any)
}, tl *panicTimeline, snapshot func() string) {
	if !t.Failed() {
		return
	}
	tl.mu.Lock()
	if tl.reported {
		tl.mu.Unlock()
		return
	}
	tl.reported = true
	tl.mu.Unlock()
	t.Log("listener failure report:\n" + tl.String() + "\n" + snapshot() + shutdownStacks())
}

func TestListenerReportOnlyOnFailure(t *testing.T) {
	tl := &panicTimeline{}
	tl.add("Start entered")
	tl.add("cancel requested")
	sink := &panicReportSink{}
	calls := 0
	snapshot := func() string { calls++; return "connections: new=1\n" }
	reportListenerFailure(sink, tl, snapshot)
	if calls != 0 || len(sink.logs) != 0 {
		t.Fatal("successful test emitted or collected listener diagnostics")
	}
	sink.failed = true
	reportListenerFailure(sink, tl, snapshot)
	if calls != 1 {
		t.Fatalf("snapshot calls=%d, want 1", calls)
	}
	report := fmt.Sprint(sink.logs...)
	for _, want := range []string{"listener failure report:", "Start entered", "cancel requested", "connections: new=1"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

func TestListenerSnapshotCountsConnectionStates(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s := &Server{connections: map[net.Conn]http.ConnState{a: http.StateNew, b: http.StateActive}}
	if report := listenerSnapshot(s); !strings.Contains(report, "new=1 active=1 idle=0") {
		t.Fatalf("connection states missing: %s", report)
	}
}
