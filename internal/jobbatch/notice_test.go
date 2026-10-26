package jobbatch

import (
	"reflect"
	"testing"
)

func coreTestNoticeIDs(tr Transition) []ID {
	var ids []ID
	for _, n := range tr.Notices {
		ids = append(ids, n.Node)
	}
	return ids
}

func coreTestWantNotices(t *testing.T, tr Transition, want ...ID) {
	t.Helper()
	if !reflect.DeepEqual(coreTestNoticeIDs(tr), want) {
		t.Fatalf("notices %v want %v", coreTestNoticeIDs(tr), want)
	}
}

func TestCoreNoticeSingleTaskAndChain(t *testing.T) {
	for _, chain := range []bool{false, true} {
		e := coreTestEngine(t, 1)
		child := coreTestTask("")
		id := ID("R.1")
		if chain {
			child = coreTestBatch(ModeParallel, 0, child)
			id = "R.1.1"
		}
		coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, child))
		tr := coreTestFinish(t, e, id, StateCompleted)
		coreTestWantNotices(t, tr, "R")
	}
}

func TestCoreNoticeNestedMultiChildAndAdd(t *testing.T) {
	for _, add := range []bool{false, true} {
		e := coreTestEngine(t, 3)
		coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask(""))))
		if add {
			coreTestApply(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestTask("")}})
		}
		coreTestWantNotices(t, coreTestFinish(t, e, "R.1.1", StateCompleted), "R.1.1")
		tr := coreTestFinish(t, e, "R.1.2", StateCompleted)
		if add {
			coreTestWantNotices(t, tr, "R.1.2", "R.1")
			coreTestWantNotices(t, coreTestFinish(t, e, "R.2", StateCompleted), "R.2", "R")
		} else {
			coreTestWantNotices(t, tr, "R.1.2", "R")
		}
	}
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestApply(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestTask("")}})
	coreTestWantNotices(t, coreTestFinish(t, e, "R.1", StateCompleted), "R.1")
	coreTestWantNotices(t, coreTestFinish(t, e, "R.2", StateCompleted), "R.2", "R")
}

func TestCoreNoticeRootStopPreservesOwnCause(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R"})
	tr := coreTestFinish(t, e, "R.1", StateCompleted)
	coreTestWantNotices(t, tr, "R")
	if tr.Notices[0].State != StateCancelled || tr.Notices[0].Cause != CauseStop {
		t.Fatal(tr.Notices)
	}
	v, _ := e.View("R.1")
	if v.Outcome.State != StateCompleted {
		t.Fatal(v)
	}
}

func TestCoreNoticeSkippedCancelledAndAllSuppression(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeSequential, 0, coreTestTask(""), coreTestBatch(ModeParallel, 0, coreTestTask(""))))
	tr := coreTestFinish(t, e, "R.1", StateFailed)
	coreTestWantNotices(t, tr, "R.1", "R.2", "R")
	if tr.Notices[1].State != StateSkipped {
		t.Fatal(tr.Notices)
	}
	e = coreTestEngine(t, 1)
	s := coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestBatch(ModeParallel, 0, coreTestTask("")))
	s.Batch.Notify = NotifyAll
	coreTestSubmit(t, e, "R", s)
	tr = coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R"})
	if len(tr.Notices) != 0 {
		t.Fatal(tr)
	}
	tr = coreTestFinish(t, e, "R.1", StateCompleted)
	coreTestWantNotices(t, tr, "R")
	if tr.Notices[0].Counts.Cancelled != 2 {
		t.Fatal(tr)
	}
}
