package server

// Task #622 (F-1 of docs/reviews/2026-08-20 ninth review, commit 3b2664da),
// redesigned in #631: the external-ownership proof for history-destructive
// handlers is a kernel-attested SHARED lock probe
// (session.TryHoldSessionLockShared via holdExternalSilenceProof), not the
// old InspectSessionLock byte-heuristic. Consequently these tests forge
// external owners with REAL exclusive locks (session.TryAcquireSessionLock
// in this test process — flock/LockFileEx conflict across handles even
// within one process), not by writing bytes to disk: inert bytes no longer
// deny anything (see TestTryHoldSessionLockShared_InertBytesNeverMatter in
// internal/session), so byte-forgery cannot exercise the refusal at all.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// holdRealExclusiveLock acquires a REAL exclusive session lock, exactly as
// a concurrent `rush run --session S` would, and returns a release func.
// This is the only faithful stand-in for an external owner under the
// probe-based proof: the shared probe denies against the kernel lock
// itself, regardless of what any file's bytes say.
func holdRealExclusiveLock(t *testing.T, dataDir, sessionID string) func() {
	t.Helper()
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err, "test must acquire the real exclusive lock")
	return func() { require.NoError(t, lk.Release()) }
}

// TestHandleRerunMessage_ExternalLockHolderFailsClosed is the core F-1
// regression under the #631 probe design: with the session idle in THIS
// process (mailbox free, reservation succeeds) but a REAL exclusive OS
// lock held (as a concurrent external `crush run` holds it), the rerun
// must be refused with an actionable error and must not delete ANY
// history — not the tail, not the target.
//
// Revert-check: delete the step 1b block in handlers_agent.go (the
// holdExternalSilenceProof branch) and this test fails: the reply becomes
// EventResponse ok and both the tail and target messages are gone.
func TestHandleRerunMessage_ExternalLockHolderFailsClosed(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	sess, err := a.Sessions.Create(t.Context(), "test-rerun-external-owner")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	// A still-streaming tail row: no Finish part, so a plain Delete would
	// refuse it with ErrMessageStillStreaming — exactly the row the flagged
	// ForceDelete branch exists to bulldoze.
	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial external reply"}},
	})
	require.NoError(t, err)
	require.False(t, tailMsg.IsFinished(), "precondition: tail row is still streaming")

	// In-process ownership proofs all pass: the (fake) coordinator is idle,
	// so Cancel/wait-for-idle succeeds and ReserveExclusive succeeds.
	a.AgentCoordinator = &mailboxLikeCoordinator{}

	// Hold the REAL exclusive lock AFTER the in-process state is proven
	// idle, so the ONLY thing the refusal can be reacting to is the OS-level
	// lock.
	release := holdRealExclusiveLock(t, dataDir, sessionID)
	defer release()

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"rerun must fail closed while a real exclusive lock owns the session")
	require.Contains(t, env.Error, "owned by another process",
		"the error must name the external owner, not a generic failure")

	// The decisive assertions: nothing was deleted.
	stillTail, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.NoError(t, getErr, "still-streaming tail must NOT be force-deleted under a real external owner")
	require.Equal(t, "partial external reply", stillTail.FullText())

	stillTarget, getErr := a.Messages.Get(ctx, userMsg.ID)
	require.NoError(t, getErr, "target user message must NOT be deleted when the rerun is refused")
	require.Equal(t, "rerun me", stillTarget.Content().Text)

	// Failing closed must not wedge the session: the handler released its
	// reservation on the bailout path, so a follow-up claim succeeds.
	_, _, cancel, ok := a.AgentCoordinator.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "refused rerun must release its reservation, not wedge the mailbox")
	a.AgentCoordinator.ReleaseExclusive(sessionID, 1, cancel)
}

// TestHandleRerunMessage_OwnHeldLockNowRefuses pins the semantic change
// the #631 redesign deliberately makes: the old heuristic's
// "PID == os.Getpid() → provably ours → allow" escape is GONE. A genuinely
// held exclusive lock — even held by THIS process — denies the shared
// probe, and that is strictly safer: a live in-process holder means a turn
// is running somewhere (a detached run, a straggler goroutine), and
// force-deleting under it is exactly what the guard exists to prevent.
// (An own RELEASED leftover still grants — pinned separately in
// p631_ownlock_test.go.)
//
// Revert-check: reintroduce an own-PID allow into
// holdExternalSilenceProofFromConfig (e.g. return the probe when
// busyErr.HolderPID == os.Getpid()) and this test fails: the reply becomes
// EventResponse ok.
func TestHandleRerunMessage_OwnHeldLockNowRefuses(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	sess, err := a.Sessions.Create(t.Context(), "test-rerun-own-held")
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
		Parts: []message.ContentPart{message.TextContent{Text: "streaming under own lock"}},
	})
	require.NoError(t, err)
	require.False(t, tailMsg.IsFinished())

	a.AgentCoordinator = &mailboxLikeCoordinator{}
	release := holdRealExclusiveLock(t, dataDir, sessionID) // OUR process holds it
	defer release()

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"a genuinely held exclusive lock must refuse the rerun even when this process holds it")
	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.NoError(t, getErr, "tail must survive while any real holder lives")
}

