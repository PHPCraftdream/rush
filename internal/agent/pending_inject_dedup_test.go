// R8C-1: a message `rush sessions inject` saved (the user message first, then
// the pending row) while nothing was running is already in the history the next
// turn loads. The step-1 pending-inject drain must not splice it a second time:
// the same exactly-once rule the in-process mailbox applies through historyIDs.
package agent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const injectedText = "also add a CHANGELOG entry for R8C-1"

// requestLog is a provider handler that records every request body and answers
// with a one-step text reply.
type requestLog struct {
	mu     sync.Mutex
	bodies []string
}

func (r *requestLog) handle(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(b))
	r.mu.Unlock()
	textFinishResponse(w, "done")
}

func (r *requestLog) first(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.bodies, "the provider saw no request")
	return r.bodies[0]
}

// persistInjectWhileIdle is `sessions inject` against a session nothing runs:
// the user message row, then the pending row that references it.
func persistInjectWhileIdle(t *testing.T, f *attemptFixture, text string) message.Message {
	t.Helper()
	ctx := context.Background()
	msg, err := f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{
		Origin: message.OriginCLI, Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	require.NoError(t, f.env.sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: f.sessID, MessageID: msg.ID, Content: text,
	}))
	return msg
}

// TestPendingInject_AlreadyInHistoryReachesThePromptOnce: the injected message
// appears exactly once in the first request of the next turn, for an ordinary
// turn and for a Drain, and the pending row is consumed either way.
//
// Revert-check: splicing every pending row without consulting historyIDs puts
// the text in the request twice (both cases red).
func TestPendingInject_AlreadyInHistoryReachesThePromptOnce(t *testing.T) {
	t.Parallel()
	for _, drain := range []bool{false, true} {
		name := "ordinary turn"
		if drain {
			name = "Drain"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			log := &requestLog{}
			f := newAttemptFixture(t, "inject-dedup", attemptFixtureOpts{noIdle: true, handler: log.handle})
			persistInjectWhileIdle(t, f, injectedText)

			var err error
			if drain {
				f.seedDebt(ctx, "call-1", false)
				_, err = f.drainRun(ctx)
			} else {
				_, err = f.sa.Run(ctx, SessionAgentCall{SessionID: f.sessID, Prompt: "continue", MaxOutputTokens: 100})
			}
			require.NoError(t, err)

			require.Equal(t, 1, strings.Count(log.first(t), injectedText), "the injected message is in the first request once")
			pending, _, err := f.env.sessions.DrainPendingInjects(ctx, f.sessID)
			require.NoError(t, err)
			require.Empty(t, pending, "the pending row is consumed, not left to splice again")
		})
	}
}

// TestPendingInject_LandingMidTurnIsSplicedOnce: the control. A message saved
// after the turn loaded its history (the provider's first answer is the moment
// `sessions inject` lands) is not in historyIDs, so the step boundary splices it
// into the second request exactly once.
//
// Revert-check: dropping the pending drain from prepareStep leaves the second
// request without the text.
func TestPendingInject_LandingMidTurnIsSplicedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	log := &requestLog{}
	var f *attemptFixture
	var step atomic.Int32
	probe := fantasy.NewAgentTool("probe_tool", "a probe", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	f = newAttemptFixture(t, "inject-mid-turn", attemptFixtureOpts{noIdle: true, tools: []fantasy.AgentTool{probe}, handler: func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		log.mu.Lock()
		log.bodies = append(log.bodies, string(b))
		log.mu.Unlock()
		if step.Add(1) > 1 {
			textFinishResponse(w, "done")
			return
		}
		persistInjectWhileIdle(t, f, injectedText)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_p","type":"function","function":{"name":"probe_tool","arguments":"{}"}}]},"finish_reason":null}]}`)
		sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}})
	// History first, so no title generation races the request count.
	_, err := f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "earlier"}},
	})
	require.NoError(t, err)

	_, err = f.sa.Run(ctx, SessionAgentCall{SessionID: f.sessID, Prompt: "go", MaxOutputTokens: 100})
	require.NoError(t, err)

	log.mu.Lock()
	defer log.mu.Unlock()
	require.Len(t, log.bodies, 2)
	require.Zero(t, strings.Count(log.bodies[0], injectedText), "the message did not exist when the turn started")
	require.Equal(t, 1, strings.Count(log.bodies[1], injectedText), "the step boundary splices it once")
}
