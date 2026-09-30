// Dead-host recovery (DUR-5/DUR-6, docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.6/3.7): converts a dead host's 'running' rows into
// fact-only terminal state. Recovery NEVER writes session history and NEVER
// wakes anyone -- the 'interrupted' notice it produces sits at
// delivery='pending' exactly like any other terminal transition's outbox
// entry, picked up by whichever session driver next pulls it (turn start or
// PrepareStep, notice_pull.go). Recovery never kills processes: on Windows
// the dead host's own job object already tore its tree down at exit; POSIX
// has no reliable process-group marker recovery could act on.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// interruptedBashText is doc sec.3.7's fixed wording for a recovered
// bash/run_command/agentic_fetch row (anything without a child session):
// this row's own output was never durably captured (a dead host cannot be
// asked for it), and the process itself may still be alive.
const interruptedBashText = "вывод не сохранялся; процесс мог остаться запущенным"

// interruptedNoChildTextText is substituted when a recovered delegation's
// child session has no finished assistant message to quote.
const interruptedNoChildTextText = "the sub-agent session finished with no textual response"

// AsyncDataRetentionAge bounds how long a terminal, delivered-or-voided
// async_jobs/session_notices row (and an empty dead host's lock file)
// survives before the purge reaps it (doc sec.3.7): the 60s pass of the web
// process and of a `rush run` loop (ClaimExternalDriver starts the ticker; the
// loop also runs it once at start). Fixed, not a config option -- the doc's
// own tests only require "old rows are purged, recent ones are kept", not a
// tunable knob.
const AsyncDataRetentionAge = 7 * 24 * time.Hour

// recoverDeadHostRowSeam is a test-only hook fired before RecoverDeadHost handles
// each listed row, so a test can hold N recoverers inside the list-then-write
// gap. nil in production.
var recoverDeadHostRowSeam func(hostID string, row db.AsyncJob)

// purgeEmptyDeadHostBeforeDeleteSeam is a test-only hook fired between
// purgeEmptyDeadHostFiles' dead verdict and its host-row delete. nil in
// production.
var purgeEmptyDeadHostBeforeDeleteSeam func(hostID string)

// RecoveryOutcome tallies one host sweep's effect, for logging and tests.
type RecoveryOutcome struct {
	Interrupted int  // running, announced=1 rows moved to 'interrupted'
	Deleted     int  // announced=0 rows (running or terminal) deleted without a trace
	Repended    int  // job_kill rows left done without a result message, made deliverable again
	HostRemoved bool // this call deleted the async_hosts row (no rows left); the lock file too unless another holder kept it (purgeOrphanHostLockFiles reaps it)
}

