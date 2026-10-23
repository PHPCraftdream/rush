// Notice-row identity for the web bg-shell auto-turn cap (docs/async-invariants.md
// ASYNC-09; plan amendment (ad)): the pending-inclusive debt snapshot's
// notice row. The insert/debt methods of the legacy store were deleted at
// the H4 cutover; the retained batch operations live in batch_reaction.go
// and session_notice_store.go.
package session

// PendingNoticeDebt is one notice row of a pending-inclusive debt: its id
// (AUTOINCREMENT, never reused) and kind.
type PendingNoticeDebt struct {
	ID   int64
	Kind string
	// NodeID and ChildSessionID carry the published batch-question marker's
	// node and child session. LiveHeldQuestions fills them; every other
	// producer leaves them empty.
	NodeID         string
	ChildSessionID string
}
