package app

// #859 Part B (design doc §7, mandatory): a call carrying
// RunOverrides.DiskProvider must NEVER reach the durable run queue.
//
// Layer 3 (front door, §7: "ExecuteRun sets FailIfSessionBusy: true when
// overrides.DiskProvider != nil") means a disk-provider run against a
// busy session is refused IMMEDIATELY with agent.ErrSessionBusy — it
// never queues in the mailbox at all, so it can never be orphaned into
// the durable run queue in the first place. This test proves that
// end-to-end through the real ExecuteRun/pump-backed app, and
// deliberately passes FailIfSessionBusy=false on the disk-provider call
// itself to prove Layer 3 forces fail-fast regardless of what the caller
// asked for.
//
// Layers 1 and 2 (the enqueue and rebuild refusals) are the actual
// "belt and braces" backstop for a call that somehow bypasses Layer 3 —
// they are covered directly and independently of ExecuteRun's front door
// by internal/agent/agent_ownership_disk_provider_test.go
// (TestRestartOrphaned_RefusesToEnqueueDiskProviderCall,
// TestStartDetachedRun_RefusesDiskProviderCall) and
// internal/agent/rebuild_diskprovider_test.go
// (TestRebuildSessionAgentCall_RefusesRowMarkedHostDiskProvider), which
// call restartOrphanedWithRetry/startDetachedRun/RebuildSessionAgentCall
// directly, so they still catch a producer that never goes through
// ExecuteRun at all.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunOrphanedDiskProviderCallIsNeverEnqueued(t *testing.T) {
	var (
		gate      = make(chan struct{})
		started   = make(chan struct{})
		startOnce sync.Once
	)

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "DISKPROVIDER_TURN_MARKER"):
			// Layer 3 must refuse this call before it ever reaches the
			// provider — reaching here at all is the failure.
			t.Error("the disk-provider call reached the provider: it must be refused up front by Layer 3, before any provider traffic")
			admissionWriteSSE(w, []string{
				admissionSSEText("cX", "MUST_NOT_HAPPEN"),
				admissionSSEStop("cX", "stop"),
			})
		case strings.Contains(string(body), "OWNER_TURN_MARKER"):
			// The owner: block mid-turn so the session reads as busy for
			// the disk-provider call below, then finish cleanly.
			startOnce.Do(func() { close(started) })
			<-gate
			admissionWriteSSE(w, []string{
				admissionSSEText("c0", "owner done"),
				admissionSSEStop("c0", "stop"),
			})
		default:
			admissionWriteSSE(w, []string{
				admissionSSEText("c0", "IGNORED"),
				admissionSSEStop("c0", "stop"),
			})
		}
	}

	application, sessionID := newAdmissionRaceApp(t, handler)

	// Only the disk-provider call carries DiskProvider + a FolderScope (a
	// DiskProvider requires a scope, one of #859's hard-error
	// validations) — the owner's turn never calls any fs_* tool, so its
	// own overrides are irrelevant to what's under test and are left
	// plain to isolate the cause.
	diskOverrides := RunOverrides{
		FolderScopes: []permission.FolderScopeEntry{{
			Dir: "scope-inside",
			Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpRead},
		}},
		DiskProvider: tools.OSDisk(),
	}

	outcomes := make(chan admissionOutcome, 2)

	// Owner turn: blocked mid-turn by the gate, queued-style
	// (FailIfSessionBusy=false) so it establishes real mailbox ownership
	// the same way a plain `rush run`/web-server caller would.
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1,
		"owner turn OWNER_TURN_MARKER hold", RunOverrides{}, false)
	close(start)
	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("owner call never reached the provider; cannot stage the mid-turn owner")
	}

	// The disk-provider call: deliberately launched with failIfBusy=false
	// to prove Layer 3 forces fail-fast REGARDLESS of what the caller
	// asked for — if Layer 3 were missing, this call would queue behind
	// the owner instead of returning immediately.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2,
		"disk-provider turn DISKPROVIDER_TURN_MARKER", diskOverrides, false)
	close(start2)

	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.Error(t, o.err, "a disk-provider call against a busy session must be refused immediately, not queued")
		require.ErrorIs(t, o.err, agent.ErrSessionBusy)
	case <-time.After(60 * time.Second):
		t.Fatal("disk-provider call did not return promptly — Layer 3 may not be forcing fail-fast")
	}

	// It never reached the mailbox's submitted queue at all.
	require.Zero(t, application.AgentCoordinator.QueuedPrompts(sessionID),
		"a disk-provider call refused by Layer 3 must never be queued in the first place")

	// The run-queue table stays empty: there is nothing to orphan because
	// there was never anything queued.
	has, err := application.Sessions.HasOutstandingRunQueueEntriesForSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.False(t, has, "a disk-provider call refused up front must never appear as a durable run-queue row")

	// Let the owner finish cleanly and confirm nothing shows up later
	// either (defense in depth).
	close(gate)
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.NoError(t, o.err, "the owner's own turn must complete normally")
	case <-time.After(60 * time.Second):
		t.Fatal("owner call did not return after the gate opened")
	}

	require.Never(t, func() bool {
		has, err := application.Sessions.HasOutstandingRunQueueEntriesForSession(context.Background(), sessionID)
		return err != nil || has
	}, 5*time.Second, 100*time.Millisecond,
		"a disk-provider call refused up front must never later appear as a durable run-queue row")
}