// TestDeleteMessageRescuingOrphan_NilStoreRefusesRescue
// covers the fail-open hole that remains unchanged by the redesign: when
// the data directory cannot be resolved (nil store), "could not look" must
// read as "refuse", not "looked and found nothing".
//
// NOTE on what this test observes: its minimal App has a NIL STORE, so it
// exercises the `a == nil || a.Store() == nil` guard — the first of the
// two unresolvable-state guards, NOT the nil-config/empty-DataDirectory
// one. That second guard is pinned separately, at the FromConfig seam, in
// TestHoldExternalSilenceProofFromConfig_UnresolvableDirectoryRefuses.
//
// Revert-check: in holdExternalSilenceProof, change the
// `if a == nil || a.Store() == nil` branch to allow (returning nil, false,
// "") and this test fails: the delete returns nil and the still-streaming
// row is gone.
func TestDeleteMessageRescuingOrphan_NilStoreRefusesRescue(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t) // minimal App: Messages only, no config store
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial, unverifiable ownership"}},
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished())

	a.AgentCoordinator = &mockAgentCoordinator{busy: false}

	delErr := deleteMessageRescuingOrphan(ctx, a, created.ID)
	require.ErrorIs(t, delErr, message.ErrMessageStillStreaming,
		"rescue must be refused when external ownership cannot be verified (nil store)")
	still, getErr := a.Messages.Get(ctx, created.ID)
	require.NoError(t, getErr, "still-streaming row must NOT be force-deleted when ownership is unverifiable")
	require.Equal(t, "partial, unverifiable ownership", still.FullText())
}

// TestHoldExternalSilenceProof_FailClosedMatrix pins the helper's
// App-level decision table. The config-level unresolvable-directory guard
// is deliberately NOT claimed here — an App with a non-nil store but an
// unresolvable directory cannot be built from this package (see
// holdExternalSilenceProof's doc) — it is pinned at the FromConfig seam in
// TestHoldExternalSilenceProofFromConfig_UnresolvableDirectoryRefuses.
func TestHoldExternalSilenceProof_FailClosedMatrix(t *testing.T) {
	// Nil app / nil store: refuse.
	probe, refuse, msg := holdExternalSilenceProof(nil, "s")
	require.Nil(t, probe)
	require.True(t, refuse)
	require.NotEmpty(t, msg)

	// No lock file at all: grant (and the caller MUST Release the probe).
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := a.Sessions.Create(t.Context(), "matrix")
	require.NoError(t, err)
	probe, refuse, _ = holdExternalSilenceProof(a, sess.ID)
	require.False(t, refuse, "no lock file at all must grant")
	require.NoError(t, probe.Release())

	// Real exclusive holder (this process — indistinguishable to the
	// kernel): refuse.
	release := holdRealExclusiveLock(t, a.Config().Options.DataDirectory, sess.ID)
	probe, refuse, msg = holdExternalSilenceProof(a, sess.ID)
	require.Nil(t, probe, "no probe on refusal")
	require.True(t, refuse, "a real exclusive holder must refuse")
	require.Contains(t, msg, "owned by another process")
	release()

	// Released leftover (real acquire then release): grant.
	probe, refuse, _ = holdExternalSilenceProof(a, sess.ID)
	require.False(t, refuse, "a released leftover must grant immediately")
	require.NoError(t, probe.Release())
}

// TestHoldExternalSilenceProofFromConfig_UnresolvableDirectoryRefuses
// pins the config-level unresolvable-directory guard — nil config, nil
// Options, and non-nil Options with empty DataDirectory — each of which
// must refuse rather than silently skip the ownership proof.
//
// This test drives holdExternalSilenceProofFromConfig directly instead of
// through an *appPkg.App because that state is not constructible at the
// App layer from this package: a config-Init'd store always resolves a
// DataDirectory (setDefaults), and app.New panics on a hand-built
// &config.Config{Options: &config.Options{}} (nil provider csync map,
// dereferenced in Config.IsConfigured) before it could return — both
// routes attempted while writing this test's #622 ancestor, both dead
// ends. The FromConfig seam exists so this guard has an observer at all.
//
// Revert-check: in holdExternalSilenceProofFromConfig, change the
// nil-config/empty-DataDirectory guard to allow (returning nil, false,
// "") and this test fails: every sub-case below returns allow.
func TestHoldExternalSilenceProofFromConfig_UnresolvableDirectoryRefuses(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"nil config":               nil,
		"nil options":              {},
		"empty data directory":     {Options: &config.Options{}},
		"resolved dir, no session": {Options: &config.Options{DataDirectory: t.TempDir()}},
	} {
		probe, refuse, msg := holdExternalSilenceProofFromConfig(cfg, "some-session")
		if name == "resolved dir, no session" {
			// Negative control: a resolvable directory with no lock file
			// must GRANT — proves the guard is about resolution, not a
			// blanket refusal.
			require.False(t, refuse, name)
			require.Empty(t, msg, name)
			require.NoError(t, probe.Release(), name)
			continue
		}
		require.Nil(t, probe, name)
		require.True(t, refuse, name)
		require.Contains(t, msg, "data directory unresolvable", name)
	}
}

