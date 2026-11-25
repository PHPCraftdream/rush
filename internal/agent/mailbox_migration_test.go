package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionAgent_IsBusyAndIsSessionBusy_TreatMbReleasingAsBusy(t *testing.T) {
	sa := &sessionAgent{mailboxes: csync.NewMap[string, *mailboxpkg.Mailbox]()}
	const sessionID = "releasing-is-busy-test"

	mb := sa.getMailbox(sessionID)
	mb.Mu.Lock()
	mb.State = mailboxpkg.MbReleasing
	mb.Mu.Unlock()

	assert.True(t, sa.IsBusy(), "IsBusy must report true while a Mailbox is mailboxpkg.MbReleasing — release() may still be "+
		"holding the OS session lock on another goroutine")
	assert.True(t, sa.IsSessionBusy(sessionID), "IsSessionBusy must report true while mailboxpkg.MbReleasing, for the same reason")

	mb.Mu.Lock()
	mb.State = mailboxpkg.MbIdle
	mb.Mu.Unlock()

	assert.False(t, sa.IsBusy(), "IsBusy must report false once the Mailbox is genuinely mailboxpkg.MbIdle")
	assert.False(t, sa.IsSessionBusy(sessionID), "IsSessionBusy must report false once the Mailbox is genuinely mailboxpkg.MbIdle")
}

func TestSessionAgent_ClearQueue_ClearsMailboxOnly(t *testing.T) {
	sa := &sessionAgent{
		mailboxes: csync.NewMap[string, *mailboxpkg.Mailbox](),
	}
	const sid = "clear-test"

	// Queue three calls via the public QueueMessage API.
	sa.QueueMessage(agentcall.SessionAgentCall{SessionID: sid, Prompt: "first"})
	sa.QueueMessage(agentcall.SessionAgentCall{SessionID: sid, Prompt: "second"})
	sa.QueueMessage(agentcall.SessionAgentCall{SessionID: sid, Prompt: "third"})

	require.Equal(t, 3, sa.QueuedPrompts(sid))
	require.Len(t, sa.QueuedPromptsList(sid), 3)

	sa.ClearQueue(sid)

	require.Equal(t, 0, sa.QueuedPrompts(sid), "ClearQueue must remove every queued entry")
	require.Empty(t, sa.QueuedPromptsList(sid), "ClearQueue must leave no entries in the list either")
}

func TestSessionAgent_QueuedPrompts_NoDoubleCount(t *testing.T) {
	sa := &sessionAgent{
		mailboxes: csync.NewMap[string, *mailboxpkg.Mailbox](),
	}
	const sid = "count-test"

	require.Equal(t, 0, sa.QueuedPrompts(sid))

	sa.QueueMessage(agentcall.SessionAgentCall{SessionID: sid, Prompt: "a"})
	require.Equal(t, 1, sa.QueuedPrompts(sid), "one message queued = count of 1")

	sa.QueueMessage(agentcall.SessionAgentCall{SessionID: sid, Prompt: "b"})
	require.Equal(t, 2, sa.QueuedPrompts(sid), "two messages queued = count of 2")

	// QueuedPromptsList must match.
	list := sa.QueuedPromptsList(sid)
	require.Len(t, list, 2)
	assert.Equal(t, []string{"a", "b"}, list)

	// Different session must have its own count.
	require.Equal(t, 0, sa.QueuedPrompts("other-session"))
}

func TestRun_QueuedMessageDuringBusySession_RunsExactlyOnce(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()

	sess, err := env.sessions.Create(t.Context(), "single execution migration test")
	require.NoError(t, err)

	var calls atomic.Int64
	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:           Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		FastModel:            Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	sa := agentIface.(*sessionAgent)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), agentcall.SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "first message",
			MaxOutputTokens: 1000,
		})
		runDone <- runErr
	}()

	// Wait for turn 1 to reach the provider.
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1 never reached the provider")
	}

	// Queue via the PUBLIC QueueMessage API during turn 1's stream —
	// this used to go to the legacy messageQueue, now goes to Mailbox.submitted.
	sa.QueueMessage(agentcall.SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "queued during turn 1",
		MaxOutputTokens: 1000,
	})

	close(proceed)

	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}

	// THE core assertion: exactly 2 provider calls — turn 1 and turn 2
	// (the queued message). If the message were double-inserted, this
	// would be 3. If it were stranded, this would be 1.
	assert.Equal(t, int64(2), calls.Load(),
		"expected exactly two provider calls: turn 1 + the queued message as turn 2 — "+
			"not 1 (stranded) and not 3 (double-inserted)")

	// The session must be idle after both turns complete.
	assert.False(t, sa.IsSessionBusy(sess.ID),
		"the session must be idle once both turns complete")
}
