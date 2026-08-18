// Mailbox submitted-queue primitives: queue, the pop accessors, the
// epoch-guarded abandon-and-pop combinations, and clearAll. Pure code
// move from mailbox.go.

package agent

// clearAll implements design §4's "ClearQueue is the one intentional
// drop-everything operation": clears submitted, replacement, and injects
// together under mu. It does NOT release ownership (state/current/
// dispatcherCancel are untouched) — the owner is still running and merely
// wants its pending queues discarded, not its reservation yanked.
func (mb *mailbox) clearAll() {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.submitted = nil
	mb.replacement = nil
	mb.injects = nil
}

// queue appends call to the submitted queue regardless of ownership state.
// Used by QueueMessage (the fire-and-forget queue primitive) and by the
// re-queue paths in Run's defer and runSummarize that used to target the
// legacy messageQueue. When the session is idle, the entry survives in
// submitted until the next submit() becomes owner and drains it via
// drainOrReleaseFinal.
func (mb *mailbox) queue(call SessionAgentCall) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.submitted = append(mb.submitted, call)
}

// popFirstSubmitted removes and returns the first entry from the submitted
// queue, regardless of mailbox state. Used by runSummarize to extract the
// first queued entry after abandonOwnership left work in submitted with
// state mbIdle, so it can start a fresh Run for it.
func (mb *mailbox) popFirstSubmitted() (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.submitted) == 0 {
		return SessionAgentCall{}, false
	}
	first := mb.submitted[0]
	mb.submitted = mb.submitted[1:]
	return first, true
}

// abandonOwnershipAndPopSubmitted atomically releases mailbox ownership and
// returns ALL entries currently in the submitted queue. This is the atomic
// combination of abandonOwnership() and popAllSubmitted() that closes the
// era-boundary reordering gap described in popAllSubmitted's doc comment.
//
// epoch must be the value submit() returned when granting the caller ownership.
// If the mailbox's current epoch has since moved on — a different, later caller
// became owner — this is a safe no-op that returns nil and touches nothing,
// since whatever is in submitted now belongs to that later owner.
//
// This method holds mb.mu for the entire operation: it first checks the epoch,
// then folds any pending replacement into submitted, flips state to mbIdle,
// clears current.cancel/dispatcherCancel, and finally copies and clears the
// submitted queue — all without releasing mb.mu in between. This ensures that
// no concurrent submit() can become the new owner and add work to submitted
// after the epoch check but before the pop, preventing the race where work from
// a new era would be handed to restartOrphanedWithRetry instead of being left
// for the new owner's own drain.
//
// Returns nil (not an empty slice) when there is no queued work, matching
// popAllSubmitted's semantics for consistency with existing callers and tests.
func (mb *mailbox) abandonOwnershipAndPopSubmitted(epoch uint64) []SessionAgentCall {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return nil
	}
	// Fold any pending replacement into submitted so the next owner
	// treats it as a normal queued follow-up, not as a pre-emption
	// target for reclaimReplacementOrKeep. A replacement left in
	// mb.replacement across an era boundary would silently jump the
	// queue of whatever the next owner's first turn happens to be.
	if mb.replacement != nil {
		mb.submitted = append(mb.submitted, *mb.replacement)
		mb.replacement = nil
	}
	// Copy the submitted queue BEFORE clearing it.
	var all []SessionAgentCall
	if len(mb.submitted) > 0 {
		all = make([]SessionAgentCall, len(mb.submitted))
		copy(all, mb.submitted)
		mb.submitted = mb.submitted[:0]
	}
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil
	return all
}

