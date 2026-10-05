package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// JobKindBGShell is async_jobs.kind for a background shell that survives its
// tool response without an async-wrapped call row (SDK-origin or Drain turns:
// the sync branch of asyncTool). The row is keyed (owner, tool_call_id = shell
// id) and its ONE terminal transition (AsyncJobStore.Transition) commits the
// result and the wake bit together (docs/plans/2026-10-01-bg-shell-ledger.md).
const JobKindBGShell JobKind = "bg_shell"

// BGShellToolName is the tool_name recorded on bg_shell rows: every background
// shell is a bash-tool shell (internal/agent/tools/bash.go is the only
// BackgroundShellManager.StartOwned caller).
const BGShellToolName = "bash"

// bgShellInput is the stable pseudo-input hashed into input_hash: a shell id is
// unique per manager, so the same id claimed twice is always the same claim
// (idempotent), never an input mismatch.
const bgShellInputPrefix = "bg-shell:"

// ErrBGShellIDCollision is ClaimShell's refusal when the shell id already
// keys a running row of another rush process's host: the per-manager counter
// is unique per process only, so this is a cross-process collision, not an
// idempotent repeat. The foreign row is not ours to touch.
type ErrBGShellIDCollision struct {
	ShellID string
	HostID  string
}

func (e *ErrBGShellIDCollision) Error() string {
	return fmt.Sprintf("background shell id %s collides with a running shell row of another rush process (host %s) on this session", e.ShellID, e.HostID)
}

// ClaimShell durably claims a background shell: kind=bg_shell, tool_call_id = shellID,
// running, and announced in one step -- the shell's start is announced by the
// tool response that reported the shell id, so the row is immediately both
// open work (running) and pullable once terminal (DUR-7 needs announced=1).
// An existing claim of the same shell id is an idempotent success (Existing), never
// a second row. The returned error is the caller's signal to refuse the
// background start (fail-closed): a shell without a row is the defect class
// this claim removes.
func (s *AsyncJobStore) ClaimShell(ctx context.Context, owner, shellID string, originCLI bool) (ClaimResult, error) {
	params := ClaimParams{
		Owner:      owner,
		ToolCallID: shellID,
		Kind:       JobKindBGShell,
		ToolName:   BGShellToolName,
		Input:      bgShellInputPrefix + shellID,
		OriginCLI:  originCLI,
	}
	result, err := s.Claim(ctx, params)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("async job store: claim shell: %w", err)
	}
	if result.Existing && result.Row.HostID != s.HostID() {
		// The id keys another host's row: give a dead host the same one
		// recovery chance Claim's child-session conflict gets, then refuse.
		if _, recErr := s.RecoverDeadHost(ctx, result.Row.HostID, s.messageService()); recErr != nil {
			slog.Warn("async job store: claim shell: dead-host recovery of the colliding row failed", "host_id", result.Row.HostID, "err", recErr)
		} else if result, err = s.Claim(ctx, params); err != nil {
			return ClaimResult{}, fmt.Errorf("async job store: claim shell: %w", err)
		}
		if result.Existing && result.Row.HostID != s.HostID() {
			return ClaimResult{}, &ErrBGShellIDCollision{ShellID: shellID, HostID: result.Row.HostID}
		}
	}
	if err := s.MarkAnnounced(ctx, owner, shellID); err != nil {
		if !errors.Is(err, ErrAsyncJobGone) {
			s.abandonShellClaim(owner, shellID, result.Row.ClaimID)
		}
		return ClaimResult{}, fmt.Errorf("async job store: claim shell: mark announced: %w", err)
	}
	return result, nil
}

// shellClaimCleanupBudget bounds abandonShellClaim on its own context: the
// claim's context may be the very thing that ran out.
const shellClaimCleanupBudget = 5 * time.Second

// abandonShellClaim removes the row ClaimShell just inserted when announcing it
// failed (B9-1, ox round 9). The caller refuses the background start and kills
// the shell, so no observer is ever registered, and nothing else closes an
// unannounced running row of a LIVE host: it would stay open work for the whole
// life of the process. The delete is scoped to this claim AND to announced=0, so
// the already-announced row of an earlier claim of the same shell id (an
// idempotent repeat) and a row another incarnation claimed meanwhile are never
// touched. Best effort: a database that cannot delete either is logged and left
// to the next restart's dead-host sweep.
func (s *AsyncJobStore) abandonShellClaim(owner, shellID, claimID string) {
	ctx, cancel := context.WithTimeout(context.Background(), shellClaimCleanupBudget)
	defer cancel()
	if _, err := s.deleteUnannouncedForClaim(ctx, owner, shellID, claimID); err != nil {
		slog.Error("background shell claim: could not remove the unannounced row", "session_id", owner, "shell_id", shellID, "err", err)
	}
}

// ShellExitState maps a finished shell's outcome to the row's terminal state.
func ShellExitState(failed bool) string {
	if failed {
		return "failed"
	}
	return "completed"
}
