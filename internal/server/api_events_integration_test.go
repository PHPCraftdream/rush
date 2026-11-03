package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
)

type apiEventsIntegrationCoordinator struct {
	agent.Coordinator
	app     *appPkg.App
	started chan struct{}
	release chan struct{}
}

func (c *apiEventsIntegrationCoordinator) ReserveExclusive(ctx context.Context, _ string) (context.Context, uint64, context.CancelFunc, bool) {
	reserved, cancel := context.WithCancel(ctx)
	return reserved, 1, cancel, true
}

func (*apiEventsIntegrationCoordinator) ReleaseExclusive(string, uint64, context.CancelFunc) {}
func (*apiEventsIntegrationCoordinator) ResetAutoResumeCounter(string)                       {}
func (*apiEventsIntegrationCoordinator) IsSessionBusy(string) bool                           { return false }
func (*apiEventsIntegrationCoordinator) Cancel(string)                                       {}

func (c *apiEventsIntegrationCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, _ uint64, _ context.CancelFunc, onHandoff func(), _, _ *agent.ModelOverride, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	if onHandoff != nil {
		onHandoff()
	}
	if _, err := c.app.Messages.Create(ctx, sessionID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "coordinator emitted: " + prompt}}}); err != nil {
		return nil, err
	}
	c.started <- struct{}{}
	select {
	case <-c.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type apiEventsIntegrationFrame struct {
	id    string
	type_ string
	data  json.RawMessage
}

func apiEventsIntegrationOpen(t *testing.T, client *http.Client, url, token, lastID string) (*http.Response, <-chan apiEventsIntegrationFrame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan apiEventsIntegrationFrame, 32)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(resp.Body)
		frame := apiEventsIntegrationFrame{}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				frame.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				frame.type_ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				frame.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
			case line == "":
				if frame.type_ != "" {
					frames <- frame
				}
				frame = apiEventsIntegrationFrame{}
			}
		}
	}()
	return resp, frames
}

func apiEventsIntegrationAwait(t *testing.T, frames <-chan apiEventsIntegrationFrame, wanted string) apiEventsIntegrationFrame {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("stream ended before %q event", wanted)
			}
			if frame.type_ == wanted {
				return frame
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %q event", wanted)
		}
	}
}

func TestServeAPIEventsProductionListenerRunReplayAndStop(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	coordinator := &apiEventsIntegrationCoordinator{app: app, started: make(chan struct{}, 1), release: make(chan struct{})}
	coordinator.Coordinator = nil // never depend on a host-configured coordinator: CI has none
	app.AgentCoordinator = coordinator
	sess, err := app.Sessions.Create(t.Context(), "event integration")
	if err != nil {
		t.Fatal(err)
	}
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("events-integration", []string{"read", "run"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	client := &http.Client{Timeout: 10 * time.Second}
	stream, frames := apiEventsIntegrationOpen(t, client, baseURL+"/api/v1/sessions/"+sess.ID+"/events", token, "")
	if stream.StatusCode != http.StatusOK {
		stream.Body.Close()
		t.Fatalf("stream status = %d", stream.StatusCode)
	}
	defer stream.Body.Close()

	created := apiRunIntegrationRequest(t, client, baseURL, http.MethodPost, "/api/v1/sessions/"+sess.ID+"/runs", token, `{"prompt":"integration-message"}`)
	if created.status != http.StatusAccepted {
		t.Fatalf("create run = %d %s", created.status, created.body)
	}
	var run struct {
		ID string `json:"run_id"`
	}
	if err := json.Unmarshal(created.body, &run); err != nil || run.ID == "" {
		t.Fatalf("invalid run response %s: %v", created.body, err)
	}
	running := apiEventsIntegrationAwait(t, frames, "run")
	var runningData struct {
		State APIRunState `json:"state"`
	}
	if err := json.Unmarshal(running.data, &runningData); err != nil || runningData.State != APIRunRunning {
		t.Fatalf("first run event = %s, err=%v", running.data, err)
	}
	select {
	case <-coordinator.started:
	case <-time.After(10 * time.Second):
		t.Fatal("fake coordinator did not start")
	}
	messageFrame := apiEventsIntegrationAwait(t, frames, "message")
	if !strings.Contains(string(messageFrame.data), "coordinator emitted: integration-message") {
		t.Fatalf("message event was not emitted by fake coordinator: %s", messageFrame.data)
	}
	close(coordinator.release)
	succeeded := apiEventsIntegrationAwait(t, frames, "run")
	var succeededData struct {
		State APIRunState `json:"state"`
	}
	if err := json.Unmarshal(succeeded.data, &succeededData); err != nil || succeededData.State != APIRunSucceeded {
		t.Fatalf("second run event = %s, err=%v", succeeded.data, err)
	}

	reconnected, replay := apiEventsIntegrationOpen(t, client, baseURL+"/api/v1/sessions/"+sess.ID+"/events", token, running.id)
	if reconnected.StatusCode != http.StatusOK {
		reconnected.Body.Close()
		t.Fatalf("reconnect status = %d", reconnected.StatusCode)
	}
	defer reconnected.Body.Close()
	got := apiEventsIntegrationAwait(t, replay, "message")
	if got.id != messageFrame.id {
		t.Fatalf("replayed message id %q, want %q", got.id, messageFrame.id)
	}
	got = apiEventsIntegrationAwait(t, replay, "run")
	if got.id != succeeded.id {
		t.Fatalf("replayed run id %q, want %q", got.id, succeeded.id)
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := api.Stop(stopCtx); err != nil {
		t.Fatalf("Stop did not join event feeder: %v", err)
	}
	// A publish after Stop cannot be observed by the stopped feeder.
	if _, err := app.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("post-stop-%d", time.Now().UnixNano())}}}); err != nil {
		t.Fatal(err)
	}
}

// CancelAll keeps App shutdown safe when no real coordinator is configured (CI has no providers).
func (c *apiEventsIntegrationCoordinator) CancelAll() bool { return false }
