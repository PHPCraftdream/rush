package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestMailboxInterruptAndReplaceIfCurrentRejectsStaleGeneration(t *testing.T) {
	mb := &mailbox{state: mbOwned, epoch: 9}
	callA := SessionAgentCall{SessionID: "same-era", Prompt: "A"}
	mb.currentCall = &callA
	mb.current.id = 3
	mb.current.cancel = func() { t.Fatal("stale generation was canceled") }

	_, tokenA, owned, published := mb.currentCallStateWithToken()
	require.True(t, owned)
	require.True(t, published)

	callB := SessionAgentCall{SessionID: callA.SessionID, Prompt: "B"}
	mb.setCurrentCall(callB)
	mb.beginGeneration(func() { t.Fatal("new generation was canceled") })

	_, matched := mb.interruptAndReplaceIfCurrent(tokenA, SessionAgentCall{SessionID: callA.SessionID, Prompt: "stale"})
	require.False(t, matched)
	mb.mu.Lock()
	require.Nil(t, mb.replacement)
	require.Equal(t, "B", mb.currentCall.Prompt)
	mb.mu.Unlock()

	_, tokenB, owned, published := mb.currentCallStateWithToken()
	require.True(t, owned)
	require.True(t, published)
	cancel, matched := mb.interruptAndReplaceIfCurrent(tokenB, SessionAgentCall{SessionID: callA.SessionID, Prompt: "replacement"})
	require.True(t, matched)
	require.NotNil(t, cancel)
	mb.mu.Lock()
	require.Equal(t, "replacement", mb.replacement.Prompt)
	mb.mu.Unlock()
}

type blockingInterruptDeleteSessions struct {
	session.Service
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func (s *blockingInterruptDeleteSessions) DeleteInterruptInject(ctx context.Context, injectID string) error {
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.release
	return s.Service.DeleteInterruptInject(ctx, injectID)
}

func (s *blockingInterruptDeleteSessions) allowDelete() {
	s.releaseOnce.Do(func() { close(s.release) })
}

type countingInterruptMessageService struct {
	message.Service
	mu       sync.Mutex
	notifies int
}

func (s *countingInterruptMessageService) Notify(msg message.Message) {
	s.mu.Lock()
	s.notifies++
	s.mu.Unlock()
	s.Service.Notify(msg)
}

func (s *countingInterruptMessageService) notifyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notifies
}

