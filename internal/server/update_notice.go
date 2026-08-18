// Startup update check for the web UI. This is the delivery path that
// replaced the one lost with the TUI: upstream published a pubsub message
// Bubble Tea rendered, the fork removed that UI, and for a while the check
// ran and discarded its answer (see the app.checkForUpdates removal).

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/update"
	"github.com/charmbracelet/crush/internal/version"
)

// updateCheckTimeout bounds the startup check. It is best-effort
// decoration: a slow or unreachable release API must never delay the
// server becoming usable, and the check runs in its own goroutine anyway.
const updateCheckTimeout = 30 * time.Second

// broadcastUpdateNotice checks for a newer release once and, if one exists,
// tells every connected client.
//
// Deliberately fire-and-forget with no retry and no schedule: the operator
// restarts crush often enough, and a background poller would be a network
// call nobody asked for on a machine that may be offline on purpose. A
// failed check is logged at debug — it is not a problem the user can act
// on, and at warn it would cry wolf on every offline start.
//
// Sends nothing when no update is available, so the client can render on
// receipt without deciding anything.
func broadcastUpdateNotice(ctx context.Context, h *Hub) {
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()

	info, err := update.Check(checkCtx, version.Version, update.Default)
	if err != nil {
		slog.Debug("update check failed", "err", err)
		return
	}
	if !info.Available() {
		return
	}

	slog.Info("a newer crush is available", "current", info.Current, "latest", info.Latest)
	h.Broadcast(EventUpdateAvailable, UpdateAvailableWire{
		Current: info.Current,
		Latest:  info.Latest,
	})
}
