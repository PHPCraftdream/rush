package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func labTestCapture(t *testing.T) (*Scenario, JournalHeader, []jobbatch.Record, JournalFooter) {
	t.Helper()
	s := labTestLoadFixture(t, "f-d")
	w := &labTestJournal{}
	if got := RunScenario(context.Background(), s, RunOptions{OpenEvents: func() (io.WriteCloser, error) { return w, nil }}).ExitCode; got != ExitSuccess {
		t.Fatalf("run=%d", got)
	}
	h, r, f, err := readJournal(bytes.NewReader(w.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return s, h, r, f
}

func labTestEncodeJournal(t *testing.T, h JournalHeader, r []jobbatch.Record, f JournalFooter) []byte {
	t.Helper()
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	if err := e.Encode(h); err != nil {
		t.Fatal(err)
	}
	for _, rec := range r {
		if err := e.Encode(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Encode(f); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestReplayRejectsExtraTransitionTails(t *testing.T) {
	// F-d/L6 mutation: skipping arbitrary command/notice/closed tails accepts invented producer records.
	s, _, records, _ := labTestCapture(t)
	for _, kind := range []jobbatch.RecordType{jobbatch.RecordCommand, jobbatch.RecordNotice, jobbatch.RecordClosed} {
		t.Run(string(kind), func(t *testing.T) {
			bad := append([]jobbatch.Record(nil), records...)
			extra := jobbatch.Record{Type: kind, Sequence: bad[0].Sequence}
			bad = append(bad[:1], append([]jobbatch.Record{extra}, bad[1:]...)...)
			for i := range bad {
				bad[i].Cursor = uint64(i + 1)
			}
			if _, _, _, err := journalRebuild(s, bad, true); err == nil {
				t.Fatal("extra tail accepted")
			}
		})
	}
	for i, rec := range records {
		if rec.Type == jobbatch.RecordCommand || rec.Type == jobbatch.RecordNotice {
			bad := append([]jobbatch.Record(nil), records[:i]...)
			bad = append(bad, records[i+1:]...)
			if _, _, _, err := journalRebuild(s, bad, true); err == nil {
				t.Fatal("missing tail accepted")
			}
		}
	}
}

func TestReplayIncompleteMetadataAndPrefix(t *testing.T) {
	// V-d/F-c mutation: write_error cannot hide holes, irrelevant fields, duplicate roots or contradictory outcomes.
	s, h, records, f := labTestCapture(t)
	f.Capture = JournalCapture{Status: CaptureWriteError}
	prefix := append([]jobbatch.Record(nil), records[:len(records)-1]...)
	f.LastCursor = prefix[len(prefix)-1].Cursor
	if got := Replay(s, bytes.NewReader(labTestEncodeJournal(t, h, prefix, f))).ExitCode; got != ExitIncompleteJournal {
		t.Fatalf("valid prefix=%d", got)
	}
	cases := []struct {
		name   string
		mutate func([]jobbatch.Record, *JournalFooter)
	}{
		{"hole", func(r []jobbatch.Record, f *JournalFooter) { r[1].Cursor++ }},
		{"field", func(r []jobbatch.Record, f *JournalFooter) { r[0].Command = jobbatch.CommandStopLeaf }},
		{"duplicate-root", func(r []jobbatch.Record, f *JournalFooter) { f.Roots = append(f.Roots, f.Roots[0]) }},
		{"closed-active", func(r []jobbatch.Record, f *JournalFooter) {
			f.Roots[0].State = jobbatch.StateRunning
			f.Roots[0].SettledSequence = nil
		}},
		{"sequence", func(r []jobbatch.Record, f *JournalFooter) { f.FinalSequence = 1 }},
		{"unknown-ref", func(r []jobbatch.Record, f *JournalFooter) { r[0].Ref = "absent" }},
		{"rejected-record", func(r []jobbatch.Record, f *JournalFooter) {
			n := ErrorTerminal
			f.Inputs[0].Result = InputRejected
			f.Inputs[0].IDs = nil
			f.Inputs[0].Error = &n
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var copyF JournalFooter
			b, _ := json.Marshal(f)
			_ = json.Unmarshal(b, &copyF)
			r := append([]jobbatch.Record(nil), prefix...)
			tc.mutate(r, &copyF)
			if got := Replay(s, bytes.NewReader(labTestEncodeJournal(t, h, r, copyF))).ExitCode; got != ExitInvalid {
				t.Fatalf("exit=%d", got)
			}
		})
	}
}

func TestReplayL6Integrity(t *testing.T) {
	// L6 mutation: framing, cursor continuity, version, digest and producer Final are independently authoritative.
	s, h, r, f := labTestCapture(t)
	original := labTestEncodeJournal(t, h, r, f)
	lines := bytes.Split(bytes.TrimSpace(original), []byte("\n"))
	cases := [][]byte{bytes.Join(lines[:len(lines)-1], []byte("\n")), labTestEncodeJournal(t, h, r[:len(r)-1], f)}
	hole := append([]jobbatch.Record(nil), r[:1]...)
	hole = append(hole, r[2:]...)
	cases = append(cases, labTestEncodeJournal(t, h, hole, f))
	wrongH := h
	wrongH.Version++
	cases = append(cases, labTestEncodeJournal(t, wrongH, r, f))
	wrongH = h
	wrongH.ScenarioSHA256 = "changed"
	cases = append(cases, labTestEncodeJournal(t, wrongH, r, f))
	wrongF := f
	n := *f.FinalCursor + 1
	wrongF.FinalCursor = &n
	cases = append(cases, labTestEncodeJournal(t, h, r, wrongF))
	for i, data := range cases {
		if got := Replay(s, bytes.NewReader(data)).ExitCode; got != ExitInvalid {
			t.Fatalf("mutation %d=%d", i, got)
		}
	}
	s.Messages = map[string]json.RawMessage{"changed": json.RawMessage(`{"text":"changed"}`)}
	if got := Replay(s, bytes.NewReader(original)).ExitCode; got != ExitInvalid {
		t.Fatalf("same refs changed scenario=%d", got)
	}
}
