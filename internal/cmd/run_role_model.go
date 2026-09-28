package cmd

// Session-level worker/reviewer model override for `rush run --role`.

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

// sessionRoleModelOverride returns "provider/model" for modelType's
// session-level override (worker/reviewer only), or "" if sessionID is
// empty, unresolvable, or the session never set that slot. Mirrors
// resolveSubAgentModelOverride's session-DB-first cascade
// (internal/agent/coordinator_models.go: session override -> config
// default) so `rush run --role worker` / `--role reviewer` picks up the
// same per-session pin the web UI's set_session_models already persists
// (task #1060), instead of only ever reading the config-wide default the
// --role fold previously used exclusively.
func sessionRoleModelOverride(ctx context.Context, a *app.App, sessionID string, modelType config.SelectedModelType) string {
	if sessionID == "" {
		return ""
	}
	sess, err := a.Sessions.Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	switch modelType {
	case config.SelectedModelTypeWorker:
		if sess.WorkerModelID == "" {
			return ""
		}
		return sess.WorkerModelProvider + "/" + sess.WorkerModelID
	case config.SelectedModelTypeReviewer:
		if sess.ReviewerModelID == "" {
			return ""
		}
		return sess.ReviewerModelProvider + "/" + sess.ReviewerModelID
	default:
		return ""
	}
}
