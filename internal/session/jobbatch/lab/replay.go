package lab

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
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

func journalActionMap(s *Scenario) map[string]Action {
	m := map[string]Action{}

	for _, a := range s.Script {
		if a.Ref != nil {
			m[*a.Ref] = a
		}
	}

	return m
}

func journalEvent(s *Scenario, r jobbatch.Record, refs map[string]Action) (jobbatch.Event, error) {
	ev := jobbatch.Event{Type: jobbatch.EventType(r.Type)}

	switch r.Type {
	case jobbatch.RecordSubmit:

		a, ok := refs[r.Ref]

		if !ok || a.Action != ActionSubmit || *a.Root != r.Root {
			return ev, scenarioInvalid("submit reference")
		}

		req, err := s.SubmitRequest(a)
		if err != nil {
			return ev, err
		}

		ev.Root, ev.Spec, ev.Ref = req.Root, req.Spec, req.Ref

	case jobbatch.RecordAdd:

		a, ok := refs[r.Ref]

		if !ok || a.Action != ActionAdd || *a.Parent != r.Node {
			return ev, scenarioInvalid("add reference")
		}

		req, err := s.AddRequest(a)
		if err != nil {
			return ev, err
		}

		ev.Parent, ev.Items, ev.Ref = req.Parent, req.Items, req.Ref

	case jobbatch.RecordStopRequested:

		ev.Node = r.Node

	case jobbatch.RecordStarted:

		ev.Node, ev.Token = r.Node, r.Token

	case jobbatch.RecordStartRejected:

		ev.Node, ev.Token, ev.Reason, ev.Code = r.Node, r.Token, r.Reason, r.Code

	case jobbatch.RecordSettled:

		ev.Node, ev.Token, ev.Result = r.Node, r.Token, jobbatch.Result{State: r.State, Code: r.Code}

	case jobbatch.RecordControl:

		ev.Node, ev.Token, ev.Op, ev.Code = r.Node, r.Token, r.Op, r.Code

	case jobbatch.RecordCapacityAvailable:

		ev.Group = r.Group

	case jobbatch.RecordDeadlineExpired:

		ev.Node, ev.Token = r.Node, r.Token

	case jobbatch.RecordHostClosing:

	case jobbatch.RecordRelease:

		ev.Root = r.Root

	default:

		return ev, scenarioInvalid("event type")
	}

	return ev, nil
}

// journalRebuild is a direct Engine consumer. It never creates an executor.

// strict compares every producer record, including transition tails and Final.

func journalRebuild(s *Scenario, records []jobbatch.Record, strict bool) (*jobbatch.Engine, []jobbatch.Record, map[jobbatch.ID]uint64, error) {
	e, err := jobbatch.NewEngine(s.Limits)
	if err != nil {
		return nil, nil, nil, err
	}

	refs := journalActionMap(s)

	settled := map[jobbatch.ID]uint64{}

	produced := []jobbatch.Record{}

	i := 0

	for i < len(records) {
		r := records[i]

		ev, err := journalEvent(s, r, refs)
		if err != nil {
			return e, produced, settled, err
		}

		existing := map[jobbatch.ID]bool{}

		if ev.Type == jobbatch.EventAdd {
			if before, ok := e.Snapshot(ev.Parent); ok {
				existing[before.Node.ID] = true

				for _, v := range before.Descendants {
					existing[v.ID] = true
				}
			}
		}

		t, err := e.Apply(ev)
		if err != nil {
			return e, produced, settled, err
		}

		for _, root := range t.SettledRoots {
			settled[root.Root] = root.Sequence
		}

		next := journalTransition(e, t, uint64(len(produced)+1))

		if ev.Type == jobbatch.EventSubmit || ev.Type == jobbatch.EventAdd {
			id := ev.Root

			if ev.Type == jobbatch.EventAdd {
				id = ev.Parent
			}

			if view, ok := e.Snapshot(id); ok {
				next[0].IDs = nil

				for _, v := range append([]jobbatch.NodeView{view.Node}, view.Descendants...) {
					if !existing[v.ID] {
						next[0].IDs = append(next[0].IDs, v.ID)
					}
				}
			}
		}

		if strict {
			if i+len(next) > len(records) {
				return e, produced, settled, scenarioInvalid("truncated transition")
			}

			for j, want := range next {
				if !journalRecordsEqual(want, records[i+j]) {
					return e, produced, settled, scenarioInvalid("transition records")
				}
			}
		}

		produced = append(produced, next...)

		if strict {
			i += len(next)
		} else {
			i++

			for i < len(records) && (records[i].Type == jobbatch.RecordCommand || records[i].Type == jobbatch.RecordNotice || records[i].Type == jobbatch.RecordClosed) {
				i++
			}
		}
	}

	return e, produced, settled, nil
}

func journalRecordsEqual(a, b jobbatch.Record) bool {
	// JSON's omitempty equates absent and empty ID slices, not omitted metadata.

	if len(a.IDs) == 0 {
		a.IDs = nil
	}

	if len(b.IDs) == 0 {
		b.IDs = nil
	}

	return reflect.DeepEqual(a, b)
}

