package server

// Sibling of the rename-broadcast ownership test (round-23 review F-3, fixed
// for handleRenameSession in ddf931ee): handleSetSessionModels broadcasts the
// re-fetched Session after writing model overrides, and unlike
// handleCreateSession/handleForkSession (which F-3 ruled harmless — a session
// born in this process cannot be foreign-locked at creation), it targets an
// EXISTING session by ID with no ownership gate anywhere in the handler, so
// its broadcast must carry AnnotateSessionExternalOwnership's annotation like
// every other Session that reaches a client. Otherwise useWS.ts's
// upsertSession replaces the client's annotated row with an un-annotated one
// and a foreign-held session transiently drops out of read-only follow mode
// in every OTHER open tab until the next sessions_list poll re-annotates it.
//
// Fixture note (same as the rename test): the holder is forged as a lock FILE
// (fresh mtime + foreign PID), not a real session.TryAcquireSessionLock in
// this process, because (1) a real in-process lock stamps THIS process's PID,
// which AnnotateSessionExternalOwnership deliberately skips (st.PID == self),
// so it could never exercise the annotation-true path, and (2) the annotation
// path (session.InspectSessionLock) never takes the OS lock — it stats the
// file and reads its bytes — so bytes + fresh mtime are exactly what it
// observes in production. The PID is os.Getppid(): genuinely live and
// genuinely not this process, with no child spawn machinery.
//
// Revert-check: delete the AnnotateSessionExternalOwnership call in
// handleSetSessionModels and this test fails: the broadcast payload arrives
// with OwnedExternal == false.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestHandleSetSessionModels_BroadcastCarriesExternalOwnership(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "models-annotate")
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

	// Broadcast capture, following the rename test's hub.Run + register
	// pattern (p2_regression_test.go's established shape).
	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	payload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:  sess.ID,
		SmartModel: &ModelOverrideWire{Provider: "smart-provider", Model: "smart-model"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, client, WSMessage{ID: "req-1", Type: CmdSetSessionModels, Payload: payload})

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
	require.NotNil(t, updated, "set_session_models must broadcast a session_updated event")
	require.Equal(t, "smart-model", updated.SmartModelID, "broadcast must carry the updated row")
	require.True(t, updated.OwnedExternal,
		"the set_session_models broadcast must carry the external-ownership annotation (F-3 sibling)")
	require.Equal(t, foreignPID, updated.OwnedByPID)
}
