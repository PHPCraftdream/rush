// The durable external-driver marker (docs/reviews/2026-09-29-async-phase4-
// round1-rerun-design.md, Problem 2): session_drivers(session_id -> host_id)
// records which `rush run` loop drives a session. The driver is alive exactly
// when its host is alive (DUR-5, the host's OS lock -- no clock is stored or
// read). Any other process never starts a reaction turn for a session whose
// marker names a live host; it may still transfer notices into history if a
// Drain was already admitted (agent.sessionDrainPolicy).
//
// Every write here is a single-statement CAS on the shared writer connection:
// no transaction, no file IO inside a transaction.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// ErrDriverMarkerUnavailable means the durable marker cannot be kept on this
// data directory because the host lock cannot be created or taken (a
// filesystem without OS locks: ErrHostLockUnavailable): the caller carries on
// with its in-memory marker only. It is ONLY that: a database or context error
// during registration (SQLITE_BUSY/FULL, a cancelled ctx) is a plain error --
// degrading on it would let another process run a paid Drain on a session a
// CLI loop drives (R3A-1).
var ErrDriverMarkerUnavailable = errors.New("session driver marker unavailable")

// ErrSessionDrivenElsewhere is Claim's refusal: another host whose liveness is
// alive or unknown already drives the session (unknown counts as alive, the
// same rule as the rest of the scope logic).
type ErrSessionDrivenElsewhere struct {
	SessionID string
	HostID    string
	PID       int64
	Status    HostLockStatus
	ProbeErr  error
}

func (e *ErrSessionDrivenElsewhere) Error() string {
	msg := fmt.Sprintf("session %s is already driven by another `rush run` (host %s, pid %d, %s); wait for it to finish",
		e.SessionID, e.HostID, e.PID, e.Status)
	if e.ProbeErr != nil {
		msg += fmt.Sprintf(" (liveness probe: %v)", e.ProbeErr)
	}
	return msg
}

// SessionDriver is a session's marker as seen by ForeignLiveDriver.
type SessionDriver struct {
	SessionID string
	HostID    string
	PID       int64
	Status    HostLockStatus
}

// sessionDriverBeforeTakeoverSeam is a test-only hook fired after a claim has
// read a dead host's marker and before its takeover CAS, so a test can hold
// N claimants at exactly the read-then-write gap. nil in production.
var sessionDriverBeforeTakeoverSeam func()

// claimSessionDriverAttempts bounds the read/CAS loop of a claim: each retry
// follows a lost CAS (someone else changed the row between our read and write).
const claimSessionDriverAttempts = 3

// hostLivenessDetail is HostLiveness with the probe error kept for messages:
// this process's (or a sibling App's) hosts are alive without probing;
// anything else goes through the shared, non-acquiring probe, and any outcome
// other than a clean "dead" is folded into unknown.
func (s *AsyncJobStore) hostLivenessDetail(hostID string) (HostLockStatus, error) {
	if hostID == "" {
		return HostStatusUnknown, errors.New("empty host id")
	}
	if IsOwnHostID(hostID) {
		return HostStatusAlive, nil
	}
	status, err := ProbeHostShared(s.dataDir, hostID)
	if err != nil && status != HostStatusDead {
		return HostStatusUnknown, err
	}
	return status, nil
}

