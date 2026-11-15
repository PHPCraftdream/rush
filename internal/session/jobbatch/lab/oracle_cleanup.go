package lab

import "context"

// Physical accounting never authorizes or rejects a delegate call. Sealing
// freezes measurements only; callbacks and later cleanup still reach delegates.
func (o *Oracle) beginPhysical() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.physical++
	if !o.sealed {
		o.tick()
	}
}

func (o *Oracle) endPhysical() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.physical--
	if !o.sealed {
		o.tick()
	}
}

// Only abandoned output needs a physical join. Stop/Dispose logical timeouts
// are authoritative; held controls must remain script-owned, not spend another
// cleanup budget waiting for their physical returns.
func (o *Oracle) finishPhysical(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for o.reads != 0 {
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			o.mu.Lock()
			o.sealed = true
			return ctx.Err()
		}
		o.mu.Lock()
	}
	o.sealed = true
	return nil
}
