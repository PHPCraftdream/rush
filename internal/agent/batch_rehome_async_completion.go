// Batch rehome (H4 lane P5, phase A): AsyncCompletion moved verbatim out of
// work_ledger.go before its deletion. The type is the retained neutral
// completion carrier: agent_notice_pull.go builds it from pulled batch
// notices, coordinator_background.go's FormatAsyncCompletion formats it, and
// the batch reaction/outcome tests (batch_agent_reaction_scope_test.go,
// batch_notice_presentation_test.go, batch_timeout_wake_parity_test.go) plus
// package app (agent.FormatAsyncCompletion) consume it.
package agent

// AsyncCompletion is the result of a tool that returned before its work ended.
type AsyncCompletion struct {
	SessionID  string
	ToolCallID string
	ToolName   string
	Content    string
	IsError    bool
	// cli is the origin of the job that produced this completion. Set only
	// on the wake-callback path, so the woken turn keeps the job's origin.
	cli bool
	// TimedOut is true when the job's terminal state was the timed-out
	// phase. Selects the contract's timeout wording and NoticeKind instead
	// of the generic finished/failed one.
	TimedOut bool
	// TimeoutSeconds is the job's requested timeout, quoted verbatim in the
	// timeout text; meaningless when !TimedOut.
	TimeoutSeconds int
	// Stopped is true when the job was stopped by an explicit plain-job
	// stop call (task #1023 §2.2) -- never true for a delegation's
	// session-cancel stop, which keeps its own pre-existing "sub-agent
	// canceled" wording. Selects the contract's "stopped" wording and
	// NoticeKind.
	Stopped bool
	// Cancelled is true when the job was cancelled by Stop (not by an
	// explicit job stop call, see Stopped above): a plain job cancelled by
	// Stop needs its own wording instead of falling into the generic
	// finished/failed branch.
	Cancelled bool
	// Interrupted is true for a row a dead-host recovery sweep transitioned
	// to 'interrupted' (doc sec.3.7, step 5): the row's own host process
	// ended before the job finished naturally. Only produced by
	// buildJobNoticeMessageParams reading a pulled row's NoticeKind.
	Interrupted bool
	// Metadata carries the inner tool's raw ToolResponse.Metadata through to
	// a sync job's result (§4.4) so the sync await can reconstruct a
	// byte-for-byte response. Unused by every async delivery, which only
	// ever formats Content/IsError into a human notice.
	Metadata string
	// Wake is the row's own committed wake bit (phase-4 step 3, doc
	// sec.3.4's wake-policy table) for a non-sync job: true for a natural
	// finish/timeout-terminated/delegation-release, false for Stop/job stop/
	// a failed wake-up marker. notifyAsyncCompletion reads this to decide
	// whether the completion's non-blocking Drain HINT is worth submitting
	// at all -- the pull itself (driver-owned, turn start/PrepareStep)
	// always happens regardless of Wake, this field only gates the HINT.
	// Always false for a sync job (no DB row, no wake concept).
	Wake bool
}