// RecoverDeadHost recovers hostID's rows IFF hostID is provably dead right
// now (doc sec.3.6's 3-way probe): alive or unknown are both no-ops --
// rows are only ever touched on a confirmed-dead verdict. Refuses (no-op,
// nil error) for this process's own host id or a sibling App's, exactly
// like ProbeHost itself.
//
// The verdict is the SHARED probe: it is kernel-attested proof that no
// exclusive holder exists, and it retains nothing, so recovery never holds the
// host's lock while it works (R3A-3). Holding the exclusive lock across the
// row-by-row work made every other reader's shared probe see contention =
// "alive" for as long as recovery took (ClaimSessionDriver refused a crashed
// driver's session, ForeignLiveDriver and the scope readers reported a
// live host). Death is irreversible (host ids are uuids never reused, and an
// id is published only while its registrant holds a verified lock), so nothing
// recovery does can race the host coming back; the exclusive lock is taken
// only at the very end, for the lock file's removal.
//
// Idempotent and safe against a concurrent recoverer, which the shared probe
// now admits: every write below is a guarded CAS/delete keyed by the row's
// identity -- Transition scoped to state='running' and the listed claim_id,
// the unannounced delete scoped to announced=0 and the listed claim_id (a
// key-only delete could remove a live host's fresh claim of the same
// tool_call_id), the host-scoped deletes/re-pend touching only rows that
// carry the dead host's id (no live host ever writes one) -- so a second
// recoverer racing this one simply finds 0 rows affected on whatever it did
// not win and moves on: never a double transition, never a double delete.
// messages may be nil (a caller with no message.Service handy): a recovered
// delegation then falls back to interruptedNoChildTextText instead of the
// child's actual last words.
func (s *AsyncJobStore) RecoverDeadHost(ctx context.Context, hostID string, messages message.Service) (RecoveryOutcome, error) {
	var out RecoveryOutcome
	if hostID == "" || IsOwnHostID(hostID) {
		return out, nil
	}
	status, err := ProbeHostShared(s.dataDir, hostID)
	if status != HostStatusDead {
		// Alive: genuinely not ours to touch. Unknown: doc sec.3.6 -- "rows
		// stay, the scope does not treat them as open only on a definite
		// 'dead'" -- leave everything alone and surface the probe error (if
		// any) so the caller can log it.
		return out, err
	}

	rows, listErr := s.q.ListRunningAsyncJobsForHost(ctx, hostID)
	if listErr != nil {
		return out, fmt.Errorf("recover dead host %s: list running rows: %w", hostID, listErr)
	}
	for _, row := range rows {
		if recoverDeadHostRowSeam != nil {
			recoverDeadHostRowSeam(hostID, row)
		}
		if row.Announced == 0 {
			// ASYNC-05: the "started" tool result never committed for this
			// row -- delete it without a trace, same rule as a live abort.
			deleted, err := s.deleteUnannouncedForClaim(ctx, row.OwnerSessionID, row.ToolCallID, row.ClaimID)
			if err != nil {
				slog.Warn("recover dead host: delete unannounced row failed; will retry on a later sweep",
					"host_id", hostID, "owner", row.OwnerSessionID, "tool_call_id", row.ToolCallID, "err", err)
				continue
			}
			if deleted {
				out.Deleted++
			}
			continue
		}
		text, isErr, readErr := recoveredResultText(ctx, messages, row)
		if readErr != nil {
			// A6: a TRANSIENT read failure (messages.List erroring, not "no
			// messages") must never commit a false "finished with no textual
			// response" -- skip this row, leave it 'running' for a later
			// sweep to retry once the read can succeed.
			slog.Warn("recover dead host: read child session messages failed; leaving row running for a later sweep",
				"host_id", hostID, "owner", row.OwnerSessionID, "tool_call_id", row.ToolCallID, "err", readErr)
			continue
		}
		result, err := s.Transition(ctx, TransitionParams{
			Owner: row.OwnerSessionID, ToolCallID: row.ToolCallID,
			State: "interrupted", NoticeKind: "interrupted",
			ResultSummary: text, ResultIsError: isErr, Wake: false,
			// ClaimID (A11): assert the SAME claim this dead host's row was
			// claimed under -- a live host racing to legitimately re-claim
			// this exact key (recovered concurrently, or a since-restarted
			// same process) after this read would have a DIFFERENT claim_id,
			// and must win over a recovery sweep that is, by construction,
			// acting on stale information about who owns it.
			ClaimID: row.ClaimID,
		})
		if err != nil {
			slog.Warn("recover dead host: transition to interrupted failed; will retry on a later sweep",
				"host_id", hostID, "owner", row.OwnerSessionID, "tool_call_id", row.ToolCallID, "err", err)
			continue
		}
		if result.Outcome == TransitionWon {
			out.Interrupted++
		}
		// TransitionLost/TransitionGone: another recoverer (or the row's own
		// legitimate winner, though a dead host cannot produce one) already
		// settled this row -- nothing more to do for it.
	}

	// R2A-7: a job that went terminal before its own "started" result
	// committed (announced=0) never produces a notice and recovery reads only
	// state='running' -- without this it leaks forever and blocks its
	// tool_call_id. Same ASYNC-05 rule as the running unannounced rows above:
	// deleted without a trace.
	if n, err := s.q.DeleteTerminalUnannouncedAsyncJobsForHost(ctx, hostID); err != nil {
		slog.Warn("recover dead host: delete terminal unannounced rows failed; will retry on a later sweep",
			"host_id", hostID, "err", err)
	} else {
		out.Deleted += int(n)
	}
	// R2A-8 (DUR-11 for job_kill): job_kill commits done/reacted=1 first and
	// names its result message (notice_message_id) later; a host that died
	// between left a row nobody will ever pull. Made deliverable again so the
	// next pull shows the result -- a plain pending, wake=0 notice, never a
	// wake.
	if n, err := s.q.RependJobKillRowsWithoutNoticeForHost(ctx, db.RependJobKillRowsWithoutNoticeForHostParams{
		UpdatedAt: time.Now().Unix(), HostID: hostID,
	}); err != nil {
		slog.Warn("recover dead host: re-pend job_kill rows without a result message failed; will retry on a later sweep",
			"host_id", hostID, "err", err)
	} else {
		out.Repended += int(n)
	}

	deletedHostRow, delErr := s.q.DeleteAsyncHostIfNoJobs(ctx, hostID)
	if delErr != nil {
		return out, fmt.Errorf("recover dead host %s: delete host row: %w", hostID, delErr)
	}
	if deletedHostRow > 0 {
		out.HostRemoved = true
		s.removeDeadHostFile(hostID)
	}
	// Otherwise rows for this host still exist (the just-recovered ones
	// themselves, still delivery='pending'/'done' until a driver pulls and
	// retention eventually purges them): the file stays for a later sweep's
	// "no rows left" check to reap.
	return out, nil
}

