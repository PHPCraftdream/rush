// Package W: unit acceptance for pwCompletionWakeHook (the ordinary batch
// completion wake bridge) against a FAKE next hook — no store, no Runner,
// no dispatcher. Pins the wake contract itself: roots come from the SELECTED
// NOTICES (Event.Root is zero on settled/control events), one wake per
// distinct root, sticky host-closing suppression, and wake-free failure and
// notice-free paths.
package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// pwHookRecorder is a fake next commit hook: it records every transition it
// sees and returns the configured error.
type pwHookRecorder struct {
	transitions []jobbatch.Transition
	err         error
}

func (r *pwHookRecorder) hook(_ context.Context, t jobbatch.Transition) error {
	r.transitions = append(r.transitions, t)
	return r.err
}

// pwWakeCollector collects the roots the wrapper wakes.
type pwWakeCollector struct {
	roots []string
}

func (c *pwWakeCollector) wake(_ context.Context, root string) {
	c.roots = append(c.roots, root)
}

// pwNotice is one selected completion notice for node root.N of root.
func pwNotice(root string) jobbatch.Notice {
	return jobbatch.Notice{
		Root: jobbatch.ID(root), Node: jobbatch.ID(root + ".1"),
		Kind: jobbatch.KindTask, State: jobbatch.StateCompleted,
		Counts: jobbatch.Counts{Completed: 1},
	}
}

// TestPwCompletionWakeHook_WakesNoticeRootsWithZeroEventRoot: a settled
// transition with an EMPTY Event.Root (the real settled shape) and one
// selected notice wakes exactly once, with the NOTICE's root.
//
// Mutation caught: reading the root from t.Event.Root would wake with ""
// (dropped by the dispatcher) — the ordinary completion would never wake
// its idle owner. First failing assertion: the single-root equality below.
func TestPwCompletionWakeHook_WakesNoticeRootsWithZeroEventRoot(t *testing.T) {
	next := &pwHookRecorder{}
	wakes := &pwWakeCollector{}
	hook := pwCompletionWakeHook(next.hook, wakes.wake)
	err := hook(context.Background(), jobbatch.Transition{
		Sequence: 2,
		Event: jobbatch.Event{
			Type: jobbatch.EventSettled, Node: jobbatch.ID("r1.1"),
			Result: jobbatch.Result{State: jobbatch.StateCompleted},
		},
		Notices: []jobbatch.Notice{pwNotice("r1")},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"r1"}, wakes.roots)
	require.Len(t, next.transitions, 1)
}

// TestPwCompletionWakeHook_TwoNoticesSameRootWakeOnce: two selected notices
// of the SAME root in one transition (a NotifyAll batch settling) wake the
// owner once, not once per notice.
//
// Mutation caught: removing the per-transition root dedupe would deliver
// duplicate wakes for one committed completion. First failing assertion:
// the single-element roots equality below.
func TestPwCompletionWakeHook_TwoNoticesSameRootWakeOnce(t *testing.T) {
	next := &pwHookRecorder{}
	wakes := &pwWakeCollector{}
	hook := pwCompletionWakeHook(next.hook, wakes.wake)
	err := hook(context.Background(), jobbatch.Transition{
		Sequence: 2,
		Event: jobbatch.Event{
			Type: jobbatch.EventSettled, Node: jobbatch.ID("r1.2"),
			Result: jobbatch.Result{State: jobbatch.StateCompleted},
		},
		Notices: []jobbatch.Notice{pwNotice("r1"), pwNotice("r1")},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"r1"}, wakes.roots)
}

