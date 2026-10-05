package session

import (
	"context"
	"fmt"
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

// ClaimShell durably claims a background shell: kind=bg_shell, tool_call_id = shellID,
// running, and announced in one step -- the shell's start is announced by the
// tool response that reported the shell id, so the row is immediately both
// open work (running) and pullable once terminal (DUR-7 needs announced=1).
// An existing claim of the same shell id is an idempotent success (Existing), never
// a second row. The returned error is the caller's signal to refuse the
// background start (fail-closed): a shell without a row is the defect class
// this claim removes.
func (s *AsyncJobStore) ClaimShell(ctx context.Context, owner, shellID string, originCLI bool) (ClaimResult, error) {
	result, err := s.Claim(ctx, ClaimParams{
		Owner:      owner,
		ToolCallID: shellID,
		Kind:       JobKindBGShell,
		ToolName:   BGShellToolName,
		Input:      bgShellInputPrefix + shellID,
		OriginCLI:  originCLI,
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("async job store: claim shell: %w", err)
	}
	if err := s.MarkAnnounced(ctx, owner, shellID); err != nil {
		return ClaimResult{}, fmt.Errorf("async job store: claim shell: mark announced: %w", err)
	}
	return result, nil
}

// ShellExitState maps a finished shell's outcome to the row's terminal state.
func ShellExitState(failed bool) string {
	if failed {
		return "failed"
	}
	return "completed"
}
