package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchflow"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// batchRootLeaseStore is the root adapter preserving the historic unexported
// acquire name over the batchflow.BatchRootLeaseStore ledger (DM5c moved the
// ledger state and behavior to runtime/batch/batchflow/lease.go). The private
// diskOwnerKey construction stays here — it is mechanically tied to the
// unexported diskCapabilityManager — exposed to batchflow only through the
// acquire/release owner callbacks taken at construction.
type batchRootLeaseStore struct {
	caps *diskCapabilityManager

	*batchflow.BatchRootLeaseStore
}

func newBatchRootLeaseStore(caps *diskCapabilityManager) *batchRootLeaseStore {
	s := &batchRootLeaseStore{caps: caps}
	s.BatchRootLeaseStore = batchflow.NewBatchRootLeaseStore(
		ErrDiskProviderUnavailable,
		// A nil manager returns the bare sentinel, as the historic acquire
		// did before ever calling the manager.
		func(binding, registry, instance string) error {
			if s.caps == nil {
				return ErrDiskProviderUnavailable
			}
			return s.caps.acquire(rootOwnerKey(binding, registry, instance))
		},
		// A nil manager is a no-op, as the historic release was.
		func(binding, registry, instance string) {
			if s.caps == nil {
				return
			}
			s.caps.release(rootOwnerKey(binding, registry, instance))
		},
	)
	return s
}

// rootOwnerKey builds the one Root capability owner key. Keep the key
// construction in exactly one place: the ledger and the capability manager
// must never disagree on the key shape.
func rootOwnerKey(binding, registry, instance string) diskOwnerKey {
	return diskOwnerKey{kind: diskOwnerRoot, instance: instance, bindingID: binding, registryID: registry}
}

// BatchRootLeaseStore is the exported alias of the per-root identity
// store. The app package only constructs and passes the store through
// (install → model control); it never calls its methods.
type BatchRootLeaseStore = batchRootLeaseStore

// NewBatchRootLeaseStore is the exported constructor for hosts that wire
// the store by hand (tests, dormant installs); newBatchInstall builds its
// own.
func NewBatchRootLeaseStore(caps *DiskProviderRegistry) *BatchRootLeaseStore {
	return newBatchRootLeaseStore(caps)
}

// acquire takes the Root owner — BEFORE Submit, using the already-minted
// root ID — and records the root's capability identity and owning session.
// A nil store is the historic nil-sentinel contract; every other method is
// the promoted batchflow surface (Acquire/Adopt/Release/Matches/
// OwnerForRoot/Roots), whose lower implementations carry their own nil
// guards.
func (s *batchRootLeaseStore) acquire(root jobbatch.ID, owner, binding, registry string) error {
	if s == nil {
		return ErrDiskProviderUnavailable
	}
	return s.BatchRootLeaseStore.Acquire(root, owner, binding, registry)
}
