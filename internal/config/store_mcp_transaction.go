package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/PHPCraftdream/rush/internal/home"
	"github.com/PHPCraftdream/rush/internal/session"
)

type mcpLockedFiles struct {
	data         map[string][]byte
	present      map[string]bool
	changed      map[string]bool
	fingerprints map[string]reloadFileFingerprint
	pathRecords  map[string]string
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
	targets := make(map[string]configWriteTarget, len(paths))
	lockPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		target, targetErr := s.resolveConfigWriteTarget(path)
		if targetErr != nil {
			return targetErr
		}
		if _, exists := targets[target.lockPath]; !exists {
			targets[target.lockPath] = target
			lockPaths = append(lockPaths, target.lockPath)
		}
	}
	slices.Sort(lockPaths)

	s.diskWriteMu.Lock()
	defer s.diskWriteMu.Unlock()
	locks := make([]*session.FileLock, 0, len(paths))
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	for _, lockPath := range lockPaths {
		lock, lockErr := session.AcquireFileLockContext(ctx, lockPath)
		if lockErr != nil {
			return fmt.Errorf("failed to lock config file %q: %w", lockPath, lockErr)
		}
		locks = append(locks, lock)
	}
	for _, target := range targets {
		if err := verifyConfigWriteTarget(target); err != nil {
			return fmt.Errorf("%w: config target %q changed while acquiring locks", ErrMCPStale, target.selectedPath)
		}
	}
	files := &mcpLockedFiles{
		data: make(map[string][]byte, len(paths)), present: make(map[string]bool, len(paths)),
		changed: make(map[string]bool), fingerprints: make(map[string]reloadFileFingerprint, len(paths)),
		pathRecords: make(map[string]string, len(paths)), records: make(map[string]*mcpFileRecord, len(paths)),
	}
	for _, path := range paths {
		expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
		if ownerErr != nil {
			return ownerErr
		}
		data, fingerprint, readErr := readStableConfigFileOwned(path, expectedOwner, enforceOwner)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				files.bindMCPPath(path, nil, false, fingerprint)
				continue
			}
			if errors.Is(readErr, errConfigOwnerMismatch) {
				return fmt.Errorf("%w: unsafe config input %s", ErrMCPStale, path)
			}
			return fmt.Errorf("failed to read config file: %w", readErr)
		}
		files.bindMCPPath(path, data, true, fingerprint)
	}
	return fn(files)
}

func (s *ConfigStore) mutateMCP(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool) (MCPMutationResult, error) {
	return s.mutateMCPWithMode(operation, scope, oldName, newName, value, disabled, false)
}

func (s *ConfigStore) mutateMCPExact(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool) (MCPMutationResult, error) {
	return s.mutateMCPWithMode(operation, scope, oldName, newName, value, disabled, true)
}

func (s *ConfigStore) mutatePendingRemoveMCP(scope Scope, name string) (MCPMutationResult, error) {
	if scope != ScopeGlobal {
		return MCPMutationResult{}, fmt.Errorf("pending MCP removal requires global scope: %w", ErrMCPStale)
	}

	path, err := s.configPath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = normalizeReloadPath(path)
	var result MCPMutationResult
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		before, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		if err := files.validateMutableTopology(); err != nil {
			return err
		}
		if _, exists := before.configs[name]; exists {
			return fmt.Errorf("%w: %q", ErrMCPTargetExists, name)
		}
		result = MCPMutationResult{
			Operation: "remove", OldName: name, NewName: name,
			OldExists: false, OldConfig: MCPConfig{}, OldOrigin: before.origins[name],
		}
		// Write the final absent state in one atomic operation. This may create
		// an otherwise empty config file, but it never writes the pending
		// definition, including if the process stops immediately afterward.
		if err := s.prepareMCPFileMutation(files, path, "remove", name, name, MCPConfig{}, nil); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
		result.committedFingerprints = committedMCPFingerprints(files)
		writeErr := s.writeMCPFileChanges(files)
		result.committedFingerprints = committedMCPFingerprints(files)
		return writeErr
	})
	if err == nil || mcpCommitWasReconciled(err) {
		result.Generation = s.loadSnapshot().generation + 1
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	if err != nil && !mcpCommitWasReconciled(err) {
		return MCPMutationResult{}, err
	}
	return result, err
}

