package app

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session"
)

// LiveSessionDrivers is the cross-process reader of the durable driver marker
// (session_drivers, ASYNC-02): which sessions a live `rush run` loop drives
// right now, keyed by session id. A loop between turns (a paced Drain retry,
// debt pending) holds no session lock and has no running row, so this marker
// is the only durable fact that its scope is open; the status surfaces
// (`sessions why`, `sessions list`, the web session list) read it so such a
// session is not reported at rest.
//
// Liveness is the host's (this process's own host and sibling Apps' hosts are
// alive; anything else goes through the shared probe; unknown counts as alive,
// only a provably dead host's marker is dropped) and is decided once per
// distinct host. The whole table is read in ONE statement on the read pool
// (never per session, never on the writer connection): it holds one row per
// running or crashed-uncleaned loop, and the maintenance sweep purges dead
// hosts' rows. An App without an AsyncJobStore or a database handle answers
// an empty map.
//
// The statement is a plain read here rather than a store method because the
// session store exposes only the per-session ForeignLiveDriver, which also
// hides this process's own host's marker.
func (app *App) LiveSessionDrivers(ctx context.Context) (map[string]session.SessionDriver, error) {
	store := app.asyncJobStore
	if store == nil {
		return nil, nil
	}
	conn := app.readDB
	if conn == nil && app.DB != nil {
		conn = app.DB()
	}
	if conn == nil {
		return nil, nil
	}
	rows, err := conn.QueryContext(ctx, `SELECT session_id, host_id, pid FROM session_drivers`)
	if err != nil {
		return nil, fmt.Errorf("session drivers: read: %w", err)
	}
	defer rows.Close()
	status := make(map[string]session.HostLockStatus)
	live := make(map[string]session.SessionDriver)
	for rows.Next() {
		var d session.SessionDriver
		if err := rows.Scan(&d.SessionID, &d.HostID, &d.PID); err != nil {
			return nil, fmt.Errorf("session drivers: scan: %w", err)
		}
		st, ok := status[d.HostID]
		if !ok {
			st = store.HostLiveness(d.HostID)
			status[d.HostID] = st
		}
		if st == session.HostStatusDead {
			continue
		}
		d.Status = st
		live[d.SessionID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session drivers: read: %w", err)
	}
	return live, nil
}
