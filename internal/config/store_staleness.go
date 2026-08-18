// Staleness tracking: snapshotting the on-disk metadata of tracked config
// files and comparing later stat results against those snapshots to detect
// external edits.
package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// fileSnapshot captures metadata about a config file at a point in time.
type fileSnapshot struct {
	Path    string
	Exists  bool
	Size    int64
	ModTime int64 // UnixNano
}

// StalenessResult contains the result of a staleness check.
type StalenessResult struct {
	Dirty   bool
	Changed []string
	Missing []string
	Errors  map[string]error
}

// ConfigStaleness checks whether any tracked config files have changed on disk
// since the last snapshot.
func (s *ConfigStore) ConfigStaleness() StalenessResult {
	var result StalenessResult
	result.Errors = make(map[string]error)

	sn := s.loadSnapshot()

	for _, path := range sn.trackedConfigPaths {
		snapshot, hadSnapshot := sn.snapshots[path]

		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		if err != nil && !os.IsNotExist(err) {
			result.Errors[path] = err
			result.Dirty = true
		}

		if !exists {
			if hadSnapshot && snapshot.Exists {
				result.Missing = append(result.Missing, path)
				result.Dirty = true
			}
			continue
		}

		if !hadSnapshot || !snapshot.Exists {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
			continue
		}

		if snapshot.Size != info.Size() || snapshot.ModTime != info.ModTime().UnixNano() {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
		}
	}

	slices.Sort(result.Changed)
	slices.Sort(result.Missing)

	return result
}

// RefreshStalenessSnapshot captures fresh snapshots of all tracked config
// files and publishes them as part of a new store generation. It only
// touches the snapshots/trackedConfigPaths pair — config, resolver,
// knownProviders etc. are carried over unchanged from whatever generation
// is current at the time of the swap.
func (s *ConfigStore) RefreshStalenessSnapshot() error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.refreshStalenessSnapshotLocked()
	return nil
}

// refreshStalenessSnapshotLocked is the lock-free body of
// RefreshStalenessSnapshot. The caller MUST hold publishMu.
func (s *ConfigStore) refreshStalenessSnapshotLocked() {
	cur := s.loadSnapshot()
	next := cur.clone()
	if next.snapshots == nil {
		next.snapshots = make(map[string]fileSnapshot)
	} else {
		next.snapshots = maps.Clone(next.snapshots)
	}

	for _, path := range next.trackedConfigPaths {
		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		snapshot := fileSnapshot{
			Path:   path,
			Exists: exists,
		}

		if exists {
			snapshot.Size = info.Size()
			snapshot.ModTime = info.ModTime().UnixNano()
		}

		next.snapshots[path] = snapshot
	}

	s.publishLocked(next)
}

// CaptureStalenessSnapshot recomputes the set of tracked config paths (the
// paths passed in, plus the store's own workspace/global paths) and
// refreshes their on-disk snapshots, publishing both as part of a single
// new generation.
func (s *ConfigStore) CaptureStalenessSnapshot(paths []string) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.captureStalenessSnapshotLocked(paths)
}

// captureStalenessSnapshotLocked does the work of CaptureStalenessSnapshot
// without taking publishMu. The caller MUST hold publishMu.
func (s *ConfigStore) captureStalenessSnapshotLocked(paths []string) {
	seen := make(map[string]struct{})
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		seen[abs] = struct{}{}
	}

	cur := s.loadSnapshot()
	workspacePath := cur.workspacePath
	if workspacePath != "" {
		abs, err := filepath.Abs(workspacePath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}
	if s.globalDataPath != "" {
		abs, err := filepath.Abs(s.globalDataPath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}

	tracked := make([]string, 0, len(seen))
	for p := range seen {
		tracked = append(tracked, p)
	}
	slices.Sort(tracked)

	next := cur.clone()
	next.trackedConfigPaths = tracked
	s.publishLocked(next)

	s.refreshStalenessSnapshotLocked()
}

// captureStalenessSnapshot is the lock-acquiring entry point for callers
// that do NOT already hold publishMu (primarily white-box tests). Production
// code paths that already hold publishMu (Load, reloadFromDiskLocked) call
// captureStalenessSnapshotLocked directly to avoid a re-entrant deadlock.
func (s *ConfigStore) captureStalenessSnapshot(paths []string) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.captureStalenessSnapshotLocked(paths)
}
