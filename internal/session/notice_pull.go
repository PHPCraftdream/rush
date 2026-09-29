// The phase-4 durable-core notice pull (DUR-3, docs/plans/2026-09-28-async-
// phase4-durable-core.md sec.3.3, step 3): the driver (package agent) calls
// PullJobNotices/PullSessionNotices at turn start (before history loads)
// and at each step boundary of a normal turn (PrepareStep) -- never during
// a compaction step. Each pending row is pulled in its OWN transaction:
// CAS delivery pending->done (voiding instead, for a session_notices row
// whose task-still-running condition fails), INSERT the history message via
// message.Service.CreateTx in the SAME transaction, record
// notice_message_id, commit, then publish the CreatedEvent (PublishCreated)
// AFTER the commit. A pull error never fails the caller's turn -- the row
// stays pending and is retried on the next pull.
//
// Exposes only lightweight snapshot types (JobNoticeRow/SessionNoticeRow),
// never the raw generated db.AsyncJob/db.SessionNotice: package agent does
// not import internal/db anywhere, and this keeps it that way.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// Session-notice kind vocabulary (doc sec.2/3.4): shared between the
// inserter (supervision.go, work_ledger_timeout.go, coordinator_background.go
// in package agent) and pullOneSessionNotice's own void-condition switch
// below, so both sides agree on the exact string without duplicating a
// magic constant. NoticeKindWakeOnly/NoticeKindSupervision double as the
// message-level NoticeKind value too (message.NoticeKind's existing
// vocabulary); NoticeKindBGShellDone does NOT (see its own doc).
const (
	// NoticeKindWakeOnly matches message.NoticeKind's existing
	// "timeout_wake_only" value directly (work_ledger_timeout.go already
	// used this exact string before step 3).
	NoticeKindWakeOnly = "timeout_wake_only"
	// NoticeKindSupervision matches message.NoticeKind's existing
	// "supervision" value directly (supervision.go's noticeKindSupervision).
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
)

// JobNoticeRow is what a pulled async_jobs row hands its caller's build
// callback: everything agent.FormatAsyncCompletion needs, taken from the
// row itself (tool_name/timeout_seconds, step 3) rather than recomputed
// elsewhere.
type JobNoticeRow struct {
	ToolCallID     string
	ToolName       string
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
}

// PullJobNotices pulls every pending, announced async_jobs notice row for
// owner (oldest first) into session history. A row another leader already
// won (0 rows from the CAS) is silently skipped. A per-row error is logged
// and skipped -- the row stays pending, never failing the whole pull.
func (s *AsyncJobStore) PullJobNotices(ctx context.Context, messages message.Service, owner string, build func(JobNoticeRow) message.CreateMessageParams) ([]PulledNotice, error) {
	rows, err := s.q.ListPendingAsyncJobNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []PulledNotice
	for _, row := range rows {
		pulled, ok, err := s.pullOneJobNotice(ctx, messages, owner, row, build)
		if err != nil {
			slog.Warn("async job store: pull job notice failed; row stays pending",
				"session_id", owner, "tool_call_id", row.ToolCallID, "err", err)
			continue
		}
		if ok {
			out = append(out, pulled)
		}
	}
	return out, nil
}