// removeDeadHostFile is the only step of recovery (and of the empty-host reap,
// purgeEmptyDeadHostFiles) that takes the exclusive lock: won here,
// RemoveDeadHostFile verifies the path still names the held
// file and unlinks it. Not winning is not an error: the file is already gone
// (ENOENT), another holder has it right now (a recoverer or reaper, which
// removes it, or a reader's shared probe, which removes nothing), or the probe
// is inconclusive. At most one removes the file; otherwise
// purgeOrphanHostLockFiles reaps it once the row is gone.
func (s *AsyncJobStore) removeDeadHostFile(hostID string) {
	status, lock, err := ProbeHost(s.dataDir, hostID)
	if status != HostStatusDead || lock == nil {
		if err != nil {
			slog.Warn("remove dead host file: lock file probe before removal failed", "host_id", hostID, "err", err)
		}
		return
	}
	if err := RemoveDeadHostFile(HostLockPath(s.dataDir, hostID), lock); err != nil {
		slog.Warn("remove dead host file: remove lock file failed", "host_id", hostID, "err", err)
	}
}

// recoveredResultText computes a recovered row's result text/isError (doc
// sec.3.7): the child's last assistant message for a delegation, the fixed
// bash/run_command wording otherwise. Always isError=true when readErr==nil
// -- an interruption is an abnormal outcome regardless of what a quoted
// child said. A non-nil readErr (A6) means the caller must NOT transition
// this row at all -- see recoveredDelegationText's own doc.
func recoveredResultText(ctx context.Context, messages message.Service, row db.AsyncJob) (text string, isError bool, readErr error) {
	if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
		text, readErr := recoveredDelegationText(ctx, messages, row.ChildSessionID.String)
		return text, true, readErr
	}
	return interruptedBashText, true, nil
}

// recoveredDelegationText mirrors the "newest finished assistant message"
// read package agent's refreshSubAgentCompletion/capturePartialDelegation
// use, scoped to this package's own dependency (message.Service) so
// recovery needs no import of package agent.
//
// A6: a TRANSIENT messages.List error is distinct from "no messages at all"
// (nil messages.Service, or an empty/no-finished-assistant-message list) --
// the former returns a non-nil readErr so the caller skips this row entirely
// rather than committing a false "finished with no textual response" that
// looks identical to a genuine empty answer. Only a definite "there is
// nothing to quote" falls back to interruptedNoChildTextText.
func recoveredDelegationText(ctx context.Context, messages message.Service, childSessionID string) (string, error) {
	if messages == nil {
		return interruptedNoChildTextText, nil
	}
	msgs, err := messages.List(ctx, childSessionID)
	if err != nil {
		return "", fmt.Errorf("read child session messages: %w", err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || !msg.IsFinished() {
			continue
		}
		if text := strings.TrimSpace(msg.FullText()); text != "" {
			return text, nil
		}
		return interruptedNoChildTextText, nil
	}
	return interruptedNoChildTextText, nil
}

// SweepDeadHosts recovers every currently dead host that owns at least one
// RUNNING row (doc sec.3.6/3.7's "раз в 60с по всем мёртвым хостам"),
// skipping hosts this process itself owns. A single host's recovery error
// is logged and skipped -- it never aborts the rest of the sweep.
func (s *AsyncJobStore) SweepDeadHosts(ctx context.Context, messages message.Service) (map[string]RecoveryOutcome, error) {
	hostIDs, err := s.q.ListDistinctRecoverableHostIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweep dead hosts: list recoverable host ids: %w", err)
	}
	out := make(map[string]RecoveryOutcome)
	for _, hostID := range hostIDs {
		if hostID == "" || IsOwnHostID(hostID) {
			continue
		}
		outcome, err := s.RecoverDeadHost(ctx, hostID, messages)
		if err != nil {
			slog.Debug("sweep dead hosts: recover failed", "host_id", hostID, "err", err)
			continue
		}
		if outcome.Interrupted > 0 || outcome.Deleted > 0 || outcome.Repended > 0 || outcome.HostRemoved {
			out[hostID] = outcome
		}
	}
	return out, nil
}

