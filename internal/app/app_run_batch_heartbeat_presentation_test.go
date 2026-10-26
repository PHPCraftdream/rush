// The batch wait heartbeat's presentation contract (package T2): a
// one-task batch root is named by its sole task child's task kind, a
// multi-task root keeps the structural "batch" label, a foreign host shows
// its retained PID next to the probe verdict, an own host stays "this
// process", and missing facts fall back unchanged (structural label /
// generic wording). Display only: scope authority, cadence and the failure
// fallback are pinned by the existing tests.
package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// pt2PresentationLoop describes the open work of one fresh session on the
// harness.
func pt2PresentationLoop(t *testing.T, h *loopHarness, name string) (string, string) {
	t.Helper()
	ctx := context.Background()
	sess, err := h.app.Sessions.Create(ctx, name)
	require.NoError(t, err)
	l := &cliLoop{app: h.app, ctx: ctx, sessionID: sess.ID}
	return sess.ID, l.describeOpenWork()
}

func pt2PresentationHarness(t *testing.T) *loopHarness {
	t.Helper()
	return newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
}

// Revert-check: removing pt2SoleTaskLabel (the one-task wrapper resolution)
// makes the heartbeat print the structural label; the first assertion sees
// `batch "call-…"` instead of the task kind.
func TestBatchHeartbeatPresentation_OneTaskRootNamesTaskKind(t *testing.T) {
	h := pt2PresentationHarness(t)
	sessID, _ := pt2PresentationLoop(t, h, "pt2-hb-one")
	f6ReserveDelegation(t, h.app, sessID, "call-pt2-one", "", "shell", "running")
	l := &cliLoop{app: h.app, ctx: context.Background(), sessionID: sessID}
	got := l.describeOpenWork()
	require.Contains(t, got, `shell "call-pt2-one" on this process`)
	require.NotContains(t, got, `batch "call-pt2-one"`)
}

// Revert-check: labelling a MULTI-task root by one child's kind would print
// `shell "call-…"`; the structural label assertion below fails first.
func TestBatchHeartbeatPresentation_MultiTaskRootKeepsStructuralLabel(t *testing.T) {
	h := pt2PresentationHarness(t)
	sessID, _ := pt2PresentationLoop(t, h, "pt2-hb-multi")
	ownHost := f6HostID(t, h.app) // register the own host identity before seeding
	pt2ReserveLiveRoot(t, h.app, sessID, ownHost, "call-pt2-multi", "shell", "agent")
	l := &cliLoop{app: h.app, ctx: context.Background(), sessionID: sessID}
	got := l.describeOpenWork()
	require.Contains(t, got, `batch "call-pt2-multi" on this process`)
	require.NotContains(t, got, `shell "call-pt2-multi"`)
}

func TestBatchHeartbeatPresentation_ForeignHostShowsRetainedPID(t *testing.T) {
	h := pt2PresentationHarness(t)
	ctx := context.Background()
	foreign, err := filelock.TryAcquireFileLock(session.HostLockPath(h.dataDir, "pt2-peer"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreign.Release() })
	_, err = db.New(h.app.DB()).RegisterAsyncHostRow(ctx, db.RegisterAsyncHostRowParams{ID: "pt2-peer", Pid: 4242, Label: "peer", StartedAt: 1700000000})
	require.NoError(t, err)
	sessID, _ := pt2PresentationLoop(t, h, "pt2-hb-pid")
	pt2ReserveLiveRoot(t, h.app, sessID, "pt2-peer", "call-pt2-pid", "argv")
	l := &cliLoop{app: h.app, ctx: ctx, sessionID: sessID}
	got := l.describeOpenWork()
	require.Contains(t, got, `argv "call-pt2-pid" on host pt2-peer (PID 4242, alive)`)
}

func TestBatchHeartbeatPresentation_OwnHostHasNoPID(t *testing.T) {
	h := pt2PresentationHarness(t)
	sessID, _ := pt2PresentationLoop(t, h, "pt2-hb-own")
	f6ReserveDelegation(t, h.app, sessID, "call-pt2-own", "", "agent", "running")
	l := &cliLoop{app: h.app, ctx: context.Background(), sessionID: sessID}
	got := l.describeOpenWork()
	require.Contains(t, got, `"call-pt2-own" on this process`)
	require.NotContains(t, got, "PID")
}

func TestBatchHeartbeatPresentation_MissingFactsFallbackUnchanged(t *testing.T) {
	h := pt2PresentationHarness(t)
	_, got := pt2PresentationLoop(t, h, "pt2-hb-none")
	require.Equal(t,
		"a running job/delegation, a host that is not provably dead, or an open wake schedule",
		got)
}
