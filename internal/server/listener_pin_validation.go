package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// ValidateEffectivePins checks that listener-declared sessions exist as
// process-owned top-level sessions. Missing record pins are intentionally soft.
func ValidateEffectivePins(ctx context.Context, app *appPkg.App, tokens []config.APIToken, policies []ListenerPolicy, logger *slog.Logger) error {
	if app == nil || app.Sessions == nil {
		return errors.New("session service required to validate listener pins")
	}
	if logger == nil {
		logger = slog.Default()
	}
	declared := make(map[string]bool)
	for _, policy := range policies {
		for tokenID := range policy.DeclaredPins {
			declared[tokenID] = true
		}
	}
	for _, policy := range policies {
		for tokenID, sessionID := range policy.Pins {
			if !declared[tokenID] {
				continue
			}
			row, err := app.Sessions.Get(ctx, sessionID)
			if err != nil {
				return fmt.Errorf("listener pin for token %s references missing session %s", tokenID, sessionID)
			}
			if row.ParentSessionID != "" || !session.Owns(row.WorkspaceRoot, appWorkspaceRoot(app), app.ProcessHome()) {
				return fmt.Errorf("listener pin for token %s must reference an owned top-level session", tokenID)
			}
		}
	}
	checked := make(map[string]bool)
	for _, token := range tokens {
		binding := token.Binding.Normalized()
		if binding.Kind != "session" || binding.Pin == nil || checked[binding.Pin.Session] {
			continue
		}
		checked[binding.Pin.Session] = true
		row, err := app.Sessions.Get(ctx, binding.Pin.Session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				logger.Warn("API token pin references missing session", "token_id", token.ID, "session_id", binding.Pin.Session)
				continue
			}
			return fmt.Errorf("validate record pin for token %s: %w", token.ID, err)
		}
		if row.ParentSessionID != "" || !session.Owns(row.WorkspaceRoot, appWorkspaceRoot(app), app.ProcessHome()) {
			return fmt.Errorf("record pin for token %s must reference an owned top-level session", token.ID)
		}
	}
	return nil
}
