package server

import "testing"

func TestForwardConfigSnapshotCounters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stopped   bool
		valid     bool
		fullQueue bool
		wantQueue int64
	}{
		{name: "accepted", valid: true, wantQueue: 1},
		{name: "accepted with full ordinary queue", valid: true, wantQueue: 1, fullQueue: true},
		{name: "snapshot unavailable", valid: false},
		{name: "stopped", stopped: true, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeProcessed := configNotificationsProcessed.Load()
			beforeEnqueued := configNotificationsEnqueued.Load()
			h := newHub()
			if tc.fullQueue {
				for range cap(h.broadcast) {
					h.broadcast <- []byte(`ordinary`)
				}
			}
			if tc.stopped {
				close(h.stopped)
			}
			wire := ConfigWire{}
			forwardConfigSnapshot(h, wire, tc.valid)
			if got := configNotificationsProcessed.Load() - beforeProcessed; got != 1 {
				t.Fatalf("processed delta = %d, want 1", got)
			}
			if got := configNotificationsEnqueued.Load() - beforeEnqueued; got != tc.wantQueue {
				t.Fatalf("enqueued delta = %d, want %d", got, tc.wantQueue)
			}
		})
	}
}
