package cmd

// Role validation for `rush run --role`.

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

// validateRunRole applies the run command's required-role and vocabulary checks.
// It is separate from boot/config loading so resume guidance can be checked
// without starting a run.
func validateRunRole(role string) (config.SelectedModelType, error) {
	if role == "" {
		return "", fmt.Errorf("--role is required: pass --role smart, --role fast, worker, reviewer, or an exact configured custom role name")
	}
	if err := config.ValidateCustomRoleName(role); err == nil {
		return "", nil
	}
	parsed, err := resolveModelRole(role)
	if err == nil {
		return parsed, nil
	}
	return "", fmt.Errorf("--role: invalid value %q (built-in role or valid exact custom name required)", role)
}

func customRunRole(role string) bool {
	if role == "" {
		return false
	}
	if _, err := resolveModelRole(role); err == nil {
		return false
	}
	return config.ValidateCustomRoleName(role) == nil
}

func requestedRunRole(role string) string {
	if customRunRole(role) {
		return role
	}
	return ""
}

// foldRoleModel validates --role and resolves --continue's session first
// (R2C-14). Since plan D2 (docs/plans/2026-10-15-role-models-hot-reload.md
// §2.2) the role's MODEL is no longer folded into the smart slot: the
// returned model is always the caller's smartModel unchanged. The
// coordinator resolves the role's config slot live per turn
// (internal/agent/coordinator_models_role.go roleSlotModel), so a
// hot-reloaded role model reaches every later turn and nothing is
// persisted. A rush run follows settings, so only a configured slot in
// models satisfies the role — a model stored on the session does not — and
// the run fails fast instead of silently running the smart slot.
// `--continue` names its session only implicitly (the most recently updated
// top-level one), so that id is resolved here first and returned with
// useLast cleared so the caller passes THAT pair on (R3C-10). Without a
// fold nothing is resolved and (sessionID, useLast) come back unchanged.
func foldRoleModel(ctx context.Context, a *app.App, models map[config.SelectedModelType]config.SelectedModel, role string, modelType config.SelectedModelType, smartModel, sessionID string, useLast bool) (model, resolvedSessionID string, resolvedUseLast bool, err error) {
	if customRunRole(role) {
		if smartModel != "" {
			return "", sessionID, useLast, fmt.Errorf("--model cannot be combined with custom --role %q", role)
		}
		return "", sessionID, useLast, nil
	}
	if modelType == config.SelectedModelTypeSmart || smartModel != "" {
		return smartModel, sessionID, useLast, nil
	}
	if sessionID == "" && useLast {
		if last, lastErr := a.Sessions.GetLast(ctx); lastErr == nil {
			sessionID, useLast = last.ID, false
		}
	}
	roleModel, ok := models[modelType]
	if !ok || roleModel.Model == "" {
		return "", sessionID, useLast, fmt.Errorf("--role %s: no %s model configured (run \"rush models use --%s <model>\" first)", role, modelType, modelType)
	}
	return "", sessionID, useLast, nil
}
