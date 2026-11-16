// Fork patch: app database + data-directory bootstrap helpers extracted
// from root.go (lane K1).

package cmdkit

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "embed"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// connectAppDB opens the process database with the migration policy the
// data-directory source dictates (SD-D #1143): a dev build running against
// the shared <main>/.rush of a linked worktree must never migrate (or
// otherwise write schema into) the shared DB; every other combination
// migrates as before. When the process resolved source=shared it also pins
// its worktree to the shared directory with the marker file, so a later
// legacy pre-feature binary cannot silently flip the worktree to
// legacy-local.
func connectAppDB(ctx context.Context, store *config.ConfigStore, cfg *config.Config) (*sql.DB, error) {
	if store.DataDirSource() == config.DataDirSourceShared {
		if err := config.MarkSharedWorkspace(cfg.Options.DataDirectory, config.WorkspaceRoot(store.WorkingDir())); err != nil {
			slog.Warn("Failed to mark shared workspace", "error", err)
		}
	}
	opts := []db.MigrateOption{}
	if !store.MayMigrateData() {
		opts = append(opts, db.WithMayMigrate(false))
	}
	return db.Connect(ctx, cfg.Options.DataDirectory, opts...)
}

func createDotRushDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create data directory: %q %w", dir, err)
	}

	gitIgnorePath := filepath.Join(dir, ".gitignore")
	content, err := os.ReadFile(gitIgnorePath)

	// create or update if old version
	if os.IsNotExist(err) || string(content) == oldGitIgnore {
		if err := os.WriteFile(gitIgnorePath, []byte(defaultGitIgnore), 0o644); err != nil {
			return fmt.Errorf("failed to create .gitignore file: %q %w", gitIgnorePath, err)
		}
	}

	return nil
}

//go:embed gitignore/old
var oldGitIgnore string

//go:embed gitignore/default
var defaultGitIgnore string
