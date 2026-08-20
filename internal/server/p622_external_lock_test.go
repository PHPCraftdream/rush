package server

// Task #622 (F-1 of docs/reviews/2026-08-20 ninth review, commit 3b2664da).
//
// 6b2cd189 made handleRerunMessage prove "nobody is writing to this session"
// before deleting the message tail — but only one layer deep: every proof it
// gathered (Cancel, ClearQueue, IsSessionBusy, ReserveExclusive) consults
// THIS process's coordinator mailbox. A `crush run --session S` executing in
// a DIFFERENT process holds the OS-level session lock (acquired inside the
// agent layer's runOwned) and is completely invisible to that reservation,
// so the ErrMessageStillStreaming -> ForceDelete branch would force-delete a
// row the external owner is still streaming into — under a comment asserting
// the row "will never receive a terminal Finish", which is true within one
// process and false across processes.
//
// The fix adds an OS-level ownership proof (session.InspectSessionLock via
// externalSessionOwnerPID) to BOTH delete paths that force-delete streaming
// rows on the strength of in-process idleness alone:
//
//   - handleRerunMessage step 1b: refuse the rerun (fail closed, reply with
//     the owning PID) before anything is deleted;
//   - deleteMessageRescuingOrphan: refuse the orphan rescue (return the
//     original ErrMessageStillStreaming) instead of force-deleting.
//
// The tests below forge a lock file whose PID is a real, live process that
// is NOT the test binary (the parent PID — the `go test` runner), so
// InspectSessionLock's fast path (fresh mtime) reports Live with a foreign
// PID, exactly what a concurrent external `crush run` looks like from this
// process.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// writeForeignLock writes a session lock file under dataDir/locks whose PID
// is `pid`, freshly touched (mtime = now) so InspectSessionLock's fast path
// reports it Live without needing a heartbeat loop. This is the exact shape
// a healthy external owner's lock has between heartbeat ticks.
func writeForeignLock(t *testing.T, dataDir, sessionID string, pid int) {
	t.Helper()
	dir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := session.SessionLockPath(dataDir, sessionID)
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o644))
	// Precondition, not the mechanism: prove the forged lock reads back as a
	// live FOREIGN owner through the very primitive the fix uses.
	st := session.InspectSessionLock(dataDir, sessionID, externalOwnerLiveThreshold)
	require.True(t, st.Live, "forged lock must read as live")
	require.Equal(t, pid, st.PID, "forged lock must read back the foreign PID")
}

// foreignLivePID returns the PID of a process that is alive right now and is
// not the test binary: the parent (`go test` runner). Using a genuinely
// alive PID matters even though the mtime fast path never probes it — it
// keeps the forged state faithful to a real external owner so the test can
// never pass against a mis-classified lock.
func foreignLivePID(t *testing.T) int {
	t.Helper()
	ppid := os.Getppid()
	require.NotZero(t, ppid)
	require.NotEqual(t, os.Getpid(), ppid, "parent PID must differ from the test binary's own PID")
	require.True(t, session.IsProcessAlive(ppid), "parent process must be alive for the whole test")
	return ppid
}