// TestPwCompletionWakeHook_TwoRootsWakeEachOnce: notices of two DISTINCT
// roots in one transition wake each root once.
//
// Mutation caught: a first-root-only or single-wake implementation would
// drop the second root's reaction. First failing assertion: the two-element
// roots equality below.
func TestPwCompletionWakeHook_TwoRootsWakeEachOnce(t *testing.T) {
	next := &pwHookRecorder{}
	wakes := &pwWakeCollector{}
	hook := pwCompletionWakeHook(next.hook, wakes.wake)
	err := hook(context.Background(), jobbatch.Transition{
		Sequence: 2,
		Event: jobbatch.Event{
			Type: jobbatch.EventSettled, Node: jobbatch.ID("r2.1"),
			Result: jobbatch.Result{State: jobbatch.StateCompleted},
		},
		Notices: []jobbatch.Notice{pwNotice("r1"), pwNotice("r2")},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"r1", "r2"}, wakes.roots)
}

// TestPwCompletionWakeHook_HostClosingStickySuppressesLaterWakes: a
// SUCCESSFUL HostClosing commit never wakes, and every LATER transition's
// notices stay suppressed (the sticky closing flag) — shutdown and recovery
// launch no paid reaction.
//
// Mutation caught: dropping the sticky mirror (waking on post-closing
// settlements) would launch a paid reaction from shutdown/recovery. First
// failing assertion: the empty roots check right after the closing commit.
func TestPwCompletionWakeHook_HostClosingStickySuppressesLaterWakes(t *testing.T) {
	next := &pwHookRecorder{}
	wakes := &pwWakeCollector{}
	hook := pwCompletionWakeHook(next.hook, wakes.wake)
	err := hook(context.Background(), jobbatch.Transition{
		Sequence: 2,
		Event:    jobbatch.Event{Type: jobbatch.EventHostClosing},
		Notices:  []jobbatch.Notice{pwNotice("r1")},
	})
	require.NoError(t, err)
	require.Empty(t, wakes.roots)
	// A later interrupted settlement still carries notices: suppressed.
	err = hook(context.Background(), jobbatch.Transition{
		Sequence: 3,
		Event: jobbatch.Event{
			Type: jobbatch.EventSettled, Node: jobbatch.ID("r1.1"),
			Result: jobbatch.Result{State: jobbatch.StateInterrupted},
		},
		Notices: []jobbatch.Notice{pwNotice("r1")},
	})
	require.NoError(t, err)
	require.Empty(t, wakes.roots)
	require.Len(t, next.transitions, 2)
}

// TestPwCompletionWakeHook_FailingNextHookNeverWakes: a FAILED commit never
// wakes (the notice is not durably selected), and the error propagates.
//
// Mutation caught: waking before checking the commit result would schedule a
// reaction for a notice that does not exist. First failing assertion: the
// empty roots check below.
func TestPwCompletionWakeHook_FailingNextHookNeverWakes(t *testing.T) {
	next := &pwHookRecorder{err: errors.New("pw projection conflict")}
	wakes := &pwWakeCollector{}
	hook := pwCompletionWakeHook(next.hook, wakes.wake)
	err := hook(context.Background(), jobbatch.Transition{
		Sequence: 2,
		Event: jobbatch.Event{
			Type: jobbatch.EventSettled, Node: jobbatch.ID("r1.1"),
			Result: jobbatch.Result{State: jobbatch.StateCompleted},
		},
		Notices: []jobbatch.Notice{pwNotice("r1")},
	})
	require.Error(t, err)
	require.Empty(t, wakes.roots)
}

// TestPwCompletionWakeHook_ZeroNoticesNeverWakes: a transition that selected
// no notices (submit, started, control) never wakes.
//
// Mutation caught: waking on every commit would fabricate reactions for
// non-completion transitions. First failing assertion: the empty roots
// check below.
func TestPwCompletionWakeHook_ZeroNoticesNeverWakes(t *testing.T) {
	next := &pwHookRecorder{}
	wakes := &pwWakeCollector{}
	hook := pwCompletionWakeHook(next.hook, wakes.wake)
	err := hook(context.Background(), jobbatch.Transition{
		Sequence: 1,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: jobbatch.ID("r1.1")},
	})
	require.NoError(t, err)
	require.Empty(t, wakes.roots)
}