// RecoverOwnerScope recovers every dead host referenced by owner's currently
// RUNNING rows (doc sec.3.5: "at the start of a turn and at scope
// evaluation, the leader recovers its own scope's dead-host rows to
// 'interrupted'"). Distinct host ids are deduplicated so a host with several
// of owner's rows is only probed/recovered once. Errors are logged and
// skipped per host -- a recovery failure never blocks the turn/scope check
// that triggered it; the row stays 'running' for a later attempt.
func (s *AsyncJobStore) RecoverOwnerScope(ctx context.Context, owner string, messages message.Service) {
	if owner == "" {
		return
	}
	rows, err := s.q.ListRunningAsyncJobsForOwners(ctx, []string{owner})
	if err != nil {
		slog.Debug("recover owner scope: list running rows failed", "owner", owner, "err", err)
		return
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.HostID == "" || IsOwnHostID(row.HostID) {
			continue
		}
		if _, ok := seen[row.HostID]; ok {
			continue
		}
		seen[row.HostID] = struct{}{}
		if _, err := s.RecoverDeadHost(ctx, row.HostID, messages); err != nil {
			slog.Debug("recover owner scope: recover host failed", "owner", owner, "host_id", row.HostID, "err", err)
		}
	}
}

// PurgeExpired is the retention half of the maintenance sweep (doc sec.3.7;
// runs every 60s in the web process and in a `rush run` loop, whose
// ClaimExternalDriver starts the ticker, plus once at loop start): terminal,
// delivered-or-voided async_jobs/session_notices rows past age (never
// unreacted debt, nor a job_kill row still unnamed, which dead-host recovery re-pends, R5A-1),
// dead drivers' markers, and dead hosts' lock files/rows once they reference
// no rows of any state at all.
// Best-effort throughout -- a failure on one sub-step is logged and the next
// one still runs; a transient DB error here is retried by the next 60s tick,
// never treated as fatal to the pass that called this.
func (s *AsyncJobStore) PurgeExpired(ctx context.Context, age time.Duration) error {
	cutoff := time.Now().Add(-age).Unix()
	var errs []error
	if err := s.purgeDeadSessionDrivers(ctx); err != nil {
		errs = append(errs, fmt.Errorf("purge expired: session drivers: %w", err))
	}
	if _, err := s.q.PurgeAsyncJobsOlderThan(ctx, cutoff); err != nil {
		errs = append(errs, fmt.Errorf("purge expired: async_jobs: %w", err))
	}
	if _, err := s.q.PurgeSessionNoticesOlderThan(ctx, cutoff); err != nil {
		errs = append(errs, fmt.Errorf("purge expired: session_notices: %w", err))
	}
	if err := s.purgeEmptyDeadHostFiles(ctx); err != nil {
		errs = append(errs, fmt.Errorf("purge expired: host files: %w", err))
	}
	if err := s.purgeOrphanHostLockFiles(ctx); err != nil {
		errs = append(errs, fmt.Errorf("purge expired: orphan host lock files: %w", err))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("purge expired: %d step(s) failed: %w", len(errs), errs[0])
}

// purgeEmptyDeadHostFiles reaps a dead host's async_hosts row and lock file
// once it references zero async_jobs rows of ANY state (doc sec.3.7): a
// host that lost all its rows to earlier recovery/purge passes, but whose
// own "no rows left" check (RecoverDeadHost's own tail) ran before the last
// row actually disappeared. A LIVE host with zero current jobs (e.g. an idle
// long-running web server) is correctly left alone -- the probe must
// independently confirm 'dead' before anything is touched.
//
// Same shape as RecoverDeadHost (R3A-3): the verdict is the SHARED probe, the
// row delete (a write that can wait behind any in-process transaction, up to
// busy_timeout) runs holding no lock, and the exclusive lock is taken only
// inside removeDeadHostFile, for the unlink -- so a host being reaped still
// probes dead to every reader. A lost delete (another reaper won the row)
// leaves the file to it or to purgeOrphanHostLockFiles.
func (s *AsyncJobStore) purgeEmptyDeadHostFiles(ctx context.Context) error {
	hosts, err := s.q.ListAsyncHostsWithNoJobs(ctx)
	if err != nil {
		return fmt.Errorf("list hosts with no jobs: %w", err)
	}
	for _, h := range hosts {
		if h.ID == "" || IsOwnHostID(h.ID) {
			continue
		}
		if status, _ := ProbeHostShared(s.dataDir, h.ID); status != HostStatusDead {
			continue
		}
		if purgeEmptyDeadHostBeforeDeleteSeam != nil {
			purgeEmptyDeadHostBeforeDeleteSeam(h.ID)
		}
		n, err := s.q.DeleteAsyncHostIfNoJobs(ctx, h.ID)
		if err != nil {
			slog.Warn("purge empty dead host files: delete host row failed", "host_id", h.ID, "err", err)
			continue
		}
		if n > 0 {
			s.removeDeadHostFile(h.ID)
		}
	}
	return nil
}

// purgeOrphanHostLockFiles is A7/C15's fix: every deleter of a host (Close,
// RecoverDeadHost, purgeEmptyDeadHostFiles above) removes the async_hosts
// ROW first and the lock FILE second, and RegisterHost releases the lock
// (without deleting the file) if the DB insert fails -- either leaves a
// *.lock file on disk with NO async_hosts row at all, which
// purgeEmptyDeadHostFiles can never find (it only walks rows). This scans
// hosts/*.lock directly: a file whose id has no async_hosts row is reaped
// once this process independently confirms (by winning the exclusive lock
// itself) that whatever held it is gone. A live host with no row yet (the
// brief window between RegisterHost's lock and its DB insert) is never
// touched -- winning the probe IS the liveness proof, same guarantee every
// other reaper in this file relies on.
func (s *AsyncJobStore) purgeOrphanHostLockFiles(ctx context.Context) error {
	entries, err := os.ReadDir(HostsDir(s.dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no hosts dir yet -- nothing to scan
		}
		return fmt.Errorf("read hosts dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		const suffix = ".lock"
		if len(name) <= len(suffix) || name[len(name)-len(suffix):] != suffix {
			continue
		}
		hostID := name[:len(name)-len(suffix)]
		if hostID == "" || IsOwnHostID(hostID) {
			continue
		}
		s.reapOrphanHostLock(ctx, hostID)
	}
	return nil
}

// reapOrphanHostLock is purgeOrphanHostLockFiles' per-entry step: remove
// hostID's listed file when no row exists and the exclusive probe wins. A file
// already gone before the probe (another process or RecoverDeadHost reaped it
// between the listing and here) is not a failure: nothing is left to do.
func (s *AsyncJobStore) reapOrphanHostLock(ctx context.Context, hostID string) {
	if _, err := s.q.GetAsyncHost(ctx, hostID); err == nil {
		return // has a row -- purgeEmptyDeadHostFiles/RecoverDeadHost own this one
	} else if !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("purge orphan host lock files: get host row failed", "host_id", hostID, "err", err)
		return
	}
	status, lock, err := ProbeHost(s.dataDir, hostID)
	if err != nil || status != HostStatusDead {
		if lock != nil {
			_ = lock.Release()
		}
		return
	}
	if lock == nil {
		return // dead with no lock: the file is already gone
	}
	if err := RemoveDeadHostFile(HostLockPath(s.dataDir, hostID), lock); err != nil {
		slog.Warn("purge orphan host lock files: remove lock file failed", "host_id", hostID, "err", err)
	}
}
