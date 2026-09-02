package agent

// #859 Layer 1 tests (design doc §7.3, T9 "refuse outright" shape): a
// call carrying a caller-supplied DiskProvider must NEVER reach the
// durable run queue. Unlike a folder scope (T12), a DiskProvider is
// arbitrary in-process Go code with no serializable form at all — a row
// rebuilt from it would silently restart the turn on the REAL disk
// instead of the host's.

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestRestartOrphaned_RefusesToEnqueueDiskProviderCall is the mandatory
// revert-checked test for restartOrphanedWithRetry's refusal (the
// finalizer path reached via abandonOwnershipWithHandoff when a session
// owner exits with queued work still in its mailbox).
//
// REVERT CHECK PROCEDURE:
//  1. In agent_ownership.go's restartOrphanedWithRetry, comment out (or
//     delete) the `if callCarriesDiskProvider(call) { ... return }` block
//     added right after the FromDurableQueue early-return.
//  2. Run: go test ./internal/agent -run TestRestartOrphaned_RefusesToEnqueueDiskProviderCall -v
//  3. The test FAILS: the call is durably enqueued (ListPendingRunQueueEntries
//     is non-empty) and the returned error no longer wraps
//     ErrDiskProviderNotDurable.
//  4. Restore the block and the test passes again.
func TestRestartOrphaned_RefusesToEnqueueDiskProviderCall(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "disk-provider-orphan-refusal")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	fake := newFakeDiskProvider(nil)
	call := SessionAgentCall{
		SessionID:   sess.ID,
		Prompt:      "orphaned work carrying a caller-supplied disk provider",
		CallOptions: &CallOptions{DiskProvider: fake},
	}

	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr,
		"a disk-provider-carrying orphaned call must be refused, not silently enqueued")
	require.ErrorIs(t, retryErr, ErrDiskProviderNotDurable)

	pending, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending,
		"the refused call must NEVER reach the durable run queue — a rebuilt row would silently restart on the real disk")
}

// TestRestartOrphaned_StillEnqueuesPlainCalls is the control: a call with
// no DiskProvider at all is unaffected by the new check and still
// durably enqueues normally.
func TestRestartOrphaned_StillEnqueuesPlainCalls(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "disk-provider-orphan-control")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	call := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "plain orphaned work, no disk provider",
	}

	require.NoError(t, sa.restartOrphanedWithRetry([]SessionAgentCall{call}))

	pending, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1, "a plain call must still be durably enqueued")
}

// TestStartDetachedRun_RefusesDiskProviderCall is the mandatory
// revert-checked test for startDetachedRun's refusal (InterruptAndSend's
// idle-session path). Mirrors p0_2_cross_process_test.go's minimal
// *coordinator construction.
func TestStartDetachedRun_RefusesDiskProviderCall(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "disk-provider-detached-refusal")
	require.NoError(t, err)

	coord := &coordinator{
		cfg:      &config.ConfigStore{},
		sessions: env.sessions,
		messages: env.messages,
	}

	fake := newFakeDiskProvider(nil)
	call := SessionAgentCall{
		SessionID:   sess.ID,
		Prompt:      "detached run carrying a caller-supplied disk provider",
		CallOptions: &CallOptions{DiskProvider: fake},
	}

	startErr := coord.startDetachedRun(t.Context(), call)
	require.Error(t, startErr,
		"a disk-provider-carrying detached call must be refused, not silently enqueued")
	require.ErrorIs(t, startErr, ErrDiskProviderNotDurable)

	pending, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending,
		"the refused call must NEVER reach the durable run queue")
}

// TestStartDetachedRun_StillEnqueuesPlainCalls is the control for
// startDetachedRun: a call with no DiskProvider still durably enqueues.
func TestStartDetachedRun_StillEnqueuesPlainCalls(t *testing.T) {
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "disk-provider-detached-control")
	require.NoError(t, err)

	coord := &coordinator{
		cfg:      &config.ConfigStore{},
		sessions: env.sessions,
		messages: env.messages,
	}

	call := SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "plain detached run, no disk provider",
	}

	require.NoError(t, coord.startDetachedRun(t.Context(), call))

	pending, err := env.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1, "a plain call must still be durably enqueued")
}
