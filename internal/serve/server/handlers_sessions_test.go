package server

// F-3 of docs/reviews/2026-08-24-twenty-third-review-7c8a170e.md: the rename
// broadcast is the ONLY notification a rename produces (session.Service.Rename
// publishes no pubsub event), so it must carry the external-ownership
// annotation like every other Session that reaches a client — otherwise every
// open tab replaces its annotated row and a foreign-held session transiently
// drops out of read-only follow mode until the next sessions_list poll.
//
// Fixture note: the holder is forged as a lock FILE (fresh mtime + foreign
// PID), not a real session.TryAcquireSessionLock in this process, for two
// reasons. (1) A real in-process lock stamps THIS process's PID, which
// AnnotateSessionExternalOwnership deliberately skips (st.PID == self), so it
// could never exercise the annotation-true path. (2) The annotation path
// (session.InspectSessionLock) never takes the OS lock — it stats the file and
// reads its bytes (readLockFile's doc explicitly lists a lock file "written
// directly via os.WriteFile" as a supported reader input) — so bytes + fresh
// mtime are exactly what it observes in production. The PID used is
// os.Getppid(): genuinely live and genuinely not this process, with no child
// spawn machinery.
//
// Revert-check: delete the AnnotateSessionExternalOwnership call in
// handleRenameSession and this test fails: the broadcast payload arrives with
// OwnedExternal == false.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestHandleRenameSession_BroadcastCarriesExternalOwnership(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "rename-annotate")
	require.NoError(t, err)

	// Negative control: with no lock file at all, annotation must leave the
	// session clean — proves the later OwnedExternal==true can only come from
	// the forged lock.
	clean, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	AnnotateSessionExternalOwnership(a, &clean)
	require.False(t, clean.OwnedExternal, "no lock file must mean not externally owned")

	// Forge the foreign holder: fresh mtime (just written) + a PID that is
	// neither 0 nor this process.
	foreignPID := os.Getppid()
	require.NotZero(t, foreignPID, "fixture needs a non-zero foreign PID")
	require.NotEqual(t, os.Getpid(), foreignPID, "fixture needs a PID other than this process")
	lockPath := session.SessionLockPath(dataDir, sess.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o755))
	require.NoError(t, os.WriteFile(lockPath, []byte(strconv.Itoa(foreignPID)+"\n"), 0o644))

	// Fixture sanity: the same annotation the handler must now apply really
	// does flag this state as externally owned.
	held, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	AnnotateSessionExternalOwnership(a, &held)
	require.True(t, held.OwnedExternal, "forged live foreign lock must annotate as externally owned")
	require.Equal(t, foreignPID, held.OwnedByPID)

	// Broadcast capture, following p2_regression_test.go's established
	// hub.Run + register pattern.
	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	payload, err := json.Marshal(RenameSessionPayload{SessionID: sess.ID, Title: "renamed"})
	require.NoError(t, err)
	handleRenameSession(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRenameSession, Payload: payload})

	var updated *session.Session
	deadline := time.Now().Add(2 * time.Second)
	for updated == nil && time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type != EventSessionUpdated {
				continue
			}
			var s session.Session
			require.NoError(t, json.Unmarshal(env.Payload, &s))
			updated = &s
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.NotNil(t, updated, "rename must broadcast a session_updated event")
	require.Equal(t, "renamed", updated.Title, "broadcast must carry the renamed row")
	require.True(t, updated.OwnedExternal,
		"the rename broadcast must carry the external-ownership annotation (F-3)")
	require.Equal(t, foreignPID, updated.OwnedByPID)
}