// TestDeleteMessageRescuingOrphan_ExternalLockHolderRefusesRescue applies
// the same yardstick to deleteMessageRescuingOrphan (handlers_messages.go): the
// orphan rescue previously force-deleted a still-streaming row on the
// strength of in-process idleness alone, which proves nothing about a writer
// in a different process. With a REAL exclusive lock held the rescue must
// be refused (original ErrMessageStillStreaming returned, row intact);
// without one, the rescue still works (negative control, same test).
//
// Revert-check: delete the holdExternalSilenceProof block in
// deleteMessageRescuingOrphan and the first half of this test fails: the
// delete succeeds and the still-streaming row is gone.
func TestDeleteMessageRescuingOrphan_ExternalLockHolderRefusesRescue(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	mkStreamingRow := func(name string) string {
		sess, err := a.Sessions.Create(ctx, name)
		require.NoError(t, err)
		created, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "partial " + name}},
		})
		require.NoError(t, err)
		require.False(t, created.IsFinished())
		return sess.ID
	}

	a.AgentCoordinator = &mailboxLikeCoordinator{} // idle in-process

	// External owner (real exclusive lock): rescue must be refused.
	extSession := mkStreamingRow("externally-owned")
	release := holdRealExclusiveLock(t, dataDir, extSession)
	extMsg, err := a.Messages.List(ctx, extSession)
	require.NoError(t, err)
	require.Len(t, extMsg, 1)

	delErr := deleteMessageRescuingOrphan(ctx, a, extMsg[0].ID)
	require.ErrorIs(t, delErr, message.ErrMessageStillStreaming,
		"rescue must be refused while a real exclusive lock owns the session")
	still, getErr := a.Messages.Get(ctx, extMsg[0].ID)
	require.NoError(t, getErr, "still-streaming row must NOT be force-deleted under a real external owner")
	require.Equal(t, "partial externally-owned", still.FullText())
	release()

	// No external owner (negative control, different session, no lock): the
	// in-process-idle rescue still force-deletes a true orphan.
	freeSession := mkStreamingRow("free")
	freeMsg, err := a.Messages.List(ctx, freeSession)
	require.NoError(t, err)
	require.Len(t, freeMsg, 1)
	require.NoError(t, deleteMessageRescuingOrphan(ctx, a, freeMsg[0].ID),
		"orphan rescue must still work when no exclusive holder owns the session")
	_, getErr = a.Messages.Get(ctx, freeMsg[0].ID)
	require.Error(t, getErr, "true orphan must still be force-deleted when nothing owns the session externally")
}

// TestDeleteMessageRescuingOrphan_HeldWithoutSidecarStillRefuses covers
// the old "held, identity unreadable" shape under the probe design. A real
// exclusive holder whose .pid sidecar is deleted (so the busy error's best-
// effort HolderPID is 0 on Windows, where the primary is unreadable while
// held) must still refuse — the refusal rests on the KERNEL conflict, not
// on any PID being readable. This is the replacement for the old forged
// writeUnknownPIDLock test: bytes alone no longer exercise the refusal.
//
// Revert-check: make TryHoldSessionLockShared skip tryLockFileShared
// (grant without probing) and this test fails: the rescue returns nil and
// the row is gone.
func TestDeleteMessageRescuingOrphan_HeldWithoutSidecarStillRefuses(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "test-orphan-rescue-no-sidecar")
	require.NoError(t, err)

	created, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial, anonymous holder"}},
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished())

	a.AgentCoordinator = &mailboxLikeCoordinator{}
	release := holdRealExclusiveLock(t, dataDir, sess.ID)
	// Strip the sidecar so the holder's identity is genuinely unreadable on
	// Windows (the primary is unreadable while held). On POSIX the primary
	// still reads, so the message names the PID — either way it refuses.
	require.NoError(t, os.Remove(session.SessionLockPath(dataDir, sess.ID)+".pid"))
	defer release()

	delErr := deleteMessageRescuingOrphan(ctx, a, created.ID)
	require.ErrorIs(t, delErr, message.ErrMessageStillStreaming,
		"a held lock with unreadable holder identity must still refuse the rescue")
	still, getErr := a.Messages.Get(ctx, created.ID)
	require.NoError(t, getErr, "still-streaming row must NOT be force-deleted under an anonymous live lock")
	require.Equal(t, "partial, anonymous holder", still.FullText())
}