func journalTransition(e *jobbatch.Engine, t jobbatch.Transition, cursor uint64) []jobbatch.Record {
	ev := t.Event

	r := jobbatch.Record{Cursor: cursor, Sequence: t.Sequence, Type: jobbatch.RecordType(ev.Type), Root: ev.Root, Node: ev.Node, Token: ev.Token, Reason: ev.Reason, Op: ev.Op, Code: ev.Code, Group: ev.Group, Ref: ev.Ref}

	// Mirror Runtime appendTransition (§4.1/5.5), including accepted view

	// metadata and the reported (not cause-mapped) Settled state.

	switch ev.Type {
	case jobbatch.EventSubmit, jobbatch.EventRelease:

		r.Node = ev.Root

	case jobbatch.EventAdd:

		r.Node = ev.Parent
	}

	if v, ok := e.View(r.Node); ok {
		r.Root, r.State, r.Cause = v.Root, v.State, v.Cause

		if v.Kind == jobbatch.KindTask {
			r.Group = v.Group
		}
	}

	if ev.Type == jobbatch.EventSettled {
		r.State, r.Code = ev.Result.State, ev.Result.Code
	}

	out := []jobbatch.Record{r}
	if ev.Type == jobbatch.EventSubmit {
		for _, v := range t.Changed {
			out[0].IDs = append(out[0].IDs, v.ID)
		}
	}

	appendRecord := func(r jobbatch.Record) {
		r.Cursor = cursor + uint64(len(out))

		r.Sequence = t.Sequence

		out = append(out, r)
	}

	for _, cmd := range t.Commands {
		node, token := cmd.Node, cmd.Token

		if cmd.Type == jobbatch.CommandStartLeaf {
			node, token = cmd.Launch.ID, cmd.Launch.Token
		}

		appendRecord(jobbatch.Record{Type: jobbatch.RecordCommand, Command: cmd.Type, Node: node, Token: token, Cause: cmd.Cause})
	}

	for _, notice := range t.Notices {
		counts := notice.Counts

		appendRecord(jobbatch.Record{Type: jobbatch.RecordNotice, Root: notice.Root, Node: notice.Node, Kind: notice.Kind, State: notice.State, Cause: notice.Cause, Counts: &counts})
	}

	if t.Final {
		appendRecord(jobbatch.Record{Type: jobbatch.RecordClosed})
	}

	return out
}

// Validate shape even when missing history prevents an Engine reconstruction.
func journalRecordMetadataValid(r jobbatch.Record) bool {
	if !scenarioRecordValid(r.Type) || !jobbatch.ValidCode(r.Code) {
		return false
	}
	want := jobbatch.Record{Cursor: r.Cursor, Sequence: r.Sequence, Type: r.Type}
	switch r.Type {
	case jobbatch.RecordSubmit:
		want.Root, want.Node, want.State, want.Cause, want.Ref, want.IDs = r.Root, r.Node, r.State, r.Cause, r.Ref, r.IDs
		if r.Root == "" || r.Node != r.Root || !scenarioStateValid(r.State) || r.Ref == "" || len(r.IDs) == 0 {
			return false
		}
	case jobbatch.RecordAdd:
		want.Root, want.Node, want.State, want.Cause, want.Ref, want.IDs = r.Root, r.Node, r.State, r.Cause, r.Ref, r.IDs
		if r.Root == "" || r.Node == "" || r.Ref == "" {
			return false
		}
	case jobbatch.RecordRelease:
		want.Root, want.Node = r.Root, r.Node
		if r.Root == "" || r.Node != r.Root {
			return false
		}
	case jobbatch.RecordCapacityAvailable:
		want.Group = r.Group
	case jobbatch.RecordHostClosing, jobbatch.RecordClosed:
	case jobbatch.RecordCommand:
		want.Command, want.Node, want.Token, want.Cause = r.Command, r.Node, r.Token, r.Cause
		if (r.Command != jobbatch.CommandStartLeaf && r.Command != jobbatch.CommandStopLeaf) || r.Node == "" || r.Token == 0 {
			return false
		}
		if r.Command == jobbatch.CommandStartLeaf && r.Cause != "" {
			return false
		}
	case jobbatch.RecordNotice:
		want.Root, want.Node, want.Kind, want.State, want.Cause, want.Counts = r.Root, r.Node, r.Kind, r.State, r.Cause, r.Counts
		if r.Root == "" || r.Node == "" || (r.Kind != jobbatch.KindTask && r.Kind != jobbatch.KindBatch && r.Kind != jobbatch.KindRoot) || !r.State.Terminal() || r.Counts == nil || !scenarioCountsValid(r.Counts) {
			return false
		}
	default:
		want.Root, want.Node, want.Token, want.State, want.Cause, want.Group = r.Root, r.Node, r.Token, r.State, r.Cause, r.Group
		if !scenarioStateValid(r.State) {
			return false
		}
		if r.Root == "" || r.Node == "" {
			return false
		}
		switch r.Type {
		case jobbatch.RecordStopRequested:
		case jobbatch.RecordStarted, jobbatch.RecordDeadlineExpired:
			if r.Token == 0 {
				return false
			}
		case jobbatch.RecordSettled:
			want.State, want.Code = r.State, r.Code
			if r.Token == 0 || !r.State.Terminal() {
				return false
			}
		case jobbatch.RecordStartRejected:
			want.Reason, want.Code = r.Reason, r.Code
			if r.Token == 0 || (r.Reason != jobbatch.RejectCapacity && r.Reason != jobbatch.RejectFailure && r.Reason != jobbatch.RejectCancelled) {
				return false
			}
		case jobbatch.RecordControl:
			want.Op, want.Code = r.Op, r.Code
			if r.Token == 0 || r.Op != jobbatch.ControlOpStop {
				return false
			}
		}
	}
	if !scenarioCauseValid(&r.Cause) {
		return false
	}
	ids := map[jobbatch.ID]bool{}
	for _, id := range r.IDs {
		if id == "" || ids[id] {
			return false
		}
		ids[id] = true
	}
	return reflect.DeepEqual(want, r)
}
