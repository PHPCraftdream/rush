package agent

import (
	"testing"
	"time"
)

// TestSetCoordinatorDiskCapabilityManagerWaitsForInFlightToolBuilds pins the
// ordering that removes a data race found by -race (TestHandleCancelWakeSchedule
// in internal/server): the asynchronous tool builds started at construction
// read coordinator.diskCaps without a lock, so the binding must not write it
// while one is pending. A regression returns from the bind at once, the
// "bound while a build was pending" assertion fails, and under -race the
// detector reports the unsynchronised read/write pair.
func TestSetCoordinatorDiskCapabilityManagerWaitsForInFlightToolBuilds(t *testing.T) {
	coord := &coordinator{}
	caps := newDiskCapabilityManager()
	started, release := make(chan struct{}), make(chan struct{})
	seen := make(chan *DiskProviderRegistry, 1)
	coord.readyWg.Go(func() error {
		close(started)
		<-release
		seen <- coord.diskCaps // the unlocked read of an in-flight build
		return nil
	})
	<-started

	bound := make(chan error, 1)
	go func() { bound <- SetCoordinatorDiskCapabilityManager(coord, caps) }()

	select {
	case <-bound:
		t.Fatal("the capability manager was bound while a tool build was still pending")
	case <-time.After(200 * time.Millisecond): // a correct bind blocks; waiting longer only delays a pass
	}
	close(release)
	if err := <-bound; err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != nil {
		t.Fatalf("the pending build observed the new manager %p: the write overtook it", got)
	}
	if coord.diskCaps != caps {
		t.Fatal("the manager was not bound after the pending build finished")
	}
}
