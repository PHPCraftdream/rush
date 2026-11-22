package batchflow

import (
	"context"
	"log/slog"
	"sync"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// The root adapter batchflow drives is two unnamed host callbacks taken at
// construction: the host builds the one Root capability owner key and drives
// acquire/release on its capability manager (both mechanically tied to
// host-private types). A nil manager is the host's concern: the acquire
// callback must refuse with the host's unavailable sentinel (also passed to
// NewBatchRootLeaseStore) exactly like the historic acquire did before ever
// calling the manager, and the release callback must be a no-op.

// batchRootIdentity is the immutable capability identity one root was
// accepted under, plus the owning session recorded at acquire time (the
// finalization holds provider reads owner back via OwnerForRoot).
type batchRootIdentity struct{ binding, registry, owner string }

// BatchRootLeaseStore is the root→immutable-capability identity index plus
// the Root owner lifecycle (R12-H2 §1). It is NOT an ownership counter:
// retention lives in the capability manager's owner set. This store records
// which binding/construction a root was accepted under AND which session
// owns it (matching live attach validates against it and acquires NO
// replacement root owner) and releases the Root owner when the root's Done
// closes — an already-closed Done is a valid observation, because Done means
// no further child can start. Lower preconditions return the host-injected
// unavailable error verbatim: the exact historic sentinel identity, never a
// second sentinel or a remap.
type BatchRootLeaseStore struct {
	mu           sync.Mutex
	entries      map[jobbatch.ID]batchRootIdentity
	acquireOwner func(binding, registry, instance string) error
	releaseOwner func(binding, registry, instance string)
	unavailable  error
}

// NewBatchRootLeaseStore builds the lease over the host acquire/release
// callbacks; unavailable is the host's ErrDiskProviderUnavailable identity.
func NewBatchRootLeaseStore(
	unavailable error,
	acquireOwner func(binding, registry, instance string) error,
	releaseOwner func(binding, registry, instance string),
) *BatchRootLeaseStore {
	return &BatchRootLeaseStore{
		entries:      make(map[jobbatch.ID]batchRootIdentity),
		acquireOwner: acquireOwner,
		releaseOwner: releaseOwner,
		unavailable:  unavailable,
	}
}

// Acquire takes the Root owner — BEFORE Submit, using the already-minted
// root ID — and records the root's capability identity and owning session.
// Definite rejection must release the owner it took; a successful Submit
// keeps the owner. An empty owner is refused fail-closed: a recorded root
// must always name its owning session, because the finalization holds
// provider attributes holds through this ledger. The owner-only path (empty
// binding AND empty registry) is the CLI submit path: no disk capability
// owner exists for such roots, so the owner-only ledger entry IS the whole
// lease; any conflicting existing entry refuses it.
func (s *BatchRootLeaseStore) Acquire(root jobbatch.ID, owner, binding, registry string) error {
	if root == "" || owner == "" {
		return s.unavailable
	}
	if binding == "" && registry == "" {
		s.mu.Lock()
		if prev, ok := s.entries[root]; ok &&
			(prev.binding != "" || prev.registry != "" || prev.owner != owner) {
			s.mu.Unlock()
			return s.unavailable
		}
		s.entries[root] = batchRootIdentity{owner: owner}
		s.mu.Unlock()
		return nil
	}
	if registry == "" {
		return s.unavailable
	}
	if err := s.acquireOwner(binding, registry, string(root)); err != nil {
		return err
	}
	s.mu.Lock()
	if prev, ok := s.entries[root]; ok &&
		(prev.binding != binding || prev.registry != registry || prev.owner != owner) {
		s.mu.Unlock()
		s.releaseOwner(binding, registry, string(root))
		return s.unavailable
	}
	s.entries[root] = batchRootIdentity{binding: binding, registry: registry, owner: owner}
	s.mu.Unlock()
	return nil
}

// Adopt arms the Done watcher for one accepted root: when Done closes the
// Root owner is released exactly once (idempotent for an already-closed
// Done).
func (s *BatchRootLeaseStore) Adopt(root jobbatch.ID, done <-chan struct{}, binding, registry string, release func(context.Context, jobbatch.ID) error) error {
	if s == nil {
		return nil
	}
	if done == nil {
		return nil
	}
	go func() {
		<-done
		s.Release(root, binding, registry)
		s.forget(root)
		if release != nil {
			if err := release(context.Background(), root); err != nil {
				slog.Error("batch root disposal after settlement failed", "root", root, "error", err)
				return
			}
		}
	}()
	return nil
}

// Release drops exactly the one Root owner key.
func (s *BatchRootLeaseStore) Release(root jobbatch.ID, binding, registry string) {
	if s == nil {
		return
	}
	s.releaseOwner(binding, registry, string(root))
}

func (s *BatchRootLeaseStore) forget(root jobbatch.ID) {
	if s == nil || root == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, root)
}

// Matches validates an existing root's binding/construction identity for a
// matching live attach. It never mutates ownership.
func (s *BatchRootLeaseStore) Matches(root jobbatch.ID, binding, registry string) bool {
	if s == nil {
		return binding == ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[root]
	if binding == "" {
		// R12-H2a (P1-2): an ORDINARY OS-disk Run-created root records the
		// empty binding under the invocation's registry, so a live Add with
		// an empty binding matches that entry — not "no entry at all".
		// RegistryID is always nonempty here (Add requires inv.RegistryID).
		return ok && e.binding == "" && e.registry == registry
	}
	return ok && e.binding == binding && e.registry == registry
}

// OwnerForRoot returns the owning session recorded for one root by the
// lease ledger, or "" when the ledger holds no entry for it.
func (s *BatchRootLeaseStore) OwnerForRoot(root jobbatch.ID) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[root].owner
}

// Roots snapshots the ledger's live root ids under one lock acquisition;
// the holds provider reads it as one of its independent observations.
func (s *BatchRootLeaseStore) Roots() map[jobbatch.ID]struct{} {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	roots := make(map[jobbatch.ID]struct{}, len(s.entries))
	for root := range s.entries {
		roots[root] = struct{}{}
	}
	return roots
}
