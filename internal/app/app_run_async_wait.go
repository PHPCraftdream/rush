package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// cliOpenWorkNamed bounds how many running rows one heartbeat line names.
const cliOpenWorkNamed = 3

// describeOpenWork names what the wait heartbeat is waiting on (ASYNC-10: any
// "why is it waiting" answer names the concrete task): the session's own
// running rows -- tool, tool_call_id, the delegated child and the host that
// runs it (this process, or another one with its pid and liveness). A read
// failure or an empty answer (the row finished a moment ago) falls back to
// the generic wording; it is display only.
func (l *cliLoop) describeOpenWork() string {
	const generic = "a running job/delegation, a host that is not provably dead, or an open wake schedule"
	// Stage 5a: the once schedule the run is held open for (empty without).
	var schedule []string
	if clause := l.describeOnceWake(); clause != "" {
		schedule = append(schedule, clause)
	}
	if l.app != nil && batchCutoverOn() {
		// R12-F: the flag-ON twin reads batch_nodes and the host lock probe
		// only -- never async_jobs. It runs BEFORE the AsyncJobStore gate.
		return l.describeOpenWorkBatch(schedule, generic)
	}
	if l.app == nil || l.app.asyncJobStore == nil {
		if len(schedule) > 0 {
			return strings.Join(schedule, "; ")
		}
		return generic
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 2*time.Second)
	defer cancel()
	store := l.app.asyncJobStore
	rows, err := store.ListRunningAsyncJobsForOwners(ctx, []string{l.sessionID})
	if err != nil || len(rows) == 0 {
		if len(schedule) > 0 {
			return strings.Join(schedule, "; ")
		}
		return generic
	}
	parts := append(schedule, make([]string, 0, cliOpenWorkNamed)...)
	for i, row := range rows {
		if i == cliOpenWorkNamed {
			parts = append(parts, fmt.Sprintf("%d more", len(rows)-cliOpenWorkNamed))
			break
		}
		what := row.ToolName
		if what == "" {
			what = row.Kind
		}
		part := fmt.Sprintf("%s %q", what, row.ToolCallID)
		if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
			part += fmt.Sprintf(" (child session %s)", row.ChildSessionID.String)
		}
		part += " on " + l.describeHost(ctx, store, row.HostID)
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// describeHost renders a job's host: this process, or "host <id> (PID n,
// alive|unknown|dead)" for another one.
func (l *cliLoop) describeHost(ctx context.Context, store *session.AsyncJobStore, hostID string) string {
	if session.IsOwnHostID(hostID) {
		return "this process"
	}
	desc := "host " + hostID
	pid := int64(0)
	if h, err := store.GetAsyncHost(ctx, hostID); err == nil {
		pid = h.Pid
	}
	status := store.HostLiveness(hostID).String()
	if pid > 0 {
		return fmt.Sprintf("%s (PID %d, %s)", desc, pid, status)
	}
	return fmt.Sprintf("%s (%s)", desc, status)
}

// describeOpenWorkBatch is the flag-ON twin of describeOpenWork: live work
// is read from batch_nodes only (BatchLiveForOwner) and host liveness from
// the shared, non-acquiring lock probe -- the async_jobs table is never
// read (it may not exist under the R12-F flag). Display only, with the same
// generic-wording fallback.
func (l *cliLoop) describeOpenWorkBatch(schedule []string, generic string) string {
	app := l.app
	writer := app.batchCutoverWriter()
	if writer == nil {
		return joinOpenWork(schedule, generic)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 2*time.Second)
	defer cancel()
	readQ := db.New(app.batchCutoverReader(writer))
	reader := session.NewBatchLiveReader(readQ, app.dataDir)
	// TxStarter: every live read runs in ONE read-only tx on the writer.
	reader.TxStarter = writer
	res := reader.BatchLiveForOwner(ctx, l.sessionID)
	if res.Err != nil || len(res.Own) == 0 {
		return joinOpenWork(schedule, generic)
	}
	parts := append(schedule, make([]string, 0, cliOpenWorkNamed)...)
	for i, n := range res.Own {
		if i == cliOpenWorkNamed {
			parts = append(parts, fmt.Sprintf("%d more", len(res.Own)-cliOpenWorkNamed))
			break
		}
		what := pt2SoleTaskLabel(ctx, readQ, n)
		if what == "" {
			what = n.TaskKind
		}
		if what == "" {
			what = n.Kind
		}
		part := fmt.Sprintf("%s %q", what, n.ToolCallID)
		if n.ChildSessionID != "" {
			part += fmt.Sprintf(" (child session %s)", n.ChildSessionID)
		}
		part += " on " + describeBatchHost(ctx, readQ, app.dataDir, n.HostID, n.HostStatus)
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// describeBatchHost renders a batch node's host: this process, or "host
// <id> (PID n, <status>)" for another one -- the PID is the retained
// display-only async_hosts row (written by every host registration,
// flag-independent), "host <id> (<status>)" when no row is readable. The
// reader's probe verdict is re-probed only if it could not decide. Display
// only; a host-read failure never changes the verdict.
func describeBatchHost(ctx context.Context, q *db.Queries, dataDir, hostID string, st session.HostLockStatus) string {
	if session.IsOwnHostID(hostID) {
		return "this process"
	}
	if st == session.HostStatusUnknown && dataDir != "" {
		if s, err := session.ProbeHostShared(dataDir, hostID); err == nil {
			st = s
		}
	}
	desc := "host " + hostID
	pid := int64(0)
	if h, err := q.GetAsyncHostRow(ctx, hostID); err == nil {
		pid = h.Pid
	}
	if pid > 0 {
		return fmt.Sprintf("%s (PID %d, %s)", desc, pid, st)
	}
	return fmt.Sprintf("%s (%s)", desc, st)
}

// joinOpenWork is the shared schedule-plus-fallback join.
func joinOpenWork(schedule []string, generic string) string {
	if len(schedule) > 0 {
		return strings.Join(schedule, "; ")
	}
	return generic
}

// pt2SoleTaskLabel resolves the meaningful label of one live batch root:
// the sole direct task child's task kind when the root wraps exactly one
// task (the one-task delegation wrapper), the empty string otherwise.
// Multi-task and nested-batch roots keep the structural label; a read
// failure falls back to it too. Read only, display only.
func pt2SoleTaskLabel(ctx context.Context, q *db.Queries, n session.BatchLiveNode) string {
	children, err := q.ListBatchChildren(ctx, db.ListBatchChildrenParams{
		RootID:         n.Ref.RootID,
		ParentID:       sql.NullString{String: n.Ref.RootID, Valid: true},
		OwnerSessionID: n.SessionID,
	})
	if err != nil {
		return ""
	}
	label := ""
	for _, c := range children {
		if c.Kind != "task" || label != "" {
			return ""
		}
		label = c.TaskKind.String
	}
	return label
}