func (s *AsyncJobStore) pullOneJobNotice(ctx context.Context, messages message.Service, owner string, row db.AsyncJob, build func(JobNoticeRow) message.CreateMessageParams) (PulledNotice, bool, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return PulledNotice{}, false, err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)

	pulledRow, err := q.PullPendingAsyncJobNotice(ctx, db.PullPendingAsyncJobNoticeParams{
		UpdatedAt: time.Now().Unix(), OwnerSessionID: owner, ToolCallID: row.ToolCallID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PulledNotice{}, false, nil // another leader already won this row
	}
	if err != nil {
		return PulledNotice{}, false, err
	}

	params := build(JobNoticeRow{
		ToolCallID: pulledRow.ToolCallID, ToolName: pulledRow.ToolName,
		NoticeKind: pulledRow.NoticeKind, TimeoutSeconds: int(pulledRow.TimeoutSeconds),
		State: pulledRow.State, ResultContent: pulledRow.ResultSummary.String,
		ResultIsError: pulledRow.ResultIsError.Int64 != 0, OriginCLI: pulledRow.OriginCli != 0,
	})
	msg, err := messages.CreateTx(ctx, tx, owner, params)
	if err != nil {
		return PulledNotice{}, false, err
	}
	if _, err := q.SetAsyncJobNoticeMessageID(ctx, db.SetAsyncJobNoticeMessageIDParams{
		NoticeMessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix(),
		OwnerSessionID: owner, ToolCallID: pulledRow.ToolCallID,
	}); err != nil {
		return PulledNotice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PulledNotice{}, false, err
	}
	// Publish AFTER commit (doc sec.3.3), exactly like message.Service.
	// Create's own Publish call -- a subscriber must never see an event for
	// a row that could still roll back.
	messages.PublishCreated(msg)
	return PulledNotice{Message: msg, Wake: pulledRow.Wake != 0}, true, nil
}

// PullSessionNotices pulls every pending session_notices row for owner
// (oldest first). A row with a job_tool_call_id whose void condition (doc
// sec.3.4) fails is marked void instead of delivered, in the same
// transaction, and produces no message.
func (s *AsyncJobStore) PullSessionNotices(ctx context.Context, messages message.Service, owner string, build func(SessionNoticeRow) message.CreateMessageParams) ([]PulledNotice, error) {
	// Captured before any transaction opens (R2A-2): nothing a writer
	// transaction runs may wait on store state.
	ownHostID := s.HostID()
	rows, err := s.q.ListPendingSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []PulledNotice
	for _, row := range rows {
		pulled, ok, err := s.pullOneSessionNotice(ctx, messages, owner, ownHostID, row, build)
		if err != nil {
			slog.Warn("async job store: pull session notice failed; row stays pending",
				"session_id", owner, "notice_id", row.ID, "err", err)
			continue
		}
		if ok {
			out = append(out, pulled)
		}
	}
	return out, nil
}

