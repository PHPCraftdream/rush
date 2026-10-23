package agent

import (
	"context"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type attachStartedDelivery struct{}

func (attachStartedDelivery) DeliverBatch(_ context.Context, root jobbatch.ID, _ BatchInvocation) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{Content: "batch root " + string(root) + " started"}, nil
}

type attachRetryLogCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *attachRetryLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *attachRetryLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureAttachRetryLogs(t *testing.T) *attachRetryLogCapture {
	t.Helper()
	capture := &attachRetryLogCapture{}
	previous := slog.Default()
	prevWriter, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(capture, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return capture
}

func TestBatchRuntimeIdenticalRetryAndChangedIdentity(t *testing.T) {
	leaf := newGatingLeaf("view")
	env := newBatchP1Env(t, leaf)
	t.Cleanup(leaf.letGo)
	invokeCtx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	t.Cleanup(cancel)
	env.install.Runtime.cfg.Delivery = attachStartedDelivery{}
	inv := BatchInvocation{
		SessionID: env.sessionID, MessageID: "msg-1", RegistryID: env.registryID,
		Origin: "coordinator", WaiterRank: batchGroupRanks[batchGroupIO],
		Policy: batchLeafPolicyContext{RegistryID: env.registryID, MessageID: "msg-1", RestrictedToolset: env.registryID},
	}
	call := fantasy.ToolCall{ID: "call-live", Name: "view", Input: `{"p":1}`}
	first, err := env.install.Runtime.Invoke(invokeCtx, inv, leaf, call)
	require.NoError(t, err)
	require.Contains(t, first.Content, " started")
	require.Eventually(t, func() bool { return leaf.runCount() == 1 }, time.Second, time.Millisecond, "physical leaf launched")
	root := env.liveRootID()
	second, err := env.install.Runtime.Invoke(invokeCtx, inv, leaf, call)
	require.NoError(t, err)
	require.Contains(t, second.Content, " started")
	require.Equal(t, root, env.liveRootID(), "an identical invocation attaches to the same live root")
	require.Equal(t, 1, leaf.runCount(), "the exact retry does not launch a second physical leaf")

	logs := captureAttachRetryLogs(t)
	changedMessage := inv
	changedMessage.MessageID = "msg-2"
	changedMessage.Policy.MessageID = "msg-2"
	_, err = env.install.Runtime.Invoke(invokeCtx, changedMessage, leaf, call)
	require.ErrorIs(t, err, session.ErrBatchInputMismatch)
	require.Contains(t, logs.String(), `submitted_message_id=msg-2`)
	require.Contains(t, logs.String(), `submitted_registry_id=`+env.registryID)
	changedPolicy := inv
	changedPolicy.Policy.RestrictedToolset = "different-policy"
	_, err = env.install.Runtime.Invoke(invokeCtx, changedPolicy, leaf, call)
	require.ErrorIs(t, err, session.ErrBatchInputMismatch)
	changedArgs := call
	changedArgs.Input = `{"p":2}`
	_, err = env.install.Runtime.Invoke(invokeCtx, inv, leaf, changedArgs)
	require.ErrorIs(t, err, session.ErrBatchInputMismatch)
	require.Equal(t, 1, leaf.runCount(), "identity and argument mismatches do not launch more physical work")

	leaf.letGo()
	_, ok, err := env.install.Done.AwaitDone(invokeCtx, env.sessionID, jobbatch.ID(root))
	require.NoError(t, err)
	require.True(t, ok)
}
