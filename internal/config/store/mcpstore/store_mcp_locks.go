package mcpstore

// The locked-file set a single MCP mutation works over: the record binding, its hard-link topology check, and the lock groups that order acquisition across the workspace, global and system config paths. Split out of store_mcp_transaction.go when the 1000-line file limit landed.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
)

// configWriteLockTimeout is a byte-identical local copy of the root const
// (internal/config/store_write.go:113) needed by the MCP lock boundary.
const configWriteLockTimeout = 30 * time.Second

type LockedFiles struct {
	Data         map[string][]byte
	Present      map[string]bool
	Changed      map[string]bool
	Fingerprints map[string]docread.Fingerprint
	PathRecords  map[string]string
	TargetKeys   map[string]string
	Records      map[string]*FileRecord
}

// mcpFileRecord is one mutable transaction document. Every pathname alias
// that opened the same file is attached to this record, so staging through a
// workspace alias cannot leave the project spelling with old bytes.
type FileRecord struct {
	Key          string
	CommitPath   string
	Data         []byte
	Identity     fileid.FileIdentity
	SelectedPath string
	Expectation  docread.Fingerprint
	Aliases      map[string]struct{}
}

// ErrMCPHardLinkTopology is returned before a mutable MCP transaction when
// two distinct regular-file dirents share one inode. Renaming one dirent is
// not an in-place hard-link update, so treating those aliases as one record
// would leave the other dirent with stale bytes while claiming both changed.
var ErrMCPHardLinkTopology = errors.New("MCP config has unsupported hard-link aliases")

type Evaluation struct {
	Configs      map[string]MCPConfig
	Origins      map[string]MCPOrigin
	Fingerprints map[string]docread.Fingerprint
	MCPInputs    map[string][sha256.Size]byte
}

func mcpRecordKey(path string, fingerprint docread.Fingerprint) string {
	if fingerprint.Exists && fingerprint.Identity.Valid {
		return fmt.Sprintf("identity:%s", fingerprint.Identity.FingerprintString())
	}
	if fingerprint.Discovery != ([sha256.Size]byte{}) {
		return fmt.Sprintf("discovery:%x", fingerprint.Discovery)
	}
	return "path:" + docread.NormalizeDiscoveryPath(path)
}

func (files *LockedFiles) BindMCPPath(path string, data []byte, present bool, fingerprint docread.Fingerprint) *FileRecord {
	discoveryPath := docread.NormalizeDiscoveryPath(path)
	key := mcpRecordKey(discoveryPath, fingerprint)
	if !fingerprint.Exists {
		if targetKey := files.TargetKeys[discoveryPath]; targetKey != "" {
			key = "target:" + targetKey
		}
	}
	record, ok := files.Records[key]
	if !ok {
		record = &FileRecord{
			Key: key, CommitPath: docread.NormalizeReloadPath(discoveryPath), Data: data,
			Identity: fingerprint.Identity, Expectation: fingerprint,
			Aliases: make(map[string]struct{}),
		}
		files.Records[key] = record
	}
	if present && !record.Expectation.Exists {
		record.Expectation = fingerprint
		record.Identity = fingerprint.Identity
		record.CommitPath = docread.NormalizeReloadPath(discoveryPath)
	}
	record.Aliases[discoveryPath] = struct{}{}
	files.PathRecords[discoveryPath] = key
	if canonical := docread.NormalizeReloadPath(discoveryPath); canonical != "" {
		files.PathRecords[canonical] = key
	}
	files.Data[discoveryPath] = record.Data
	files.Present[discoveryPath] = present
	files.Fingerprints[discoveryPath] = fingerprint
	if canonical := docread.NormalizeReloadPath(discoveryPath); canonical != "" {
		files.Data[canonical] = record.Data
	}
	return record
}

func (files *LockedFiles) MCPRecord(path string) *FileRecord {
	key := docread.NormalizeDiscoveryPath(path)
	if recordKey, ok := files.PathRecords[key]; ok {
		return files.Records[recordKey]
	}
	if canonical := docread.NormalizeReloadPath(path); canonical != "" {
		if recordKey, ok := files.PathRecords[canonical]; ok {
			return files.Records[recordKey]
		}
	}
	return nil
}

func (files *LockedFiles) MCPData(path string) []byte {
	if record := files.MCPRecord(path); record != nil {
		return record.Data
	}
	return files.Data[docread.NormalizeDiscoveryPath(path)]
}

