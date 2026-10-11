package lab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

const JournalFormat = "batchlab"

func ScenarioDigest(s *Scenario) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func journalHeader(s *Scenario) JournalHeader {
	digest, _ := ScenarioDigest(s)
	return JournalHeader{Type: "header", Format: JournalFormat, Version: 1, ScenarioSHA256: digest, Limits: s.Limits, Runner: s.Runner}
}

type journalCapture struct {
	mu          sync.Mutex
	scenario    *Scenario
	written     uint64
	writer      *json.Encoder
	sink        io.Writer
	status      JournalCapture
	records     []jobbatch.Record
	cursor      uint64
	head, final uint64
	runner      *jobbatch.Runner
	batches     map[jobbatch.ID]*jobbatch.Batch
	oracle      *Oracle
	oracleRule  string
}

func newJournalCapture(s *Scenario, w io.Writer, o *Oracle) *journalCapture {
	c := &journalCapture{scenario: s, status: JournalCapture{Status: CaptureComplete}, cursor: 1, oracle: o, sink: w}
	if w != nil {
		c.writer = json.NewEncoder(w)
		if c.writer.Encode(journalHeader(s)) != nil {
			c.status.Status = CaptureWriteError
		}
	}
	return c
}

func (c *journalCapture) page(p jobbatch.EventPage, err error, cut uint64) {
	c.head, c.final = p.Head, p.Final
	if errors.Is(err, jobbatch.ErrGap) {
		if c.status.Status == CaptureComplete {
			first, resume := c.cursor, p.Oldest
			c.status = JournalCapture{Status: CaptureGap, FirstMissing: &first, ResumedAt: &resume}
		}
		c.cursor = p.Oldest
		return
	}
	for _, r := range p.Records {
		if cut != 0 && r.Cursor > cut {
			break
		}
		if r.Cursor < c.cursor {
			continue
		}
		c.records = append(c.records, r)
		c.oracle.record(r)
		if c.writer != nil && c.status.Status != CaptureWriteError {
			if c.writer.Encode(r) != nil {
				c.status = JournalCapture{Status: CaptureWriteError}
			} else {
				c.written = r.Cursor
			}
		}
		c.cursor = r.Cursor + 1
	}
}

func (c *journalCapture) follow(ctx context.Context, r *jobbatch.Runner) {
	for {
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		cursor := c.cursor
		c.mu.Unlock()
		p, err := r.Events(ctx, cursor)
		c.mu.Lock()
		if ctx.Err() != nil {
			c.mu.Unlock()
			return // Cancellation freezes capture before a bounded reader join.
		}
		c.page(p, err, 0)
		c.mu.Unlock()
		if err != nil && !errors.Is(err, jobbatch.ErrGap) {
			return
		}
		if p.Final != 0 && c.cursor > p.Final {
			return
		}
	}
}

func (c *journalCapture) drain(ctx context.Context, r *jobbatch.Runner) {
	c.runner = r
	// A cancelled probe reads available records without waiting; its Head/Final
	// fixes this drain boundary. Later completions must not enlarge a pre-F cut.
	probe, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := r.Events(probe, c.cursor)
	cut, final := p.Head, p.Final
	c.page(p, err, cut)
	for c.cursor <= cut {
		readCtx := ctx
		if final == 0 {
			readCtx = probe
		}
		p, err = r.Events(readCtx, c.cursor)
		c.page(p, err, cut)
		if err != nil && !errors.Is(err, jobbatch.ErrGap) {
			if c.cursor <= cut && c.status.Status == CaptureComplete {
				c.status.Status = CaptureWriteError
			}
			break
		}
	}
	c.head, c.final = cut, final
}

func (c *journalCapture) footer(s *Scenario, inputs []JournalInput, counts RequestCounts) JournalFooter {
	f := JournalFooter{Type: "footer", Roots: []JournalRoot{}, Capture: c.status, Inputs: inputs, RequestCounts: counts, Outcome: JournalOutcome{Status: OutcomeUnfinished}}
	if inputs == nil {
		inputs = []JournalInput{}
		f.Inputs = inputs
	}
	if len(c.records) > 0 {
		last := c.records[len(c.records)-1]
		f.LastCursor = last.Cursor
		f.FinalSequence = last.Sequence
	}
	if c.final != 0 {
		f.FinalCursor = &c.final
		f.Outcome.Status = OutcomeClosed
	}
	if c.writer != nil {
		f.LastCursor = c.written
	}
	state, err := journalState(s, c.records)
	if err == nil {
		f.Roots = journalSortedRoots(state.roots)
		if f.Outcome.Status == OutcomeUnfinished {
			ids := journalUnfinished(state, inputs)
			f.Outcome.IDs = &ids
		}
	} else {
		// Lost history is not reconstructed from snapshots.
		if c.final != 0 {
			probe, cancel := context.WithCancel(context.Background())
			cancel()
			for _, in := range inputs {
				if in.Action != ActionSubmit || in.Result != InputAccepted || in.IDs == nil {
					continue
				}
				id := (*in.IDs)[0]
				if view, err := c.runner.Snapshot(probe, id); err == nil {
					jr := JournalRoot{ID: id, State: view.Node.State, Counts: view.Counts}
					for _, r := range c.records {
						if r.Type == jobbatch.RecordNotice && r.Node == id {
							seq := r.Sequence
							jr.SettledSequence = &seq
						}
					}
					if batch := c.batches[id]; batch != nil {
						if summary, err := batch.Wait(probe); err == nil {
							jr.State, jr.Counts = summary.State, summary.Counts
							seq := summary.Sequence
							jr.SettledSequence = &seq
						}
					}
					f.Roots = append(f.Roots, jr)
				}
			}
		}
		if f.Outcome.Status == OutcomeUnfinished {
			ids := []jobbatch.ID{}
			f.Outcome.IDs = &ids
		}
	}
	if f.FinalCursor != nil && (len(c.records) == 0 || c.records[len(c.records)-1].Type != jobbatch.RecordClosed || f.LastCursor != *f.FinalCursor) && f.Capture.Status == CaptureComplete {
		f.Capture.Status = CaptureWriteError
		c.status = f.Capture
	}
	return f
}

func (c *journalCapture) writeFooter(f JournalFooter) {
	if c.sink != nil && json.NewEncoder(c.sink).Encode(f) != nil {
		c.status = JournalCapture{Status: CaptureWriteError}
	}
}

func journalSortedRoots(m map[jobbatch.ID]JournalRoot) []JournalRoot {
	keys := make([]string, 0, len(m))
	for id := range m {
		keys = append(keys, string(id))
	}
	sort.Strings(keys)
	out := make([]JournalRoot, 0, len(keys))
	for _, id := range keys {
		out = append(out, m[jobbatch.ID(id)])
	}
	return out
}
