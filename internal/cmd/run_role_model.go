package cmd

// Session-level worker/reviewer model override for `rush run --role`.

import (
	"context"
	"fmt"

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

// foldRoleModel folds --role into the smart slot: without an explicit --model,
// prefer a worker/reviewer override pinned on THIS session (task #1060,
// sessionRoleModelOverride -- same set_session_models path as smart/fast),
// else the config's default for that role's slot. The agent always uses its
// `smart` slot for the turn; --role decides which catalog entry fills it.
// `--continue` names its session only implicitly (the most recently updated
// top-level one), so that id is resolved here first (R2C-14): the pin is read
// from the session the run will actually continue. The resolved id is returned
// with useLast cleared, and the caller must pass THAT pair on: resolving "the
// last session" a second time inside the run could land on another session
// (one created or touched between the two reads), so the pin would come from
// session A while the run continues B (R3C-10). Without a fold nothing is
// resolved and (sessionID, useLast) come back unchanged.
func foldRoleModel(ctx context.Context, a *app.App, role string, modelType config.SelectedModelType, smartModel, sessionID string, useLast bool) (model, resolvedSessionID string, resolvedUseLast bool, err error) {
	if modelType == config.SelectedModelTypeSmart || smartModel != "" {
		return smartModel, sessionID, useLast, nil
	}
	if sessionID == "" && useLast {
		if last, lastErr := a.Sessions.GetLast(ctx); lastErr == nil {
			sessionID, useLast = last.ID, false
		}
	}
	if override := sessionRoleModelOverride(ctx, a, sessionID, modelType); override != "" {
		return override, sessionID, useLast, nil
	}
	roleModel, ok := a.Config().Models[modelType]
	if !ok || roleModel.Model == "" {
		return "", sessionID, useLast, fmt.Errorf("--role %s: no %s model configured (run \"rush models use --%s <model>\" first)", role, modelType, modelType)
	}
	return roleModel.Provider + "/" + roleModel.Model, sessionID, useLast, nil
}
