package cmd

import "sync"

type processStopTrace struct {
	mu   sync.Mutex
	data []byte
}

func (b *processStopTrace) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 32768
	if len(b.data) < limit {
		n := min(len(p), limit-len(b.data))
		b.data = append(b.data, p[:n]...)
	}
	return len(p), nil
}

func (b *processStopTrace) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
