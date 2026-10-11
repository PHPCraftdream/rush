package lab

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

type ReplayResult struct {
	ExitCode int
	Footer   JournalFooter
}

// Replay validates framing and diagnostics before rebuilding an intact feed.
func Replay(s *Scenario, r io.Reader) ReplayResult {
	result := ReplayResult{ExitCode: ExitInvalid}
	if ValidateScenario(s) != nil {
		return result
	}
	header, records, footer, err := readJournal(r)
	if err != nil {
		return result
	}
	if !reflect.DeepEqual(header, journalHeader(s)) {
		return result
	}
	if journalValidate(s, records, footer) != nil {
		return result
	}
	result.Footer = footer
	if footer.Capture.Status != CaptureComplete {
		if footer.Outcome.Status == OutcomeUnfinished {
			result.ExitCode = ExitUnfinished
		} else {
			result.ExitCode = ExitIncompleteJournal
		}
		return result
	}
	_, _, settled, err := journalRebuild(s, records, true)
	if err != nil {
		return result
	}
	derived, err := journalState(s, records)
	if err != nil {
		return result
	}
	roots := map[jobbatch.ID]JournalRoot{}
	unfinished := journalUnfinished(derived, footer.Inputs)
	for _, in := range footer.Inputs {
		if in.Result != InputAccepted {
			continue
		}
		var rec jobbatch.Record
		for _, r := range records {
			if r.Ref == in.Ref {
				rec = r
				break
			}
		}
		if in.Action == ActionSubmit {
			if len(*in.IDs) != 1 || (*in.IDs)[0] != rec.Root {
				return result
			}
			jr, ok := derived.roots[rec.Root]
			if !ok {
				return result
			}
			if seq, ok := settled[rec.Root]; ok {
				jr.SettledSequence = &seq
			}
			roots[rec.Root] = jr
		} else {
			// Add records enumerate created nodes; direct reply IDs are selected by
			// explicit Parent ownership, never by splitting an ordinal path.
			ids := []jobbatch.ID{}
			for _, id := range rec.IDs {
				v, ok := derived.nodes[id]
				if !ok {
					return result
				}
				if v.Parent == rec.Node {
					ids = append(ids, id)
				}
			}
			if !reflect.DeepEqual(ids, *in.IDs) {
				return result
			}
		}
	}
	if !journalRootsEqual(journalSortedRoots(roots), footer.Roots) {
		return result
	}
	if footer.Outcome.Status == OutcomeUnfinished {
		if footer.Outcome.IDs == nil || !reflect.DeepEqual(unfinished, *footer.Outcome.IDs) {
			return result
		}
		result.ExitCode = ExitUnfinished
		return result
	}
	c := &journalCapture{scenario: s, records: records, oracle: NewOracle()}
	for _, rec := range records {
		c.oracle.record(rec)
	}
	result.ExitCode = ExitSuccess
	for _, root := range footer.Roots {
		if root.State != jobbatch.StateCompleted {
			result.ExitCode = ExitFailed
		}
	}
	if !c.checkOracle(s.Expect, c.oracle, true) {
		result.ExitCode = ExitMismatch
	}
	// ExitUnfinished may describe invocation/physical cleanup, not the intact feed.
	if s.Expect != nil && s.Expect.ExitCode != nil && *s.Expect.ExitCode != ExitUnfinished && *s.Expect.ExitCode != result.ExitCode {
		result.ExitCode = ExitMismatch
	}
	return result
}

func journalRootsEqual(a, b []JournalRoot) bool {
	m := map[jobbatch.ID]JournalRoot{}
	for _, r := range b {
		if _, ok := m[r.ID]; ok {
			return false
		}
		m[r.ID] = r
	}
	return reflect.DeepEqual(a, journalSortedRoots(m))
}

