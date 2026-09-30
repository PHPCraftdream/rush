package agent

import "context"

// foreignLiveDriver reports whether another live process drives owner's
// session (a `rush run` loop whose durable session_drivers marker names a
// host that is alive or not provably dead). A nil store (isolated ledger
// tests) reports false. See session.AsyncJobStore.ForeignLiveDriver.
func (l *workLedger) foreignLiveDriver(ctx context.Context, owner string) (bool, error) {
	if l.store == nil {
		return false, nil
	}
	_, foreign, err := l.store.ForeignLiveDriver(ctx, owner)
	return foreign, err
}