func (s *ConfigStore) mutateMCPEnableRollback(scope Scope, name string, token MCPMutationResult) (MCPMutationResult, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = normalizeReloadPath(path)
	var result MCPMutationResult
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		before, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		if !s.mcpEnableRollbackTokenMatches(before, scope, name, token) {
			return ErrMCPMutationStale
		}
		current := before.configs[name]
		origin := before.origins[name]
		result = MCPMutationResult{
			Operation: "disable", OldName: name, NewName: name,
			OldExists: true, OldConfig: cloneMCPConfig(current), OldOrigin: origin,
		}
		if err := validateMCPMutation("disable", scope, name, name, true, origin, before.configs); err != nil {
			return err
		}
		disabled := true
		if err := s.prepareMCPFileMutation(files, path, "disable", name, name, MCPConfig{}, &disabled); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
		writeErr := s.writeMCPFileChanges(files)
		after, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		result.NewExists, result.NewConfig, result.NewOrigin = afterValue(after, name)
		result.committedFingerprints = committedMCPFingerprints(files)
		return writeErr
	})
	if err == nil || mcpCommitWasReconciled(err) {
		result.Generation = s.loadSnapshot().generation + 1
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	if err != nil && !mcpCommitWasReconciled(err) {
		return MCPMutationResult{}, err
	}
	return result, err
}

func (s *ConfigStore) mcpEnableRollbackTokenMatches(before mcpEvaluation, scope Scope, name string, token MCPMutationResult) bool {
	if token.NewName != name || !token.NewExists || token.Operation != "add" && token.Operation != "disable" {
		return false
	}
	if token.Generation != 0 && s.loadSnapshot().generation != token.Generation {
		return false
	}
	current, exists := before.configs[name]
	if !exists || !reflect.DeepEqual(current, token.NewConfig) || before.origins[name] != token.NewOrigin {
		return false
	}
	if token.NewOrigin.Kind == MCPOriginExternal {
		if scope != ScopeWorkspace {
			return false
		}
	} else if token.NewOrigin.Scope != scope || !token.NewOrigin.Writable {
		return false
	}
	for expectedPath, expected := range token.committedFingerprints {
		actual, ok := mcpEvaluationFingerprint(before.fingerprints, expectedPath)
		if !ok || actual != expected {
			return false
		}
	}
	return len(token.committedFingerprints) > 0
}

func mcpEvaluationFingerprint(fingerprints map[string]reloadFileFingerprint, path string) (reloadFileFingerprint, bool) {
	if fingerprint, ok := fingerprints[path]; ok {
		return fingerprint, true
	}
	discovery := normalizeDiscoveryPath(path)
	for candidate, fingerprint := range fingerprints {
		if normalizeDiscoveryPath(candidate) == discovery {
			return fingerprint, true
		}
	}
	return reloadFileFingerprint{}, false
}

