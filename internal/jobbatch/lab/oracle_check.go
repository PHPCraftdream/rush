package lab

import (
	"context"
	"reflect"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func oracleEqual[T comparable](p *T, v T) bool { return p == nil || *p == v }
func (c *journalCapture) checkOracle(expect *Expect, o *Oracle, replay bool) bool {
	if expect == nil {
		return true
	}
	c.oracleRule = "journal state"
	state, err := journalState(c.scenario, c.records)
	if err != nil || !replay && c.status.Status == CaptureGap {
		// Final views may verify state, never repair missing feed or notices
		// (§4.1/5.5). Replay still cannot reconstruct incomplete history.
		if replay || c.final == 0 || c.runner == nil {
			return false
		}
		state = journalDerived{nodes: map[jobbatch.ID]jobbatch.NodeView{}, roots: map[jobbatch.ID]JournalRoot{}}
		probe, cancel := context.WithCancel(context.Background())
		cancel()
		for id := range c.batches {
			view, err := c.runner.Snapshot(probe, id)
			if err != nil {
				return false
			}
			state.roots[id] = JournalRoot{ID: id, State: view.Node.State, Counts: view.Counts}
			state.nodes[id] = view.Node
			for _, v := range view.Descendants {
				state.nodes[v.ID] = v
			}
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c.oracleRule = "roots"
	if expect.Roots != nil {
		if len(*expect.Roots) != len(state.roots) {
			return false
		}
		for _, want := range *expect.Roots {
			got, ok := state.roots[want.ID]
			if !ok || !oracleEqual(want.State, got.State) || !oracleEqual(want.Counts, got.Counts) {
				return false
			}
		}
	}
	c.oracleRule = "nodes"
	if expect.Nodes != nil {
		for _, want := range *expect.Nodes {
			got, ok := state.nodes[want.ID]
			if !ok {
				return false
			}
			control := string(got.Control)
			if control == "" {
				control = "none"
			}
			if got.Control == jobbatch.ControlFailed {
				control += "(" + got.ControlError + ")"
			}
			if !oracleEqual(want.Control, control) {
				c.oracleRule = "node control: actual " + control
				return false
			}
			if !oracleEqual(want.Parent, got.Parent) || !oracleEqual(want.State, got.State) || !oracleEqual(want.Cause, got.Cause) || !oracleEqual(want.Control, control) || !oracleEqual(want.ControlPending, got.ControlPending) || !oracleEqual(want.Attempts, got.Attempts) {
				return false
			}
			if want.Outcome != nil && (!oracleEqual(want.Outcome.State, got.Outcome.State) || !oracleEqual(want.Outcome.Code, got.Outcome.Code)) {
				return false
			}
			if !replay {
				starts, accepted, stops, disposes := o.counters(want.ID)
				if !oracleEqual(want.Starts, starts) || !oracleEqual(want.Accepted, accepted) || !oracleEqual(want.Stops, stops) || !oracleEqual(want.Disposes, disposes) {
					return false
				}
			}
		}
	}
	c.oracleRule = "max_active"
	if !replay && !oracleEqual(expect.MaxActive, o.maximum()) {
		return false
	}
	c.oracleRule = "notices"
	if expect.Notices != nil {
		actual := map[jobbatch.ID][]jobbatch.Record{}
		for _, r := range c.records {
			if r.Type == jobbatch.RecordNotice {
				actual[r.Root] = append(actual[r.Root], r)
			}
		}
		if len(actual) != len(*expect.Notices) {
			return false
		}
		for root, wants := range *expect.Notices {
			got := actual[root]
			if len(got) != len(wants) {
				return false
			}
			for i, want := range wants {
				r := got[i]
				if r.Node != want.Node || !oracleEqual(want.State, r.State) || !oracleEqual(want.Cause, r.Cause) || want.Counts != nil && (r.Counts == nil || !reflect.DeepEqual(*want.Counts, *r.Counts)) {
					return false
				}
			}
		}
	}
	c.oracleRule = "before"
	if expect.Before != nil {
		for _, pair := range *expect.Before {
			// Replay checks only feed-expressible labels. Physical call labels are not
			// history and cannot be recovered from a journal.
			left, right := o.labelTime(pair[0]), o.labelTime(pair[1])
			if !oraclePhysicalLabel(pair[0]) && !oraclePhysicalLabel(pair[1]) {
				left, right = oracleFeedCursor(c.records, pair[0]), oracleFeedCursor(c.records, pair[1])
			}
			if replay && (oraclePhysicalLabel(pair[0]) || oraclePhysicalLabel(pair[1])) {
				continue
			}
			if left == 0 || right == 0 || left >= right {
				return false
			}
		}
	}
	c.oracleRule = ""
	return true
}

func oracleFeedCursor(records []jobbatch.Record, label string) uint64 {
	for _, r := range records {
		if r.Type == jobbatch.RecordClosed && label == "closed" || r.Node != "" && string(r.Node)+":"+string(r.Type) == label {
			return r.Cursor
		}
	}
	return 0
}

func oraclePhysicalLabel(s string) bool {
	for _, suffix := range []string{":stop_entered", ":stop_returned", ":dispose_entered", ":dispose_returned", ":read_entered", ":read_returned"} {
		for i := 0; i+len(suffix) <= len(s); i++ {
			if s[i:i+len(suffix)] == suffix {
				return true
			}
		}
	}
	return false
}

type journalDerived struct {
	nodes    map[jobbatch.ID]jobbatch.NodeView
	roots    map[jobbatch.ID]JournalRoot
	sequence uint64
}

func journalState(s *Scenario, records []jobbatch.Record) (journalDerived, error) {
	state := journalDerived{nodes: map[jobbatch.ID]jobbatch.NodeView{}, roots: map[jobbatch.ID]JournalRoot{}}
	e, err := jobbatch.NewEngine(s.Limits)
	if err != nil {
		return state, err
	}
	refs := journalActionMap(s)
	for _, r := range records {
		if r.Type == jobbatch.RecordCommand || r.Type == jobbatch.RecordNotice || r.Type == jobbatch.RecordClosed {
			continue
		}
		ev, err := journalEvent(s, r, refs)
		if err != nil {
			return state, err
		}
		t, err := e.Apply(ev)
		if err != nil {
			return state, err
		}
		state.sequence = t.Sequence
		for _, v := range t.Changed {
			state.nodes[v.ID] = v
			if v.Parent == "" {
				if _, ok := state.roots[v.ID]; !ok {
					state.roots[v.ID] = JournalRoot{ID: v.ID}
				}
			}
		}
		for id, root := range state.roots {
			if view, ok := e.Snapshot(id); ok {
				root.State, root.Counts = view.Node.State, view.Counts
				state.nodes[id] = view.Node
				for _, v := range view.Descendants {
					state.nodes[v.ID] = v
				}
				state.roots[id] = root
			}
		}
		for _, summary := range t.SettledRoots {
			root := state.roots[summary.Root]
			root.State, root.Counts = summary.State, summary.Counts
			seq := summary.Sequence
			root.SettledSequence = &seq
			state.roots[summary.Root] = root
		}
	}
	return state, nil
}
