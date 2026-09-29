// Reaction debt (DUR-4, docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.4, step 4): the durable `reacted` marker, the debt predicate, the
// settle-by-failure counters, the Stop-tree wake=0 pass, and the host-
// liveness/running-work readers the CLI loop's scope predicate (sec.3.5)
// needs. All of it wraps queries that already exist from step 0's schema
// (async_jobs.sql/session_notices.sql) -- this file adds no new SQL, only
// the Go orchestration around it.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// ReactionDebtExists is doc sec.3.4's one indexed EXISTS check across both
// tables (async_jobs requires announced=1 too -- see the query's own doc).
// Includes 'pending' rows -- a pull that has not succeeded yet (or is
// permanently failing) still counts as debt here. Callers deciding whether
// to run a PROVIDER TURN must use VisibleReactionDebtExists instead (review
// fix, doc sec.6): this predicate alone would let a permanently failing
// pull force an endless chain of empty-prompt turns, since 'pending' rows
// never appear in the history the turn could actually react to.
func (s *AsyncJobStore) ReactionDebtExists(ctx context.Context, owner string) (bool, error) {
	has, err := s.readQuerier().AsyncReactionDebtExists(ctx, owner)
	if err != nil {
		return false, fmt.Errorf("async job store: reaction debt check: %w", err)
	}
	return has.Bool, nil
}

// VisibleReactionDebtExists is ReactionDebtExists scoped to delivery='done'
// (doc sec.6 review fix, P1): debt already visible in history right now,
// not merely 'pending'. This is what a Drain's turn-start decision must use
// to decide whether to run the provider -- see agent_turn.go.
func (s *AsyncJobStore) VisibleReactionDebtExists(ctx context.Context, owner string) (bool, error) {
	has, err := s.readQuerier().VisibleAsyncReactionDebtExists(ctx, owner)
	if err != nil {
		return false, fmt.Errorf("async job store: visible reaction debt check: %w", err)
	}
	return has.Bool, nil
}

// HasRunningDelegationFor reports whether a RUNNING async_jobs row currently
// claims childSessionID as its delegation target (doc sec.3.4's session-
// policy table: "child session: a turn on debt only while its delegation
// row is running").
func (s *AsyncJobStore) HasRunningDelegationFor(ctx context.Context, childSessionID string) (bool, error) {
	_, err := s.q.GetRunningAsyncJobByChildSession(ctx, sql.NullString{String: childSessionID, Valid: true})
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("async job store: running delegation check: %w", err)
}

