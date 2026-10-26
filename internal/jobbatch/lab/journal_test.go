package lab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestJournalFrozenGapWithReaderBarrier(t *testing.T) {
	// F-c mutation: rebuilding lost history or draining before Final hides frozen retention loss.
	s := labTestLoadFixture(t, "f-c")
	w := &labTestJournal{}
	got := RunScenario(context.Background(), s, RunOptions{
		OpenEvents:        func() (io.WriteCloser, error) { return w, nil },
		BeforeEventsRead:  func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		BeforeCloseReturn: func(ctx context.Context) error { return nil },
	})
	if got.ExitCode != ExitIncompleteJournal || got.Footer.Capture.Status != CaptureGap || got.Footer.Outcome.Status != OutcomeClosed {
		t.Fatalf("result=%+v", got)
	}
	if replay := Replay(s, bytes.NewReader(w.Bytes())).ExitCode; replay != ExitIncompleteJournal {
		t.Fatalf("replay=%d", replay)
	}
}

func TestJournalIndependentClosedReaders(t *testing.T) {
	// F-b mutation: consuming Events globally or returning the requested cursor at EOF breaks independent readers.
	s := labTestLoadFixture(t, "f-b")
	got := RunScenario(context.Background(), s, RunOptions{BeforeAction: func(ctx context.Context, i int, h *RunHarness) error {
		if s.Script[i].Action != ActionJoin {
			return nil
		}
		if err := h.Oracle.AwaitLabel(ctx, "closed"); err != nil {
			return err
		}
		pages := make(chan jobbatch.EventPage, 2)
		errs := make(chan error, 2)
		for range 2 {
			go func() { p, e := h.Runner.Events(ctx, 1); pages <- p; errs <- e }()
		}
		p, q := <-pages, <-pages
		e1, e2 := <-errs, <-errs
		if e1 != nil || e2 != nil || !reflect.DeepEqual(p, q) || p.Final == 0 || p.Head != p.Final {
			return errScenarioMismatch
		}
		for _, cursor := range []uint64{p.Final + 1, p.Final + 3} {
			tail, err := h.Runner.Events(ctx, cursor)
			if !errors.Is(err, jobbatch.ErrClosed) || len(tail.Records) != 0 || tail.Next != p.Final+1 {
				return errScenarioMismatch
			}
		}
		return nil
	}})
	if got.ExitCode != ExitFailed {
		t.Fatalf("exit=%d", got.ExitCode)
	}
}

func TestScenarioPhysicalStopReturnBarriers(t *testing.T) {
	// W-c-i/ii mutation: retrying or reporting before the held physical Stop returns violates control ownership.
	for _, name := range []string{"w-c-i", "w-c-ii"} {
		t.Run(name, func(t *testing.T) {
			s := labTestLoadFixture(t, name)
			got := labTestRunScenario(t, name, s, RunOptions{BeforeAction: func(ctx context.Context, i int, h *RunHarness) error {
				if i > 0 && s.Script[i-1].Action == ActionReturnStop {
					if err := h.Oracle.AwaitLabel(ctx, "x1:stop_returned#1"); err != nil {
						return err
					}
				}
				if s.Script[i].Action == ActionSubmit && s.Script[i].Root != nil && *s.Script[i].Root == "T" || s.Script[i].Action == ActionReturnStop && name == "w-c-i" {
					snap, err := h.Runner.Snapshot(ctx, "B")
					if err != nil {
						return err
					}
					if len(snap.Descendants) != 1 || snap.Descendants[0].State != jobbatch.StatePending || snap.Descendants[0].Attempts != 0 {
						return errScenarioMismatch
					}
				}
				return nil
			}})
			if got.ExitCode != *s.Expect.ExitCode || len(got.Mismatches) != 0 {
				t.Fatalf("result=%+v", got)
			}
		})
	}
}
