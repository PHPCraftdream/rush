package agent

import "sync"

// Подписка использует тот же hint ledger; после сигнала факты перечитываются.
func (h *batchDriverHints) subscribe(owner string) (<-chan struct{}, func()) {
	if h == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.waiters == nil {
		h.waiters = make(map[string]map[chan struct{}]struct{})
	}
	if h.waiters[owner] == nil {
		h.waiters[owner] = make(map[chan struct{}]struct{})
	}
	h.waiters[owner][ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { h.mu.Lock(); delete(h.waiters[owner], ch); h.mu.Unlock() }) }
}