func (s *AsyncJobStore) pullOneSessionNotice(ctx context.Context, messages message.Service, owner, ownHostID string, row db.SessionNotice, build func(SessionNoticeRow) message.CreateMessageParams) (PulledNotice, bool, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return PulledNotice{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	q := db.New(tx)

	pulledRow, err := q.PullPendingSessionNotice(ctx, db.PullPendingSessionNoticeParams{
		UpdatedAt: time.Now().Unix(), ID: row.ID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return PulledNotice{}, false, nil // another leader already won this row
	}
	if err != nil {
		return PulledNotice{}, false, err
	}

	void, err := s.sessionNoticeVoidCondition(ctx, q, owner, ownHostID, pulledRow)
	if err != nil {
		return PulledNotice{}, false, err
	}
	if void {
		// Downgrades the delivery='done' this same transaction's pull CAS
		// above just set to 'void' -- see VoidPendingSessionNotice's own doc
		// for why the guard is 'done', not 'pending'. rows==0 here would mean
		// our own just-committed-in-tx write vanished, which cannot happen
		// short of DB corruption -- treated as a hard error rather than a
		// silent no-op so it is never mistaken for a benign lost race.
		rows, err := q.VoidPendingSessionNotice(ctx, db.VoidPendingSessionNoticeParams{
			UpdatedAt: time.Now().Unix(), ID: pulledRow.ID,
		})
		if err != nil {
			return PulledNotice{}, false, err
		}
		if rows == 0 {
			return PulledNotice{}, false, fmt.Errorf("void: row %d not in expected delivery='done' state within its own transaction", pulledRow.ID)
		}
		if err := tx.Commit(); err != nil {
			return PulledNotice{}, false, err
		}
		return PulledNotice{}, false, nil
	}

	params := build(SessionNoticeRow{ID: pulledRow.ID, Kind: pulledRow.Kind, Text: pulledRow.Text})
	msg, err := messages.CreateTx(ctx, tx, owner, params)
	if err != nil {
		return PulledNotice{}, false, err
	}
	if _, err := q.SetSessionNoticeMessageID(ctx, db.SetSessionNoticeMessageIDParams{
		NoticeMessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix(), ID: pulledRow.ID,
	}); err != nil {
		return PulledNotice{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PulledNotice{}, false, err
	}
	messages.PublishCreated(msg)
	return PulledNotice{Message: msg, Wake: pulledRow.Wake != 0}, true, nil
}

// sessionNoticeVoidCondition implements doc sec.3.4's two task-scoped void
// checks, inside the SAME transaction as the pull CAS: wake_only voids if
// its named job is no longer running; supervision voids if the owner's
// scope (excluding supervision itself, which has no async_jobs row) has no
// other running row. Every other kind has no condition and never voids.
func (s *AsyncJobStore) sessionNoticeVoidCondition(ctx context.Context, q *db.Queries, owner, ownHostID string, row db.SessionNotice) (bool, error) {
	switch row.Kind {
	case NoticeKindWakeOnly:
		if !row.JobToolCallID.Valid {
			return false, nil
		}
		job, err := q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: owner, ToolCallID: row.JobToolCallID.String})
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		// A job voided by a Rerun never surfaces its check-in either, even while
		// its (possibly unreachable) executor is still running.
		return job.State != "running" || job.Delivery == "void", nil
	case NoticeKindSupervision:
		// A15: a running row on a PROVABLY DEAD host is not open scope --
		// reuse the same liveness predicate the rest of the scope logic uses
		// (HostNotDead, doc sec.3.5/3.6), rather than counting any running
		// row regardless of whether its host is still alive.
		running, err := q.ListRunningAsyncJobsForOwners(ctx, []string{owner})
		if err != nil {
			return false, err
		}
		for _, r := range running {
			// A job voided by a Rerun is not open scope either, even while its
			// (possibly unreachable) executor still runs (R2A-9).
			if r.Delivery == "void" {
				continue
			}
			if s.hostNotDeadFor(ownHostID, r.HostID) {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, nil
	}
}

// InsertSessionNotice persists a session_notices row with delivery='pending'
// (doc sec.2/3.2): supervision, the wake_failed marker, SDK background-shell
// completion, and the wake_only timeout check-in all go through this instead
// of InjectMessage (step 3 removes that path for these four notice kinds --
// see the NoticeKind* constants above for the kind vocabulary). jobToolCallID,
// when non-empty, names the async_jobs row sessionNoticeVoidCondition checks
// at pull time; pass "" for a notice with no such condition (wake_failed,
// bg_shell_done).
func (s *AsyncJobStore) InsertSessionNotice(ctx context.Context, owner, kind, text string, wake bool, jobToolCallID string) error {
	wakeInt := int64(0)
	if wake {
		wakeInt = 1
	}
	var jobToolCallIDParam sql.NullString
	if jobToolCallID != "" {
		jobToolCallIDParam = sql.NullString{String: jobToolCallID, Valid: true}
	}
	now := time.Now().Unix()
	_, err := s.q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{
		Owner: owner, Kind: kind, Text: text, Wake: wakeInt,
		JobToolCallID: jobToolCallIDParam, CreatedAt: now, UpdatedAt: now,
	})
	return err
}

// AnnounceStarted is the ack gate's fused transaction (DUR-7, doc sec.3.8):
// the "started" tool-result message and announced=1 commit TOGETHER, with
// no condition on the row's state -- a job that races to terminal before
// its own "started" write commits must still be marked announced, so its
// already-pending notice becomes pullable. rows==0 (ErrAsyncJobGone) means
// the row no longer exists (e.g. a Rerun truncation raced it): a benign
// no-op for the caller, mirroring MarkAnnounced's own doc. The caller
// (workLedger.acknowledgeWithMessageTx) is responsible for the in-memory
// tail (announced flag, delivery via the existing wake-hint machinery) --
// this function only owns the durable half.
func (s *AsyncJobStore) AnnounceStarted(ctx context.Context, messages message.Service, owner, toolCallID string, params message.CreateMessageParams) (message.Message, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce started: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)

	msg, err := messages.CreateTx(ctx, tx, owner, params)
	if err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce started: create message: %w", err)
	}
	rows, err := q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
		UpdatedAt: time.Now().Unix(), OwnerSessionID: owner, ToolCallID: toolCallID,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce started: mark announced: %w", err)
	}
	if rows == 0 {
		return message.Message{}, ErrAsyncJobGone
	}
	// Record the announcing message: Rerun voids the row by the message id it
	// deleted, not by a possibly provider-reused tool_call_id.
	if _, err := q.SetAsyncJobAnnounceMessageID(ctx, db.SetAsyncJobAnnounceMessageIDParams{
		AnnounceMessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix(),
		OwnerSessionID: owner, ToolCallID: toolCallID,
	}); err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce started: set announce message id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce started: commit: %w", err)
	}
	// Publish AFTER commit (doc sec.3.3's same rule applied to the ack gate):
	// a subscriber must never observe an event for a row that could still
	// roll back.
	messages.PublishCreated(msg)
	return msg, nil
}

