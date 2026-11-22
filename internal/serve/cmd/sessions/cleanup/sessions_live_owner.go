package sessclean

import (
	"context"
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/app"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	sesslocks "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/live/locks"
	sessbatch "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/batch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

func liveOwnerProtection(ctx context.Context, a *app.App, sessionID string) string {
	return sesslocks.LiveOwnerProtection(ctx, a, sessionID)
}

// gcDropMaintenanceProtected keeps every session with live-owner protection
// out of the gc candidate list, reporting each keep to stderr.
func gcDropMaintenanceProtected(ctx context.Context, a *app.App, candidates []sessbatch.GcItem) []sessbatch.GcItem {
	kept := make([]sessbatch.GcItem, 0, len(candidates))
	for _, candidate := range candidates {
		if reason := sesslocks.LiveOwnerProtection(ctx, a, candidate.ID); reason != "" {
			fmt.Fprintf(os.Stderr, "kept session %s (%s): %s\n", cliout.Short(coresession.HashID(candidate.ID)), cliout.Truncate(candidate.Title, 40), reason)
			continue
		}
		kept = append(kept, candidate)
	}
	return kept
}