func (s *ConfigStore) mutateMCPWithMode(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool, exact bool) (MCPMutationResult, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = normalizeReloadPath(path)
	var result MCPMutationResult
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		before, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		if err := files.validateMutableTopology(); err != nil {
			return err
		}
		old, oldOK := before.configs[oldName]
		oldOrigin := before.origins[oldName]
		result = MCPMutationResult{
			Operation: operation, OldName: oldName, NewName: newName,
			OldExists: oldOK, OldConfig: cloneMCPConfig(old), OldOrigin: oldOrigin,
		}
		if err := validateMCPMutation(operation, scope, oldName, newName, oldOK, oldOrigin, before.configs); err != nil && !exact {
			// A zero-working-directory test store has no discoverable project
			// pipeline. Its in-memory config is the only origin available.
			if s.workingDir != "" || operation == "add" {
				return err
			}
		}
		if exact {
			literalExists := literalMCPEntryExists(files.mcpData(path), oldName)
			switch operation {
			case "add":
				if literalExists {
					return fmt.Errorf("%w: %q", ErrMCPTargetExists, newName)
				}
			case "remove", "set":
				if !literalExists {
					return fmt.Errorf("%w: %q", ErrMCPNotFound, oldName)
				}
			case "disable":
				if literalExists {
					break
				}
				if !oldOK {
					return fmt.Errorf("%w: %q", ErrMCPNotFound, oldName)
				}
				if oldOrigin.Kind != MCPOriginExternal || scope != ScopeWorkspace {
					return validateMCPMutation(operation, scope, oldName, newName, oldOK, oldOrigin, before.configs)
				}
			}
		}
		if err := s.prepareMCPFileMutation(files, path, operation, oldName, newName, value, disabled); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
		writeErr := s.writeMCPFileChanges(files)
		after, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		result.NewExists, result.NewConfig, result.NewOrigin = afterValue(after, newName)
		if operation == "remove" || (operation == "replace" && oldName != newName) {
			result.FallbackExists, result.FallbackConfig, result.FallbackOrigin = afterValue(after, oldName)
		}
		if operation == "disable" {
			result.NewExists, result.NewConfig, result.NewOrigin = afterValue(after, oldName)
		}
		result.committedFingerprints = committedMCPFingerprints(files)
		return writeErr
	})
	if err == nil || mcpCommitWasReconciled(err) {
		result.Generation = s.loadSnapshot().generation + 1
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	if err != nil && !mcpCommitWasReconciled(err) {
		return MCPMutationResult{}, err
	}
	return result, err
}

func validateMCPMutation(operation string, scope Scope, oldName, newName string, oldOK bool, oldOrigin MCPOrigin, configs map[string]MCPConfig) error {
	if operation == "add" {
		if _, exists := configs[newName]; exists {
			return fmt.Errorf("%w: %q", ErrMCPTargetExists, newName)
		}
		return nil
	}
	if !oldOK {
		return fmt.Errorf("%w: %q", ErrMCPNotFound, oldName)
	}
	if operation == "replace" && oldOrigin.Kind == MCPOriginExternal {
		return fmt.Errorf("%w: %q", ErrMCPExternal, oldName)
	}
	if operation == "disable" && oldOrigin.Kind == MCPOriginExternal {
		if scope != ScopeWorkspace {
			return fmt.Errorf("%w: external server overrides require workspace scope", ErrMCPExternal)
		}
	} else if !oldOrigin.Writable {
		return fmt.Errorf("%w: %q is defined in %s", ErrMCPUnwritableOrigin, oldName, oldOrigin.Path)
	} else if oldOrigin.Scope != scope {
		return fmt.Errorf("%w: %q is owned by %s, not %s", ErrMCPStale, oldName, oldOrigin.Scope, scope)
	}
	if operation == "replace" && oldName != newName {
		if _, exists := configs[newName]; exists {
			return fmt.Errorf("%w: %q", ErrMCPTargetExists, newName)
		}
	}
	return nil
}

func (s *ConfigStore) prepareMCPFileMutation(files *mcpLockedFiles, path, operation, oldName, newName string, value MCPConfig, disabled *bool) error {
	root, err := decodeMCPRoot(files.mcpData(path))
	if err != nil {
		return err
	}
	servers := root.mcp
	if servers == nil {
		servers = make(map[string]json.RawMessage)
	}
	if disabled != nil {
		var entry map[string]any
		if raw := servers[oldName]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &entry); err != nil {
				return fmt.Errorf("failed to parse MCP server %q: %w", oldName, err)
			}
		}
		if entry == nil {
			entry = make(map[string]any)
		}
		entry["disabled"] = *disabled
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		servers[oldName] = raw
	} else {
		switch operation {
		case "add":
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			servers[newName] = raw
		case "remove":
			delete(servers, oldName)
		case "replace":
			delete(servers, oldName)
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			servers[newName] = raw
		}
	}
	root.mcp = servers
	root.raw["mcp"], err = json.Marshal(root.mcp)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(root.raw, "", "  ")
	if err != nil {
		return err
	}
	return files.setMCPData(path, append(data, '\n'))
}

