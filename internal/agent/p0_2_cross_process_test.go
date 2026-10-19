package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

var errP02CrossProcessEnqueueFailure = errors.New("p0-2 test: simulated enqueue failure")

type p02CrossProcessEnqueueFailingSessions struct {
	session.Service
}

func (s *p02CrossProcessEnqueueFailingSessions) EnqueueRunQueueEntry(context.Context, string, string, []byte) error {
	return errP02CrossProcessEnqueueFailure
}

type p02CrossProcessPumpCoordinator struct {
	inner *pumpTestCoordinator
	done  chan error
}

func (c *p02CrossProcessPumpCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	result, err := c.inner.Run(ctx, callData)
	select {
	case c.done <- err:
	default:
	}
	return result, err
}

func TestP0_2_CrossProcessInterrupt_RowRecreatedOnFailure(t *testing.T) {
	t.Parallel()

	var providerCalls atomic.Int64
	busyLockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(busyLockSrv.Close)

	provider, err := openaicompat.New(openaicompat.WithBaseURL(busyLockSrv.URL), openaicompat.WithAPIKey("probe"))
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	model := Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}}

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true,
	}).(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p0-2-cross-process-interrupt-test")
	require.NoError(t, err)
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "interrupted message"}},
	})
	require.NoError(t, err)
	inject := session.PendingInject{
		ID: "test-inject-id-recreate-" + sess.ID, SessionID: sess.ID,
		MessageID: msg.ID, Content: msg.FullText(), Interrupt: true,
	}
	require.NoError(t, env.sessions.CreatePendingInject(ctx, inject))
	call := SessionAgentCall{SessionID: sess.ID, Prompt: msg.FullText(), ExistingMessageID: msg.ID, InjectID: inject.ID}

	failingSessions := &p02CrossProcessEnqueueFailingSessions{Service: env.sessions}
	coord := &coordinator{cfg: &config.ConfigStore{}, sessions: failingSessions, messages: env.messages, currentAgent: sa}
	require.ErrorIs(t, coord.startDetachedRun(ctx, call), errP02CrossProcessEnqueueFailure)
	pi, err := env.sessions.PeekInterruptInject(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pi, "enqueue failure must durably restore the consumed interrupt inject")
	require.Equal(t, sess.ID, pi.SessionID)
	require.Equal(t, msg.ID, pi.MessageID)
	require.Equal(t, msg.FullText(), pi.Content)
	require.True(t, pi.Interrupt)
	require.NotEqual(t, inject.ID, pi.ID, "recreation must restore a replacement row")

	// The restored inject must itself be consumed into durable work.
	coord.sessions = env.sessions
	require.NoError(t, coord.startDetachedRun(ctx, SessionAgentCall{
		SessionID: sess.ID, Prompt: pi.Content, ExistingMessageID: pi.MessageID, InjectID: pi.ID,
	}))
	piAfterConsume, err := env.sessions.PeekInterruptInject(ctx, sess.ID)
	require.NoError(t, err)
	require.Nil(t, piAfterConsume, "restored inject must be consumed into durable work")
	pendingEntries, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingEntries, 1)

	done := make(chan error, 1)
	mockCoord := &p02CrossProcessPumpCoordinator{
		inner: &pumpTestCoordinator{sessionAgent: sa, dataDir: env.workingDir},
		done:  done,
	}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions: env.sessions, Coordinator: mockCoord,
		PumpInstanceID: "test-pump-p0-2-cross",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	stopRunQueuePumpForTest(t, pump)
	t.Cleanup(func() {
		sa.CancelAll()
		sa.runWg.Wait()
	})
	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(10 * time.Second):
		t.Fatal("pump coordinator did not complete")
	}
	require.Eventually(t, func() bool {
		pending, checkErr := env.sessions.ListPendingRunQueueEntries(ctx)
		if checkErr != nil || len(pending) != 0 {
			return false
		}
		leased, checkErr := env.sessions.ListStaleLeasedRunQueueEntries(ctx, int64(^uint64(0)>>1))
		return checkErr == nil && len(leased) == 0 && providerCalls.Load() >= 1
	}, 10*time.Second, 100*time.Millisecond, "pump should execute the restored inject work")
	require.GreaterOrEqual(t, providerCalls.Load(), int64(1))
	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	found := false
	for _, persisted := range msgs {
		for _, part := range persisted.Parts {
			if text, ok := part.(message.TextContent); ok && text.Text == pi.Content {
				found = true
			}
		}
	}
	require.True(t, found, "restored inject content must reach the executed turn")
}
