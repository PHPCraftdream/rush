package app

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	const generic = "a running job/delegation, or a host that is not provably dead"
	if l.app == nil || l.app.asyncJobStore == nil {
		return generic
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 2*time.Second)
	defer cancel()
	store := l.app.asyncJobStore
	rows, err := store.ListRunningAsyncJobsForOwners(ctx, []string{l.sessionID})
	if err != nil || len(rows) == 0 {
		return generic
	}
	parts := make([]string, 0, cliOpenWorkNamed)
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