type mcpRoot struct {
	raw map[string]json.RawMessage
	mcp map[string]json.RawMessage
}

func decodeMCPRoot(data []byte) (mcpRoot, error) {
	root := make(map[string]json.RawMessage)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return mcpRoot{}, fmt.Errorf("failed to parse config file: %w", err)
		}
	}
	servers := make(map[string]json.RawMessage)
	if raw := root["mcp"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return mcpRoot{}, fmt.Errorf("failed to parse MCP config: %w", err)
		}
	}
	return mcpRoot{raw: root, mcp: servers}, nil
}

func literalMCPEntryExists(data []byte, name string) bool {
	root, err := decodeMCPRoot(data)
	if err != nil {
		return false
	}
	_, ok := root.mcp[name]
	return ok
}

func updateMCPFile(files *mcpLockedFiles, path, name string, mutate func(map[string]any) error) error {
	root, err := decodeMCPRoot(files.mcpData(path))
	if err != nil {
		return err
	}
	entry := make(map[string]any)
	if raw := root.mcp[name]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return err
		}
	}
	if err := mutate(entry); err != nil {
		return err
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if root.mcp == nil {
		root.mcp = make(map[string]json.RawMessage)
	}
	root.mcp[name] = raw
	root.raw["mcp"], err = json.Marshal(root.mcp)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(root.raw, "", "  ")
	if err != nil {
		return err
	}
	return files.setMCPData(path, append(data, '\n'))
}

func (s *ConfigStore) writeMCPFileChanges(files *mcpLockedFiles) error {
	for key, changed := range files.changed {
		if !changed {
			continue
		}
		record := files.records[key]
		if record == nil || record.selectedPath == "" {
			return fmt.Errorf("MCP transaction record %q has no selected destination", key)
		}
		if !record.expectation.exists {
			parentPath := filepath.Dir(record.commitPath)
			if currentParent := configDiscoveryFingerprint(filepath.Dir(record.selectedPath)); currentParent != record.expectation.parentDiscovery {
				return fmt.Errorf("%w: config parent changed", ErrMCPStale)
			}
			if err := os.MkdirAll(parentPath, 0o755); err != nil {
				return err
			}
			record.expectation.parentDiscovery = configDiscoveryFingerprint(filepath.Dir(record.selectedPath))
		}
		owner, enforce, err := s.mcpOwnerPolicy(record.selectedPath)
		if err != nil {
			return fmt.Errorf("failed to determine config owner: %w", err)
		}
		committed, commitErr := commitConfigFile(record.selectedPath, record.commitPath, record.data, 0o600, record.expectation, owner, enforce)
		commitReturnedNil := commitErr == nil
		if commitErr != nil {
			if errors.Is(commitErr, errConfigCommitDurabilityUncertain) {
				// Directory fsync failure is never converted to success. The
				// bytes are reconciled into the transaction so its caller can
				// publish a fresh snapshot, but the durability uncertainty is
				// returned after that publication and must not be retried blindly.
				reconciled, ok := s.reconcileMCPCommit(record)
				if !ok {
					return &mcpCommitUncertainError{cause: commitErr}
				}
				committed = reconciled
				if fingerprintErr := s.applyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
					cause := commitErr
					if outcome, outcomeOK := CommitOutcomeFromError(commitErr); outcomeOK {
						cause = cloneCommitOutcome(outcome, false)
					}
					return &mcpCommitUncertainError{cause: errors.Join(cause, fingerprintErr)}
				}
				record.expectation = committed
				if outcome, outcomeOK := CommitOutcomeFromError(commitErr); outcomeOK {
					commitErr = cloneCommitOutcome(outcome, true)
				}
				return &mcpCommitUncertainError{cause: commitErr}
			}
			if errors.Is(commitErr, errConfigCommitUncertain) || errors.Is(commitErr, errConfigCommitCommitted) {
				// A readback/check-hook failure is recoverable when a fresh
				// read proves that the requested bytes are present. Preserve the
				// public outcome and upgrade its reconciliation status so every
				// caller observes one authoritative status source.
				if outcome, outcomeOK := CommitOutcomeFromError(commitErr); outcomeOK && outcome.Reconciled &&
					!errors.Is(outcome, errConfigCommitDurabilityUncertain) {
					commitErr = nil
				} else {
					reconciled, ok := s.reconcileMCPCommit(record)
					if !ok {
						return &mcpCommitUncertainError{cause: commitErr}
					}
					committed = reconciled
					if fingerprintErr := s.applyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
						cause := commitErr
						if outcome, outcomeOK := CommitOutcomeFromError(commitErr); outcomeOK {
							cause = cloneCommitOutcome(outcome, false)
						}
						return &mcpCommitUncertainError{cause: errors.Join(cause, fingerprintErr)}
					}
					record.expectation = committed
					if outcome, outcomeOK := CommitOutcomeFromError(commitErr); outcomeOK {
						commitErr = cloneCommitOutcome(outcome, true)
					}
					return &mcpCommitUncertainError{cause: commitErr}
				}
			} else if errors.Is(commitErr, errConfigCommitVerification) {
				return fmt.Errorf("%w: %w", ErrMCPStale, commitErr)
			}
			if commitErr != nil {
				return fmt.Errorf("failed to write config file: %w", commitErr)
			}
		}
		if commitReturnedNil {
			runConfigAfterMCPCommitHook(record.commitPath)
		}
		if fingerprintErr := s.applyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
			outcome := newCommitOutcome(record.commitPath, true, false,
				errConfigCommitCommitted, errConfigCommitUncertain, fingerprintErr)
			return &mcpCommitUncertainError{cause: outcome}
		}
		record.expectation = committed
	}
	return nil
}

