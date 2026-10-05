package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
)

// bgShellMissingRowGrace bounds how long finishBGShellRow keeps retrying a
// missing bg_shell row. Guarded by a mutex so tests can shorten it without
// racing the goroutines that read it.
var bgShellMissingRowGraceMu sync.Mutex

var bgShellMissingRowGrace = 30 * time.Second

func setBGShellMissingRowGrace(d time.Duration) {
	bgShellMissingRowGraceMu.Lock()
	defer bgShellMissingRowGraceMu.Unlock()
	bgShellMissingRowGrace = d
}

func bgShellMissingRowGraceDuration() time.Duration {
	bgShellMissingRowGraceMu.Lock()
	defer bgShellMissingRowGraceMu.Unlock()
	return bgShellMissingRowGrace
}

// notifyOnBackgroundJobDone mirrors buildTools' kill-switch default
// (coordinator_tools.go): a nil store/options or a nil pointer means enabled.
func (c *coordinator) notifyOnBackgroundJobDone() bool {
	if c.cfg == nil {
		return true
	}
	opts := c.cfg.Config().Options
	return opts == nil || opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
}

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
// The independent row observer is registered after ClaimShell below, even when
// notification callbacks are disabled. Notification callbacks only persist the
// wake notice; this observer is the sole terminal-row writer.
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
		var collision *session.ErrBGShellIDCollision
		if errors.As(err, &collision) {
			// Cross-process shell id collision: tell the model why, and how out.
			completion.Content = fmt.Sprintf("background shell %s refused: its id already keys a running background shell of another rush process (host %s) on this session; re-run the command to get a fresh shell id", metadata.ShellID, collision.HostID)
		} else {
			completion.Content = fmt.Sprintf("background shell %s refused: its ledger row could not be committed (%s)", metadata.ShellID, err)
		}
		return
	}
	manager := t.coordinator.background
	if manager == nil {
		return
	}
	sh, ok := manager.GetOwned(sessionID, metadata.ShellID)
	if !ok {
		t.finishBGShellRow(context.Background(), sessionID, metadata.ShellID, "output unavailable", true)
		return
	}
	sh.OnDone(func() {
		stdout, stderr, _, runErr := sh.GetOutput()
		summary := backgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed())
		t.finishBGShellRow(context.Background(), sessionID, sh.ID, summary, shell.ExitCode(runErr) != 0)
		// Notifications off: this observer is the only callback and the row it
		// just committed is the durable outcome, so it ends the hold itself --
		// before its recheck (R7B-1), never while a notifier owns the notice.
		if !t.coordinator.notifyOnBackgroundJobDone() {
			sh.MarkCompletionRecorded()
		}
		t.coordinator.noteSubAgentChildRunEnded(sessionID)
	})
}

func (t *asyncTool) finishBGShellRow(ctx context.Context, sessionID, shellID, summary string, failed bool) {
	if t.coordinator == nil || t.coordinator.asyncJobs == nil || t.coordinator.asyncJobs.store == nil {
		return
	}
	store := t.coordinator.asyncJobs.store
	var missingSince time.Time
	if err := t.coordinator.asyncJobs.retryAsyncStoreOp(ctx, func() error {
		row, err := store.Get(ctx, sessionID, shellID)
		if err != nil {
			// Only a missing row gets a grace period; every other error
			// keeps the existing retry behaviour.
			if errors.Is(err, sql.ErrNoRows) {
				if missingSince.IsZero() {
					missingSince = time.Now()
				} else if time.Since(missingSince) > bgShellMissingRowGraceDuration() {
					slog.Warn("background shell row is gone; giving up",
						"session_id", sessionID, "shell_id", shellID)
					return nil
				}
			}
			return err
		}
		missingSince = time.Time{} // Readable again; the timer is irrelevant.
		if row.State != "running" {
			return nil
		}
		if session.JobKind(row.Kind) != session.JobKindBGShell {
			return nil
		}
		result, err := store.Transition(ctx, session.TransitionParams{
			Owner: sessionID, ToolCallID: shellID, State: session.ShellExitState(failed),
			ResultSummary: summary, ResultIsError: failed, Delivery: "done", Reacted: true,
		})
		if err != nil {
			return err
		}
		if result.Outcome == session.TransitionWon || result.Outcome == session.TransitionLost {
			return nil
		}
		return fmt.Errorf("background shell transition lost: %s", shellID)
	}); err != nil {
		slog.Error("failed to finish background-shell row", "session_id", sessionID, "shell_id", shellID, "err", err)
	}
}