func (files *LockedFiles) MCPFingerprint(path string) docread.Fingerprint {
	if fingerprint, ok := files.Fingerprints[docread.NormalizeDiscoveryPath(path)]; ok {
		return fingerprint
	}
	if record := files.MCPRecord(path); record != nil {
		return record.Expectation
	}
	return docread.Fingerprint{}
}

func (files *LockedFiles) SetMCPData(path string, data []byte) error {
	record := files.MCPRecord(path)
	if record == nil {
		return fmt.Errorf("MCP transaction path %q was not opened", path)
	}
	record.Data = data
	record.SelectedPath = docread.NormalizeDiscoveryPath(path)
	if expectation, ok := files.Fingerprints[record.SelectedPath]; ok {
		record.Expectation = expectation
	} else {
		for alias := range record.Aliases {
			record.SelectedPath = alias
			record.Expectation = files.Fingerprints[alias]
			break
		}
	}
	files.Changed[record.Key] = true
	for alias := range record.Aliases {
		files.Data[alias] = data
		files.Present[alias] = true
	}
	return nil
}

func (files *LockedFiles) validateMutableTopology() error {
	for _, record := range files.Records {
		if !record.Expectation.Exists || len(record.Aliases) < 2 {
			continue
		}
		for alias := range record.Aliases {
			if docread.NormalizeReloadPath(alias) != record.CommitPath {
				return fmt.Errorf("%w: %s and %s", ErrMCPHardLinkTopology, record.CommitPath, alias)
			}
		}
	}
	return nil
}

// withMCPWriteLocks is the single lock boundary for MCP lifecycle writes.
// Lock order is publishMu (caller) -> diskWriteMu -> sorted sidecar locks.
// Both writable files are locked even when only one is mutated; this makes
// origin/existence/target checks one cross-process linearization point.
func (s *Store) withMCPWriteLocks(fn func(*LockedFiles) error) error {
	ctx, cancel := cfgcommit.ConfigContextWithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.withMCPLocks(ctx, fn)
}

// WithMCPAdmissionLocks is the admission-side lock boundary (root white-box
// tests call it through the root forwarder withMCPAdmissionLocks).
func (s *Store) WithMCPAdmissionLocks(fn func(*LockedFiles) error) error {
	ctx, cancel := cfgcommit.ConfigContextWithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.withMCPLocks(ctx, fn)
}

func (s *Store) withMCPLocks(ctx context.Context, fn func(*LockedFiles) error) error {
	return s.WithMCPLocksUsingCaseSensitivity(ctx, fn, casefold.ConfigPlatformCaseSensitivity)
}

func (s *Store) withMCPAdmissionLocksContext(ctx context.Context, fn func(*LockedFiles) error) error {
	return s.WithMCPLocksUsingCaseSensitivity(ctx, fn, casefold.ConfigPlatformCaseSensitivity)
}

// LockGroup is one acquired-lock group (exported for the root white-box
// sidecar test forwarder).
type LockGroup struct {
	key      string
	lockPath string
	targets  []LockTarget
}

