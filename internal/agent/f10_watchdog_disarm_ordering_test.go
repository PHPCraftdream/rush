// Test for F10: every runTurn return path must disarm its stream watchdog
// before waiting for title generation.
package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type erroringModel struct{}

func (erroringModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) Provider() string { return "test" }
func (erroringModel) Model() string    { return "erroring" }

type hangingTitleModel struct {
	started chan struct{}
	done    chan struct{}
}

func newHangingTitleModel() hangingTitleModel {
	return hangingTitleModel{
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (hangingTitleModel) Generate(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m hangingTitleModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	close(m.started)
	<-ctx.Done()
	close(m.done)
	return nil, ctx.Err()
}

func (hangingTitleModel) GenerateObject(ctx context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hangingTitleModel) StreamObject(ctx context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hangingTitleModel) Provider() string { return "test" }
func (hangingTitleModel) Model() string    { return "hanging-title" }

type blockingMessageUpdate struct {
	message.Service
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingMessageUpdate) Update(ctx context.Context, msg message.Message) error {
	if err := b.Service.Update(ctx, msg); err != nil {
		return err
	}
	close(b.entered)
	<-b.release
	return nil
}

func (b *blockingMessageUpdate) unblock() {
	b.once.Do(func() { close(b.release) })
}

// TestRunTurn_DisarmsWatchdogOnErrorReturn_NotJustSuccessPath keeps the
// generic provider-error path and the real deferred title join in play. The
// update gate lets the real DB work finish before virtual time advances.
func TestRunTurn_DisarmsWatchdogOnErrorReturn_NotJustSuccessPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := testEnv(t)
		hangingModel := newHangingTitleModel()
		agentIface := testSessionAgent(env, erroringModel{}, hangingModel, "test system prompt")
		sa := agentIface.(*sessionAgent)

		const hardCap = 100 * time.Millisecond
		const titleGrace = 200 * time.Millisecond
		sa.SetTimeoutOptions(false, hardCap)
		sa.titleJoinGrace = titleGrace
		sa.titleGenerationMaxDuration = time.Hour
		sa.streamWatchdogTick = 10 * time.Millisecond

		updates := &blockingMessageUpdate{
			Service: env.messages,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		sa.messages = updates
		t.Cleanup(updates.unblock)

		sess, err := env.sessions.Create(t.Context(), "New Session")
		require.NoError(t, err)

		joinEntered := make(chan struct{})
		turnTitleJoinAfterDisarmSeam = func() {
			close(joinEntered)
		}
		t.Cleanup(func() { turnTitleJoinAfterDisarmSeam = nil })

		var runErr error
		runDone := make(chan struct{})
		go func() {
			defer close(runDone)
			_, runErr = agentIface.Run(t.Context(), SessionAgentCall{
				Prompt:          "hello",
				SessionID:       sess.ID,
				MaxOutputTokens: 100,
			})
		}()

		// The real final assistant update completes before this gate. Holding
		// handleStreamFailure here keeps DB latency out of virtual time.
		<-updates.entered
		<-hangingModel.started
		updates.unblock()

		var titleCanceledEarly bool
		select {
		case <-joinEntered:
		case <-hangingModel.done:
			titleCanceledEarly = true
		}
		if titleCanceledEarly {
			<-runDone
			sa.runWg.Wait()
			require.Fail(t, "title generation was canceled before the deferred join disarmed the watchdog")
			return
		}

		// The join hook proves disarm ran before the wait. Advance virtual time
		// past the hard cap while it holds the turn.
		time.Sleep(hardCap + sa.streamWatchdogTick)
		synctest.Wait()
		select {
		case <-hangingModel.done:
			t.Fatal("watchdog canceled title generation after the deferred join disarmed it")
		default:
		}

		time.Sleep(titleGrace + sa.streamWatchdogTick)
		synctest.Wait()
		<-runDone
		<-hangingModel.done
		sa.runWg.Wait()
		require.ErrorContains(t, runErr, "boom: main provider call failed")
	})
}
