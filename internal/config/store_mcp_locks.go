package config

// The locked-file set a single MCP mutation works over: the record binding, its hard-link topology check, and the lock groups that order acquisition across the workspace, global and system config paths. Split out of store_mcp_transaction.go when the 1000-line file limit landed.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/session"
)

type mcpLockedFiles struct {
	data         map[string][]byte
	present      map[string]bool
	changed      map[string]bool
	fingerprints map[string]reloadFileFingerprint
	pathRecords  map[string]string
	targetKeys   map[string]string
	records      map[string]*mcpFileRecord
}

// mcpFileRecord is one mutable transaction document. Every pathname alias
// that opened the same file is attached to this record, so staging through a
// workspace alias cannot leave the project spelling with old bytes.
type mcpFileRecord struct {
	key          string
	commitPath   string
	data         []byte
	identity     configFileIdentity
	selectedPath string
	expectation  reloadFileFingerprint
	aliases      map[string]struct{}
}

// ErrMCPHardLinkTopology is returned before a mutable MCP transaction when
// two distinct regular-file dirents share one inode. Renaming one dirent is
// not an in-place hard-link update, so treating those aliases as one record
// would leave the other dirent with stale bytes while claiming both changed.
var ErrMCPHardLinkTopology = errors.New("MCP config has unsupported hard-link aliases")

type mcpEvaluation struct {
	configs      map[string]MCPConfig
	origins      map[string]MCPOrigin
	fingerprints map[string]reloadFileFingerprint
	mcpInputs    map[string][sha256.Size]byte
}

func mcpRecordKey(path string, fingerprint reloadFileFingerprint) string {
	if fingerprint.exists && fingerprint.identity.valid {
		return fmt.Sprintf("identity:%d:%d", fingerprint.identity.device, fingerprint.identity.inode)
	}
	if fingerprint.discovery != ([sha256.Size]byte{}) {
		return fmt.Sprintf("discovery:%x", fingerprint.discovery)
	}
	return "path:" + normalizeDiscoveryPath(path)
}

func (files *mcpLockedFiles) bindMCPPath(path string, data []byte, present bool, fingerprint reloadFileFingerprint) *mcpFileRecord {
	discoveryPath := normalizeDiscoveryPath(path)
	key := mcpRecordKey(discoveryPath, fingerprint)
	if !fingerprint.exists {
		if targetKey := files.targetKeys[discoveryPath]; targetKey != "" {
			key = "target:" + targetKey
		}
	}
	record, ok := files.records[key]
	if !ok {
		record = &mcpFileRecord{
			key: key, commitPath: normalizeReloadPath(discoveryPath), data: data,
			identity: fingerprint.identity, expectation: fingerprint,
			aliases: make(map[string]struct{}),
		}
		files.records[key] = record
	}
	if present && !record.expectation.exists {
		record.expectation = fingerprint
		record.identity = fingerprint.identity
		record.commitPath = normalizeReloadPath(discoveryPath)
	}
	record.aliases[discoveryPath] = struct{}{}
	files.pathRecords[discoveryPath] = key
	if canonical := normalizeReloadPath(discoveryPath); canonical != "" {
		files.pathRecords[canonical] = key
	}
	files.data[discoveryPath] = record.data
	files.present[discoveryPath] = present
	files.fingerprints[discoveryPath] = fingerprint
	if canonical := normalizeReloadPath(discoveryPath); canonical != "" {
		files.data[canonical] = record.data
	}
	return record
}

func (files *mcpLockedFiles) mcpRecord(path string) *mcpFileRecord {
	key := normalizeDiscoveryPath(path)
	if recordKey, ok := files.pathRecords[key]; ok {
		return files.records[recordKey]
	}
	if _, known := files.data[key]; !known {
		return nil
	}
	if canonical := normalizeReloadPath(path); canonical != "" {
		if recordKey, ok := files.pathRecords[canonical]; ok {
			return files.records[recordKey]
		}
	}
	return nil
}

func (files *mcpLockedFiles) mcpData(path string) []byte {
	if record := files.mcpRecord(path); record != nil {
		return record.data
	}
	return files.data[normalizeDiscoveryPath(path)]
}

