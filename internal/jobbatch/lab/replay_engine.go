package lab

import (
	"reflect"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

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
