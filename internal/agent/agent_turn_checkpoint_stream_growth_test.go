package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestTurnCheckpointWriterGetsFreshDeadlinePerWrite(t *testing.T) {
	env := testEnv(t)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "checkpoint fresh deadline")
	require.NoError(t, err)
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: "assistant"})
	require.NoError(t, err)
	current := &msg
	var lock sync.Mutex
	deadlines := make(chan time.Time, 2)
	messages := &checkpointDeadlineMessages{Service: env.messages, deadlines: deadlines}
	a := &sessionAgent{messages: messages, checkpointInterval: 10 * time.Millisecond}
	cp := newTurnCheckpointWriter(a, sess.ID, ctx, &lock, &current, &stallClock{})
	cp.start()
	defer cp.stop()

	lock.Lock()
	current.AppendContent("first")
	lock.Unlock()
	var firstDeadline time.Time
	select {
	case firstDeadline = <-deadlines:
	case <-time.After(5 * time.Second):
		t.Fatal("first checkpoint did not write")
	}
	lock.Lock()
	current.AppendContent(" second")
	lock.Unlock()
	select {
	case secondDeadline := <-deadlines:
		require.True(t, secondDeadline.After(firstDeadline), "each write must get a new 30s deadline")
	case <-time.After(5 * time.Second):
		t.Fatal("second checkpoint did not write")
	}
}

type checkpointDeadlineMessages struct {
	message.Service
	deadlines chan<- time.Time
}

func (m *checkpointDeadlineMessages) Update(ctx context.Context, msg message.Message) error {
	if msg.FinishPart() != nil {
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			m.deadlines <- deadline
		}
	}
	return m.Service.Update(ctx, msg)
}

func TestTurnCheckpointWriterPersistsStreamingContentGrowth(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "checkpoint stream growth")
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		part message.ContentPart
		grow func(message.ContentPart) message.ContentPart
		want string
	}{
		{
			name: "text",
			part: message.TextContent{Text: "initial"},
			grow: func(part message.ContentPart) message.ContentPart {
				text := part.(message.TextContent)
				text.Text += " streamed"
				return text
			},
			want: "initial streamed",
		},
		{
			name: "reasoning",
			part: message.ReasoningContent{Thinking: "initial"},
			grow: func(part message.ContentPart) message.ContentPart {
				reasoning := part.(message.ReasoningContent)
				reasoning.Thinking += " streamed"
				return reasoning
			},
			want: "initial streamed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: "assistant"})
			require.NoError(t, err)
			current := &msg
			var lock sync.Mutex
			a := &sessionAgent{messages: env.messages, checkpointInterval: 10 * time.Millisecond}
			cp := newTurnCheckpointWriter(a, sess.ID, ctx, &lock, &current, &stallClock{})
			cp.start()
			defer cp.stop()

			initial := tc.part
			lock.Lock()
			current.Parts = append(current.Parts, initial)
			lock.Unlock()
			require.Eventually(t, func() bool {
				persisted, err := env.messages.Get(ctx, msg.ID)
				return err == nil && len(persisted.Parts) > 0 && persisted.Parts[0] == initial
			}, time.Second, 5*time.Millisecond, "initial checkpoint was not persisted")

			lock.Lock()
			current.Parts[0] = tc.grow(current.Parts[0])
			grown := current.Parts[0]
			lock.Unlock()
			require.Eventually(t, func() bool {
				persisted, err := env.messages.Get(ctx, msg.ID)
				return err == nil && len(persisted.Parts) > 0 && persisted.Parts[0] == grown
			}, time.Second, 5*time.Millisecond, "grown content was not checkpointed")

			persisted, err := env.messages.Get(ctx, msg.ID)
			require.NoError(t, err)
			require.NotEmpty(t, persisted.Parts)
			if text, ok := persisted.Parts[0].(message.TextContent); ok {
				require.Equal(t, tc.want, text.Text)
				return
			}
			reasoning, ok := persisted.Parts[0].(message.ReasoningContent)
			require.True(t, ok)
			require.Equal(t, tc.want, reasoning.Thinking)
		})
	}
}
