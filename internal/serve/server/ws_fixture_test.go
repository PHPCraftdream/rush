package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newTestClient builds a Client detached from any real connection. Both
// c.reply and hub.Broadcast are non-blocking (select/default), so no hub
// event loop needs to be running for a handler to complete.
func newTestClient() *Client {
	c := newClient(newHub(), nil)
	c.ResizeSendChan(8)
	return c
}

// panicTimeline tracks package-local test diagnostics without changing test logic.
type panicTimeline struct {
	mu       sync.Mutex
	start    time.Time
	events   []string
	reported bool
	snapshot func() string
}

func (tl *panicTimeline) add(event string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.start.IsZero() {
		tl.start = time.Now()
	}
	tl.events = append(tl.events, fmt.Sprintf("+%s %s", time.Since(tl.start).Round(time.Millisecond), event))
}

func (tl *panicTimeline) String() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return strings.Join(tl.events, "\n")
}

type panicReportSink struct {
	failed bool
	logs   []any
}

func (s *panicReportSink) Failed() bool    { return s.failed }
func (s *panicReportSink) Log(args ...any) { s.logs = append(s.logs, args...) }

func callSetSessionModels(t *testing.T, ctx context.Context, a *appPkg.App, reqID string, payload SetSessionModelsPayload) *WSMessage {
	t.Helper()
	client := newTestClient()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, client, WSMessage{ID: reqID, Type: CmdSetSessionModels, Payload: raw})
	for {
		select {
		case raw := <-client.SendChan():
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.ID != reqID {
				continue
			}
			return &env
		default:
			require.FailNowf(t, "handler must reply to the request", "no reply with ID %s queued", reqID)
			return nil
		}
	}
}

func replyByID(t *testing.T, c *Client, id string) WSMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case raw := <-c.SendChan():
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.ID == id {
				return env
			}
		case <-time.After(50 * time.Millisecond):
			require.True(t, time.Now().Before(deadline), "no reply with id %q arrived", id)
		}
	}
}

// The test double blocks until its caller releases it; rerun stop is unused.
func (*blockingCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func addTestProvider(t *testing.T, a *appPkg.App) {
	t.Helper()
	payload, err := json.Marshal(AddCustomProviderPayload{
		ID:      "acme",
		BaseURL: "https://acme.example.invalid/v1",
		Type:    "openai-compat",
		Models: []CustomModelPayload{
			{ID: "acme-large-v1", Name: "Acme Large v1", ContextWindow: 128000},
			{ID: "small-global", Name: "Small Global", ContextWindow: 32000},
			{ID: "small-workspace", Name: "Small Workspace", ContextWindow: 32000},
			{ID: "large-global", Name: "Large Global", ContextWindow: 128000},
			{ID: "large-workspace", Name: "Large Workspace", ContextWindow: 128000},
		},
	})
	require.NoError(t, err)
	client := newTestClient()
	handleAddCustomProvider(a, client, WSMessage{ID: "seed", Type: CmdAddCustomProvider, Payload: payload})
}
