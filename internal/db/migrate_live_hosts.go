package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/filelock"
)

// hostLockPath mirrors session.HostLockPath (<dataDir>/hosts/<id>.lock). It
// is duplicated here because db cannot import session.
func hostLockPath(dataDir, hostID string) string {
	return filepath.Join(dataDir, "hosts", hostID+".lock")
}

// CheckLiveHosts refuses the destructive async_jobs migration while any
// enumerated host looks alive. The future destructive migration step (R12-H)
// calls it as a PreMigrationCheck while the migration decision lock
// (<dataDir>/migrate.lock) is still held, so no host can register between the
// probe and the DROP.
//
// Every async_hosts row is probed, including hosts with zero jobs (a live
// host with no jobs may still be mid-write). Dead-only hosts proceed. Alive
// hosts, unknown probes, probe errors, and own-process hosts all refuse.
// Every blocking host is collected into ONE error so the operator sees the
// full set at once.
func CheckLiveHosts(ctx context.Context, conn *sql.DB, dataDir string, isOwnHost func(hostID string) bool) error {
	rows, err := conn.QueryContext(ctx, "SELECT id FROM async_hosts")
	if err != nil {
		return fmt.Errorf("live-host gate: failed to enumerate async_hosts: %w", err)
	}
	defer rows.Close()

	var blocking []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("live-host gate: failed to read async_hosts row: %w", err)
		}
		if isOwnHost != nil && isOwnHost(id) {
			blocking = append(blocking, id+" (own process)")
			continue
		}
		status, probeErr := filelock.ProbeShared(hostLockPath(dataDir, id))
		if probeErr == nil && status == filelock.LockStatusDead {
			continue
		}
		label := "unknown"
		if probeErr == nil && status == filelock.LockStatusAlive {
			label = "alive"
		}
		blocking = append(blocking, id+" ("+label+")")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("live-host gate: failed to enumerate async_hosts: %w", err)
	}
	if len(blocking) == 0 {
		return nil
	}
	return fmt.Errorf(
		"live-host gate: %d host(s) block the destructive migration: %s; stop other Rush processes before retrying",
		len(blocking), strings.Join(blocking, ", "),
	)
}
