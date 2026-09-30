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

// DebtJobRef names one async_jobs debt row exactly as the turn saw it
// (R2A-4/R2A-5). Identity is the claim, not the tool_call_id text: a reused
// tool_call_id archives the old row (renaming its key) and a later claim puts
// a NEW row under the same text, so a text match would hit a row the model
// never saw. NoticeMessageID is the history message the pull created for the
// row; a Rerun re-pend followed by a re-pull gives the row a new one, which is
// how a late settle recognises "not the delivery the turn reacted to".
type DebtJobRef struct {
	ToolCallID      string // only disambiguates legacy rows whose claim_id is empty
	ClaimID         string
	NoticeMessageID string
}

// DebtNoticeRef is DebtJobRef's session_notices counterpart (the row id is
// AUTOINCREMENT, never reused).
type DebtNoticeRef struct {
	ID              int64
	NoticeMessageID string
}

// DebtSnapshot is the row set a Drain turn's failure handling (settle-by-
// failure, doc sec.3.4) acts on: "captured at the START of the turn", never
// re-evaluated against whatever is pending by the time a failure is
// classified. Every write made from it re-checks that the row is still the
// one captured (see DebtJobRef).
type DebtSnapshot struct {
	Jobs    []DebtJobRef
	Notices []DebtNoticeRef
}

// Empty reports whether the snapshot captured nothing (no debt existed at
// capture time, or the store is unavailable).
func (d DebtSnapshot) Empty() bool {
	return len(d.Jobs) == 0 && len(d.Notices) == 0
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
// whose whole contract is "what did the turn actually see". A permanently
// failing pull is bounded elsewhere: a Drain decides by VISIBLE debt
// (decideDrainTurn), so with only 'pending' rows it ends without a provider
// turn, and the no-turn release rule plus the 60s pass allow at most one such
// Drain per hint or pass. An empty snapshot makes settle-by-failure and
// checkStuckDrainProgress no-ops -- debt it never saw is never settled.
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
			snap.Jobs = append(snap.Jobs, DebtJobRef{ToolCallID: j.ToolCallID, ClaimID: j.ClaimID, NoticeMessageID: j.NoticeMessageID.String})
		}
	}
	for _, n := range notices {
		if n.Wake != 0 && n.Reacted == 0 && n.Delivery == "done" {
			snap.Notices = append(snap.Notices, DebtNoticeRef{ID: n.ID, NoticeMessageID: n.NoticeMessageID.String})
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
// (doc sec.3.4's settle-by-failure step 1). Each row is bumped only while it
// is still the row the snapshot saw (same claim, delivery='done', same notice
// message, wake=1, reacted=0), so a row that settled, was re-pended or voided
// by a Rerun, or was replaced by a later claim under a reused tool_call_id in
// the meantime is left alone (R2A-4/R2A-5).
func (s *AsyncJobStore) IncrementWakeAttempts(ctx context.Context, owner string, snap DebtSnapshot) error {
	if snap.Empty() {
		return nil
	}
	now := time.Now().Unix()
	for _, ref := range snap.Jobs {
		if _, err := s.q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, db.IncrementAsyncJobWakeAttemptsForSnapshotRowParams{
			UpdatedAt: now, Owner: owner, ClaimID: ref.ClaimID, ToolCallID: ref.ToolCallID, NoticeMessageID: ref.NoticeMessageID,
		}); err != nil {
			return fmt.Errorf("async job store: increment wake attempts: async_jobs: %w", err)
		}
	}
	for _, ref := range snap.Notices {
		if _, err := s.q.IncrementSessionNoticeWakeAttemptsForSnapshotRow(ctx, db.IncrementSessionNoticeWakeAttemptsForSnapshotRowParams{
			UpdatedAt: now, ID: ref.ID, NoticeMessageID: ref.NoticeMessageID,
		}); err != nil {
			return fmt.Errorf("async job store: increment wake attempts: session_notices: %w", err)
		}
	}
	return nil
}

// MaxWakeAttempts reads the highest wake_attempts currently on snap's rows
// that are STILL debt AND still the rows the snapshot saw (the same predicate
// IncrementWakeAttempts/SettleReactedFailed write under) -- rows that settled
// independently in the meantime, were re-pended/voided, or were replaced by a
// later claim are excluded, matching doc sec.3.4's "only for rows that were
// done at the moment [the failed turn] started" scoping. Returns 0 if none of
// snap's rows are still debt (nothing left to settle).
func (s *AsyncJobStore) MaxWakeAttempts(ctx context.Context, owner string, snap DebtSnapshot) (int, error) {
	max := 0
	for _, ref := range snap.Jobs {
		row, err := s.q.GetAsyncJobByClaimID(ctx, db.GetAsyncJobByClaimIDParams{
			Owner: owner, ClaimID: ref.ClaimID, ToolCallID: ref.ToolCallID,
		})
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("async job store: max wake attempts: async_jobs: %w", err)
		}
		if !snapshotRowStillDebt(row.Delivery, row.NoticeMessageID, row.Wake, row.Reacted, ref.NoticeMessageID) {
			continue
		}
		if int(row.WakeAttempts) > max {
			max = int(row.WakeAttempts)
		}
	}
	for _, ref := range snap.Notices {
		row, err := s.q.GetSessionNotice(ctx, ref.ID)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("async job store: max wake attempts: session_notices: %w", err)
		}
		if !snapshotRowStillDebt(row.Delivery, row.NoticeMessageID, row.Wake, row.Reacted, ref.NoticeMessageID) {
			continue
		}
		if int(row.WakeAttempts) > max {
			max = int(row.WakeAttempts)
		}
	}
	return max, nil
}