// ClaimSessionDriver records this store's host as sessionID's driver. It is
// idempotent for the same host; refuses (*ErrSessionDrivenElsewhere) while
// another host that is alive or unknown holds the marker; takes a dead
// host's marker over with a CAS on that exact host (so of N concurrent takers
// exactly one wins). A host lock the data dir cannot provide is
// ErrDriverMarkerUnavailable; every other ensureHost failure is a plain error
// (the caller must not proceed without the marker).
func (s *AsyncJobStore) ClaimSessionDriver(ctx context.Context, sessionID string) error {
	hostID, err := s.ensureHost(ctx)
	if err != nil {
		if errors.Is(err, ErrHostLockUnavailable) {
			return fmt.Errorf("%w: %w", ErrDriverMarkerUnavailable, err)
		}
		return fmt.Errorf("session driver: register host: %w", err)
	}
	for attempt := 0; attempt < claimSessionDriverAttempts; attempt++ {
		now := time.Now().Unix()
		row, err := s.q.GetSessionDriver(ctx, sessionID)
		switch {
		case err == nil:
			if row.HostID == hostID {
				return nil
			}
			status, probeErr := s.hostLivenessDetail(row.HostID)
			if status != HostStatusDead {
				return &ErrSessionDrivenElsewhere{
					SessionID: sessionID, HostID: row.HostID, PID: row.Pid, Status: status, ProbeErr: probeErr,
				}
			}
			if sessionDriverBeforeTakeoverSeam != nil {
				sessionDriverBeforeTakeoverSeam()
			}
			n, err := s.q.TakeOverSessionDriver(ctx, db.TakeOverSessionDriverParams{
				HostID: hostID, Pid: int64(s.pid), ClaimedAt: now, SessionID: sessionID, ExpectedHostID: row.HostID,
			})
			if err != nil {
				return fmt.Errorf("session driver: take over: %w", err)
			}
			if n == 1 {
				return nil
			}
		case errors.Is(err, sql.ErrNoRows):
			n, err := s.q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{
				SessionID: sessionID, HostID: hostID, Pid: int64(s.pid), ClaimedAt: now,
			})
			if err != nil {
				return fmt.Errorf("session driver: insert: %w", err)
			}
			if n == 1 {
				return nil
			}
		default:
			return fmt.Errorf("session driver: read: %w", err)
		}
		// A CAS or insert lost a race: re-observe who holds the row now.
	}
	return fmt.Errorf("session driver: claim of %s kept losing races after %d attempts", sessionID, claimSessionDriverAttempts)
}

// ReleaseSessionDriver deletes this store's own claim on sessionID (never
// another host's). A no-op for a store that never registered a host.
func (s *AsyncJobStore) ReleaseSessionDriver(ctx context.Context, sessionID string) error {
	hostID := s.HostID()
	if hostID == "" {
		return nil
	}
	if _, err := s.q.DeleteSessionDriver(ctx, db.DeleteSessionDriverParams{SessionID: sessionID, HostID: hostID}); err != nil {
		return fmt.Errorf("session driver: release: %w", err)
	}
	return nil
}

// ForeignLiveDriver reports whether ANOTHER host that is alive or unknown
// drives sessionID: no marker, or a marker naming this store's own host, is
// not foreign; a dead host's marker is not foreign either. Reads on the
// reader pool (a PK lookup).
func (s *AsyncJobStore) ForeignLiveDriver(ctx context.Context, sessionID string) (SessionDriver, bool, error) {
	row, err := s.readQuerier().GetSessionDriver(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionDriver{}, false, nil
	}
	if err != nil {
		return SessionDriver{}, false, fmt.Errorf("session driver: read: %w", err)
	}
	if row.HostID == s.HostID() {
		return SessionDriver{}, false, nil
	}
	status, _ := s.hostLivenessDetail(row.HostID)
	d := SessionDriver{SessionID: sessionID, HostID: row.HostID, PID: row.Pid, Status: status}
	return d, status != HostStatusDead, nil
}

// purgeDeadSessionDrivers deletes every marker whose host is provably dead
// (a crashed `rush run` never released it). Host ids are uuids never reused
// and death is irreversible (RegisterHost publishes an id only while holding a
// verified lock on its file, and removers unlink only under the lock), so no
// lock is needed to delete; a host that is alive, unknown, or this process's
// own is left alone.
func (s *AsyncJobStore) purgeDeadSessionDrivers(ctx context.Context) error {
	hosts, err := s.q.ListSessionDriverHostIDs(ctx)
	if err != nil {
		return fmt.Errorf("list session driver hosts: %w", err)
	}
	for _, hostID := range hosts {
		if hostID == "" || IsOwnHostID(hostID) {
			continue
		}
		status, probeErr := ProbeHostShared(s.dataDir, hostID)
		if probeErr != nil || status != HostStatusDead {
			continue
		}
		if _, err := s.q.DeleteSessionDriversForHost(ctx, hostID); err != nil {
			slog.Warn("purge dead session drivers: delete failed", "host_id", hostID, "err", err)
		}
	}
	return nil
}
