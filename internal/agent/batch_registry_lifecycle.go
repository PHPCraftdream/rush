package agent

import (
	"errors"
	"sync"
)

// batchLeafRegistryLeaseStore tracks process-local policy registries by
// reference. Each admitted execution pins before Start and releases only
// after its wrapped Disposer succeeds; a released registry ID never resolves.
type batchLeafRegistryLeaseStore struct {
	mu         sync.Mutex
	registries *BatchLeafRegistryStore
	refs       map[string]int
}

func newBatchLeafRegistryLeaseStore(registries *BatchLeafRegistryStore) *batchLeafRegistryLeaseStore {
	return &batchLeafRegistryLeaseStore{registries: registries, refs: make(map[string]int)}
}

func (s *batchLeafRegistryLeaseStore) acquire(registryID string) (func(), error) {
	if registryID == "" || s == nil || s.registries == nil {
		return nil, ErrDiskProviderUnavailable
	}
	if err := s.retainOwner(registryID); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { s.releaseOwner(registryID) }) }, nil
}

func (s *batchLeafRegistryLeaseStore) releaseOwner(registryID string) {
	if s == nil || s.registries == nil {
		return
	}
	s.mu.Lock()
	if s.refs[registryID] > 0 {
		s.refs[registryID]--
	}
	remove := s.refs[registryID] == 0
	if remove {
		delete(s.refs, registryID)
	}
	s.mu.Unlock()
	if remove {
		s.registries.Release(registryID)
	}
}

func (s *batchLeafRegistryLeaseStore) retainOwner(registryID string) error {
	if registryID == "" || s == nil || s.registries == nil {
		return ErrDiskProviderUnavailable
	}
	if err := s.registries.Retain(registryID); err != nil {
		return errors.Join(ErrDiskProviderUnavailable, err)
	}
	s.mu.Lock()
	s.refs[registryID]++
	s.mu.Unlock()
	return nil
}
