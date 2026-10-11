package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	oauthJournalQuarantineRename = os.Rename
	oauthJournalQuarantineCopy   = func(source, destination string) error {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return atomicWriteFile(destination, data, 0o600)
	}
)

// Caller holds the refresh sidecar; availability wins if preservation fails.
func quarantineOAuthJournal(path string) {
	quarantineOAuthJournalWithLogging(path, false)
}

func quarantineOwnedOAuthJournal(path string) {
	quarantineOAuthJournalWithLogging(path, true)
}

func quarantineOAuthJournalWithLogging(path string, owned bool) {
	name := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
	err := os.Chmod(path, 0o600)
	if err == nil {
		err = syncOAuthQuarantineFile(path)
	}
	if err == nil {
		err = oauthJournalQuarantineRename(path, name)
	}
	if err != nil {
		err = oauthJournalQuarantineCopy(path, name)
		if err == nil {
			err = os.Remove(path)
		}
	}
	if err == nil {
		err = syncOAuthQuarantineFile(name)
	}
	if err == nil {
		err = syncConfigParent(filepath.Dir(path))
	}
	if err != nil {
		if owned {
			logOAuthJournalFailure(path, "quarantine", "Failed to preserve corrupt OAuth journal")
		} else {
			slog.Error("Failed to preserve corrupt OAuth journal", "file", filepath.Base(path))
		}
		return
	}
	slog.Error("Quarantined corrupt OAuth journal", "file", filepath.Base(name))
	pruneOAuthJournalQuarantines(path)
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
func pruneOAuthJournalQuarantines(path string) {
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
	if syncConfigParent(filepath.Dir(path)) != nil {
		slog.Error("Failed to sync OAuth journal quarantine directory", "file", filepath.Base(path))
	}
}
