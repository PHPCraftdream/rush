package agent

import (
	"context"
	"log/slog"
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// batchRootLeaseStore is the root→immutable-capability identity index plus
// the Root owner lifecycle (R12-H2 §1). It is NOT an ownership counter:
// retention lives in the capability manager's owner set. This store only
// records which binding/construction a root was accepted under (matching
// live attach validates against it and acquires NO replacement root
// owner) and releases the Root owner when the root's Done closes — an
// already-closed Done is a valid observation, because Done means no
// further child can start. The paired provider/registry release closures
// of the previous design are gone: release drops exactly the one Root
// owner key.
type batchRootLeaseStore struct {
	mu      sync.Mutex
	caps    *diskCapabilityManager
	entries map[jobbatch.ID]batchRootIdentity
}

// batchRootIdentity is the immutable capability identity one root was
// accepted under.
type batchRootIdentity struct{ binding, registry string }

func newBatchRootLeaseStore(caps *diskCapabilityManager) *batchRootLeaseStore {
	return &batchRootLeaseStore{caps: caps, entries: make(map[jobbatch.ID]batchRootIdentity)}
}

// BatchRootLeaseStore is the exported alias of the per-root identity
// store. The app package only constructs and passes the store through
// (install → model control); it never calls its methods, so the method set
// stays unexported.
type BatchRootLeaseStore = batchRootLeaseStore

// NewBatchRootLeaseStore is the exported constructor for hosts that wire
// the store by hand (tests, dormant installs); newBatchInstall builds its
// own.
func NewBatchRootLeaseStore(caps *DiskProviderRegistry) *BatchRootLeaseStore {
	return newBatchRootLeaseStore(caps)
}

func (s *batchRootLeaseStore) key(root jobbatch.ID, binding, registry string) diskOwnerKey {
	return diskOwnerKey{kind: diskOwnerRoot, instance: string(root), bindingID: binding, registryID: registry}
}

// acquire takes the Root owner — BEFORE Submit, using the already-minted
// root ID — and records the root's capability identity. Definite rejection
// must call release; a successful Submit keeps the owner.
func (s *batchRootLeaseStore) acquire(root jobbatch.ID, binding, registry string) error {
	if binding == "" && registry == "" {
		return nil
	}
	if s == nil || s.caps == nil || root == "" || registry == "" {
		return ErrDiskProviderUnavailable
	}
	if err := s.caps.acquire(s.key(root, binding, registry)); err != nil {
		return err
	}
	s.mu.Lock()
	if prev, ok := s.entries[root]; ok && (prev.binding != binding || prev.registry != registry) {
		s.mu.Unlock()
		s.release(root, binding, registry)
		return ErrDiskProviderUnavailable
	}
	s.entries[root] = batchRootIdentity{binding: binding, registry: registry}
	s.mu.Unlock()
	return nil
}

// adopt arms the Done watcher for one accepted root: when Done closes the
// Root owner is released exactly once (idempotent for an already-closed
// Done).
func (s *batchRootLeaseStore) adopt(root jobbatch.ID, done <-chan struct{}, binding, registry string, release func(context.Context, jobbatch.ID) error) error {
	if s == nil || done == nil {
		return nil
	}
	go func() {
		<-done
		s.release(root, binding, registry)
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

// release drops exactly the one Root owner key.
func (s *batchRootLeaseStore) release(root jobbatch.ID, binding, registry string) {
	if s == nil || s.caps == nil {
		return
	}
	s.caps.release(s.key(root, binding, registry))
}

func (s *batchRootLeaseStore) forget(root jobbatch.ID) {
	if s == nil || root == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, root)
}

// matches validates an existing root's binding/construction identity for a
// matching live attach. It never mutates ownership.
func (s *batchRootLeaseStore) matches(root jobbatch.ID, binding, registry string) bool {
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