// MarkReactedWithMessageUpdate is doc sec.3.4's reaction marker: ONE
// transaction that writes the turn step's final message (persistStepFinish's
// job) AND marks reacted=1 on every wake=1/reacted=0/delivery='done' row of
// owner, on BOTH tables. Pulls only ever move a row into delivery='done'
// during PrepareStep/turn-start (before the step's own provider call), so
// every row this statement can see was already visible in the model's
// prompt by the time this step's content was produced -- no separate
// "pulled before this step began" id set needs to be threaded through (see
// the file doc for the argument in full).
//
// Callers MUST only invoke this for a step whose finish is real content
// (text/tool-calls/reasoning) and not error/canceled/empty -- see
// agent_turn_step.go's stepIsReaction gate.
func (s *AsyncJobStore) MarkReactedWithMessageUpdate(ctx context.Context, messages message.Service, owner string, msg message.Message) error {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("async job store: mark reacted: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	publish, err := messages.UpdateTx(ctx, tx, msg)
	if err != nil {
		return fmt.Errorf("async job store: mark reacted: message update: %w", err)
	}
	q := db.New(tx)
	now := time.Now().Unix()
	if _, err := q.MarkAsyncJobsReactedForOwner(ctx, db.MarkAsyncJobsReactedForOwnerParams{
		UpdatedAt: now, OwnerSessionID: owner,
	}); err != nil {
		return fmt.Errorf("async job store: mark reacted: async_jobs: %w", err)
	}
	if _, err := q.MarkSessionNoticesReactedForOwner(ctx, db.MarkSessionNoticesReactedForOwnerParams{
		UpdatedAt: now, Owner: owner,
	}); err != nil {
		return fmt.Errorf("async job store: mark reacted: session_notices: %w", err)
	}
	// A1 item 1: a real reaction (this step's own finish carries real
	// content) proves owner is alive and answering again -- any
	// reacted_failed an earlier settle-by-failure closure left on owner's
	// rows (same delegation, an earlier job; or a stale flag surviving from
	// before) is superseded now, in the SAME transaction as the reaction
	// itself, so a verdict reader never resurfaces it.
	if _, err := q.ClearReactedFailedForOwner(ctx, db.ClearReactedFailedForOwnerParams{
		UpdatedAt: now, OwnerSessionID: owner,
	}); err != nil {
		return fmt.Errorf("async job store: mark reacted: clear reacted_failed (async_jobs): %w", err)
	}
	if _, err := q.ClearSessionNoticesReactedFailedForOwner(ctx, db.ClearSessionNoticesReactedFailedForOwnerParams{
		UpdatedAt: now, Owner: owner,
	}); err != nil {
		return fmt.Errorf("async job store: mark reacted: clear reacted_failed (session_notices): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("async job store: mark reacted: commit: %w", err)
	}
	if publish != nil {
		publish()
	}
	return nil
}

// DebtSnapshot is the id set a Drain turn's failure handling (settle-by-
// failure, doc sec.3.4) acts on: "captured at the START of the turn", never
// re-evaluated against whatever is pending by the time a failure is
// classified.
type DebtSnapshot struct {
	JobIDs    []string // async_jobs.tool_call_id
	NoticeIDs []int64  // session_notices.id
}

// Empty reports whether the snapshot captured nothing (no debt existed at
// capture time, or the store is unavailable).
func (d DebtSnapshot) Empty() bool {
	return len(d.JobIDs) == 0 && len(d.NoticeIDs) == 0
}

// CaptureDebtSnapshot reads owner's current debt row ids from both tables --
// the id set a Drain turn's eventual failure handling must act on, captured
// BEFORE the turn runs (doc: "the id set is captured at the start of the
// turn", so a notice arriving mid-retry is never silently absorbed by a
// later settle).
// Doc sec.3.4: "закрытие долга неудачей ... и только для строк, которые
// были done на момент его начала" -- settle-by-failure (and the stuck-
// progress success-path variant, checkStuckDrainProgress) may only ever act
// on rows that were ALREADY VISIBLE (delivery='done') when the turn started,
// never delivery='pending'. A pending row's pull has not (yet, or ever)
// succeeded -- it was never in the model's prompt, so a failed/stuck-Drain
// classification has no evidence about it at all; including it here would
// let a permanently failing pull (a corrupt row, an unmarshalable payload,
// whatever ReactionDebtExists' own pending branch worries about) get
// wake_attempts bumped and eventually SETTLED BY FAILURE purely because a
// same-owner turn kept failing/succeeding for an unrelated reason -- closing
// debt the model never got a chance to react to. Review finding C18/B-dev1
// (docs/reviews/2026-09-29-async-phase4-round1.md): this used to be
// `Delivery != "void"` (pending+done), which is right for
// VisibleReactionDebtExists' SIBLING predicates (deciding whether a turn is
// NEEDED at all can and should count pending) but wrong for THIS capture,
// whose whole contract is "what did the turn actually see". A permanently-
// failing pull is instead bounded by checkStuckDrainProgress/
// incrementThenSettleIfThreshold observing an EMPTY snapshot forever (no
// done rows ever materialize) -- doc sec.6's "does not loop" is satisfied by
// the turn making no progress at all, not by settling debt it never saw.
// async_jobs also requires announced=1, matching AsyncReactionDebtExists'
// own guard (DUR-7: an unannounced row can never produce a notice).
func (s *AsyncJobStore) CaptureDebtSnapshot(ctx context.Context, owner string) (DebtSnapshot, error) {
	jobs, err := s.q.ListAsyncJobsForOwner(ctx, owner)
	if err != nil {
		return DebtSnapshot{}, fmt.Errorf("async job store: capture debt: async_jobs: %w", err)
	}
	notices, err := s.q.ListSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return DebtSnapshot{}, fmt.Errorf("async job store: capture debt: session_notices: %w", err)
	}
	var snap DebtSnapshot
	for _, j := range jobs {
		if j.Wake != 0 && j.Reacted == 0 && j.Delivery == "done" && j.Announced != 0 {
			snap.JobIDs = append(snap.JobIDs, j.ToolCallID)
		}
	}
	for _, n := range notices {
		if n.Wake != 0 && n.Reacted == 0 && n.Delivery == "done" {
			snap.NoticeIDs = append(snap.NoticeIDs, n.ID)
		}
	}
	return snap, nil
}

// PendingInclusiveDebtSummary reports whether owner has ANY job-id debt row
// (pending-inclusive: wake=1/reacted=0/delivery<>'void'/announced=1 -- the
// SAME scope as ReactionDebtExists) and the Kind of every notice-debt row at
// that same pending-inclusive scope. Unlike CaptureDebtSnapshot (settle-by-
// failure's delivery='done'-only scope, "what did the failed/stuck turn
// actually see"), this answers a DIFFERENT question a PRE-turn policy check
// needs: "what is this session's ENTIRE current debt, right now, before any
// turn has run" -- sessionDrainPolicy's bg-shell-only gate (coordinator_
// drain_policy.go's sessionDebtIsBGShellOnly) runs at release-recheck/60s-
// pass time, strictly BEFORE any Drain call is even submitted, so the notice
// it needs to classify is normally still 'pending' (nothing has pulled it
// yet) -- using the done-only scope there wrongly reported "no debt" for
// the single most common case (a freshly-arrived, not-yet-pulled bg-shell
// notice), silently skipping the AutoResumeOnJobDone=off refusal (found via
// the full internal/agent suite after the C18/B-dev1 fix landed).
func (s *AsyncJobStore) PendingInclusiveDebtSummary(ctx context.Context, owner string) (hasJobDebt bool, noticeKinds []string, err error) {
	jobs, err := s.q.ListAsyncJobsForOwner(ctx, owner)
	if err != nil {
		return false, nil, fmt.Errorf("async job store: pending-inclusive debt summary: async_jobs: %w", err)
	}
	for _, j := range jobs {
		if j.Wake != 0 && j.Reacted == 0 && j.Delivery != "void" && j.Announced != 0 {
			hasJobDebt = true
			break
		}
	}
	notices, err := s.q.ListSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return false, nil, fmt.Errorf("async job store: pending-inclusive debt summary: session_notices: %w", err)
	}
	for _, n := range notices {
		if n.Wake != 0 && n.Reacted == 0 && n.Delivery != "void" {
			noticeKinds = append(noticeKinds, n.Kind)
		}
	}
	return hasJobDebt, noticeKinds, nil
}

