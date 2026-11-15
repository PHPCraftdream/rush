package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// liveOwnerProtection briefly probes the exclusive session lock, then checks
// the run-driver marker. Either check fails closed so maintenance cannot
// remove history underneath a live owner.
func liveOwnerProtection(ctx context.Context, a *app.App, id string) string {
	lock, err := session.TryAcquireSessionLock(a.Config().Options.DataDirectory, id)
	if err != nil {
		var busy *session.SessionLockBusyError
		if errors.As(err, &busy) {
			return "a turn holds the session lock"
		}
		return fmt.Sprintf("session lock state could not be read (fail closed): %v", err)
	}
	_ = lock.Release()

	activity, err := a.SessionActivity(ctx, id)
	if err != nil {
		return "live-owner state could not be read (fail closed)"
	}
	if activity.Facts.Driver != nil {
		return "a live `rush run` loop still drives it"
	}
	if containsUnreadableFact(activity.Facts.Unreadable, "the session's driver markers") {
		return "live-owner state could not be read (fail closed)"
	}
	return ""
}

func gcDropMaintenanceProtected(ctx context.Context, a *app.App, candidates []gcItem) []gcItem {
	kept := make([]gcItem, 0, len(candidates))
	for _, candidate := range candidates {
		if reason := liveOwnerProtection(ctx, a, candidate.ID); reason != "" {
			fmt.Fprintf(os.Stderr, "kept session %s (%s): %s\n", short(session.HashID(candidate.ID)), truncate(candidate.Title, 40), reason)
			continue
		}
		kept = append(kept, candidate)
	}
	return kept
}

func containsUnreadableFact(facts []string, want string) bool {
	for _, fact := range facts {
		if fact == want {
			return true
		}
	}
	return false
}