// AnnounceJobKillResult is job_kill's own fused transaction (A3, doc
// sec.3.2's law "delivery='done' => the row names the message that carries
// its result"): job_kill's Transition call (causeJobKill) already set
// delivery='done'/reacted=1 directly, bypassing the ordinary pull -- so
// nothing else ever writes notice_message_id for that row. This mirrors
// AnnounceStarted's pattern: the tool-result message insert and the
// notice_message_id write commit in ONE transaction, so a crash between them
// can never leave the row 'done' with no named message.
//
// jobToolCallID is the TARGET async_jobs row's tool_call_id (the job that
// was killed) -- distinct from params' own tool_call_id (job_kill's own
// call). 0 rows affected by the guarded write (SetAsyncJobNoticeMessageIDIfDone)
// is not an error: it means THIS job_kill call's own transition lost the
// race to a different cause (the row is still 'pending', to be pulled
// normally instead) -- job_kill's tool-result message is persisted
// regardless, since a tool_use must always get a tool_result.
func (s *AsyncJobStore) AnnounceJobKillResult(ctx context.Context, messages message.Service, owner, jobToolCallID string, params message.CreateMessageParams) (message.Message, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)

	msg, err := messages.CreateTx(ctx, tx, owner, params)
	if err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: create message: %w", err)
	}
	if _, err := q.SetAsyncJobNoticeMessageIDIfDone(ctx, db.SetAsyncJobNoticeMessageIDIfDoneParams{
		NoticeMessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix(),
		OwnerSessionID: owner, ToolCallID: jobToolCallID,
	}); err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: set notice message id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return message.Message{}, fmt.Errorf("async job store: announce job_kill result: commit: %w", err)
	}
	// Publish AFTER commit (doc sec.3.3's same rule applied here).
	messages.PublishCreated(msg)
	return msg, nil
}

// ListSessionNotices is a thin read-only wrapper for tests and diagnostics
// (the full `sessions why`/`sessions jobs` reader is doc sec.5 step 7's
// job) -- every row for owner, oldest first, regardless of delivery state.
func (s *AsyncJobStore) ListSessionNotices(ctx context.Context, owner string) ([]SessionNoticeRow, error) {
	rows, err := s.q.ListSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]SessionNoticeRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, SessionNoticeRow{ID: row.ID, Kind: row.Kind, Text: row.Text})
	}
	return out, nil
}