// IncrementWakeAttempts bumps wake_attempts on exactly snap's captured rows
// (doc sec.3.4's settle-by-failure step 1), guarded server-side to rows
// still wake=1/reacted=0 so a row that settled in the meantime (a real turn,
// or an earlier settle) is left alone.
func (s *AsyncJobStore) IncrementWakeAttempts(ctx context.Context, owner string, snap DebtSnapshot) error {
	if snap.Empty() {
		return nil
	}
	now := time.Now().Unix()
	if len(snap.JobIDs) > 0 {
		if _, err := s.q.IncrementAsyncJobWakeAttempts(ctx, db.IncrementAsyncJobWakeAttemptsParams{
			UpdatedAt: now, OwnerSessionID: owner, ToolCallIds: snap.JobIDs,
		}); err != nil {
			return fmt.Errorf("async job store: increment wake attempts: async_jobs: %w", err)
		}
	}
	if len(snap.NoticeIDs) > 0 {
		if _, err := s.q.IncrementSessionNoticeWakeAttempts(ctx, db.IncrementSessionNoticeWakeAttemptsParams{
			UpdatedAt: now, Ids: snap.NoticeIDs,
		}); err != nil {
			return fmt.Errorf("async job store: increment wake attempts: session_notices: %w", err)
		}
	}
	return nil
}