func readJournal(r io.Reader) (JournalHeader, []jobbatch.Record, JournalFooter, error) {
	var h JournalHeader
	var f JournalFooter
	records := []jobbatch.Record{}
	// Decode each line separately so framing cannot hide multiple JSON values.
	data, err := io.ReadAll(r)
	if err != nil {
		return h, nil, f, err
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) < 2 {
		return h, nil, f, scenarioInvalid("journal framing")
	}
	for i, line := range lines {
		var target any
		if i == 0 {
			target = &h
		} else if i == len(lines)-1 {
			target = &f
		} else {
			target = &jobbatch.Record{}
		}
		d := json.NewDecoder(bytes.NewReader(line))
		d.DisallowUnknownFields()
		if d.Decode(target) != nil {
			return h, nil, f, scenarioInvalid("journal fields")
		}
		var tail any
		if d.Decode(&tail) != io.EOF {
			return h, nil, f, scenarioInvalid("journal line")
		}
		if scenarioCheckJSON(line, reflect.TypeOf(target).Elem()) != nil {
			return h, nil, f, scenarioInvalid("journal shape")
		}
		if rec, ok := target.(*jobbatch.Record); ok {
			records = append(records, *rec)
		}
	}
	return h, records, f, nil
}

func journalValidate(s *Scenario, records []jobbatch.Record, f JournalFooter) error {
	bad := func() error { return scenarioInvalid("journal integrity") }
	if f.Type != "footer" || f.RequestCounts.CapacityAvailable < 0 || f.RequestCounts.Release < 0 {
		return bad()
	}
	intact := f.Capture.Status == CaptureComplete
	switch f.Capture.Status {
	case CaptureComplete, CaptureWriteError:
		if f.Capture.FirstMissing != nil || f.Capture.ResumedAt != nil {
			return bad()
		}
	case CaptureGap:
		if f.Capture.FirstMissing == nil || f.Capture.ResumedAt == nil || *f.Capture.FirstMissing == 0 || *f.Capture.ResumedAt <= *f.Capture.FirstMissing {
			return bad()
		}
	default:
		return bad()
	}
	switch f.Outcome.Status {
	case OutcomeClosed:
		if f.FinalCursor == nil || *f.FinalCursor == 0 || f.FinalSequence == 0 || f.Outcome.IDs != nil {
			return bad()
		}
	case OutcomeUnfinished:
		if f.FinalCursor != nil || f.Outcome.IDs == nil {
			return bad()
		}
	default:
		return bad()
	}
	actions := []Action{}
	for _, a := range s.Script {
		if a.Ref != nil {
			actions = append(actions, a)
		}
	}
	if len(f.Inputs) > len(actions) {
		return bad()
	}
	accepted := map[string]JournalInput{}
	all := map[string]JournalInput{}
	for i, in := range f.Inputs {
		a := actions[i]
		if in.Ref != *a.Ref || in.Action != a.Action {
			return bad()
		}
		switch in.Result {
		case InputAccepted:
			if in.IDs == nil || in.Error != nil || in.Action == ActionSubmit && (len(*in.IDs) != 1 || (*in.IDs)[0] != *a.Root) {
				return bad()
			}
			ids := map[jobbatch.ID]bool{}
			for _, id := range *in.IDs {
				if id == "" || ids[id] {
					return bad()
				}
				ids[id] = true
			}
			accepted[in.Ref] = in
		case InputRejected, InputAbandoned:
			if in.IDs != nil || in.Error == nil || (*in.Error != "" && !scenarioErrorValid(*in.Error)) {
				return bad()
			}
		default:
			return bad()
		}
		all[in.Ref] = in
	}
	last, sequence := uint64(0), uint64(0)
	gapSeen := false
	seen := map[string]bool{}
	lastAction := -1
	capacity, releases := 0, 0
	closed := false
	for i, rec := range records {
		if rec.Cursor == 0 || rec.Sequence == 0 || !journalRecordMetadataValid(rec) || closed {
			return bad()
		}
		if rec.Cursor != last+1 {
			if f.Capture.Status != CaptureGap || rec.Cursor <= last {
				return bad()
			}
			if gapSeen || last+1 != *f.Capture.FirstMissing || rec.Cursor != *f.Capture.ResumedAt {
				return bad()
			}
			gapSeen = true
		}
		if rec.Sequence < sequence || f.Capture.Status != CaptureGap && rec.Sequence > sequence+1 {
			return bad()
		}
		if rec.Type == jobbatch.RecordCommand || rec.Type == jobbatch.RecordNotice || rec.Type == jobbatch.RecordClosed {
			if f.Capture.Status != CaptureGap && rec.Sequence != sequence {
				return bad()
			}
		} else if f.Capture.Status != CaptureGap && rec.Sequence != sequence+1 {
			return bad()
		}
		if rec.Type == jobbatch.RecordClosed {
			closed = true
			if i != len(records)-1 || f.FinalCursor == nil || rec.Cursor != *f.FinalCursor || rec.Sequence != f.FinalSequence {
				return bad()
			}
		}
		if rec.Type == jobbatch.RecordSubmit || rec.Type == jobbatch.RecordAdd {
			in, ok := all[rec.Ref]
			if !ok || seen[rec.Ref] || in.Result != InputAccepted || jobbatch.RecordType(in.Action) != rec.Type {
				return bad()
			}
			seen[rec.Ref] = true
			index := -1
			for j, a := range actions {
				if *a.Ref == rec.Ref {
					if a.Action == ActionSubmit && *a.Root != rec.Root || a.Action == ActionAdd && *a.Parent != rec.Node {
						return bad()
					}
					index = j
					break
				}
			}
			if index <= lastAction {
				return bad()
			}
			lastAction = index
		} else if rec.Ref != "" {
			return bad()
		}
		if rec.Type == jobbatch.RecordCapacityAvailable {
			capacity++
		}
		if rec.Type == jobbatch.RecordRelease {
			releases++
		}
		last, sequence = rec.Cursor, rec.Sequence
	}
	if f.Capture.Status == CaptureGap && !gapSeen {
		return bad()
	}
	if sequence > f.FinalSequence || last != f.LastCursor {
		return bad()
	}
	if intact && (len(seen) != len(accepted) || capacity != f.RequestCounts.CapacityAvailable || releases != f.RequestCounts.Release || sequence != f.FinalSequence) {
		return bad()
	}
	if f.Outcome.Status == OutcomeClosed {
		if f.LastCursor > *f.FinalCursor || intact && (!closed || f.LastCursor != *f.FinalCursor) {
			return bad()
		}
	} else if closed {
		return bad()
	}
	rootSeen := map[jobbatch.ID]bool{}
	acceptedRoots := map[jobbatch.ID]bool{}
	for _, in := range f.Inputs {
		if in.Action == ActionSubmit && in.Result == InputAccepted {
			acceptedRoots[(*in.IDs)[0]] = true
		}
	}
	for _, root := range f.Roots {
		if !acceptedRoots[root.ID] {
			return bad()
		}
		if rootSeen[root.ID] || f.Outcome.Status == OutcomeClosed && !root.State.Terminal() {
			return bad()
		}
		rootSeen[root.ID] = true
		if root.ID == "" || !scenarioStateValid(root.State) || !scenarioCountsValid(&root.Counts) {
			return bad()
		}
		if root.SettledSequence != nil && (*root.SettledSequence == 0 || *root.SettledSequence > f.FinalSequence || !root.State.Terminal()) {
			return bad()
		}
		if root.State.Terminal() && root.SettledSequence == nil {
			return bad()
		}
	}
	if f.Outcome.IDs != nil {
		seenIDs := map[jobbatch.ID]bool{}
		for _, id := range *f.Outcome.IDs {
			if id == "" || seenIDs[id] {
				return bad()
			}
			seenIDs[id] = true
		}
	}
	return nil
}
