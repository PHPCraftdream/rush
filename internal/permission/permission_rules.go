package permission

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/db"
)

func (s *permissionService) ListSessionPermissions(ctx context.Context, sessionID string) ([]db.SessionPermission, error) {
	return s.q.ListSessionPermissions(ctx, sessionID)
}

func (s *permissionService) UpdatePermissionEnabled(ctx context.Context, ruleID string, enabled bool) error {
	var enabledInt int64
	if enabled {
		enabledInt = 1
	}
	return s.q.UpdatePermissionEnabled(ctx, db.UpdatePermissionEnabledParams{
		Enabled: enabledInt,
		ID:      ruleID,
	})
}

func (s *permissionService) DeletePermission(ctx context.Context, ruleID string) error {
	return s.q.DeletePermission(ctx, ruleID)
}
