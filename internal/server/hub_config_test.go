package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestBroadcastConfigCoalescesLatestWhileQueueFull(t *testing.T) {
	for _, tc := range []struct {
		name         string
		updates      []string
		want         string
		genericOld   bool
		fullQueue    bool
		wantOrdinary int
	}{
		{name: "single", updates: []string{"latest"}, want: "latest", fullQueue: true, wantOrdinary: 1024},
		{name: "latest wins", updates: []string{"first", "latest"}, want: "latest", fullQueue: true, wantOrdinary: 1024},
		{name: "generic then specialized", updates: []string{"old", "latest"}, want: "latest", genericOld: true, fullQueue: true, wantOrdinary: 1024},
		{name: "generic then specialized with space", updates: []string{"old", "latest"}, want: "latest", genericOld: true, wantOrdinary: 0},
		{name: "specialized then generic", updates: []string{"old", "latest"}, want: "latest", fullQueue: true, wantOrdinary: 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub()
			client := &Client{send: make(chan []byte, 1025)}
			h.clients[client] = struct{}{}
			if tc.fullQueue {
				for range cap(h.broadcast) {
					h.broadcast <- []byte(`{"type":"ordinary"}`)
				}
			}
			if len(h.broadcast) != tc.wantOrdinary {
				t.Fatalf("ordinary queue length = %d, want %d", len(h.broadcast), tc.wantOrdinary)
			}
			for _, model := range tc.updates {
				generic := (tc.genericOld && model == "old") || (!tc.genericOld && tc.name == "specialized then generic" && model == "latest")
				if generic {
					h.Broadcast(EventConfig, map[string]string{"model": model})
				} else if !h.BroadcastConfig(map[string]string{"model": model}) {
					t.Fatal("config update rejected")
				}
			}
			if tc.genericOld && !tc.fullQueue && len(h.broadcast) != 0 {
				t.Fatalf("generic config entered ordinary queue: len = %d", len(h.broadcast))
			}
			if len(h.configBroadcast) != 1 {
				t.Fatalf("config wakeups = %d, want 1", len(h.configBroadcast))
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { h.Run(ctx); close(done) }()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			select {
			case <-h.WaitDelivery():
			case <-time.After(5 * time.Second):
				t.Fatal("delivery barrier timed out")
			}

			ordinary, configs := 0, 0
			for len(client.send) > 0 {
				raw := <-client.send
				var env WSMessage
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
				switch env.Type {
				case EventConfig:
					configs++
					var payload map[string]string
					if err := json.Unmarshal(env.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if payload["model"] != tc.want {
						t.Fatalf("config model = %q, want %q", payload["model"], tc.want)
					}
				case "ordinary":
					ordinary++
				}
			}
			if configs != 1 {
				t.Fatalf("config events = %d, want 1", configs)
			}
			if ordinary != tc.wantOrdinary {
				t.Fatalf("ordinary events = %d, want %d", ordinary, tc.wantOrdinary)
			}
		})
	}
}

func TestBroadcastConfigRejectsInvalidAndStoppedHub(t *testing.T) {
	h := newHub()
	if h.BroadcastConfig(make(chan int)) {
		t.Fatal("unsupported payload accepted")
	}
	close(h.stopped)
	if h.BroadcastConfig(map[string]string{"model": "stale"}) {
		t.Fatal("stopped hub accepted config")
	}
}