func (s *ConfigStore) reconcileMCPCommit(record *mcpFileRecord) (reloadFileFingerprint, bool) {
	owner, enforce, err := s.mcpOwnerPolicy(record.selectedPath)
	if err != nil {
		return reloadFileFingerprint{}, false
	}
	runConfigBeforeMCPReconcileHook(record.selectedPath, record.data)
	data, fingerprint, err := readStableConfigFileOwned(record.selectedPath, owner, enforce)
	if err != nil || !sameBytesFingerprint(data, sha256.Sum256(record.data)) {
		return reloadFileFingerprint{}, false
	}
	return fingerprint, true
}

func (s *ConfigStore) applyCommittedMCPFingerprint(files *mcpLockedFiles, record *mcpFileRecord, committed reloadFileFingerprint) error {
	paths := make(map[string]struct{}, len(record.aliases)+2)
	for alias := range record.aliases {
		paths[normalizeDiscoveryPath(alias)] = struct{}{}
	}
	paths[normalizeDiscoveryPath(record.selectedPath)] = struct{}{}
	paths[normalizeDiscoveryPath(record.commitPath)] = struct{}{}
	delete(paths, "")

	for path := range paths {
		expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
		if ownerErr != nil {
			return fmt.Errorf("%w: failed to determine config owner for %s: %w", errConfigCommitUncertain, path, ownerErr)
		}
		_, fingerprint, readErr := readStableConfigFileOwned(path, expectedOwner, enforceOwner)
		if readErr != nil && !os.IsNotExist(readErr) {
			return fmt.Errorf("%w: failed to reread committed config spelling %s: %w", errConfigCommitUncertain, path, readErr)
		}
		if fingerprint.exists != committed.exists || committed.exists &&
			(fingerprint.digest != committed.digest || committed.identity.valid && fingerprint.identity != committed.identity) {
			return fmt.Errorf("%w: committed config spelling %s does not match the published file", errConfigCommitUncertain, path)
		}
		files.fingerprints[path] = fingerprint
	}
	return nil
}

