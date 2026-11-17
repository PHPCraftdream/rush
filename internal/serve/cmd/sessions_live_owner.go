package cmd

// gc-drop live-owner protection half of the old sessions_live_owner.go:
// liveOwnerProtection itself moved to the sesslocks package (exported as
// sesslocks.LiveOwnerProtection); this root wrapper stays because the gc
// command (root sessions_gc.go) still owns gcItem filtering here.

import (
	"context"
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/locks"
	"github.com/PHPCraftdream/rush/internal/session"
)

// liveOwnerProtection is the root wrapper over sesslocks.LiveOwnerProtection;
// it stays because the gc command (root sessions_gc.go) still owns gcItem
// filtering here.
func liveOwnerProtection(ctx context.Context, a *app.App, sessionID string) string {
	return locks.LiveOwnerProtection(ctx, a, sessionID)
}

// gcDropMaintenanceProtected keeps every session that still has live-owner
// protection (a held turn lock or a live run driver) out of the gc candidate
// list, reporting each keep to stderr.
func gcDropMaintenanceProtected(ctx context.Context, a *app.App, candidates []gcItem) []gcItem {
	kept := make([]gcItem, 0, len(candidates))
	for _, candidate := range candidates {
		if reason := locks.LiveOwnerProtection(ctx, a, candidate.ID); reason != "" {
			fmt.Fprintf(os.Stderr, "kept session %s (%s): %s\n", short(session.HashID(candidate.ID)), truncate(candidate.Title, 40), reason)
			continue
		}
		kept = append(kept, candidate)
	}
	return kept
}
