package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/config"
)

func (c *coordinator) finishCustomRoleResolveSnapshot(ctx context.Context, cfg *config.Config, smartModel Model, selected config.SelectedModel, source string, wantTools bool) (*resolvedOverrides, error) {
	return newModelsComponent(c).finishCustomRoleResolveSnapshot(ctx, cfg, smartModel, selected, source, wantTools)
}