// TestHandleRerunMessage_ExternalLockHolderFailsClosed is the core F-1
// regression: with the session idle in THIS process (mailbox free, reservation
// succeeds) but a live OS lock held by a DIFFERENT process, the rerun must be
// refused with an actionable error and must not delete ANY history — not the
// tail, not the target.
//
// Revert-check: delete the step 1b block in handlers_agent.go (the
// `if extPID := externalSessionOwnerPID(a, sessionID); extPID != 0 { ... }`
// branch) and this test fails: the reply becomes EventResponse ok and both
// the tail and target messages are gone, force-driven through the same
// in-process-only proofs 6b2cd189 relied on.
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

	// Forge the external owner AFTER the in-process state is proven idle, so
	// the ONLY thing the refusal can be reacting to is the OS-level lock.
	writeForeignLock(t, dataDir, sessionID, foreignLivePID(t))

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"rerun must fail closed while a live external process owns the session")
	require.Contains(t, env.Error, "another process",
		"the error must name the external owner, not a generic failure")

	// The decisive assertions: nothing was deleted. The still-streaming tail
	// row especially must survive — pre-fix, this is the row the
	// ForceDelete branch would have destroyed.
	stillTail, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.NoError(t, getErr, "still-streaming tail must NOT be force-deleted under a live external owner")
	require.Equal(t, "partial external reply", stillTail.FullText())

	stillTarget, getErr := a.Messages.Get(ctx, userMsg.ID)
	require.NoError(t, getErr, "target user message must NOT be deleted when the rerun is refused")
	require.Equal(t, "rerun me", stillTarget.Content().Text)

	// Failing closed must not wedge the session: the handler released its
	// reservation on the bailout path, so a follow-up claim succeeds.
	_, cancel, ok := a.AgentCoordinator.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "refused rerun must release its reservation, not wedge the mailbox")
	a.AgentCoordinator.ReleaseExclusive(sessionID, 1, cancel)
}

// TestHandleRerunMessage_OwnProcessLockDoesNotBlockRerun pins the other side
// of the fix's semantics: a lock held by THIS process's PID is not an
// external owner (mirroring annotateExternalOwnership), so a leftover fresh
// lock from our own just-finished turn must not refuse the rerun.
func TestHandleRerunMessage_OwnProcessLockDoesNotBlockRerun(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	sess, err := a.Sessions.Create(t.Context(), "test-rerun-own-lock")
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
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	a.AgentCoordinator = &mailboxLikeCoordinator{}
	writeForeignLock(t, dataDir, sessionID, os.Getpid())

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	env := decodeReply(t, client)
	require.Equal(t, EventResponse, env.Type,
		"a lock held by this process's own PID must not refuse the rerun")
	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr, "tail must have been deleted on the uncontested rerun")
}

// writeUnknownPIDLock writes a session lock file whose PID cannot be read:
// non-numeric content, no .pid sidecar, fresh mtime. This is exactly the
// LockState a real external owner produces on Windows (mandatory LockFileEx
// makes the lock file itself unreadable for the holder's whole lifetime —
// see readLockFile's doc in internal/session/lock.go) when the best-effort
// .pid sidecar is also missing or failed to write: Exists: true, Live:
// true, PID: 0. Forging it directly avoids needing a real mandatory lock.
func writeUnknownPIDLock(t *testing.T, dataDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := session.SessionLockPath(dataDir, sessionID)
	require.NoError(t, os.WriteFile(path, []byte("not-a-pid\n"), 0o644))
	// Precondition, not the mechanism: prove this state reads back as
	// "live lock, unreadable PID" through the primitive the fix uses.
	st := session.InspectSessionLock(dataDir, sessionID, externalOwnerLiveThreshold)
	require.True(t, st.Live, "forged lock must read as live")
	require.Zero(t, st.PID, "forged lock must read back PID 0 (unreadable holder)")
}

// TestHandleRerunMessage_LiveLockUnknownPIDFailsClosed covers the fail-open
// hole the first cut of the fix shipped: a live lock whose holder PID is
// unreadable (Windows-realistic state, see writeUnknownPIDLock) used to be
// classified as "no owner" because the predicate treated PID==0 as absence.
// The rerun must be REFUSED and the still-streaming tail row must survive.
//
// Revert-check: in externalSessionOwnerRefusal, change the `if st.PID == 0`
// branch to `return false, ""` (treating an unreadable PID as no owner,
// restoring the first-cut behavior) and this test fails: the reply becomes
// EventResponse ok and the streaming row is deleted.
func TestHandleRerunMessage_LiveLockUnknownPIDFailsClosed(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	sess, err := a.Sessions.Create(t.Context(), "test-rerun-unknown-pid")
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
		Parts: []message.ContentPart{message.TextContent{Text: "partial under unreadable lock"}},
	})
	require.NoError(t, err)
	require.False(t, tailMsg.IsFinished())

	a.AgentCoordinator = &mailboxLikeCoordinator{}
	writeUnknownPIDLock(t, dataDir, sessionID)

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"rerun must fail closed when a live lock's holder PID is unreadable")
	require.Contains(t, env.Error, "PID unreadable",
		"the error must say the owner is live-but-unidentifiable, not pretend there is none")

	stillTail, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.NoError(t, getErr, "still-streaming tail must NOT be deleted under a live lock of unknown owner")
	require.Equal(t, "partial under unreadable lock", stillTail.FullText())
}

