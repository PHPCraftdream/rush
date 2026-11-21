// Rehomed retained in-process state from the deleted workLedger
// (work_ledger_reaction.go, work_ledger_driver.go): the per-session fact-hint
// counter + waiter set, the external-driver registration mirror, the durable
// foreign-driver read, and the SessionNoticeStore binding the completion
// notice writers (supervision tick, reaction-chain marker, bg-shell done,
// held child question) use. None of this is ledger state: the hint counter
// and the driver mirror are per-process coordination memory that outlives the
// ledger, the driver READ is the retained session_drivers host marker, and
// session_notices is retained notice infrastructure.
package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
)

// foreignLiveDriver reports whether another live process drives owner's
// session (a `rush run` loop whose durable session_drivers marker names a
// host that is alive or not provably dead). Rehome of work_ledger_driver.go:
// the read goes through the bound HostStore (SetBatchDriverClaimer wires it;
// the claim/release and the foreign read are the same store in production).
// A coordinator without a HostStore-shaped claimer (bare fixtures) reports
// false -- the conservative answer is live/fact-driven, but a fixture has no
// other process to be foreign.
func (c *coordinator) foreignLiveDriver(ctx context.Context, owner string) (bool, error) {
	return (&batchRehomeCoordRuntimeComponent{host: c}).foreignLiveDriver(ctx, owner)
}

// sessionNoticeStore returns the bound SessionNoticeStore, or nil.
func (c *coordinator) sessionNoticeStore() *session.SessionNoticeStore {
	return (&batchRehomeCoordRuntimeComponent{host: c}).sessionNoticeStore()
}

type batchRehomeCoordRuntimeHost interface {
	foreignDriverClaimHostStore() *sessionhosts.HostStore
	boundSessionNoticeStore() *session.SessionNoticeStore
}

type batchRehomeCoordRuntimeComponent struct{ host batchRehomeCoordRuntimeHost }

func (w *batchRehomeCoordRuntimeComponent) foreignLiveDriver(ctx context.Context, owner string) (bool, error) {
	hs := w.host.foreignDriverClaimHostStore()
	if hs == nil {
		return false, nil
	}
	_, foreign, err := hs.ForeignLiveDriver(ctx, owner)
	return foreign, err
}

func (w *batchRehomeCoordRuntimeComponent) sessionNoticeStore() *session.SessionNoticeStore {
	return w.host.boundSessionNoticeStore()
}

func (c *coordinator) foreignDriverClaimHostStore() *sessionhosts.HostStore {
	if c == nil {
		return nil
	}
	hs, ok := c.batchDriverClaimer.(*sessionhosts.HostStore)
	if !ok {
		return nil
	}
	return hs
}

func (c *coordinator) boundSessionNoticeStore() *session.SessionNoticeStore {
	if c == nil {
		return nil
	}
	return c.noticeStore
}