func (files *mcpLockedFiles) mcpFingerprint(path string) reloadFileFingerprint {
	if fingerprint, ok := files.fingerprints[normalizeDiscoveryPath(path)]; ok {
		return fingerprint
	}
	if record := files.mcpRecord(path); record != nil {
		return record.expectation
	}
	return reloadFileFingerprint{}
}

func (files *mcpLockedFiles) setMCPData(path string, data []byte) error {
	record := files.mcpRecord(path)
	if record == nil {
		return fmt.Errorf("MCP transaction path %q was not opened", path)
	}
	record.data = data
	record.selectedPath = normalizeDiscoveryPath(path)
	if expectation, ok := files.fingerprints[record.selectedPath]; ok {
		record.expectation = expectation
	} else {
		for alias := range record.aliases {
			record.selectedPath = alias
			record.expectation = files.fingerprints[alias]
			break
		}
	}
	files.changed[record.key] = true
	for alias := range record.aliases {
		files.data[alias] = data
		files.present[alias] = true
	}
	return nil
}

func (files *mcpLockedFiles) validateMutableTopology() error {
	for _, record := range files.records {
		if !record.expectation.exists || len(record.aliases) < 2 {
			continue
		}
		for alias := range record.aliases {
			if normalizeReloadPath(alias) != record.commitPath {
				return fmt.Errorf("%w: %s and %s", ErrMCPHardLinkTopology, record.commitPath, alias)
			}
		}
	}
	return nil
}

// withMCPWriteLocks is the single lock boundary for MCP lifecycle writes.
// Lock order is publishMu (caller) -> diskWriteMu -> sorted sidecar locks.
// Both writable files are locked even when only one is mutated; this makes
// origin/existence/target checks one cross-process linearization point.
func (s *ConfigStore) withMCPWriteLocks(fn func(*mcpLockedFiles) error) error {
	ctx, cancel := configContextWithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.withMCPLocks(ctx, fn)
}

func (s *ConfigStore) withMCPAdmissionLocks(fn func(*mcpLockedFiles) error) error {
	ctx, cancel := configContextWithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.withMCPAdmissionLocksContext(ctx, fn)
}

func (s *ConfigStore) withMCPAdmissionLocksContext(ctx context.Context, fn func(*mcpLockedFiles) error) error {
	return s.withMCPLocks(ctx, fn)
}

func (s *ConfigStore) withMCPLocks(ctx context.Context, fn func(*mcpLockedFiles) error) error {
	return s.withMCPLocksUsingCaseSensitivity(ctx, fn, configPlatformCaseSensitivity)
}

type mcpLockGroup struct {
	key      string
	lockPath string
	targets  []configWriteTarget
}

func configMCPAdmissionLockGroups(
	targets []configWriteTarget,
	caseSensitivity func(string) configCaseSensitivity,
) ([]*mcpLockGroup, map[string]configWriteTarget, map[string]string) {
	groupsByKey := make(map[string]*mcpLockGroup, len(targets))
	targetBySelectedPath := make(map[string]configWriteTarget, len(targets))
	targetKeyBySelectedPath := make(map[string]string, len(targets))
	caseDecisions := make(map[configFileIdentity]configCaseSensitivity, len(targets))
	for _, target := range targets {
		sensitivity := configCaseSensitivityUnknown
		parent := target.expected.parentIdentity
		if parent.valid {
			var ok bool
			sensitivity, ok = caseDecisions[parent]
			if !ok {
				if caseSensitivity != nil {
					sensitivity = caseSensitivity(filepath.Dir(target.path))
				}
				caseDecisions[parent] = sensitivity
			}
		}
		targetKey := configWriteTargetDedupKeyWithCaseSensitivity(target, sensitivity)
		if group, exists := groupsByKey[targetKey]; exists {
			group.targets = append(group.targets, target)
		} else {
			groupsByKey[targetKey] = &mcpLockGroup{
				key: targetKey, lockPath: target.lockPath,
				targets: []configWriteTarget{target},
			}
		}
		targetBySelectedPath[target.selectedPath] = target
		targetKeyBySelectedPath[target.selectedPath] = targetKey
	}
	groups := make([]*mcpLockGroup, 0, len(groupsByKey))
	for _, group := range groupsByKey {
		groups = append(groups, group)
	}
	slices.SortFunc(groups, func(left, right *mcpLockGroup) int {
		if left.lockPath < right.lockPath {
			return -1
		}
		if left.lockPath > right.lockPath {
			return 1
		}
		return strings.Compare(left.key, right.key)
	})
	// A pathological retarget during resolution can produce distinct target
	// identities with one lock pathname. Acquire that pathname only once while
	// retaining every logical target for binding verification.
	mergedGroups := make([]*mcpLockGroup, 0, len(groups))
	groupsByLockPath := make(map[string]*mcpLockGroup)
	for _, group := range groups {
		if existing, ok := groupsByLockPath[group.lockPath]; ok {
			existing.targets = append(existing.targets, group.targets...)
			continue
		}
		groupsByLockPath[group.lockPath] = group
		mergedGroups = append(mergedGroups, group)
	}
	return mergedGroups, targetBySelectedPath, targetKeyBySelectedPath
}