// ConfigMCPAdmissionLockGroups is the exported form used by the root
// white-box sidecar test forwarder (configMCPAdmissionLockGroups).
func ConfigMCPAdmissionLockGroups(
	targets []LockTarget,
	caseSensitivity func(string) casefold.ConfigCaseSensitivity,
) ([]*LockGroup, map[string]LockTarget, map[string]string) {
	groupsByKey := make(map[string]*LockGroup, len(targets))
	targetBySelectedPath := make(map[string]LockTarget, len(targets))
	targetKeyBySelectedPath := make(map[string]string, len(targets))
	caseDecisions := make(map[fileid.FileIdentity]casefold.ConfigCaseSensitivity, len(targets))
	for _, target := range targets {
		sensitivity := casefold.ConfigCaseSensitivityUnknown
		parent := target.Expected.ParentIdentity
		if parent.Valid {
			var ok bool
			sensitivity, ok = caseDecisions[parent]
			if !ok {
				if caseSensitivity != nil {
					sensitivity = caseSensitivity(filepath.Dir(target.Path))
				}
				caseDecisions[parent] = sensitivity
			}
		}
		targetKey := mcpWriteTargetDedupKeyWithCaseSensitivity(target, sensitivity)
		if group, exists := groupsByKey[targetKey]; exists {
			group.targets = append(group.targets, target)
		} else {
			groupsByKey[targetKey] = &LockGroup{
				key: targetKey, lockPath: target.LockPath,
				targets: []LockTarget{target},
			}
		}
		targetBySelectedPath[target.SelectedPath] = target
		targetKeyBySelectedPath[target.SelectedPath] = targetKey
	}
	groups := make([]*LockGroup, 0, len(groupsByKey))
	for _, group := range groupsByKey {
		groups = append(groups, group)
	}
	slices.SortFunc(groups, func(left, right *LockGroup) int {
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
	mergedGroups := make([]*LockGroup, 0, len(groups))
	groupsByLockPath := make(map[string]*LockGroup)
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

// mcpWriteTargetDedupKeyWithCaseSensitivity is the LockTarget form of
// cfgcommit's configWriteTargetDedupKeyWithCaseSensitivity, reached through
// the exported cfgcommit API (same bytes, LockTarget fields).
func mcpWriteTargetDedupKeyWithCaseSensitivity(target LockTarget, sensitivity casefold.ConfigCaseSensitivity) string {
	ct := cfgcommit.NewConfigWriteTarget(target.Path, target.SelectedPath, target.LockPath, target.Expected, target.Owner, target.Enforce)
	return cfgcommit.ConfigWriteTargetDedupKeyWithCaseSensitivity(ct, sensitivity)
}

// WithMCPLocksUsingCaseSensitivity resolves the two writable config slots,
// takes the ordered sidecar locks under diskWriteMu (Port.DiskWrite), and
// binds the locked documents into one LockedFiles set for fn. The core-side
// parts of the original ConfigStore body (resolveConfigWriteTarget,
// runConfigAfterMCPResolveTargetHook, diskWriteMu, verifyConfigTargetBinding
// and its hooks) moved behind the DiskWriter seam with identical timing.
func (s *Store) WithMCPLocksUsingCaseSensitivity(
	ctx context.Context,
	fn func(*LockedFiles) error,
	caseSensitivity func(string) casefold.ConfigCaseSensitivity,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	paths := make([]string, 0, 2)
	globalPath, err := s.port.ScopePath(ScopeGlobal)
	if err != nil {
		return err
	}
	paths = append(paths, docread.NormalizeDiscoveryPath(globalPath))
	if workspacePath, workspaceErr := s.port.ScopePath(ScopeWorkspace); workspaceErr == nil {
		paths = append(paths, docread.NormalizeDiscoveryPath(workspacePath))
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	return s.port.DiskWrite(func(dw DiskWriter) error {
		targets, targetsErr := dw.ResolveWriteTargets(paths)
		if targetsErr != nil {
			return targetsErr
		}
		groups, targetBySelectedPath, targetKeyBySelectedPath := ConfigMCPAdmissionLockGroups(targets, caseSensitivity)

		locks := make([]*filelock.FileLock, 0, len(groups))
		defer func() {
			for i := len(locks) - 1; i >= 0; i = i - 1 {
				_ = locks[i].Release()
			}
		}()
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, group := range groups {
			lock, lockErr := cfgcommit.AcquireConfigFileLock(ctx, group.lockPath)
			if lockErr != nil {
				return fmt.Errorf("failed to lock config file %q: %w", group.lockPath, lockErr)
			}
			locks = append(locks, lock)
			slices.SortFunc(group.targets, func(left, right LockTarget) int {
				return strings.Compare(left.SelectedPath, right.SelectedPath)
			})
			for _, target := range group.targets {
				if err := dw.VerifyTargetBindingAfterLock(target); err != nil {
					return fmt.Errorf("%w: config target %q changed while acquiring locks", ErrMCPStale, target.SelectedPath)
				}
			}
		}
		files := &LockedFiles{
			Data: make(map[string][]byte, len(paths)), Present: make(map[string]bool, len(paths)),
			Changed: make(map[string]bool), Fingerprints: make(map[string]docread.Fingerprint, len(paths)),
			PathRecords: make(map[string]string, len(paths)), TargetKeys: make(map[string]string, len(paths)),
			Records: make(map[string]*FileRecord, len(paths)),
		}
		for _, path := range paths {
			expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
			if ownerErr != nil {
				return ownerErr
			}
			data, fingerprint, readErr := docread.ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
			target := targetBySelectedPath[docread.NormalizeDiscoveryPath(path)]
			files.TargetKeys[docread.NormalizeDiscoveryPath(path)] = targetKeyBySelectedPath[target.SelectedPath]
			if readErr != nil {
				if os.IsNotExist(readErr) {
					if err := dw.VerifyTargetBinding(target, fingerprint, readErr); err != nil {
						return fmt.Errorf("%w: config target %q changed while reading", ErrMCPStale, path)
					}
					files.BindMCPPath(path, nil, false, fingerprint)
					continue
				}
				if errors.Is(readErr, errConfigOwnerMismatch) {
					return fmt.Errorf("%w: unsafe config input %s", ErrMCPStale, path)
				}
				return fmt.Errorf("failed to read config file: %w", readErr)
			}
			// The locked document is decoded through the settings-document
			// layer before binding: record.Data is always JSON-shaped, the
			// fingerprint stays bound to the RAW bytes as read off disk. A
			// decode failure aborts the transaction; it is never "no servers".
			decoded, decodeErr := docread.DecodeConfigDocument(path, data)
			if decodeErr != nil {
				return decodeErr
			}
			if err := dw.VerifyTargetBinding(target, fingerprint, nil); err != nil {
				return fmt.Errorf("%w: config target %q changed while reading", ErrMCPStale, path)
			}
			files.BindMCPPath(path, decoded, true, fingerprint)
		}
		return fn(files)
	})
}

// Moved from mcpstore.go (file deleted to keep the directory at 7 files).
// UpdateMCP applies a copy-on-write mutation to one MCP server configuration.
// It never mutates a published Config or any of its nested MCP maps.
func (s *Store) UpdateMCP(name string, mutate func(*MCPConfig)) (MCPConfig, bool) {
	var updated MCPConfig
	var ok bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil || config.MCP == nil {
			return nil
		}
		current, exists := config.MCP[name]
		if !exists {
			return nil
		}
		updated = cloneMCPConfig(current)
		mutate(&updated)
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		cfgCopy.MCP[name] = cloneMCPConfig(updated)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs, name)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		ok = true
		return nil
	})
	return updated, ok
}

// SetMCPDisabled updates one MCP server's disabled flag through the
// copy-on-write publication path and returns the detached new value.
func (s *Store) SetMCPDisabled(name string, disabled bool) (MCPConfig, bool) {
	return s.UpdateMCP(name, func(m *MCPConfig) { m.Disabled = disabled })
}

// AddMCP publishes a new MCP server configuration if name is unused.
func (s *Store) AddMCP(name string, mcpConfig MCPConfig) bool {
	var added bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil {
			return nil
		}
		if _, exists := config.MCP[name]; exists {
			return nil
		}
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		if cfgCopy.MCP == nil {
			cfgCopy.MCP = make(MCPs)
		}
		cfgCopy.MCP[name] = cloneMCPConfig(mcpConfig)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		added = true
		return nil
	})
	return added
}

