package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// claimBackgroundShellRow gives a SYNC bash call's background escape a durable
// async_jobs row (R-BG-1, docs/plans/2026-10-01-bg-shell-ledger.md). The
// CLI/web branch needs none: its row already exists (the async job's own) and
// awaitShell finishes it. But a sync call (SDK origin, or a Drain/wake turn
// launched on context.Background(), whose OriginUnspecified makes asyncTool
// treat it as sync) whose bash moves to the background -- explicitly via
// run_in_background or by the auto-background threshold -- leaves a shell that
// survives the response with no row: its completion was a session_notices row
// written later by the shell's OnDone callback, the "second truth" this work
// removes. The claim (kind=bg_shell, tool_call_id = shell id) happens here,
// BEFORE the job's completion is delivered, so the row exists by the time the
// model sees the "moved to background" answer.
//
// Fail-closed (orchestrator decision 4): a failed claim kills the shell and
// turns the tool result into an error -- a shell without a row is exactly the
// defect class being removed. A store-less ledger (isolated tests) skips the
// claim; the OnDone callback's session_notices fallback still covers it.
//
// The shell's OnDone callback (registered by the bash tool itself, never
// suppressed on the sync branch) later drives persistBGShellCompletion, which
// transitions THIS row instead of writing a notice when it finds one.
func (t *asyncTool) claimBackgroundShellRow(job *asyncJob, sessionID string, response fantasy.ToolResponse, completion *AsyncCompletion) {
	if t.coordinator == nil || t.coordinator.asyncJobs == nil || t.coordinator.asyncJobs.store == nil {
		return
	}
	var metadata tools.BashResponseMetadata
	if json.Unmarshal([]byte(response.Metadata), &metadata) != nil || metadata.ShellID == "" {
		return
	}
	store := t.coordinator.asyncJobs.store
	claimCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := store.ClaimShell(claimCtx, sessionID, metadata.ShellID, job.cli); err != nil {
		slog.Error("background shell claim failed; refusing the background start",
			"session_id", sessionID, "shell_id", metadata.ShellID, "err", err)
		if manager := t.coordinator.background; manager != nil {
			killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = manager.KillOwned(killCtx, sessionID, metadata.ShellID)
			killCancel()
		}
		completion.IsError = true
		completion.Content = fmt.Sprintf("background shell %s refused: its ledger row could not be committed (%s)", metadata.ShellID, err)
		return
	}
}