// TestDeleteMessageRescuingOrphan_UnresolvableDataDirectoryRefusesRescue
// covers the second fail-open hole: when the data directory cannot be
// resolved (nil config store, or nil/empty Options.DataDirectory), the
// first cut silently disabled the external-owner check. "Could not look"
// must read as "refuse", not "looked and found nothing".
//
// NOTE on what this test observes: its minimal App has a NIL STORE, so it
// exercises the `a == nil || a.Store() == nil` guard — the first of the
// two unresolvable-state guards, NOT the nil-config/empty-DataDirectory
// one. That second guard is pinned separately, at the FromConfig seam, in
// TestExternalSessionOwnerRefusalFromConfig_UnresolvableDirectoryRefuses;
// the two cannot be merged into one handler-path test because an App with
// a non-nil store but empty DataDirectory cannot be constructed from this
// package (config-Init always resolves a directory, and a hand-built
// &config.Config{Options: &config.Options{}} panics inside app.New before
// returning — see externalSessionOwnerRefusal's doc).
//
// Revert-check: in externalSessionOwnerRefusal, change the
// `if a == nil || a.Store() == nil` branch to `return false, ""` (restoring
// the first-cut fail-open) and this test fails: the delete returns nil and
// the still-streaming row is gone.
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

// TestExternalSessionOwnerRefusal_FailClosedMatrix pins the helper's
// App-level decision table. The config-level unresolvable-directory guard
// is deliberately NOT claimed here — an App with a non-nil store but an
// unresolvable directory cannot be built from this package (see
// externalSessionOwnerRefusal's doc) — it is pinned at the FromConfig
// seam in TestExternalSessionOwnerRefusalFromConfig_UnresolvableDirectoryRefuses.
func TestExternalSessionOwnerRefusal_FailClosedMatrix(t *testing.T) {
	// Nil app / nil store: refuse.
	refuse, msg := externalSessionOwnerRefusal(nil, "s")
	require.True(t, refuse)
	require.NotEmpty(t, msg)

	// No live lock: allow.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := a.Sessions.Create(t.Context(), "matrix")
	require.NoError(t, err)
	refuse, _ = externalSessionOwnerRefusal(a, sess.ID)
	require.False(t, refuse, "no lock file at all must allow")

	// Live lock, own readable PID: allow (not an external owner).
	writeForeignLock(t, a.Config().Options.DataDirectory, sess.ID, os.Getpid())
	refuse, _ = externalSessionOwnerRefusal(a, sess.ID)
	require.False(t, refuse, "live lock held by this process's own PID must allow")

	// Live lock, foreign readable PID: refuse.
	writeForeignLock(t, a.Config().Options.DataDirectory, sess.ID, foreignLivePID(t))
	refuse, msg = externalSessionOwnerRefusal(a, sess.ID)
	require.True(t, refuse)
	require.Contains(t, msg, "another process (PID")

	// Live lock, unreadable PID: refuse (the Windows-realistic case).
	writeUnknownPIDLock(t, a.Config().Options.DataDirectory, sess.ID)
	refuse, msg = externalSessionOwnerRefusal(a, sess.ID)
	require.True(t, refuse)
	require.Contains(t, msg, "PID unreadable")
}