// RemoveMCP removes one MCP server and returns its detached configuration.
func (s *Store) RemoveMCP(name string) (MCPConfig, bool) {
	var removed MCPConfig
	var ok bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil {
			return nil
		}
		current, exists := config.MCP[name]
		if !exists {
			return nil
		}
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		delete(cfgCopy.MCP, name)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		removed = cloneMCPConfig(current)
		ok = true
		return nil
	})
	return removed, ok
}

// RemoveMCPIfCurrent removes name only when the in-memory snapshot still has
// the expected config at the expected MCP revision.
func (s *Store) RemoveMCPIfCurrent(name string, expected MCPConfig, revision uint64) (MCPConfig, bool) {
	var removed MCPConfig
	var ok bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil || revision != 0 && cur.MCPRevisions()[name] != revision {
			return nil
		}
		current, exists := config.MCP[name]
		if !exists || !reflect.DeepEqual(current, expected) {
			return nil
		}
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		delete(cfgCopy.MCP, name)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		removed = cloneMCPConfig(current)
		ok = true
		return nil
	})
	return removed, ok
}

// cloneMCPConfig is a byte-identical local copy of the root helper
// (internal/config/store.go cloneMCPConfig) that moved with the MCP
// component; the root keeps its own copy for MCPConfig.
func cloneMCPConfig(m MCPConfig) MCPConfig {
	m.Env = maps.Clone(m.Env)
	m.Args = slices.Clone(m.Args)
	m.DisabledTools = slices.Clone(m.DisabledTools)
	m.EnabledTools = slices.Clone(m.EnabledTools)
	m.Headers = maps.Clone(m.Headers)
	return m
}
