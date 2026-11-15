package server

import (
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
)

// The production adapter reports a session as busy while another process holds
// its exclusive lock, refuses to delete it, and lets go once the lock is gone.
func TestAPIAppWriteStoreHonoursRealSessionLock(t *testing.T) {
	isolateAPIGlobals(t)
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := apiAppWriteStore{app: app}
	item, err := app.Sessions.Create(t.Context(), "locked")
	if err != nil {
		t.Fatal(err)
	}
	dataDir := app.Config().Options.DataDirectory
	if store.SessionBusy(item.ID) {
		t.Fatal("idle session reported busy")
	}
	lock, err := session.TryAcquireSessionLock(dataDir, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !store.SessionBusy(item.ID) {
		t.Fatal("locked session not reported busy")
	}
	if err := store.DeleteSession(lockTestRequest(t), item.ID); err == nil {
		t.Fatal("delete succeeded while the session lock was held")
	}
	if _, err := app.Sessions.Get(t.Context(), item.ID); err != nil {
		t.Fatalf("session vanished although delete was refused: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if store.SessionBusy(item.ID) {
		t.Fatal("released session still reported busy")
	}
	if err := store.DeleteSession(lockTestRequest(t), item.ID); err != nil {
		t.Fatalf("delete after release: %v", err)
	}
}

func lockTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/v1/sessions/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
