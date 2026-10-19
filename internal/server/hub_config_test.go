package server

import (
	"context"
	"encoding/json"
	"testing"
)

func TestBroadcastConfigCoalescesLatestWhileQueueFull(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates []string
		want    string
	}{
		{name: "single", updates: []string{"latest"}, want: "latest"},
		{name: "latest wins", updates: []string{"first", "latest"}, want: "latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub()
			client := &Client{send: make(chan []byte, 1025)}
			h.clients[client] = struct{}{}
			for range cap(h.broadcast) {
				h.broadcast <- []byte(`{"type":"ordinary"}`)
			}
			if len(h.broadcast) != cap(h.broadcast) {
				t.Fatal("ordinary queue is not full")
			}
			for _, model := range tc.updates {
				if !h.BroadcastConfig(map[string]string{"model": model}) {
					t.Fatal("config update rejected")
				}
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
			<-h.WaitDelivery()

			ordinary, configs := 0, 0
			for len(client.send) > 0 {
				raw := <-client.send
				var env WSMessage
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
				if env.Type == EventConfig {
					configs++
					var payload map[string]string
					if err := json.Unmarshal(env.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if payload["model"] != tc.want {
						t.Fatalf("config model = %q, want %q", payload["model"], tc.want)
					}
				} else if env.Type == "ordinary" {
					ordinary++
				}
			}
			if configs != 1 {
				t.Fatalf("config events = %d, want 1", configs)
			}
			if ordinary != cap(h.broadcast) {
				t.Fatalf("ordinary events = %d, want %d", ordinary, cap(h.broadcast))
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