// MaxWakeAttempts reads the highest wake_attempts currently on snap's rows
// that are STILL debt (wake=1/reacted=0) -- rows that settled independently
// in the meantime (a real turn reacted, or an earlier pass already settled
// them) are excluded, matching doc sec.3.4's "only for rows that were done
// at the moment [the failed turn] started" scoping. Returns 0 if none of
// snap's rows are still debt (nothing left to settle).
func (s *AsyncJobStore) MaxWakeAttempts(ctx context.Context, owner string, snap DebtSnapshot) (int, error) {
	max := 0
	for _, id := range snap.JobIDs {
		row, err := s.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: owner, ToolCallID: id})
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("async job store: max wake attempts: async_jobs: %w", err)
		}
		if row.Wake == 0 || row.Reacted != 0 {
			continue
		}
		if int(row.WakeAttempts) > max {
			max = int(row.WakeAttempts)
		}
	}
	for _, id := range snap.NoticeIDs {
		row, err := s.q.GetSessionNotice(ctx, id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("async job store: max wake attempts: session_notices: %w", err)
		}
		if row.Wake == 0 || row.Reacted != 0 {
			continue
		}
		if int(row.WakeAttempts) > max {
			max = int(row.WakeAttempts)
		}
	}
	return max, nil
}

// SettleReactedFailed closes debt by failure (doc sec.3.4) on exactly snap's
// captured rows: reacted=1 AND reacted_failed=1, scoped server-side to
// wake=1/reacted=0 so nothing outside the captured set is ever touched.
func (s *AsyncJobStore) SettleReactedFailed(ctx context.Context, owner string, snap DebtSnapshot) error {
	if snap.Empty() {
		return nil
	}
	now := time.Now().Unix()
	if len(snap.JobIDs) > 0 {
		if _, err := s.q.SettleAsyncJobsReactedFailed(ctx, db.SettleAsyncJobsReactedFailedParams{
			UpdatedAt: now, OwnerSessionID: owner, ToolCallIds: snap.JobIDs,
		}); err != nil {
			return fmt.Errorf("async job store: settle reacted failed: async_jobs: %w", err)
		}
	}
	if len(snap.NoticeIDs) > 0 {
		if _, err := s.q.SettleSessionNoticesReactedFailed(ctx, db.SettleSessionNoticesReactedFailedParams{
			UpdatedAt: now, Ids: snap.NoticeIDs,
		}); err != nil {
			return fmt.Errorf("async job store: settle reacted failed: session_notices: %w", err)
		}
	}
	return nil
}