func (s *ConfigStore) evaluateMCPFiles(files *mcpLockedFiles) (mcpEvaluation, error) {
	paths := s.orderedMCPPaths()
	externalPaths := uniqueMCPPaths(mcpJSONCandidatePaths(s.workingDir))
	input := make([][]byte, 0, len(paths))
	fingerprints := make(map[string]reloadFileFingerprint, len(paths)+len(externalPaths))
	origins := make(map[string]MCPOrigin)
	rushDocuments := make([]stableConfigDocument, 0, len(paths))
	seenRushRecords := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		data, present, err := s.mcpPathData(files, path)
		if err != nil {
			return mcpEvaluation{}, err
		}
		fingerprint := files.mcpFingerprint(path)
		fingerprints[path] = fingerprint
		rushDocuments = append(rushDocuments, stableConfigDocument{
			path: path, data: data, fingerprint: fingerprint, present: present,
		})
		if present {
			record := files.mcpRecord(path)
			duplicate := record != nil
			if duplicate {
				_, duplicate = seenRushRecords[record.key]
				seenRushRecords[record.key] = struct{}{}
			}
			if len(data) > 0 {
				if !json.Valid(data) {
					return mcpEvaluation{}, fmt.Errorf("invalid JSON in config file %s", path)
				}
				if err := validateMCPDisabledOverlays(data); err != nil {
					return mcpEvaluation{}, fmt.Errorf("invalid MCP configuration in config file %s: %w", path, err)
				}
				if !duplicate {
					input = append(input, data)
				}
				entryOrigins(origins, path, s.workspacePathValue(), s.globalDataPath, s.systemConfigPathValue(), data)
			}
		}
	}
	cfg, err := loadFromBytes(input)
	if err != nil {
		return mcpEvaluation{}, err
	}
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	// Keep Rush-defined names separate from the effective origins map. The
	// latter is populated with external origins below, so using it as the
	// precedence set would make the global external document shadow the
	// higher-priority project external document.
	rushDefined := make(map[string]struct{}, len(origins))
	for name := range origins {
		rushDefined[name] = struct{}{}
	}
	externalDocuments := make([]stableConfigDocument, 0, len(externalPaths))
	seenExternalRecords := make(map[string]struct{}, len(externalPaths))
	for _, path := range externalPaths {
		data, present, err := s.mcpPathData(files, path)
		if err != nil {
			return mcpEvaluation{}, err
		}
		fingerprint := files.mcpFingerprint(path)
		fingerprints[path] = fingerprint
		document := stableConfigDocument{path: path, data: data, fingerprint: fingerprint, present: present}
		externalDocuments = append(externalDocuments, document)
		if !present {
			continue
		}
		if record := files.mcpRecord(path); record != nil {
			if _, seen := seenExternalRecords[record.key]; seen {
				continue
			}
			seenExternalRecords[record.key] = struct{}{}
		}
		external, err := loadMCPJSONBytes(data)
		if err != nil {
			return mcpEvaluation{}, err
		}
		for name, ext := range external {
			if _, defined := rushDefined[name]; defined {
				continue
			}
			ext.Disabled = externalOverlayValue(rushDocuments, name, ext.Disabled)
			cfg.MCP[name] = ext
			origin := MCPOrigin{Kind: MCPOriginExternal, Path: normalizeReloadPath(path), Scope: ScopeGlobal, Writable: false}
			if current, ok := origins[name]; !ok || mcpOriginPriority(origin) >= mcpOriginPriority(current) {
				origins[name] = origin
			}
		}
	}
	return mcpEvaluation{configs: cfg.MCP, origins: origins, fingerprints: fingerprints}, nil
}

func (s *ConfigStore) orderedMCPPaths() []string {
	paths := make([]string, 0, 8)
	appendSafe := func(path string, owner int) {
		if eligible := eligibleConfigCandidate(path, owner); eligible != "" {
			paths = append(paths, eligible)
		}
	}
	if systemPath := s.systemConfigPathValue(); systemPath != "" {
		appendSafe(systemPath, systemConfigOwner())
	}
	appendSafe(GlobalConfig(), homeConfigOwner())
	appendSafe(s.globalDataPath, homeConfigOwner())
	for _, path := range lookupConfigCandidates(s.workingDir) {
		// lookupConfigCandidates applies the same global/project owner policy;
		// retain its discovery spelling so mcpPathData can bind the alias to the
		// opened file identity.
		paths = append(paths, path)
	}
	if workspacePath := s.configPathOrEmpty(ScopeWorkspace); workspacePath != "" {
		if owner, enforce, err := configOwnerForWorkingDir(s.workingDir); err == nil && (!enforce || eligibleConfigCandidate(workspacePath, owner) != "") {
			paths = append(paths, workspacePath)
		}
	}
	return uniqueMCPPaths(paths)
}

