package runfinish

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	"github.com/stretchr/testify/require"
)

// R6C-5: the default cap (no --timeout) used to be a bare time.AfterFunc ->
// os.Exit(124): a loop legitimately waiting on a job was killed without an
// envelope, --on-finish or Shutdown. It is now the same graceful context
// deadline as --timeout; only a process that has not exited past the grace is
// force-killed.

// lockedBuf is a stderr stand-in the timer goroutines may write concurrently.
type lockedBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	sawN chan struct{} // closed on the first write
	once sync.Once
}

func newLockedBuf() *lockedBuf { return &lockedBuf{sawN: make(chan struct{})} }

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.once.Do(func() { close(b.sawN) })
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Revert-check: InstallRunDeadline's WithTimeoutCause pins the typed default cap and RunTimeoutCause.Error wording.
func TestInstallRunDeadline_DefaultCapIsAGracefulDeadline(t *testing.T) {
	t.Parallel()
	stderr, rec := newLockedBuf(), cmdtest.NewExitRecorder()
	ctx, stop := InstallRunDeadline(context.Background(), 0, 30*time.Millisecond, time.Hour, stderr, rec.Exit)

	defer stop()
	cmdtest.AwaitClosed(t, ctx.Done(), "the default cap to end the run's context")
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.ErrorIs(t, context.Cause(ctx), watchdog.ErrRunDefaultCap, "the cap tags its deadline so a cut-off turn names it (R7C-5)")
	var cause *watchdog.RunTimeoutCause
	require.ErrorAs(t, context.Cause(ctx), &cause)
	require.Equal(t, 30*time.Millisecond, cause.Duration)
	require.True(t, cause.DefaultCap)
	require.EqualError(t, cause, "run timeout 30ms exceeded (source: default cap (RUSH_RUN_DEFAULT_HARD_TIMEOUT; no --timeout set); pass --timeout to change the cap)")
	require.Empty(t, stderr.String(), "only the command handler renders graceful timeouts")
	require.Empty(t, rec.Calls(), "the cap is graceful: nothing is force-killed at the deadline")

	stop()
	require.Empty(t, rec.Calls())
}

// Revert-check: InstallRunDeadline preserves earlier parent deadline/cancel without an owned timeout marker.
func TestInstallRunDeadline_ParentEndsEarlier(t *testing.T) {
	for _, mode := range []string{"default-cap", "explicit"} {
		for _, ending := range []string{"deadline", "cancel"} {
			t.Run(mode+"/"+ending, func(t *testing.T) {
				var parent context.Context
				var cancel context.CancelFunc
				wantErr := context.Canceled
				if ending == "deadline" {
					parent, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
					wantErr = context.DeadlineExceeded
				} else {
					parent, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				timeout := time.Duration(0)
				if mode == "explicit" {
					timeout = time.Hour
				}
				stderr, rec := newLockedBuf(), cmdtest.NewExitRecorder()
				ctx, stop := InstallRunDeadline(parent, timeout, time.Hour, time.Hour, stderr, rec.Exit)
				defer stop()
				if ending == "cancel" {
					cancel()
				}
				cmdtest.AwaitClosed(t, ctx.Done(), "the parent to end the run's context")
				require.ErrorIs(t, ctx.Err(), wantErr)
				require.ErrorIs(t, context.Cause(ctx), wantErr)
				var cause *watchdog.RunTimeoutCause
				require.False(t, errors.As(context.Cause(ctx), &cause))
				require.NotErrorIs(t, context.Cause(ctx), watchdog.ErrRunDefaultCap)
				require.Empty(t, stderr.String())
				require.Empty(t, rec.Calls())
			})
		}
	}
}

// A process that has not exited by cap + grace is force-killed with 124.
//
// Revert-check: dropping the exit(124) call makes the wait time out.
func TestInstallRunDeadline_HardKillOnlyPastTheGrace(t *testing.T) {
	t.Parallel()
	stderr, rec := newLockedBuf(), cmdtest.NewExitRecorder()
	ctx, stop := InstallRunDeadline(context.Background(), 0, 10*time.Millisecond, 10*time.Millisecond, stderr, rec.Exit)
	defer stop()

	cmdtest.AwaitClosed(t, rec.First, "the hard kill past the grace")
	require.Equal(t, []int{124}, rec.Calls())
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "the graceful deadline had ended the context first")
	require.Contains(t, stderr.String(), "default cap of 10ms + 10ms grace")
	require.Contains(t, stderr.String(), "force-killing")
}

type enteredDoneContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *enteredDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestInstallRunDeadline_HardKillWaitsForTimeoutCause(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	wrapped := &enteredDoneContext{Context: ctx, entered: make(chan struct{})}
	stderr, rec := newLockedBuf(), cmdtest.NewExitRecorder()
	callbackDone := make(chan struct{})
	go func() {
		hardKillCallback(wrapped, 0, 0, false, stderr, rec.Exit)()
		close(callbackDone)
	}()
	select {
	case <-wrapped.entered:
	case <-callbackDone:
		require.FailNow(t, "hard-kill callback returned before deadline cancellation")
	}
	require.Nil(t, context.Cause(ctx))
	require.Empty(t, rec.Calls())
	cancel(&watchdog.RunTimeoutCause{Duration: time.Second, Source: "test"})
	cmdtest.AwaitClosed(t, callbackDone, "hard-kill callback completion")
	require.Equal(t, []int{124}, rec.Calls())
	require.Contains(t, stderr.String(), "force-killing")
}

// --timeout is unchanged: its own deadline and grace, its own message, no cap
// notice; a stopped run never fires either timer.
//
// Revert-check: giving --timeout the cap's message (or its notice) fails the
// text assertions; dropping the deadline fails the first wait.
func TestInstallRunDeadline_TimeoutFlagUnchanged(t *testing.T) {
	t.Parallel()
	stderr, rec := newLockedBuf(), cmdtest.NewExitRecorder()
	ctx, stop := InstallRunDeadline(context.Background(), 10*time.Millisecond, time.Hour, 10*time.Millisecond, stderr, rec.Exit)
	defer stop()

	cmdtest.AwaitClosed(t, ctx.Done(), "the --timeout deadline")
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.NotErrorIs(t, context.Cause(ctx), watchdog.ErrRunDefaultCap, "an explicit --timeout is not the default cap")
	var cause *watchdog.RunTimeoutCause
	require.ErrorAs(t, context.Cause(ctx), &cause)
	require.EqualError(t, cause, "run timeout 10ms exceeded (source: --timeout)")
	cmdtest.AwaitClosed(t, rec.First, "the --timeout hard kill")
	require.Equal(t, []int{124}, rec.Calls())
	out := stderr.String()
	require.Contains(t, out, "timeout 10ms + 10ms grace")
	require.False(t, strings.Contains(out, "default wall-clock cap"), "no cap notice under --timeout: %q", out)

	quietErr, quietRec := newLockedBuf(), cmdtest.NewExitRecorder()
	_, quietStop := InstallRunDeadline(context.Background(), 0, 10*time.Millisecond, 10*time.Millisecond, quietErr, quietRec.Exit)
	quietStop()
	time.Sleep(60 * time.Millisecond) // longer than cap + grace: nothing may fire after stop
	require.Empty(t, quietRec.Calls())
	require.Empty(t, quietErr.String())
}
