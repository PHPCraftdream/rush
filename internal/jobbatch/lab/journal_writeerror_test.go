package lab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type labTestFailWriter struct {
	bytes.Buffer
	calls, fail int
}

func (w *labTestFailWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.fail {
		return 0, errors.New("PRIVATE_SENTINEL writer error")
	}
	return w.Buffer.Write(p)
}
func (*labTestFailWriter) Close() error { return nil }
func TestJournalWriteFailureStopsAtContiguousPrefix(t *testing.T) {
	// F/L6 mutation: continuing record writes after the first error creates unannounced holes.
	s := labTestLoadFixture(t, "f-d")
	w := &labTestFailWriter{fail: 4}
	got := RunScenario(context.Background(), s, RunOptions{OpenEvents: func() (io.WriteCloser, error) { return w, nil }})
	if got.ExitCode != ExitIncompleteJournal || got.Footer.Capture.Status != CaptureWriteError {
		t.Fatalf("result=%+v", got)
	}
	_, records, f, err := readJournal(bytes.NewReader(w.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || f.LastCursor != 2 {
		t.Fatalf("records=%d last=%d", len(records), f.LastCursor)
	}
	if f.FinalCursor == nil || *f.FinalCursor <= f.LastCursor {
		t.Fatalf("producer Final=%v must exceed written LastCursor=%d", f.FinalCursor, f.LastCursor)
	}
	if code := Replay(s, bytes.NewReader(w.Bytes())).ExitCode; code != ExitIncompleteJournal {
		t.Fatalf("replay=%d", code)
	}
	if bytes.Contains(w.Bytes(), []byte("PRIVATE_SENTINEL")) {
		t.Fatal("writer error leaked")
	}
}
