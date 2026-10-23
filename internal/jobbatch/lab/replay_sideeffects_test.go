package lab

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestReplayNeverExecutesProcessPayload(t *testing.T) {
	// L4 mutation: constructing an executor during replay would invoke a missing program instead of consuming recorded results.
	s := labTestLoadFixture(t, "f-d")
	s.Expect = nil
	destination := filepath.Join(t.TempDir(), "replay-side-effect")
	program := filepath.Join(t.TempDir(), "not-a-program")
	args := []string{destination}
	s.Payloads["p"] = Payload{Kind: "process", Program: &program, Args: &args}
	e, err := jobbatch.NewEngine(s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.SubmitRequest(s.Script[0])
	if err != nil {
		t.Fatal(err)
	}
	records := []jobbatch.Record{}
	appendEvent := func(ev jobbatch.Event) jobbatch.Transition {
		t.Helper()
		tr, err := e.Apply(ev)
		if err != nil {
			t.Fatal(err)
		}
		tail := journalTransition(e, tr, uint64(len(records)+1))
		records = append(records, tail...)
		return tr
	}
	tr := appendEvent(jobbatch.Event{Type: jobbatch.EventSubmit, Root: req.Root, Spec: req.Spec, Ref: req.Ref})
	token := tr.Commands[0].Launch.Token
	appendEvent(jobbatch.Event{Type: jobbatch.EventStarted, Node: "A.1", Token: token})
	appendEvent(jobbatch.Event{Type: jobbatch.EventSettled, Node: "A.1", Token: token, Result: jobbatch.Result{State: jobbatch.StateCompleted}})
	appendEvent(jobbatch.Event{Type: jobbatch.EventHostClosing})
	c := &journalCapture{scenario: s, records: records, final: records[len(records)-1].Cursor, status: JournalCapture{Status: CaptureComplete}}
	ids := []jobbatch.ID{"A"}
	f := c.footer(s, []JournalInput{{Ref: req.Ref, Action: ActionSubmit, Result: InputAccepted, IDs: &ids}}, RequestCounts{})
	encoded := labTestEncodeJournal(t, journalHeader(s), records, f)
	if err := journalValidate(s, records, f); err != nil {
		t.Fatalf("validation rule: %s", ScenarioValidationRule(err))
	}
	if _, _, _, err := journalRebuild(s, records, true); err != nil {
		t.Fatalf("rebuild rule: %s", ScenarioValidationRule(err))
	}
	if code := Replay(s, bytes.NewReader(encoded)).ExitCode; code != ExitSuccess {
		t.Fatalf("replay=%d", code)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("replay side effect", err)
	}
	b, err := json.Marshal(s)
	if err != nil || !json.Valid(b) {
		t.Fatal(err)
	}
}