// TestExternalSessionOwnerRefusalFromConfig_UnresolvableDirectoryRefuses
// pins the config-level unresolvable-directory guard — nil config, nil
// Options, and non-nil Options with empty DataDirectory — each of which
// must refuse rather than silently skip the external-owner check.
//
// This test drives externalSessionOwnerRefusalFromConfig directly instead
// of through an *appPkg.App because that state is not constructible at the
// App layer from this package: a config-Init'd store always resolves a
// DataDirectory (setDefaults), and app.New panics on a hand-built
// &config.Config{Options: &config.Options{}} (nil provider csync map,
// dereferenced in Config.IsConfigured) before it could return — both
// routes attempted while writing this test, both dead ends. The FromConfig
// seam exists so this guard has an observer at all.
//
// Revert-check: in externalSessionOwnerRefusalFromConfig, change the
// nil-config/empty-DataDirectory guard to `return false, ""` (restoring
// the fail-open) and this test fails: every sub-case below returns
// allow with an empty message.
func TestExternalSessionOwnerRefusalFromConfig_UnresolvableDirectoryRefuses(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"nil config":               nil,
		"nil options":              {},
		"empty data directory":     {Options: &config.Options{}},
		"resolved dir, no session": {Options: &config.Options{DataDirectory: t.TempDir()}},
	} {
		refuse, msg := externalSessionOwnerRefusalFromConfig(cfg, "some-session")
		if name == "resolved dir, no session" {
			// Negative control: a resolvable directory with no lock file
			// must ALLOW — proves the guard is about resolution, not a
			// blanket refusal.
			require.False(t, refuse, name)
			require.Empty(t, msg, name)
			continue
		}
		require.True(t, refuse, name)
		require.Contains(t, msg, "data directory unresolvable", name)
	}
}

// TestDeleteMessageRescuingOrphan_ExternalLockHolderRefusesRescue applies
// the same yardstick to deleteMessageRescuingOrphan (handlers_messages.go): the
// orphan rescue previously force-deleted a still-streaming row on the
// strength of in-process idleness alone, which proves nothing about a writer
// in a different process. With a live external lock holder the rescue must
// be refused (original ErrMessageStillStreaming returned, row intact);
// without one, the rescue still works (negative control, same test).
//
// Revert-check: delete the externalSessionOwnerRefusal block in
// deleteMessageRescuingOrphan and the first half of this test fails: the
// delete succeeds and the still-streaming row is gone, exactly the
// force-delete the fix exists to prevent.
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

	// External owner: rescue must be refused.
	extSession := mkStreamingRow("externally-owned")
	writeForeignLock(t, dataDir, extSession, foreignLivePID(t))
	extMsg, err := a.Messages.List(ctx, extSession)
	require.NoError(t, err)
	require.Len(t, extMsg, 1)

	delErr := deleteMessageRescuingOrphan(ctx, a, extMsg[0].ID)
	require.ErrorIs(t, delErr, message.ErrMessageStillStreaming,
		"rescue must be refused while a live external process owns the session")
	still, getErr := a.Messages.Get(ctx, extMsg[0].ID)
	require.NoError(t, getErr, "still-streaming row must NOT be force-deleted under a live external owner")
	require.Equal(t, "partial externally-owned", still.FullText())

	// No external owner (negative control, different session, no lock): the
	// in-process-idle rescue still force-deletes a true orphan.
	freeSession := mkStreamingRow("free")
	freeMsg, err := a.Messages.List(ctx, freeSession)
	require.NoError(t, err)
	require.Len(t, freeMsg, 1)
	require.NoError(t, deleteMessageRescuingOrphan(ctx, a, freeMsg[0].ID),
		"orphan rescue must still work when no external owner holds the lock")
	_, getErr = a.Messages.Get(ctx, freeMsg[0].ID)
	require.Error(t, getErr, "true orphan must still be force-deleted when nothing owns the session externally")
}