func (s *ConfigStore) withMCPLocksUsingCaseSensitivity(
	ctx context.Context,
	fn func(*mcpLockedFiles) error,
	caseSensitivity func(string) configCaseSensitivity,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	paths := make([]string, 0, 2)
	globalPath, err := s.configPath(ScopeGlobal)
	if err != nil {
		return err
	}
	paths = append(paths, normalizeDiscoveryPath(globalPath))
	if workspacePath, workspaceErr := s.configPath(ScopeWorkspace); workspaceErr == nil {
		paths = append(paths, normalizeDiscoveryPath(workspacePath))
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	targets := make([]configWriteTarget, 0, len(paths))
	for _, path := range paths {
		target, targetErr := s.resolveConfigWriteTarget(path)
		if targetErr != nil {
			return targetErr
		}
		runConfigAfterMCPResolveTargetHook(&target)
		targets = append(targets, target)
	}
	groups, targetBySelectedPath, targetKeyBySelectedPath := configMCPAdmissionLockGroups(targets, caseSensitivity)

	s.diskWriteMu.Lock()
	defer s.diskWriteMu.Unlock()
	locks := make([]*session.FileLock, 0, len(groups))
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Release()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, group := range groups {
		lock, lockErr := acquireConfigFileLock(ctx, group.lockPath)
		if lockErr != nil {
			return fmt.Errorf("failed to lock config file %q: %w", group.lockPath, lockErr)
		}
		locks = append(locks, lock)
		slices.SortFunc(group.targets, func(left, right configWriteTarget) int {
			return strings.Compare(left.selectedPath, right.selectedPath)
		})
		for _, target := range group.targets {
			if err := verifyConfigTargetBindingAfterLock(target); err != nil {
				return fmt.Errorf("%w: config target %q changed while acquiring locks", ErrMCPStale, target.selectedPath)
			}
		}
	}
	files := &mcpLockedFiles{
		data: make(map[string][]byte, len(paths)), present: make(map[string]bool, len(paths)),
		changed: make(map[string]bool), fingerprints: make(map[string]reloadFileFingerprint, len(paths)),
		pathRecords: make(map[string]string, len(paths)), targetKeys: make(map[string]string, len(paths)),
		records: make(map[string]*mcpFileRecord, len(paths)),
	}
	for _, path := range paths {
		expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
		if ownerErr != nil {
			return ownerErr
		}
		data, fingerprint, readErr := readStableConfigFileOwned(path, expectedOwner, enforceOwner)
		target := targetBySelectedPath[normalizeDiscoveryPath(path)]
		files.targetKeys[normalizeDiscoveryPath(path)] = targetKeyBySelectedPath[target.selectedPath]
		if readErr != nil {
			if os.IsNotExist(readErr) {
				if err := verifyConfigTargetBinding(target, fingerprint, readErr); err != nil {
					return fmt.Errorf("%w: config target %q changed while reading", ErrMCPStale, path)
				}
				files.bindMCPPath(path, nil, false, fingerprint)
				continue
			}
			if errors.Is(readErr, errConfigOwnerMismatch) {
				return fmt.Errorf("%w: unsafe config input %s", ErrMCPStale, path)
			}
			return fmt.Errorf("failed to read config file: %w", readErr)
		}
		if err := verifyConfigTargetBinding(target, fingerprint, nil); err != nil {
			return fmt.Errorf("%w: config target %q changed while reading", ErrMCPStale, path)
		}
		files.bindMCPPath(path, data, true, fingerprint)
	}
	return fn(files)
}
