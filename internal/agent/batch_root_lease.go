package agent

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

type batchRootLeaseEntry struct {
	binding  string
	registry string
	lease    *batchRootLease
}
type batchRootLease struct {
	releaseProvider func()
	releaseRegistry func()
	once            sync.Once
}
type batchRootLeasePending struct {
	binding  string
	registry string
	lease    *batchRootLease
}
type batchRootLeaseStore struct {
	mu         sync.Mutex
	entries    map[jobbatch.ID]batchRootLeaseEntry
	bindings   *DiskProviderRegistry
	registries *batchLeafRegistryLeaseStore
}

func newBatchRootLeaseStore(bindings *DiskProviderRegistry, registries *batchLeafRegistryLeaseStore) *batchRootLeaseStore {
	return &batchRootLeaseStore{entries: make(map[jobbatch.ID]batchRootLeaseEntry), bindings: bindings, registries: registries}
}

func (s *batchRootLeaseStore) acquire(binding, registry string) (*batchRootLeasePending, error) {
	if binding == "" {
		return nil, nil
	}
	if s == nil || s.bindings == nil || s.registries == nil || registry == "" {
		return nil, ErrDiskProviderUnavailable
	}
	_, rp, err := s.bindings.Acquire(binding)
	if err != nil {
		return nil, err
	}
	rr, err := s.registries.acquire(registry)
	if err != nil {
		rp()
		return nil, err
	}
	return &batchRootLeasePending{binding, registry, &batchRootLease{releaseProvider: rp, releaseRegistry: rr}}, nil
}

func (p *batchRootLeasePending) release() {
	if p != nil && p.lease != nil {
		p.lease.release()
	}
}

func (l *batchRootLease) release() { l.once.Do(func() { l.releaseProvider(); l.releaseRegistry() }) }

// adopt transfers a provisional lease to one accepted root owner. A closed
// done channel is safe: the lease remains held until this goroutine observes it.
func (s *batchRootLeaseStore) adopt(root jobbatch.ID, done <-chan struct{}, p *batchRootLeasePending) error {
	if p == nil {
		return nil
	}
	if s == nil || root == "" || done == nil {
		p.release()
		return ErrDiskProviderUnavailable
	}
	s.mu.Lock()
	if prev, ok := s.entries[root]; ok {
		if prev.binding != p.binding || prev.registry != p.registry {
			s.mu.Unlock()
			p.release()
			return ErrDiskProviderUnavailable
		}
		s.mu.Unlock()
		p.release()
		return nil
	}
	e := batchRootLeaseEntry{p.binding, p.registry, p.lease}
	s.entries[root] = e
	s.mu.Unlock()
	go func() {
		<-done
		s.mu.Lock()
		if cur, ok := s.entries[root]; ok && cur.lease == e.lease {
			delete(s.entries, root)
		}
		s.mu.Unlock()
		e.lease.release()
	}()
	return nil
}

func (s *batchRootLeaseStore) matches(root jobbatch.ID, binding, registry string) bool {
	if s == nil {
		return binding == ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[root]
	if binding == "" {
		return !ok
	}
	return ok && e.binding == binding && e.registry == registry
}
