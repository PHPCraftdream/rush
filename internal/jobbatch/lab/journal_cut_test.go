package lab

import (
	"bytes"
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestJournalUnfinishedCutAndLaterFinal(t *testing.T) {
	// F-e mutation: deriving a cut footer from a later Snapshot invents completion after the captured prefix.

	synctest.Test(t, func(t *testing.T) {
		s := labTestLoadFixture(t, "f-e")

		// Keep the join: the shortened script must still validate, while work

		// reporting remains available after the frozen unfinished cut.

		s.Script = s.Script[:7]

		s.Expect = nil

		if err := ValidateScenario(s); err != nil {
			t.Fatal(err)
		}

		w := &labTestJournal{}

		var h *RunHarness

		defer func() { labTestJoinHarness(t, h) }()

		got := RunScenario(context.Background(), s, RunOptions{CleanupBound: time.Millisecond, OpenEvents: func() (io.WriteCloser, error) { return w, nil }, BeforeAction: func(ctx context.Context, i int, run *RunHarness) error { h = run; return nil }})

		if got.ExitCode != ExitUnfinished || got.Footer.FinalCursor != nil || got.Footer.Outcome.Status != OutcomeUnfinished {
			t.Fatalf("result=%+v", got)
		}

		original := append([]byte(nil), w.Bytes()...)

		if code := Replay(s, bytes.NewReader(original)).ExitCode; code != ExitUnfinished {
			t.Fatalf("cut replay=%d", code)
		}

		if err := h.Controlled.Complete(context.Background(), h.Controlled.order[0].launch.Token, jobbatch.Result{State: jobbatch.StateInterrupted}); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)

		defer cancel()

		if err := h.Runner.Close(ctx); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(original, w.Bytes()) {
			t.Fatal("later Final changed journal")
		}
	})
}

func TestJournalCancelledAttemptedCloseUsesFreshCleanup(t *testing.T) {
	// A scripted Close with a cancelled invocation must not suppress cleanup.

	synctest.Test(t, func(t *testing.T) {
		s := labTestLoadFixture(t, "f-f")

		s.Script = append(s.Script[:4], Action{Action: ActionClose})

		s.Expect = nil

		ctx, cancel := context.WithCancel(context.Background())

		defer cancel()

		w := &labTestJournal{}

		done := make(chan error, 1)

		attempted := false

		var harness *RunHarness

		defer func() { labTestJoinHarness(t, harness) }()

		got := RunScenario(ctx, s, RunOptions{
			CleanupBound: time.Second,

			OpenEvents: func() (io.WriteCloser, error) { return w, nil },

			BeforeCloseReturn: func(closeCtx context.Context) error {
				attempted = closeCtx.Err() == context.Canceled

				return nil
			},

			BeforeAction: func(actionCtx context.Context, i int, h *RunHarness) error {
				harness = h

				if i != 4 {
					return nil
				}

				// await_start already consumed registration (§5.2/5.3).

				h.Controlled.mu.Lock()

				launch := h.Controlled.order[0].launch

				h.Controlled.mu.Unlock()

				if err := h.Controlled.HoldStop(launch.Token); err != nil {
					return err
				}

				go func() {
					// Observe and release Stop/report gates with a fresh bound,

					// not the cancelled invocation or scripted Close context.

					fresh, stop := context.WithTimeout(context.Background(), time.Second)

					defer stop()

					err := h.Controlled.AwaitStop(fresh, launch.Token, 1)

					if err == nil {
						err = h.Controlled.ReturnStop(launch.Token, 1, GateOK)
					}

					if err == nil {
						err = h.Oracle.AwaitLabel(fresh, "a1:stop_returned#1")
					}

					if err == nil {
						err = h.Controlled.Complete(fresh, launch.Token, jobbatch.Result{State: jobbatch.StateInterrupted})
					}

					done <- err
				}()

				cancel()

				return nil
			},
		})

		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if !attempted {
			t.Fatal("scripted Close did not observe the cancelled invocation")
		}

		if got.ExitCode != ExitUnfinished || got.Footer.FinalCursor == nil || got.Footer.Outcome.Status != OutcomeClosed || got.Footer.Capture.Status != CaptureComplete {
			t.Fatalf("result=%+v", got)
		}
	})
}

func TestJournalInvocationCancellationDrainsClosed(t *testing.T) {
	// F-f/L5 mutation: using the cancelled invocation for cleanup loses Final or returns success for cancellation.

	s := labTestLoadFixture(t, "f-f")

	// Invocation cancellation replaces the scripted Close; no new schema action.

	s.Script = s.Script[:4]

	s.Script = append(s.Script, Action{Action: ActionClose})

	s.Expect = nil

	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	w := &labTestJournal{}

	done := make(chan error, 1)

	got := RunScenario(ctx, s, RunOptions{OpenEvents: func() (io.WriteCloser, error) { return w, nil }, BeforeAction: func(actionCtx context.Context, i int, h *RunHarness) error {
		if i != 4 {
			return nil
		}

		token := h.Controlled.order[0].launch.Token

		go func() {
			err := h.Controlled.AwaitStop(context.Background(), token, 1)

			if err == nil {
				err = h.Controlled.Complete(context.Background(), token, jobbatch.Result{State: jobbatch.StateInterrupted})
			}

			done <- err
		}()

		cancel()

		return nil
	}})

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if got.ExitCode != ExitUnfinished || got.Footer.Outcome.Status != OutcomeClosed || got.Footer.Capture.Status != CaptureComplete {
		t.Fatalf("result=%+v", got)
	}

	if code := Replay(s, bytes.NewReader(w.Bytes())).ExitCode; code != ExitFailed {
		t.Fatalf("closed replay=%d", code)
	}
}