// abandonOwnershipAndPopFirstSubmitted atomically releases mailbox
// ownership and returns the FIRST entry in the submitted queue, leaving
// any remaining entries in place for the next owner's own end-of-turn
// drain. This is the popFirstSubmitted counterpart to
// abandonOwnershipAndPopSubmitted above: runSummarize's manual-compaction
// success path (agent.go) needs exactly popFirstSubmitted's existing
// "pop only the first entry, leave the rest queued" semantics, but
// sequencing abandonOwnership() and popFirstSubmitted() as two separate
// lock acquisitions there reopened the identical era-boundary reordering
// window abandonOwnershipAndPopSubmitted was added to close for the
// "pop all" case — found during the closing review of the release-
// readiness round, since P2-5's fix only touched
// abandonOwnershipWithHandoff and this second call site was missed.
//
// epoch must be the value submit() returned when granting the caller
// ownership. If the mailbox's current epoch has since moved on, this is a
// safe no-op that returns (zero value, false) and touches nothing.
func (mb *mailbox) abandonOwnershipAndPopFirstSubmitted(epoch uint64) (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return SessionAgentCall{}, false
	}
	if mb.replacement != nil {
		mb.submitted = append(mb.submitted, *mb.replacement)
		mb.replacement = nil
	}
	mb.state = mbIdle
	mb.current.cancel = nil
	mb.dispatcherCancel = nil

	if len(mb.submitted) == 0 {
		return SessionAgentCall{}, false
	}
	first := mb.submitted[0]
	mb.submitted = mb.submitted[1:]
	return first, true
}

// popAllSubmitted removes and returns ALL entries currently in the
// submitted queue, regardless of mailbox state. Used by
// abandonOwnershipWithHandoff to start detached runs for all work left in
// the mailbox after a non-cancel error.
//
// IMPORTANT: This method does NOT check epochs. If called outside the
// abandonOwnershipWithHandoff path (or any other path that does not
// already hold exclusive control over the mailbox's era), it can return
// work that belongs to a later ownership era. For finalizer paths that
// need epoch safety, use abandonOwnershipAndPopSubmitted instead, which
// combines abandonOwnership and popAllSubmitted into a single atomic
// operation that checks the epoch before popping.
//
// The era-boundary reordering gap that this doc comment originally
// described (see the comment preserved below for historical context) is
// now closed by abandonOwnershipAndPopSubmitted, which abandonOwnershipWithHandoff
// uses exclusively. Callers should NOT manually sequence abandonOwnership
// followed by popAllSubmitted, as that would re-open the gap.
//
// HISTORICAL CONTEXT (preserved for reference):
//
// popFirstSubmitted/popAllSubmitted remain the only submitted-queue
// mutators that take no epoch argument (contrast drainOrRelease/
// drainOrReleaseFinal, which reject a stale epoch). This was previously
// documented as safe because "no new submit can land on mbIdle — they all
// queue into submitted under the same lock" — that statement describes
// submit()'s own behavior correctly, but does NOT cover queue() (used by
// QueueMessage and the legacy re-queue paths), which appends to submitted
// unconditionally regardless of mailbox state.
//
// The actual, narrower guarantee: abandonOwnershipWithHandoff calls
// mb.abandonOwnership(epoch) and this method as TWO SEPARATE lock
// acquisitions, not one atomic section. Between them, a new submit() can
// legitimately become the new owner (mbIdle -> mbOwned, epoch bumped), and
// if that NEW owner's session then receives an unrelated queue() call
// (e.g. a concurrent QueueMessage for the same session) before this method
// runs, popAllSubmitted has no epoch check to exclude it — it will
// scoop up an entry that logically belongs to the new owner's era and
// hand it to restartOrphanedWithRetry instead of leaving it for the new
// owner's own end-of-turn drain.
//
// As of this writing that is a REORDERING defect, not a data-loss one: the
// call still gets a runner (restartOrphanedWithRetry durably enqueues it,
// task #340), just via the detached path instead of the new owner's normal
// turn. If a future change makes that distinction matter (e.g. ordering
// guarantees callers start depending on), give popAllSubmitted the same
// epoch parameter drainOrRelease/drainOrReleaseFinal already have, rather
// than assuming this doc comment alone keeps the two in sync.
func (mb *mailbox) popAllSubmitted() []SessionAgentCall {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.submitted) == 0 {
		return nil
	}
	all := make([]SessionAgentCall, len(mb.submitted))
	copy(all, mb.submitted)
	mb.submitted = mb.submitted[:0]
	return all
}
