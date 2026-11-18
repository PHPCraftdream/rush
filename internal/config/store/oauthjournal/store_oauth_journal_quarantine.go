package oauthjournal

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
)

var (
	OAuthJournalQuarantineRename = os.Rename
	OAuthJournalQuarantineCopy   = func(source, destination string) error {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return cfgcommit.AtomicWriteFile(destination, data, 0o600)
	}
)

// Caller holds the refresh sidecar; availability wins if preservation fails.
func Quarantine(path string) {
	quarantineOAuthJournalWithLogging(path, false)
}

func QuarantineOwned(path string) {
	quarantineOAuthJournalWithLogging(path, true)
}

func quarantineOAuthJournalWithLogging(path string, owned bool) {
	name := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
	err := os.Chmod(path, 0o600)
	if err == nil {
		err = syncOAuthQuarantineFile(path)
	}
	if err == nil {
		err = OAuthJournalQuarantineRename(path, name)
	}
	if err != nil {
		err = OAuthJournalQuarantineCopy(path, name)
		if err == nil {
			err = os.Remove(path)
		}
	}
	if err == nil {
		err = syncOAuthQuarantineFile(name)
	}
	if err == nil {
		err = cfgcommit.SyncConfigParent(filepath.Dir(path))
	}
	if err != nil {
		if owned {
			LogOAuthJournalFailure(path, "quarantine", "Failed to preserve corrupt OAuth journal")
		} else {
			slog.Error("Failed to preserve corrupt OAuth journal", "file", filepath.Base(path))
		}
		return
	}
	slog.Error("Quarantined corrupt OAuth journal", "file", filepath.Base(name))
	PruneQuarantines(path)
}

func syncOAuthQuarantineFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Only this exact provider journal's numeric quarantine suffixes are eligible.
func PruneQuarantines(path string) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return
	}
	prefix := filepath.Base(path) + ".corrupt-"
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		if suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := strings.TrimPrefix(names[i], prefix), strings.TrimPrefix(names[j], prefix)
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return a > b
	})
	if len(names) <= 5 {
		return
	}
	for _, name := range names[5:] {
		if os.Remove(filepath.Join(filepath.Dir(path), name)) != nil {
			slog.Error("Failed to prune corrupt OAuth journal", "file", name)
		}
	}
	if cfgcommit.SyncConfigParent(filepath.Dir(path)) != nil {
		slog.Error("Failed to sync OAuth journal quarantine directory", "file", filepath.Base(path))
	}
}
