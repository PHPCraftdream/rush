// Mailbox message-inject path (design §5): inject, the atomic
// busy-check-plus-enqueue injectIfBusy, and the generation-aware
// drainInjects. Pure code move from mailbox.go.

package agent

import (
	"github.com/PHPCraftdream/rush/internal/message"
)

// inject implements design §5: appends msg to the pending-inject list,
// stamping it with the mailbox's CURRENT generation id at submit time so
// drainInjects can later decide which generation(s) are responsible for
// splicing it into prepared.Messages. 0 (no owner) is a valid, meaningful
// stamp.
func (mb *mailbox) inject(msg message.Message) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.injects = append(mb.injects, pendingInject{
		msg:        msg,
		afterGenID: mb.current.id,
	})
}

// injectIfBusy is the atomic busy-check-plus-enqueue operation InjectMessage
// needs (design §5, stage 2.4 of the mailbox migration): it checks whether
// the mailbox currently has an owner and, if so, appends msg to the
// pending-inject list stamped with the current generation id — all under a
// single mb.mu hold. This closes the P1-1 race where the old code's separate
// IsSessionBusy check and injectQueue.Append allowed the owner to finish and
// release to mbIdle between the two operations, stranding the message in the
// queue with nobody left to drain it (or, from the other direction,
// duplicating it when the next Run's preamble DB read already included the
// row).
//
// Returns true when the message was queued (session was busy); false when the
// session was idle, in which case the message lives only in the DB and will
// be picked up by the next Run's natural preamble DB read.
//
// This method does NOT change ownership state (state/current/dispatcherCancel
// are untouched) and therefore does NOT appear in mailbox_invariant_test.go's
// postcondition table — that table covers operations that hand work to a turn
// loop or end an era, which injectIfBusy deliberately does neither.
//
// mbReleasing is treated the same as mbIdle here — unlike IsSessionBusy/
// submit (gated on OS-lock availability, see the mbReleasing const's doc),
// injectIfBusy only cares whether a live generation loop will ever call
// drainInjects again. The CRITICAL INVARIANT in drainOrReleaseFinal's doc
// guarantees it never will once mbReleasing is entered, so queuing here
// would strand the message in mb.injects — nobody drains it until some
// unrelated future generation does, risking a duplicate splice on top of
// the natural DB read that already covers the mbIdle case.
func (mb *mailbox) injectIfBusy(msg message.Message) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.state != mbOwned {
		return false
	}
	mb.injects = append(mb.injects, pendingInject{
		msg:        msg,
		afterGenID: mb.current.id,
	})
	return true
}

// drainInjects implements design §5: called by the owner's PrepareStep
// (replacing today's unconditional injectQueue.TakeAll) with the
// generation id of the turn currently preparing. Entries stamped at or
// before genID are due now; entries stamped against a strictly future
// generation id are kept for later (not possible via today's call sites,
// kept for forward-compat per the design doc).
func (mb *mailbox) drainInjects(genID uint64) []pendingInject {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	var due, later []pendingInject
	for _, inj := range mb.injects {
		if inj.afterGenID <= genID {
			due = append(due, inj)
		} else {
			later = append(later, inj)
		}
	}
	mb.injects = later
	return due
}
