package cmd

// Session-level worker/reviewer model override for `rush run --role`.

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

// sessionRoleModelOverride returns "provider/model" for modelType's
// session-level override (worker/reviewer only), or "" if sessionID is
// empty, unresolvable, or the session never set that slot. Mirrors
// resolveSubAgentModelOverride's session-DB-first cascade
// (internal/agent/coordinator_models.go: session override -> config
// default). Since plan D2 it is used for VALIDATION only — the coordinator
// resolves the same cascade live per turn — so the fold merely confirms a
// pin exists before the run starts.
func sessionRoleModelOverride(ctx context.Context, a *app.App, sessionID string, modelType config.SelectedModelType) string {
	if sessionID == "" || agent.WebModelPolicy(ctx) {
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

// foldRoleModel validates --role and resolves --continue's session first
// (R2C-14). Since plan D2 (docs/plans/2026-10-15-role-models-hot-reload.md
// §2.2) the role's MODEL is no longer folded into the smart slot: the
// returned model is always the caller's smartModel unchanged. The
// coordinator resolves the role's session pin or config slot live per turn
// (internal/agent/coordinator_models_role.go roleSlotModel), so a
// hot-reloaded role model reaches every later turn and nothing is
// persisted. The validation — a session pin or a configured config slot
// must exist for the role — is kept so the run fails fast instead of
// silently running the smart slot. `--continue` names its session only
// implicitly (the most recently updated top-level one), so that id is
// resolved here first: validation reads the pin of the session the run
// will actually continue, and the resolved id is returned with useLast
// cleared so the caller passes THAT pair on (R3C-10). Without a fold
// nothing is resolved and (sessionID, useLast) come back unchanged.
func foldRoleModel(ctx context.Context, a *app.App, role string, modelType config.SelectedModelType, smartModel, sessionID string, useLast bool) (model, resolvedSessionID string, resolvedUseLast bool, err error) {
	if modelType == config.SelectedModelTypeSmart || smartModel != "" {
		return smartModel, sessionID, useLast, nil
	}
	if sessionID == "" && useLast {
		if last, lastErr := a.Sessions.GetLast(ctx); lastErr == nil {
			sessionID, useLast = last.ID, false
		}
	}
	roleModel, ok := a.Config().Models[modelType]
	if !ok || roleModel.Model == "" {
		return "", sessionID, useLast, fmt.Errorf("--role %s: no %s model configured (run \"rush models use --%s <model>\" first)", role, modelType, modelType)
	}
	return "", sessionID, useLast, nil
}
