package agent

// CI-only tests for the two holder-leak repairs in the durable-enqueue
// producers (gh#10c, gh#10e): a row-keyed Continuation owner must never
// outlive the attempt it pins, and an outbox-keyed replacement must be
// acquired BEFORE the original holder is released, so a sole-owner binding
// survives the transfer.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// consumedInjectSessions stubs the pending_injects delete as "another
// detached consumer already won ownership" (sql.ErrNoRows).
type consumedInjectSessions struct{ session.Service }

func (s *consumedInjectSessions) DeleteInterruptInject(context.Context, string) error {
	return sql.ErrNoRows
}

// TestStartDetachedRun_AlreadyConsumedInjectReleasesHolder pins gh#10c: the
// duplicate detached consumer that loses the inject delete must release its
// row-keyed holder. Statically: startDetachedRun acquires the holder at
// coordinator_interrupt.go (~:746-753); the sql.ErrNoRows branch returns nil
// BEFORE ToSessionAgentCallData/EnqueueRunQueueEntry, so the stub needs
// nothing else.
//
// WHY IT FAILS ON OLD CODE: the ErrNoRows branch returned nil with the
// holder still live, so owners stayed at 1 until process exit.
func TestStartDetachedRun_AlreadyConsumedInjectReleasesHolder(t *testing.T) {
	env := testEnv(t)
	caps := newDiskCapabilityManager()
	provider := newFakeDiskProvider(nil)
	bindingID, err := caps.RegisterProvider(provider)
	require.NoError(t, err)

	coord := &coordinator{
		sessions:        &consumedInjectSessions{Service: env.sessions},
		diskCaps:        caps,
		subAgentDrivers: newSubAgentDriverRegistry(),
	}
	call := SessionAgentCall{
		SessionID:       "s-consumed",
		LogicalCallID:   "d-1",
		InjectID:        "inj-1",
		Prompt:          "p",
		CallOptions:     &CallOptions{DiskBindingID: bindingID, DiskProvider: provider},
		DiskRequirement: &session.SessionExecutionRequirement{BindingID: bindingID},
	}
	err = coord.startDetachedRun(context.Background(), call)
	require.NoError(t, err, "the loser must not fail the caller; it just must not leak")

	owners, _, _ := caps.OwnerDiagnostics()
	require.Equal(t, 0, owners, "the abandoned duplicate attempt's row-keyed holder must die with it")
}

// outboxFallbackSessions stubs the primary durable enqueue as unavailable,
// forcing restartOrphanedWithRetry onto its orphan-outbox fallback.
type outboxFallbackSessions struct{ session.Service }

func (s *outboxFallbackSessions) EnqueueRunQueueEntry(context.Context, string, string, []byte) error {
	return errors.New("primary queue unavailable")
}

// TestRestartOrphanedWithRetry_SoleOwnerOutboxTransferKeepsBinding pins
// gh#10e: the OUTBOX-keyed holder is acquired BEFORE the original
// row-keyed holder is released, so a sole-owner binding survives the
// transfer instead of being pruned and the record persisted against a dead
// provider. Statically: on this path a bare &sessionAgent{} touches only
// a.sessions (EnqueueRunQueueEntry, WriteToOrphanOutbox) and
// a.diskCapsRef(); splitCallsWithoutWaiters only reads call hooks.
//
// WHY IT FAILS ON OLD CODE: the old code released the sole primary holder
// FIRST, so the binding was pruned; the replacement acquire then failed
// (silently ignored) and the outbox row was persisted with owners==0 — the
// Lookup, owners and live-holder assertions below all go red.
func TestRestartOrphanedWithRetry_SoleOwnerOutboxTransferKeepsBinding(t *testing.T) {
	env := testEnv(t)
	caps := newDiskCapabilityManager()
	provider := newFakeDiskProvider(nil)
	bindingID, err := caps.RegisterProvider(provider)
	require.NoError(t, err)

	sa := &sessionAgent{sessions: &outboxFallbackSessions{Service: env.sessions}}
	sa.bindDiskCaps(caps)

	// orphan_call_outbox.session_id is a foreign key (foreign_keys is on for
	// every connection): the fallback insert needs a real session row.
	sess, err := env.sessions.Create(context.Background(), "sole-owner-outbox")
	require.NoError(t, err)

	call := SessionAgentCall{
		SessionID:       sess.ID,
		LogicalCallID:   "orphan-1",
		Prompt:          "recover",
		CallOptions:     &CallOptions{DiskBindingID: bindingID, DiskProvider: provider},
		DiskRequirement: &session.SessionExecutionRequirement{BindingID: bindingID},
	}
	err = sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, err, "the primary enqueue failure is still surfaced")

	// Sole-owner transfer: the binding must still be LIVE under the
	// OUTBOX-keyed holder.
	_, lookupErr := caps.Lookup(bindingID)
	require.NoError(t, lookupErr)
	owners, _, _ := caps.OwnerDiagnostics()
	require.Equal(t, 1, owners)

	// And the outbox row really was written under the id the pump will
	// rebuild from (restartOrphanedWithRetry: outboxID = "orphan-" +
	// call.SessionID + "-" + idempotencyKey, where idempotencyKey =
	// SessionID + "-" + LogicalCallID; db.OrphanCallOutbox.ID is the row's
	// primary key, session.go:399 ListPendingOrphanOutboxEntries).
	entries, err := env.sessions.ListPendingOrphanOutboxEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	outboxID := fmt.Sprintf("orphan-%s-%s", sess.ID, sess.ID+"-orphan-1")
	require.Equal(t, outboxID, entries[0].ID)

	// Releasing the outbox-keyed owner leaves nothing pinned.
	releaseDurableContinuationOwner(caps, bindingID, outboxID)
	owners, _, _ = caps.OwnerDiagnostics()
	require.Equal(t, 0, owners)
}