// SettleReactedFailedWithMarker is A10's fix: SettleReactedFailed's two-table
// settle and the wake_failed marker notice it implies were previously two
// separate commits (coordinator.recordDrainOutcome calling SettleReactedFailed
// then InsertSessionNotice) -- a marker-insert failure after the settle
// committed closed debt SILENTLY (ASYNC-09), with no visible trace at all.
// This does both in ONE transaction: settle snap's captured rows on both
// tables, then insert the NoticeKindWakeFailed marker IFF at least one row
// was actually settled (settling nothing means there was nothing to explain
// a marker for -- e.g. a snapshot that already fully resolved by the time
// the failure was classified). Returns the total row count settled across
// both tables.
func (s *AsyncJobStore) SettleReactedFailedWithMarker(ctx context.Context, owner string, snap DebtSnapshot, markerText string) (int64, error) {
	if snap.Empty() {
		return 0, nil
	}
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("async job store: settle reacted failed with marker: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)
	now := time.Now().Unix()

	var settled int64
	if len(snap.JobIDs) > 0 {
		rows, err := q.SettleAsyncJobsReactedFailed(ctx, db.SettleAsyncJobsReactedFailedParams{
			UpdatedAt: now, OwnerSessionID: owner, ToolCallIds: snap.JobIDs,
		})
		if err != nil {
			return 0, fmt.Errorf("async job store: settle reacted failed with marker: async_jobs: %w", err)
		}
		settled += rows
	}
	if len(snap.NoticeIDs) > 0 {
		rows, err := q.SettleSessionNoticesReactedFailed(ctx, db.SettleSessionNoticesReactedFailedParams{
			UpdatedAt: now, Ids: snap.NoticeIDs,
		})
		if err != nil {
			return 0, fmt.Errorf("async job store: settle reacted failed with marker: session_notices: %w", err)
		}
		settled += rows
	}
	if settled > 0 {
		if _, err := q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{
			Owner: owner, Kind: NoticeKindWakeFailed, Text: markerText,
			Wake: 0, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return 0, fmt.Errorf("async job store: settle reacted failed with marker: insert marker: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("async job store: settle reacted failed with marker: commit: %w", err)
	}
	return settled, nil
}

// ReactedFailedText is one settle-by-failure-closed row's text, for the
// parent-notification path (doc sec.3.4: "a child session with a failure-
// closed debt ... hands the parent the text of the unreacted notices").
type ReactedFailedText struct {
	ToolCallID string // "" for a session_notices-origin row
	Text       string
}

// ListReactedFailedText returns the text of every row settle-by-failure
// closed for owner (async_jobs.result_summary / session_notices.text),
// oldest first within each table.
func (s *AsyncJobStore) ListReactedFailedText(ctx context.Context, owner string) ([]ReactedFailedText, error) {
	jobs, err := s.q.ListReactedFailedAsyncJobsForOwner(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("async job store: list reacted-failed: async_jobs: %w", err)
	}
	notices, err := s.q.ListReactedFailedSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("async job store: list reacted-failed: session_notices: %w", err)
	}
	out := make([]ReactedFailedText, 0, len(jobs)+len(notices))
	for _, j := range jobs {
		out = append(out, ReactedFailedText{ToolCallID: j.ToolCallID, Text: j.ResultSummary.String})
	}
	for _, n := range notices {
		out = append(out, ReactedFailedText{Text: n.Text})
	}
	return out, nil
}

// SetWakeZeroForOwners is Stop-tree transitivity's wake=0 pass (doc
// sec.3.4/3.8, DUR-9): every pending/done row of owners loses its wake bit
// in the same pass, closing the race between a natural completion and Stop.
// Scoped separately per table (no combined transaction required -- neither
// side depends on the other's commit for correctness, and a partial
// failure just leaves that table's rows to a later retry of the same
// idempotent pass).
func (s *AsyncJobStore) SetWakeZeroForOwners(ctx context.Context, owners []string) error {
	if len(owners) == 0 {
		return nil
	}
	now := time.Now().Unix()
	if _, err := s.q.SetAsyncJobsWakeZeroPendingForOwners(ctx, db.SetAsyncJobsWakeZeroPendingForOwnersParams{
		UpdatedAt: now, OwnerIds: owners,
	}); err != nil {
		return fmt.Errorf("async job store: set wake zero: async_jobs: %w", err)
	}
	if _, err := s.q.SetSessionNoticesWakeZeroPendingForOwners(ctx, db.SetSessionNoticesWakeZeroPendingForOwnersParams{
		UpdatedAt: now, OwnerIds: owners,
	}); err != nil {
		return fmt.Errorf("async job store: set wake zero: session_notices: %w", err)
	}
	return nil
}

// RunningJobRow is a minimal snapshot of a running async_jobs row for scope
// evaluation (doc sec.3.5): just enough to probe the owning host's liveness.
type RunningJobRow struct {
	Owner      string
	ToolCallID string
	HostID     string
}

// ListRunningForOwners lists every currently-running async_jobs row for
// owners (doc sec.3.5's scope predicate: "a running task row on a LIVE
// host"; liveness itself is decided by HostNotDead, not by this query).
func (s *AsyncJobStore) ListRunningForOwners(ctx context.Context, owners []string) ([]RunningJobRow, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListRunningAsyncJobsForOwners(ctx, owners)
	if err != nil {
		return nil, fmt.Errorf("async job store: list running for owners: %w", err)
	}
	out := make([]RunningJobRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, RunningJobRow{Owner: r.OwnerSessionID, ToolCallID: r.ToolCallID, HostID: r.HostID})
	}
	return out, nil
}

// HostNotDead reports whether hostID is not PROVABLY dead (doc sec.3.5/3.6:
// "treat unknown as not-dead"; the actual recovery of a dead host's rows is
// step 5, deferred -- this is read-only liveness classification). Never
// probes this store's own host id (ProbeHost's guard). A probe that WINS the
// lock (status dead, lock non-nil) releases it immediately without deleting
// the file -- this function only answers a liveness question, it never
// performs recovery.
func (s *AsyncJobStore) HostNotDead(hostID string) bool {
	if hostID == "" {
		return false
	}
	if hostID == s.HostID() {
		return true
	}
	status, lock, err := ProbeHost(s.dataDir, hostID)
	if lock != nil {
		_ = lock.Release()
	}
	if err != nil {
		return true // unknown -> not dead (doc sec.3.6)
	}
	return status != HostStatusDead
}