// snapshotRowStillDebt is the Go twin of the *ForSnapshotRow queries'
// guard: delivery='done', wake=1, reacted=0 and the snapshot's notice message.
func snapshotRowStillDebt(delivery string, noticeMessageID sql.NullString, wake, reacted int64, snapshotNoticeMessageID string) bool {
	return delivery == "done" && wake != 0 && reacted == 0 && noticeMessageID.String == snapshotNoticeMessageID
}

// SettleReactedFailed closes debt by failure (doc sec.3.4) on exactly snap's
// captured rows: reacted=1 AND reacted_failed=1, each row only while it is
// still the row the snapshot saw (see IncrementWakeAttempts), so nothing
// outside the captured set -- and no row a Rerun or a later claim replaced --
// is ever touched.
func (s *AsyncJobStore) SettleReactedFailed(ctx context.Context, owner string, snap DebtSnapshot) error {
	if snap.Empty() {
		return nil
	}
	_, err := settleSnapshotRows(ctx, s.q, owner, snap, time.Now().Unix())
	return err
}

// settleSnapshotRows settles every snapshot row that is still the row the
// snapshot saw, on q (a plain or transaction-bound Queries), and returns the
// number of rows settled across both tables.
func settleSnapshotRows(ctx context.Context, q *db.Queries, owner string, snap DebtSnapshot, now int64) (int64, error) {
	var settled int64
	for _, ref := range snap.Jobs {
		rows, err := q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, db.SettleAsyncJobReactedFailedForSnapshotRowParams{
			UpdatedAt: now, Owner: owner, ClaimID: ref.ClaimID, ToolCallID: ref.ToolCallID, NoticeMessageID: ref.NoticeMessageID,
		})
		if err != nil {
			return 0, fmt.Errorf("async job store: settle reacted failed: async_jobs: %w", err)
		}
		settled += rows
	}
	for _, ref := range snap.Notices {
		rows, err := q.SettleSessionNoticeReactedFailedForSnapshotRow(ctx, db.SettleSessionNoticeReactedFailedForSnapshotRowParams{
			UpdatedAt: now, ID: ref.ID, NoticeMessageID: ref.NoticeMessageID,
		})
		if err != nil {
			return 0, fmt.Errorf("async job store: settle reacted failed: session_notices: %w", err)
		}
		settled += rows
	}
	return settled, nil
}

// SettleReactedFailedWithMarker is A10's fix: SettleReactedFailed's two-table
// settle and the wake_failed marker notice it implies were previously two
// separate commits (the former coordinator outcome recorder calling
// SettleReactedFailed then InsertSessionNotice) -- a marker-insert failure after the settle
// committed closed debt SILENTLY (ASYNC-09), with no visible trace at all.
// This does both in ONE transaction: settle snap's captured rows on both
// tables, then insert the NoticeKindWakeFailed marker IFF at least one row
// was actually settled (settling nothing means there was nothing to explain
// a marker for -- e.g. a snapshot that already fully resolved, or whose rows a
// Rerun/a later claim replaced, by the time the failure was classified).
// Returns the total row count settled across both tables.
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

	settled, err := settleSnapshotRows(ctx, q, owner, snap, now)
	if err != nil {
		return 0, fmt.Errorf("async job store: settle reacted failed with marker: %w", err)
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
// "treat unknown as not-dead"; recovery of a dead host's rows is
// RecoverDeadHost's job -- this is read-only liveness classification). Never
// probes this store's own host id. Read-only question, so it uses the
// SHARED, non-acquiring probe (HostLiveness): an exclusive probe would see a
// concurrent shared prober's momentary hold as contention and report a
// crashed host "alive" for one evaluation. Recovery paths that must WIN the
// lock keep ProbeHost.
func (s *AsyncJobStore) HostNotDead(hostID string) bool {
	return s.hostNotDeadFor(s.HostID(), hostID)
}

// hostNotDeadFor is HostNotDead with the caller's own host id supplied, for
// code that runs inside a writer transaction and captured it beforehand.
func (s *AsyncJobStore) hostNotDeadFor(ownHostID, hostID string) bool {
	if hostID == "" {
		return false
	}
	if hostID == ownHostID {
		return true
	}
	return s.HostLiveness(hostID) != HostStatusDead
}
