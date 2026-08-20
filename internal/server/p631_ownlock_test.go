package server

// Task #631: after cancelling a turn, our own SessionLock.Release leaves
// an empty lock file with a fresh mtime ("released leftover"). Under the
// byte-heuristic rounds this briefly looked like an anonymous live owner
// and refused rerun/orphan-rescue for ~20s. Under the #631 probe redesign
// the destructive path holds a SHARED kernel probe instead, and a released
// leftover — no exclusive holder exists — grants it immediately. These
// tests pin that end-to-end through the handlers, and pin the handoff
// ordering (probe released before RunWithReservedOwnership).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// writeReleasedLeftoverLock forges the exact on-disk shape
// SessionLock.Release leaves behind: lock file present but EMPTY, no .pid
// sidecar, mtime = now (the release-time truncate refreshes it). Under the
// probe design this shape is INERT (nothing denies a probe — no kernel
// holder exists), which is exactly the point: bytes cannot make the
// destructive path refuse, and cannot make it allow under a real holder.
func writeReleasedLeftoverLock(t *testing.T, dataDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := session.SessionLockPath(dataDir, sessionID)
	require.NoError(t, os.WriteFile(path, []byte{}, 0o644))
	now := time.Now()
	require.NoError(t, os.Chtimes(path, now, now))
	// Precondition, not the mechanism: prove this state grants the probe
	// through the very primitive the fix uses.
	probe, err := session.TryHoldSessionLockShared(dataDir, sessionID)
	require.NoError(t, err, "forged leftover must grant the shared probe (no kernel holder)")
	require.NoError(t, probe.Release())
}

// TestHandleRerunMessage_ReleasedLeftoverLockAllowsRerun is the core #631
// regression, probe edition: after cancelling a turn (our own Release just
// ran), pressing rerun must PROCEED. The kernel — not the bytes — is the
// source of truth, and no exclusive holder exists.
//
// Revert-check: in holdExternalSilenceProofFromConfig, treat any existing
// lock file as a refusal (restore a byte-heuristic shape, e.g. refuse
// whenever the file exists) and this test fails: the reply becomes
// EventError and the tail row survives.
func TestHandleRerunMessage_ReleasedLeftoverLockAllowsRerun(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	sess, err := a.Sessions.Create(t.Context(), "test-rerun-released-leftover")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "cancelled mid-stream"}},
	})
	require.NoError(t, err)
	require.False(t, tailMsg.IsFinished())

	a.AgentCoordinator = &mailboxLikeCoordinator{}
	writeReleasedLeftoverLock(t, dataDir, sessionID)

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	env := decodeReply(t, client)
	require.Equal(t, EventResponse, env.Type,
		"a released leftover lock (our own just-finished turn) must not refuse the rerun")

	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr,
		"the still-streaming tail must be deleted once the released leftover is recognized as unowned")
}

// TestHandleRerunMessage_ProbeReleasedBeforeHandoff pins the handoff
// ordering: the shared probe must be released BEFORE
// RunWithReservedOwnership, or the replacement turn's own exclusive lock
// acquire would collide with this process's still-held shared lock and
// bounce the rerun. mailboxLikeCoordinator's RunWithReservedOwnership does
// not take a real lock, so the observable here is indirect but real: with
// rerunPreHandoffSeam (fires after the probe release, before the handoff)
// a test can attempt an EXCLUSIVE acquire at that instant and it must
// SUCCEED — proving no probe is still held at handoff time.
//
// Revert-check: move the probe.Release()/probeHeld = false block to AFTER
// the RunWithReservedOwnership call and this test fails at "expected no
// error" on the exclusive acquire inside the seam.
func TestHandleRerunMessage_ProbeReleasedBeforeHandoff(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	sess, err := a.Sessions.Create(t.Context(), "test-rerun-probe-handoff")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	a.AgentCoordinator = &mailboxLikeCoordinator{}

	var seamErr error
	rerunPreHandoffSeam = func() {
		// At this instant (after probe release, before handoff) an
		// exclusive acquire on the session lock must succeed: the probe is
		// gone. Release it immediately so the handler's handoff (and the
		// negative-control rerun below) proceed normally.
		lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
		if err != nil {
			seamErr = err
			return
		}
		seamErr = lk.Release()
	}
	t.Cleanup(func() { rerunPreHandoffSeam = nil })

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	require.NoError(t, seamErr,
		"the shared probe must be released before the handoff — an exclusive acquire at handoff time must succeed")
	env := decodeReply(t, client)
	require.Equal(t, EventResponse, env.Type, "negative control: the rerun itself must succeed")
}

// TestDeleteMessageRescuingOrphan_ReleasedLeftoverAllowsRescue applies the
// same yardstick to deleteMessageRescuingOrphan: with our own released
// leftover as the only lock evidence, the orphan rescue must proceed and
// force-delete the still-streaming row.
//
// Revert-check: same as the rerun test — restore a byte-heuristic refusal
// on any existing lock file and this test fails with ErrorIs
// ErrMessageStillStreaming while the row survives.
func TestDeleteMessageRescuingOrphan_ReleasedLeftoverAllowsRescue(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "test-orphan-rescue-released-leftover")
	require.NoError(t, err)

	created, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial after cancel"}},
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished())

	a.AgentCoordinator = &mailboxLikeCoordinator{}
	writeReleasedLeftoverLock(t, dataDir, sess.ID)

	require.NoError(t, deleteMessageRescuingOrphan(ctx, a, created.ID),
		"orphan rescue must proceed when the only lock is our own released leftover")
	_, getErr := a.Messages.Get(ctx, created.ID)
	require.Error(t, getErr, "the still-streaming row must be force-deleted once the rescue proceeds")
}
