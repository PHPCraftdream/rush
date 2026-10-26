// Retained session-notice snapshot types (H4 cutover extraction from the
// legacy notice-pull machinery, doc sec.3.3): the lightweight row snapshots
// and the session-notice kind vocabulary every consumer shares. The pull
// itself lives on SessionNoticeStore (session_notice_store.go) and on the
// batch outbox (batch_pull.go); the raw generated db rows never cross this
// package boundary (package agent does not import internal/db anywhere,
// and this keeps it that way).
package session

import (
	"github.com/PHPCraftdream/rush/internal/message"
)

// Session-notice kind vocabulary (doc sec.2/3.4): shared between the
// inserter (supervision.go, work_ledger_timeout.go, coordinator_background.go
// in package agent) and the pull-side void-condition switches, so both sides
// agree on the exact string without duplicating a magic constant.
// NoticeKindWakeOnly/NoticeKindSupervision double as the message-level
// NoticeKind value too (message.NoticeKind's existing vocabulary);
// NoticeKindBGShellDone does NOT (see its own doc).
const (
	// NoticeKindWakeOnly matches message.NoticeKind's existing
	// "timeout_wake_only" value directly.
	NoticeKindWakeOnly = "timeout_wake_only"
	// NoticeKindSupervision matches message.NoticeKind's existing
	// "supervision" value directly.
	NoticeKindSupervision = "supervision"
	// NoticeKindWakeFailed matches message.NoticeKind's existing
	// "wake_failed" value directly.
	NoticeKindWakeFailed = "wake_failed"
	// NoticeKindBGShellDone is the session_notices bookkeeping label for an
	// SDK background-shell completion. Deliberately NOT reused as the
	// message-level NoticeKind (which stays "" for this case, as it always
	// has -- BackgroundJobNotice=true alone already satisfies the "every
	// notice message carries BackgroundJobNotice or a non-empty NoticeKind"
	// invariant); callers building the notice message must map this kind to
	// "" themselves.
	NoticeKindBGShellDone = "bg_shell_done"
	// NoticeKindChildQuestion announces a delegated child paused on a
	// question it asked while its delegation is still held (#1157). Bound to
	// the work row via job_tool_call_id and voided exactly like wake_only:
	// if the work reached terminal before the pull, the release itself
	// carries the question.
	NoticeKindChildQuestion = "child_question"
	// NoticeKindReactionChain is the marker persisted once when the reaction
	// chain guard (#1113) stops a session's automatic turns: like
	// wake_failed it describes an outcome, so Rerun voids it instead of
	// re-pending it.
	NoticeKindReactionChain = "reaction_chain"
)

// JobNoticeRow is what a pulled work row hands its caller's build callback:
// everything agent.FormatAsyncCompletion needs, taken from the row itself
// (tool_name/timeout_seconds) rather than recomputed elsewhere.
type JobNoticeRow struct {
	ToolCallID string
	ToolName   string
	// ClaimID is the row's claim_id (the key the reaction chain guard tracks
	// idle launches by; empty on legacy rows that never had one).
	ClaimID        string
	NoticeKind     string
	TimeoutSeconds int
	State          string
	ResultContent  string
	ResultIsError  bool
	OriginCLI      bool
}

// SessionNoticeRow is what a pulled session_notices row hands its caller's
// build callback.
type SessionNoticeRow struct {
	ID   int64
	Kind string
	Text string
}

// PulledNotice is one row actually turned into a history message (never
// returned for a voided row).
type PulledNotice struct {
	Message message.Message
	Wake    bool
	// ClaimID is the work row's claim_id (empty for session_notices rows):
	// the reaction chain guard's key for its idle launches.
	ClaimID string
}

// DebtNoticeRef is one session_notices debt row reference (the row id is
// AUTOINCREMENT, never reused): the settle-by-failure accounting acts only
// on rows still matching this identity.
type DebtNoticeRef struct {
	ID              int64
	NoticeMessageID string
}
