package memory

import (
	"context"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

type Store struct {
	mu     sync.RWMutex
	limits tasktree.Limits
	boards map[tasktree.TreeKey]tasktree.Envelope
}

var _ tasktree.Store = (*Store)(nil)

func NewStore(limits tasktree.Limits, seeds map[tasktree.TreeKey]tasktree.Envelope) (*Store, error) {
	if err := tasktree.ValidateLimits(limits); err != nil {
		return nil, err
	}
	store := &Store{limits: limits, boards: make(map[tasktree.TreeKey]tasktree.Envelope, len(seeds))}
	for key, seed := range seeds {
		if err := validKey(key); err != nil {
			return nil, err
		}
		if err := tasktree.CheckEnvelope(seed, limits); err != nil {
			return nil, err
		}
		store.boards[key] = tasktree.CloneEnvelope(seed)
	}
	return store, nil
}

func validKey(key tasktree.TreeKey) error {
	if strings.TrimSpace(string(key)) == "" {
		return &tasktree.Problem{Code: tasktree.CodeInvalidInput, Message: "tree key is required"}
	}
	return nil
}

func (s *Store) Load(ctx context.Context, key tasktree.TreeKey) (tasktree.Envelope, error) {
	if err := validKey(key); err != nil {
		return tasktree.Envelope{}, err
	}
	if err := ctx.Err(); err != nil {
		return tasktree.Envelope{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return tasktree.Envelope{}, err
	}
	if envelope, ok := s.boards[key]; ok {
		return tasktree.CloneEnvelope(envelope), nil
	}
	return tasktree.Envelope{Snapshot: tasktree.EmptySnapshot(), Receipts: make(map[tasktree.RequestID]tasktree.Receipt)}, nil
}

func (s *Store) Commit(ctx context.Context, key tasktree.TreeKey, expected tasktree.Revision, candidate tasktree.Envelope) error {
	if err := validKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected == ^tasktree.Revision(0) || candidate.Revision != expected+1 {
		return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "candidate revision must be expected plus one without overflow"}
	}
	if err := tasktree.CheckEnvelope(candidate, s.limits); err != nil {
		return err
	}
	owned := tasktree.CloneEnvelope(candidate)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current := s.boards[key]
	if current.Revision != expected {
		return &tasktree.Problem{Code: tasktree.CodeConflict, Message: "compare-and-swap lost", Expected: expected, Current: current.Revision}
	}
	for id, previous := range current.Receipts {
		next, ok := owned.Receipts[id]
		if !ok || !sameReceipt(previous, next) {
			return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "candidate rewrites receipt history"}
		}
	}
	if len(owned.Receipts) != len(current.Receipts)+1 {
		return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "candidate must add exactly one receipt"}
	}
	for id, receipt := range owned.Receipts {
		if _, existed := current.Receipts[id]; !existed && receipt.CommittedRevision != owned.Revision {
			return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "new receipt must carry candidate revision"}
		}
	}
	for id, previous := range current.Snapshot.Nodes {
		if next, live := owned.Snapshot.Nodes[id]; live {
			if next.Kind != previous.Kind {
				return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "candidate changes existing node kind"}
			}
		} else if tombstone, removed := owned.Snapshot.Tombstones[id]; !removed || tombstone.Kind != previous.Kind {
			return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "existing node must remain live or have same-kind tombstone"}
		}
	}
	if owned.Snapshot.NextID < current.Snapshot.NextID || (current.Snapshot.Initialized && !owned.Snapshot.Initialized) {
		return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "candidate rewinds identity or initialization"}
	}
	for id, previous := range current.Snapshot.Tombstones {
		if next, ok := owned.Snapshot.Tombstones[id]; !ok || next != previous {
			return &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "candidate rewrites tombstones"}
		}
	}
	s.boards[key] = owned
	return nil
}

func sameIDs(a, b []tasktree.NodeID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameReceipt(a, b tasktree.Receipt) bool {
	return a.RequestID == b.RequestID && a.Actor == b.Actor && a.Fingerprint == b.Fingerprint && a.CommittedRevision == b.CommittedRevision &&
		sameIDs(a.Delta.Created, b.Delta.Created) && sameIDs(a.Delta.Updated, b.Delta.Updated) && sameIDs(a.Delta.Removed, b.Delta.Removed) && sameIDs(a.Delta.Completed, b.Delta.Completed)
}