func TestHandleInterruptTick_StaleNonDurableSnapshotRestoresAndRetriesForNewGeneration(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "interrupt-generation-fence")
	require.NoError(t, err)

	sessions := &blockingInterruptDeleteSessions{
		Service: env.sessions,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	messages := &countingInterruptMessageService{Service: env.messages}
	coord := &coordinator{sessions: sessions, messages: messages}

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	coord.currentAgent = sa
	mb := sa.getMailbox(sess.ID)

	aDisk := newFakeDiskProvider(nil)
	bDisk := newFakeDiskProvider(nil)
	aCreds := &CredentialSet{Credentials: []Credential{{APIKey: "A"}}}
	bCreds := &CredentialSet{Credentials: []Credential{{APIKey: "B"}}}
	callA := SessionAgentCall{
		SessionID:   sess.ID,
		Prompt:      "call A",
		Credentials: aCreds,
		CallOptions: &CallOptions{
			DiskProvider: aDisk,
			ModelRole:    config.SelectedModelTypeSmart,
		},
	}
	callB := SessionAgentCall{
		SessionID:   sess.ID,
		Prompt:      "call B",
		Credentials: bCreds,
		CallOptions: &CallOptions{
			DiskProvider:     bDisk,
			ModelRole:        config.SelectedModelTypeFast,
			DisableSubAgents: true,
		},
	}
	aCanceled := false
	bCanceled := false
	_, epoch := mb.submit(callA, func() {})
	require.NotZero(t, epoch)
	mb.beginGeneration(func() { aCanceled = true })

	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt payload"}},
	})
	require.NoError(t, err)
	const injectID = "stale-generation-inject"
	require.NoError(t, env.sessions.CreatePendingInject(t.Context(), session.PendingInject{
		ID: injectID, SessionID: sess.ID, MessageID: msg.ID, Content: msg.FullText(), Interrupt: true, CreatedAt: 1000,
	}))
	const laterInjectID = "later-generation-inject"
	require.NoError(t, env.sessions.CreatePendingInject(t.Context(), session.PendingInject{
		ID: laterInjectID, SessionID: sess.ID, MessageID: msg.ID, Content: "later payload", Interrupt: true, CreatedAt: 2000,
	}))

	type tickResult struct {
		fired bool
		err   error
	}
	result := make(chan tickResult, 1)
	go func() {
		fired, tickErr := coord.handleInterruptTick(t.Context(), sess.ID)
		result <- tickResult{fired: fired, err: tickErr}
	}()
	<-sessions.entered

	// Reuse the same ownership era but publish a new generation and call while
	// the coordinator is blocked after consuming the row and before delivery.
	mb.setCurrentCall(callB)
	mb.beginGeneration(func() { bCanceled = true })
	sessions.allowDelete()

	first := <-result
	require.NoError(t, first.err)
	require.False(t, first.fired)
	require.False(t, aCanceled)
	require.False(t, bCanceled, "a stale A snapshot must not cancel B")
	require.Equal(t, 0, messages.notifyCount())
	mb.mu.Lock()
	require.Nil(t, mb.replacement, "a stale A snapshot must not replace B")
	current := *mb.currentCall
	mb.mu.Unlock()
	require.Equal(t, callB.Prompt, current.Prompt)

	pending, err := env.sessions.PeekInterruptInject(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, injectID, pending.ID, "stale delivery restores the original row exactly")
	require.Equal(t, sess.ID, pending.SessionID)
	require.Equal(t, msg.ID, pending.MessageID)
	require.Equal(t, msg.FullText(), pending.Content)
	require.True(t, pending.Interrupt)
	require.Equal(t, int64(1000), pending.CreatedAt, "restoration preserves FIFO ordering metadata")

	fired, err := coord.handleInterruptTick(t.Context(), sess.ID)
	require.NoError(t, err)
	require.True(t, fired)
	require.False(t, aCanceled)
	require.True(t, bCanceled, "matching B delivery cancels B's generation")
	require.Equal(t, 1, messages.notifyCount(), "only the successful retry notifies")

	mb.mu.Lock()
	require.NotNil(t, mb.replacement)
	require.Same(t, bDisk, mb.replacement.CallOptions.DiskProvider)
	require.Same(t, bCreds, mb.replacement.Credentials)
	require.Equal(t, config.SelectedModelTypeFast, mb.replacement.CallOptions.ModelRole)
	require.True(t, mb.replacement.CallOptions.DisableSubAgents)
	require.Equal(t, msg.ID, mb.replacement.ExistingMessageID)
	mb.mu.Unlock()

	remaining, err := env.sessions.PeekInterruptInject(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, remaining)
	require.Equal(t, laterInjectID, remaining.ID)
	require.NoError(t, env.sessions.DeleteInterruptInject(t.Context(), laterInjectID))
}

func TestStartDetachedRun_SkipsAlreadyConsumedInject(t *testing.T) {
	env := testEnv(t)
	coord := &coordinator{sessions: env.sessions, messages: env.messages}
	sess, err := env.sessions.Create(t.Context(), "detached-already-consumed")
	require.NoError(t, err)
	msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "already consumed"}},
	})
	require.NoError(t, err)

	err = coord.startDetachedRun(t.Context(), SessionAgentCall{
		SessionID:         sess.ID,
		ExistingMessageID: msg.ID,
		InjectID:          "won-by-another-consumer",
		LogicalCallID:     "stale-detached-call",
	})
	require.NoError(t, err)

	queued, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Empty(t, queued, "a loser of inject ownership must not enqueue a duplicate")
}
