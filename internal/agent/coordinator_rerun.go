// StopRerunJobs wires the Rerun handler (internal/server) into the ledger's
// Rerun-scoped stop (work_ledger_rerun.go). The durable reconciliation is
// session.TruncateForRerun's single transaction; this runs only after it
// committed.
package agent

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/session"
)

// StopRerunJobs implements Coordinator.StopRerunJobs: voided is the committed
// truncation's Voided set. A coordinator with no async job store wired is a
// no-op (nothing durable was voided). Best effort: the rows are already void,
// so a stop that misses (foreign host, already terminal) only means the
// executor's completion commits void instead of being stopped early.
func (c *coordinator) StopRerunJobs(_ context.Context, sessionID string, voided []session.VoidedAsyncJob) {
	if c.asyncJobs == nil || len(voided) == 0 {
		return
	}
	ownHost := ""
	if c.asyncJobs.store != nil {
		ownHost = c.asyncJobs.store.HostID()
	}
	var running []string
	var children []string
	for _, v := range voided {
		if v.State == "running" {
			running = append(running, v.ToolCallID)
			if v.HostID != ownHost {
				slog.Warn("coordinator.StopRerunJobs: voided job runs on another host; its executor is not stopped, its completion will commit void",
					"session_id", sessionID, "tool_call_id", v.ToolCallID, "host_id", v.HostID)
			}
		}
		if v.ChildSessionID != "" {
			children = append(children, v.ChildSessionID)
		}
	}
	c.asyncJobs.stopToolCallsForRerun(sessionID, running)
	for _, child := range children {
		c.stopTree(child)
	}
}
