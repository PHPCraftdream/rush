package jobbatch

import (
	"context"
	"sync"
)

// The retained feed has its own lock and remains immutable after Final.
type runnerFeed struct {
	mu          sync.Mutex
	retention   int
	records     []Record
	head, final uint64
	changed     chan struct{}
	// Only the loop uses known; it distinguishes creation from changed views.
	known map[ID]bool
}

func (r *Runner) appendTransition(t Transition) {
	if r.requestHooks != nil && r.requestHooks.beforeAppend != nil {
		r.requestHooks.beforeAppend(t)
	}
	e := t.Event
	rec := Record{Sequence: t.Sequence, Type: RecordType(e.Type), Root: e.Root, Node: e.Node, Token: e.Token, Reason: e.Reason, Op: e.Op, Code: e.Code, Group: e.Group, Ref: e.Ref}
	if e.Type == EventSubmit {
		rec.Node = e.Root
	}
	if e.Type == EventAdd {
		rec.Node = e.Parent
	}
	if e.Type == EventRelease {
		rec.Node = e.Root
	}
	if rec.Node != "" {
		if v, ok := r.engine.View(rec.Node); ok {
			rec.Root = v.Root
			// State and cause describe accepted state; outcome code is separately
			// preserved for Settled, including when cancellation wins the state.
			rec.State = v.State
			rec.Cause = v.Cause
			if v.Kind == KindTask {
				rec.Group = v.Group
			}
		}
	}
	if e.Type == EventSettled {
		// Replay needs the reported state, not the cancellation-mapped leaf state.
		rec.State = e.Result.State
		rec.Code = e.Result.Code
	}
	f := &r.feed
	if f.known == nil {
		f.known = make(map[ID]bool)
	}
	for _, v := range t.Changed {
		if !f.known[v.ID] {
			if e.Type == EventSubmit || e.Type == EventAdd {
				rec.IDs = append(rec.IDs, v.ID)
			}
			f.known[v.ID] = true
		}
	}
	records := []Record{rec}
	for _, cmd := range t.Commands {
		node, token := cmd.Node, cmd.Token
		if cmd.Type == CommandStartLeaf {
			node = cmd.Launch.ID
			token = cmd.Launch.Token
		}
		records = append(records, Record{Sequence: t.Sequence, Type: RecordCommand, Command: cmd.Type, Node: node, Token: token, Cause: cmd.Cause})
	}
	for _, n := range t.Notices {
		counts := n.Counts
		records = append(records, Record{Sequence: t.Sequence, Type: RecordNotice, Root: n.Root, Node: n.Node, Kind: n.Kind, State: n.State, Cause: n.Cause, Counts: &counts})
	}
	if t.Final {
		records = append(records, Record{Sequence: t.Sequence, Type: RecordClosed})
	}
	f.mu.Lock()
	for _, record := range records {
		f.head++
		record.Cursor = f.head
		if len(f.records) == f.retention {
			copy(f.records, f.records[1:])
			f.records[len(f.records)-1] = record
		} else {
			f.records = append(f.records, record)
		}
	}
	if t.Final {
		f.final = f.head
	}
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func runnerCopyRecord(rec Record) Record {
	rec.IDs = append([]ID(nil), rec.IDs...)
	if rec.Counts != nil {
		counts := *rec.Counts
		rec.Counts = &counts
	}
	return rec
}

func (r *Runner) Events(ctx context.Context, cursor uint64) (EventPage, error) {
	f := &r.feed
	for {
		f.mu.Lock()
		oldest := f.head + 1
		if len(f.records) > 0 {
			oldest = f.records[0].Cursor
		}
		page := EventPage{Next: cursor, Oldest: oldest, Head: f.head, Final: f.final}
		if cursor < oldest {
			page.Next = oldest
			f.mu.Unlock()
			return page, ErrGap
		}
		if cursor <= f.head {
			for _, rec := range f.records {
				if rec.Cursor >= cursor {
					page.Records = append(page.Records, runnerCopyRecord(rec))
				}
			}
			page.Next = page.Records[len(page.Records)-1].Cursor + 1
			f.mu.Unlock()
			return page, nil
		}
		if f.final != 0 {
			page.Next = f.final + 1
			f.mu.Unlock()
			return page, ErrClosed
		}
		changed := f.changed
		f.mu.Unlock()
		// Future cursors wait for producer progress as well; no polling or skipping.
		if err := ctx.Err(); err != nil {
			return page, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return page, ctx.Err()
		}
	}
}