func (s *ConfigStore) systemConfigPathValue() string {
	if s.systemConfigPathOverride != "" {
		return s.systemConfigPathOverride
	}
	return SystemConfig()
}

func (s *ConfigStore) mcpPathData(files *mcpLockedFiles, path string) ([]byte, bool, error) {
	key := normalizeDiscoveryPath(path)
	if record := files.mcpRecord(key); record != nil {
		return record.data, record.expectation.exists, nil
	}
	expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
	if ownerErr != nil {
		return nil, false, ownerErr
	}
	data, fingerprint, err := readStableConfigFileOwned(path, expectedOwner, enforceOwner)
	if err != nil {
		if os.IsNotExist(err) {
			files.bindMCPPath(path, nil, false, fingerprint)
			return nil, false, nil
		}
		if errors.Is(err, errConfigOwnerMismatch) {
			return nil, false, fmt.Errorf("%w: unsafe config input %s", ErrMCPStale, path)
		}
		return nil, false, err
	}
	record := files.bindMCPPath(path, data, fingerprint.exists, fingerprint)
	return record.data, fingerprint.exists, nil
}

func (s *ConfigStore) mcpOwnerPolicy(path string) (int, bool, error) {
	canonical := normalizeReloadPath(path)
	if systemPath := s.systemConfigPathValue(); systemPath != "" && canonical == normalizeReloadPath(systemPath) {
		return systemConfigOwner(), true, nil
	}
	if canonical == normalizeReloadPath(GlobalConfig()) || canonical == normalizeReloadPath(s.globalDataPath) {
		return homeConfigOwner(), true, nil
	}
	if canonical == normalizeReloadPath(filepath.Join(home.Dir(), ".claude", ".mcp.json")) {
		return homeConfigOwner(), true, nil
	}
	return configOwnerForWorkingDir(s.workingDir)
}

func entryOrigins(origins map[string]MCPOrigin, path, workspacePath, globalPath, systemPath string, data []byte) {
	path = normalizeReloadPath(path)
	workspacePath = normalizeReloadPath(workspacePath)
	globalPath = normalizeReloadPath(globalPath)
	systemPath = normalizeReloadPath(systemPath)
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if json.Unmarshal(data, &root) != nil {
		return
	}
	for name, raw := range root.MCP {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || isMCPDisabledOnlyEntry(entry, true) {
			continue
		}
		origin := MCPOrigin{Kind: MCPOriginProject, Path: path, Scope: ScopeGlobal, Writable: false}
		if path == workspacePath {
			origin.Kind, origin.Scope, origin.Writable = MCPOriginWorkspace, ScopeWorkspace, true
		} else if path == globalPath {
			origin.Kind, origin.Scope, origin.Writable = MCPOriginGlobal, ScopeGlobal, true
		} else if path == systemPath {
			origin.Kind = MCPOriginSystem
		}
		if current, ok := origins[name]; !ok || mcpOriginPriority(origin) >= mcpOriginPriority(current) {
			origins[name] = origin
		}
	}
}

func mcpOriginPriority(origin MCPOrigin) int {
	switch origin.Kind {
	case MCPOriginSystem:
		return 1
	case MCPOriginGlobal:
		return 2
	case MCPOriginProject:
		return 3
	case MCPOriginWorkspace:
		return 4
	case MCPOriginExternal:
		return 5
	default:
		return 0
	}
}

