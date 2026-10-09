package lab

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

// A typed, allocation-free lower bound, not an encoding-size oracle. Raw bytes
// and minimum member sizes share one budget; subtraction avoids integer overflow.
// Exact escaped JSON size is still enforced by the encoded counters/writer.
type preflightBudget struct {
	remaining int
	err       error
}

func (b *preflightBudget) take(n int) {
	if b.err != nil {
		return
	}
	if n > b.remaining {
		b.err = fmt.Errorf("lab preflight exceeds %d input bytes", MaxInputBytes)
		return
	}
	b.remaining -= n
}

func (b *preflightBudget) collection(n, minimum int) {
	if b.err != nil {
		return
	}
	if n > b.remaining/minimum {
		b.err = fmt.Errorf("lab preflight collection exceeds input bound")
		return
	}
	b.take(n * minimum)
}

func (b *preflightBudget) strings(values ...string) {
	for _, value := range values {
		b.take(len(value))
	}
}

func (b *preflightBudget) actor(actor tasktree.Actor) {
	b.strings(string(actor.Kind), actor.ID)
}

func (b *preflightBudget) ids(ids []tasktree.NodeID) {
	b.collection(len(ids), 2) // Each JSON string needs at least two quotes.
	if b.err != nil {
		return
	}
	for _, id := range ids {
		b.take(len(id))
		if b.err != nil {
			return
		}
	}
}

func (b *preflightBudget) expectation(e Expectation) {
	b.strings(string(e.ProblemCode))
	if e.ActiveID != nil {
		b.take(len(*e.ActiveID))
	}
	if e.NextID != nil {
		b.take(len(*e.NextID))
	}
	b.collection(len(e.Nodes), 2)
	b.ids(e.RemovedIDs)
	b.ids(e.AbsentIDs)
	if b.err != nil {
		return
	}
	for _, n := range e.Nodes {
		b.take(len(n.ID))
		if n.ParentID != nil {
			b.take(len(*n.ParentID))
		}
		if n.Kind != nil {
			b.take(len(*n.Kind))
		}
		if n.Title != nil {
			b.take(len(*n.Title))
		}
		if n.Status != nil {
			b.take(len(*n.Status))
		}
		if n.Reason != nil {
			b.take(len(*n.Reason))
		}
		if n.ActiveForm != nil {
			b.take(len(*n.ActiveForm))
		}
		if n.Children != nil {
			b.ids(*n.Children)
		}
		if b.err != nil {
			return
		}
	}
}

func preflightScenario(s Scenario) error {
	if len(s.Steps) > MaxScenarioSteps {
		return fmt.Errorf("lab scenario exceeds %d steps", MaxScenarioSteps)
	}
	b := preflightBudget{remaining: MaxInputBytes}
	b.take(len(s.TreeKey))
	b.collection(len(s.Steps), 2)
	if b.err != nil {
		return b.err
	}
	for _, step := range s.Steps {
		b.strings(step.Label, string(step.RequestID), step.Mode)
		b.actor(step.Actor)
		b.take(len(step.Payload))
		b.expectation(step.Expect)
		if b.err != nil {
			return b.err
		}
	}
	return b.err
}

func preflightCheckpoint(c Checkpoint) error {
	b := preflightBudget{remaining: MaxInputBytes}
	b.strings(string(c.TreeKey), string(c.Envelope.Snapshot.RootID))
	s := c.Envelope.Snapshot
	// Map members require a quoted key, colon, and at least an empty object.
	// Check all top-level lengths before traversing any of their elements.
	b.collection(len(s.Nodes), 5)
	b.collection(len(s.Tombstones), 5)
	b.collection(len(s.TitleGuards), 2)
	b.collection(len(c.Envelope.Receipts), 5)
	if b.err != nil {
		return b.err
	}
	for key, n := range s.Nodes {
		b.strings(string(key), string(n.ID), string(n.ParentID), string(n.Kind), n.Title, n.ActiveForm, string(n.Status), n.Reason)
		b.ids(n.Children)
		if b.err != nil {
			return b.err
		}
	}
	for key, n := range s.Tombstones {
		b.strings(string(key), string(n.ID), string(n.Kind), n.Title)
		b.actor(n.Actor)
		if b.err != nil {
			return b.err
		}
	}
	for _, guard := range s.TitleGuards {
		b.strings(string(guard.Kind), guard.Title)
		if b.err != nil {
			return b.err
		}
	}
	for key, r := range c.Envelope.Receipts {
		b.strings(string(key), string(r.RequestID), r.Fingerprint)
		b.actor(r.Actor)
		b.ids(r.Delta.Created)
		b.ids(r.Delta.Updated)
		b.ids(r.Delta.Removed)
		b.ids(r.Delta.Completed)
		if b.err != nil {
			return b.err
		}
	}
	return b.err
}
