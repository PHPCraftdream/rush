package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// gatedAfterAnswerProvider is questionProvider whose child holds its final
// reply until the gate opens, so the node keeps working after it was answered.
func gatedAfterAnswerProvider(gate <-chan struct{}) http.HandlerFunc {
	inner := questionProvider()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "an agent for Rush") && strings.Contains(string(body), `"role":"tool"`) {
			select {
			case <-gate:
			case <-time.After(60 * time.Second):
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner(w, r)
	}
}

// Once a question is answered the node must stop reporting awaiting_answer even
// though its notice stays unreacted until the node ends; otherwise a client that
// answers and waits again is told to answer the same question once more.
func TestAPIBatchQuestion_AwaitingAnswerClearsOnceAnswered(t *testing.T) {
	gate := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	f := newBatchFixture(t, gatedAfterAnswerProvider(gate))
	f.grantChildQuestionTools(t)
	token := f.token(t, "ask", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "ask me something"}).accepted(t)

	_, held := f.waitAwaiting(t, token, accepted.RootID)
	answerPath := "/batches/" + accepted.RootID + "/nodes/" + held.ID + "/answer"
	require.Equal(t, http.StatusAccepted, f.do(t, token, http.MethodPost, answerPath, map[string]any{"text": "8080"}).Status)

	// The child is still working: the batch is not done, yet nothing awaits an answer.
	for range 5 {
		current := f.do(t, token, http.MethodGet, "/batches/"+accepted.RootID, nil).batch(t)
		require.False(t, current.Done, "the gated child has not finished")
		for _, node := range current.Nodes {
			require.False(t, node.AwaitingAnswer, "node %s still reports a question that was answered", node.ID)
			require.Empty(t, node.Question)
		}
		node := f.do(t, token, http.MethodGet, "/batches/"+accepted.RootID+"/nodes/"+held.ID, nil)
		require.Equal(t, http.StatusOK, node.Status, string(node.Body))
		require.NotContains(t, string(node.Body), `"awaiting_answer":true`)
		time.Sleep(50 * time.Millisecond)
	}

	close(gate)
	done := f.waitBatchDone(t, token, accepted.RootID)
	require.Equal(t, "completed", done.State)
}