func externalOverlayValue(documents []stableConfigDocument, name string, fallback bool) bool {
	for _, document := range documents {
		if !document.present {
			continue
		}
		entry, ok := mcpEntryFromJSON(document.data, name)
		if !isMCPDisabledOnlyEntry(entry, ok) {
			continue
		}
		var disabled bool
		if json.Unmarshal(entry["disabled"], &disabled) == nil {
			fallback = disabled
		}
	}
	return fallback
}

func afterValue(eval mcpEvaluation, name string) (bool, MCPConfig, MCPOrigin) {
	cfg, ok := eval.configs[name]
	return ok, cloneMCPConfig(cfg), eval.origins[name]
}

func (s *ConfigStore) verifyMCPReadOnlyInputs(expected map[string]reloadFileFingerprint, selected string) error {
	for path, fingerprint := range expected {
		if normalizeReloadPath(path) == normalizeReloadPath(selected) {
			continue
		}
		expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
		if ownerErr != nil {
			return fmt.Errorf("%w: %s", ErrMCPStale, path)
		}
		_, actual, err := readStableConfigFileOwned(path, expectedOwner, enforceOwner)
		if os.IsNotExist(err) {
			// A missing candidate still carries its discovery-chain fingerprint.
			err = nil
		}
		if err != nil || actual != fingerprint {
			return fmt.Errorf("%w: %s", ErrMCPStale, path)
		}
	}
	return nil
}

func (s *ConfigStore) publishMCPMutationLocked(result MCPMutationResult) {
	cur := s.loadSnapshot()
	if cur.config == nil {
		return
	}
	next := cur.clone()
	cfg := *cur.config
	cfg.MCP = maps.Clone(cur.config.MCP)
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	if result.Operation == "remove" || (result.Operation == "replace" && result.OldName != result.NewName) {
		delete(cfg.MCP, result.OldName)
	}
	if result.Operation == "add" || result.Operation == "replace" {
		if result.NewExists {
			cfg.MCP[result.NewName] = cloneMCPConfig(result.NewConfig)
		}
	}
	if result.Operation == "disable" || result.Operation == "remove" || (result.Operation == "replace" && result.OldName != result.NewName) {
		if result.Operation == "replace" && result.FallbackExists {
			cfg.MCP[result.OldName] = cloneMCPConfig(result.FallbackConfig)
		} else if result.NewExists && result.Operation != "replace" {
			cfg.MCP[result.OldName] = cloneMCPConfig(result.NewConfig)
		}
	}
	next.config = &cfg
	if len(result.committedFingerprints) > 0 {
		if next.snapshots == nil {
			next.snapshots = make(map[string]fileSnapshot)
		} else {
			next.snapshots = maps.Clone(next.snapshots)
		}
		for path, fingerprint := range result.committedFingerprints {
			key := normalizeDiscoveryPath(path)
			snapshot, ok := next.snapshots[key]
			if !ok {
				continue
			}
			snapshot.Path = key
			snapshot.Exists = fingerprint.exists
			snapshot.Size = fingerprint.size
			snapshot.ModTime = fingerprint.modTime
			snapshot.ContentHash = fingerprint.digest
			snapshot.fingerprint = fingerprint
			next.snapshots[key] = snapshot
		}
	}
	s.publishLocked(next)
}

func (s *ConfigStore) configPathOrEmpty(scope Scope) string {
	path, _ := s.configPath(scope)
	return path
}

func (s *ConfigStore) workspacePathValue() string { return s.loadSnapshot().workspacePath }

func uniqueNormalizedPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = normalizeReloadPath(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func uniqueMCPPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		identity := normalizeDiscoveryPath(path)
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		result = append(result, path)
	}
	return result
}

func fingerprintForBytes(path string, data []byte) reloadFileFingerprint {
	return dataFingerprint(path, data)
}

func dataFingerprint(path string, data []byte) reloadFileFingerprint {
	digest := sha256.Sum256(data)
	if fingerprint, err := readReloadFingerprint(path); err == nil && fingerprint.exists && fingerprint.digest == digest {
		return fingerprint
	}
	return reloadFileFingerprint{exists: true, size: int64(len(data)), digest: digest, nlink: 1}
}
