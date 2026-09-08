package config

import (
	"bytes"
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
	"strings"

	"github.com/PHPCraftdream/rush/internal/home"
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
	type lockGroup struct {
		key      string
		lockPath string
		targets  []configWriteTarget
	}
	groupsByKey := make(map[string]*lockGroup, len(paths))
	targetBySelectedPath := make(map[string]configWriteTarget, len(paths))
	for _, path := range paths {
		target, targetErr := s.resolveConfigWriteTarget(path)
		if targetErr != nil {
			return targetErr
		}
		runConfigAfterMCPResolveTargetHook(&target)
		targetKey := configWriteTargetDedupKey(target)
		if group, exists := groupsByKey[targetKey]; exists {
			group.targets = append(group.targets, target)
		} else {
			groupsByKey[targetKey] = &lockGroup{
				key: targetKey, lockPath: target.lockPath,
				targets: []configWriteTarget{target},
			}
		}
		targetBySelectedPath[target.selectedPath] = target
	}
	groups := make([]*lockGroup, 0, len(groupsByKey))
	for _, group := range groupsByKey {
		groups = append(groups, group)
	}
	slices.SortFunc(groups, func(left, right *lockGroup) int {
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
	mergedGroups := make([]*lockGroup, 0, len(groups))
	groupsByLockPath := make(map[string]*lockGroup, len(groups))
	for _, group := range groups {
		if existing, ok := groupsByLockPath[group.lockPath]; ok {
			existing.targets = append(existing.targets, group.targets...)
			continue
		}
		groupsByLockPath[group.lockPath] = group
		mergedGroups = append(mergedGroups, group)
	}
	groups = mergedGroups

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
		files.targetKeys[normalizeDiscoveryPath(path)] = configWriteTargetDedupKey(target)
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
		after, evalErr := s.evaluateMCPFiles(files)
		if evalErr != nil {
			return evalErr
		}
		result.committedMCPInputs = after.mcpInputs
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
		result.committedMCPInputs = after.mcpInputs
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
		result.committedMCPInputs = after.mcpInputs
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
	data, err := editMCPDocumentWithKind(files.mcpData(path), mcpDocumentRush, operation, oldName, newName, value, disabled, nil)
	if err != nil {
		return err
	}
	return files.setMCPData(path, data)
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
	data, err := editMCPDocumentWithKind(files.mcpData(path), mcpDocumentRush, "update", name, name, MCPConfig{}, nil, mutate)
	if err != nil {
		return err
	}
	return files.setMCPData(path, data)
}

type mcpJSONMember struct {
	key                  string
	keyStart, keyEnd     int
	valueStart, valueEnd int
}

type mcpJSONObject struct {
	start, end int
	members    []mcpJSONMember
}

type mcpJSONEdit struct {
	start, end  int
	replacement []byte
}

type mcpDocumentKind uint8

const (
	mcpDocumentRush mcpDocumentKind = iota
	mcpDocumentExternal
)

// editMCPDocument changes only the selected MCP container or entry. The JSON
// decoder remains the semantic validator; the scanner supplies byte spans so
// unrelated user formatting never passes through a whole-document encoder.
func editMCPDocument(data []byte, path, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	return editMCPDocumentWithKind(data, mcpDocumentKindForPath(path), operation, oldName, newName, value, disabled, mutate)
}

func editMCPDocumentWithKind(data []byte, kind mcpDocumentKind, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	containerName := mcpContainerNameForKind(kind)
	var rootRaw map[string]json.RawMessage
	if len(data) > 0 {
		if err := json.Unmarshal(data, &rootRaw); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
		if rootRaw == nil {
			rootRaw = make(map[string]json.RawMessage)
		}
	}

	if len(data) == 0 {
		return newMCPDocument(containerName, operation, oldName, newName, value, disabled, mutate)
	}
	root, err := scanMCPJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	containerMember := lastMCPMember(root.members, containerName)
	var container *mcpJSONObject
	if containerMember != nil && string(data[containerMember.valueStart:containerMember.valueEnd]) != "null" {
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(data[containerMember.valueStart:containerMember.valueEnd], &entries); err != nil {
			return nil, fmt.Errorf("failed to parse MCP config: %w", err)
		}
		if entries == nil {
			entries = make(map[string]json.RawMessage)
		}
		_, container, err = scanMCPJSONValue(data, containerMember.valueStart)
		if err != nil || container == nil {
			return nil, fmt.Errorf("failed to parse MCP config: expected %s to be an object", containerName)
		}
	}

	var entryRaw []byte
	if operation != "remove" {
		var entryErr error
		entryRaw, entryErr = mcpMutationEntry(data, container, oldName, newName, value, disabled, mutate)
		if entryErr != nil {
			return nil, entryErr
		}
	}
	entryName := newName
	if operation == "remove" {
		entryName = oldName
	}
	var entryMember *mcpJSONMember
	if container != nil {
		entryMember = lastMCPMember(container.members, entryName)
	}

	switch operation {
	case "remove":
		if container == nil {
			if containerMember != nil {
				return applyMCPJSONEdits(data, mcpJSONEdit{start: containerMember.valueStart, end: containerMember.valueEnd, replacement: []byte("{}")}), nil
			}
			return addMCPContainer(data, root, containerName, []byte("{}")), nil
		}
		if entryMember == nil {
			return data, nil
		}
		return removeMCPMembers(data, *container, oldName), nil
	case "replace":
		if oldName != newName && container != nil {
			if existing := lastMCPMember(container.members, newName); existing != nil {
				return nil, fmt.Errorf("MCP target already exists: %q", newName)
			}
			oldCount := 0
			for _, member := range container.members {
				if member.key == oldName {
					oldCount++
				}
			}
			if oldCount > 1 {
				withoutOld := removeMCPMembers(data, *container, oldName)
				withoutRoot, scanErr := scanMCPJSONObject(withoutOld)
				if scanErr != nil {
					return nil, scanErr
				}
				withoutMember := lastMCPMember(withoutRoot.members, containerName)
				if withoutMember == nil {
					return nil, errors.New("MCP container disappeared during rename")
				}
				_, withoutContainer, scanErr := scanMCPJSONValue(withoutOld, withoutMember.valueStart)
				if scanErr != nil || withoutContainer == nil {
					return nil, errors.New("MCP container became invalid during rename")
				}
				return insertMCPMember(withoutOld, *withoutContainer, newName, entryRaw), nil
			}
			if old := lastMCPMember(container.members, oldName); old != nil {
				return applyMCPJSONEdits(data,
					mcpJSONEdit{start: old.keyStart, end: old.keyEnd, replacement: mustJSONMarshal(newName)},
					mcpJSONEdit{start: old.valueStart, end: old.valueEnd, replacement: entryRaw}), nil
			}
		}
		fallthrough
	case "add", "disable", "update":
		if container == nil {
			containerValue := newMCPContainer(entryName, entryRaw, operation == "remove")
			if containerMember != nil {
				return applyMCPJSONEdits(data, mcpJSONEdit{start: containerMember.valueStart, end: containerMember.valueEnd, replacement: containerValue}), nil
			}
			return addMCPContainer(data, root, containerName, containerValue), nil
		}
		if entryMember != nil {
			return applyMCPJSONEdits(data, mcpJSONEdit{start: entryMember.valueStart, end: entryMember.valueEnd, replacement: entryRaw}), nil
		}
		return insertMCPMember(data, *container, entryName, entryRaw), nil
	default:
		return data, nil
	}
}

func mcpMutationEntry(data []byte, container *mcpJSONObject, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	name := newName
	if disabled != nil || mutate != nil {
		name = oldName
	}
	var raw []byte
	if container != nil {
		if member := lastMCPMember(container.members, name); member != nil {
			raw = append([]byte(nil), data[member.valueStart:member.valueEnd]...)
		}
	}
	entry := make(map[string]any)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("failed to parse MCP server %q: %w", name, err)
		}
		if entry == nil {
			entry = make(map[string]any)
		}
	}
	if disabled != nil {
		entry["disabled"] = *disabled
	}
	if mutate != nil {
		if err := mutate(entry); err != nil {
			return nil, err
		}
	}
	if disabled == nil && mutate == nil {
		var err error
		raw, err = json.Marshal(value)
		return raw, err
	}
	return json.Marshal(entry)
}

func mcpContainerName(path string) string {
	return mcpContainerNameForKind(mcpDocumentKindForPath(path))
}

func mcpDocumentKindForPath(path string) mcpDocumentKind {
	if strings.EqualFold(filepath.Base(path), ".mcp.json") {
		return mcpDocumentExternal
	}
	return mcpDocumentRush
}

func mcpContainerNameForKind(kind mcpDocumentKind) string {
	if kind == mcpDocumentExternal {
		return "mcpServers"
	}
	return "mcp"
}

func newMCPDocument(containerName, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	if operation == "remove" {
		return []byte("{\n  " + string(mustJSONMarshal(containerName)) + ": {}\n}\n"), nil
	}
	entry, err := mcpMutationEntry(nil, nil, oldName, newName, value, disabled, mutate)
	if err != nil {
		return nil, err
	}
	return prettyMCPDocument(containerName, newName, entry), nil
}

func prettyMCPDocument(containerName, name string, entry []byte) []byte {
	var indented bytes.Buffer
	if err := json.Indent(&indented, entry, "", "  "); err != nil {
		indented.Write(entry)
	}
	lines := bytes.Split(indented.Bytes(), []byte{'\n'})
	var value bytes.Buffer
	value.Write(lines[0])
	for _, line := range lines[1:] {
		value.WriteByte('\n')
		value.WriteString("    ")
		value.Write(line)
	}
	return []byte("{\n  " + string(mustJSONMarshal(containerName)) + ": {\n    " + string(mustJSONMarshal(name)) + ": " + value.String() + "\n  }\n}\n")
}

func newMCPContainer(name string, entry []byte, empty bool) []byte {
	if empty {
		return []byte("{}")
	}
	key := mustJSONMarshal(name)
	return append(append(append([]byte{'{'}, key...), ':'), append(entry, '}')...)
}

func addMCPContainer(data []byte, root mcpJSONObject, name string, value []byte) []byte {
	return insertMCPMember(data, root, name, value)
}

func insertMCPMember(data []byte, object mcpJSONObject, name string, value []byte) []byte {
	key := mustJSONMarshal(name)
	member := append(append(append(append([]byte(nil), key...), ':'), value...), nil...)
	contentStart, contentEnd := object.start+1, object.end-1
	layout := mcpJSONLayoutForObject(data, object)
	if len(object.members) == 0 {
		if layout.multiline {
			replacement := []byte(layout.newline + layout.memberIndent + string(member) + layout.newline + layout.closingIndent)
			return applyMCPJSONEdits(data, mcpJSONEdit{start: contentStart, end: contentEnd, replacement: replacement})
		}
		return applyMCPJSONEdits(data, mcpJSONEdit{start: contentStart, end: contentEnd, replacement: member})
	}

	last := object.members[len(object.members)-1]
	trailing := data[last.valueEnd:contentEnd]
	if layout.multiline {
		replacement := append([]byte{','}, []byte(layout.newline+layout.memberIndent)...)
		replacement = append(replacement, member...)
		replacement = append(replacement, trailing...)
		return applyMCPJSONEdits(data, mcpJSONEdit{start: last.valueEnd, end: contentEnd, replacement: replacement})
	}
	separator := []byte(",")
	if bytes.Contains(data[contentStart:contentEnd], []byte(", ")) {
		separator = []byte(", ")
	}
	replacement := append(separator, member...)
	replacement = append(replacement, trailing...)
	return applyMCPJSONEdits(data, mcpJSONEdit{start: last.valueEnd, end: contentEnd, replacement: replacement})
}

func removeMCPMember(data []byte, object mcpJSONObject, member mcpJSONMember) []byte {
	index := -1
	for i := range object.members {
		if object.members[i].keyStart == member.keyStart {
			index = i
			break
		}
	}
	if len(object.members) == 1 {
		replacement := append([]byte(nil), data[member.valueEnd:object.end-1]...)
		return applyMCPJSONEdits(data, mcpJSONEdit{start: object.start + 1, end: object.end - 1, replacement: replacement})
	}
	if index < len(object.members)-1 {
		return applyMCPJSONEdits(data, mcpJSONEdit{start: member.keyStart, end: object.members[index+1].keyStart})
	}
	trailing := append([]byte(nil), data[member.valueEnd:object.end-1]...)
	return applyMCPJSONEdits(data, mcpJSONEdit{start: object.members[index-1].valueEnd, end: object.end - 1, replacement: trailing})
}

func removeMCPMembers(data []byte, object mcpJSONObject, key string) []byte {
	indices := make([]int, 0, 1)
	for index, member := range object.members {
		if member.key == key {
			indices = append(indices, index)
		}
	}
	if len(indices) <= 1 {
		if len(indices) == 0 {
			return data
		}
		return removeMCPMember(data, object, object.members[indices[0]])
	}
	if len(indices) == len(object.members) {
		last := object.members[len(object.members)-1]
		replacement := append([]byte(nil), data[last.valueEnd:object.end-1]...)
		return applyMCPJSONEdits(data, mcpJSONEdit{start: object.start + 1, end: object.end - 1, replacement: replacement})
	}

	edits := make([]mcpJSONEdit, 0, len(indices))
	for start := 0; start < len(indices); {
		runStart := indices[start]
		runEnd := runStart
		for start+1 < len(indices) && indices[start+1] == runEnd+1 {
			start++
			runEnd = indices[start]
		}
		if runEnd == len(object.members)-1 {
			previous := object.members[runStart-1]
			trailing := append([]byte(nil), data[object.members[runEnd].valueEnd:object.end-1]...)
			edits = append(edits, mcpJSONEdit{start: previous.valueEnd, end: object.end - 1, replacement: trailing})
		} else {
			edits = append(edits, mcpJSONEdit{
				start: object.members[runStart].keyStart,
				end:   object.members[runEnd+1].keyStart,
			})
		}
		start++
	}
	return applyMCPJSONEdits(data, edits...)
}

func lastMCPMember(members []mcpJSONMember, key string) *mcpJSONMember {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].key == key {
			member := members[i]
			return &member
		}
	}
	return nil
}

func applyMCPJSONEdits(data []byte, edits ...mcpJSONEdit) []byte {
	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			if edits[j].start > edits[i].start {
				edits[i], edits[j] = edits[j], edits[i]
			}
		}
	}
	result := append([]byte(nil), data...)
	for _, edit := range edits {
		result = append(append(append([]byte(nil), result[:edit.start]...), edit.replacement...), result[edit.end:]...)
	}
	return result
}

func mustJSONMarshal(value string) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func mcpJSONNewline(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	if bytes.Contains(data, []byte{'\n'}) {
		return "\n"
	}
	return ""
}

type mcpJSONLayout struct {
	newline       string
	multiline     bool
	memberIndent  string
	closingIndent string
}

func mcpJSONLayoutForObject(data []byte, object mcpJSONObject) mcpJSONLayout {
	contentStart, contentEnd := object.start+1, object.end-1
	if len(object.members) == 0 {
		gap := data[contentStart:contentEnd]
		newline := mcpJSONNewline(gap)
		if newline == "" {
			return mcpJSONLayout{}
		}
		closingIndent, ok := mcpJSONWhitespaceSuffixAfterNewline(gap)
		if !ok {
			return mcpJSONLayout{newline: newline, multiline: true}
		}
		return mcpJSONLayout{
			newline: newline, multiline: true,
			memberIndent: closingIndent + "  ", closingIndent: closingIndent,
		}
	}

	leading := data[contentStart:object.members[0].keyStart]
	trailing := data[object.members[len(object.members)-1].valueEnd:contentEnd]
	gaps := make([][]byte, 0, len(object.members)+1)
	gaps = append(gaps, leading)
	for index := 1; index < len(object.members); index++ {
		gaps = append(gaps, data[object.members[index-1].valueEnd:object.members[index].keyStart])
	}
	gaps = append(gaps, trailing)

	layout := mcpJSONLayout{}
	for index, gap := range gaps {
		newline := mcpJSONNewline(gap)
		if newline == "" {
			continue
		}
		layout.multiline = true
		if layout.newline == "" {
			layout.newline = newline
		}
		if index < len(gaps)-1 && layout.memberIndent == "" {
			if indent, ok := mcpJSONWhitespaceSuffixAfterNewline(gap); ok {
				layout.memberIndent = indent
			}
		}
	}
	if newline := mcpJSONNewline(trailing); newline != "" {
		if indent, ok := mcpJSONWhitespaceSuffixAfterNewline(trailing); ok {
			layout.closingIndent = indent
		}
	}
	return layout
}

func mcpJSONWhitespaceSuffixAfterNewline(data []byte) (string, bool) {
	index := bytes.LastIndexByte(data, '\n')
	if index < 0 {
		return "", false
	}
	suffix := data[index+1:]
	for _, char := range suffix {
		if char != ' ' && char != '\t' {
			return "", false
		}
	}
	return string(suffix), true
}

func mcpJSONMemberIndent(data []byte, keyStart int) string {
	lineStart := bytes.LastIndexByte(data[:keyStart], '\n') + 1
	indent := data[lineStart:keyStart]
	for _, char := range indent {
		if char != ' ' && char != '\t' {
			return ""
		}
	}
	return string(indent)
}

func scanMCPJSONObject(data []byte) (mcpJSONObject, error) {
	end, object, err := scanMCPJSONValue(data, mcpJSONSkipSpace(data, 0))
	if err != nil || object == nil || mcpJSONSkipSpace(data, end) != len(data) {
		if err != nil {
			return mcpJSONObject{}, err
		}
		return mcpJSONObject{}, errors.New("JSON root must be an object")
	}
	return *object, nil
}

func scanMCPJSONValue(data []byte, pos int) (int, *mcpJSONObject, error) {
	if pos >= len(data) {
		return 0, nil, errors.New("unexpected end of JSON")
	}
	switch data[pos] {
	case '{':
		object := &mcpJSONObject{start: pos}
		pos = mcpJSONSkipSpace(data, pos+1)
		if pos == len(data) {
			return 0, nil, errors.New("unterminated JSON object")
		}
		if data[pos] == '}' {
			object.end = pos + 1
			return object.end, object, nil
		}
		for {
			keyStart := mcpJSONSkipSpace(data, pos)
			keyEnd, key, err := scanMCPJSONString(data, keyStart)
			if err != nil {
				return 0, nil, err
			}
			pos = mcpJSONSkipSpace(data, keyEnd)
			if pos >= len(data) || data[pos] != ':' {
				return 0, nil, errors.New("JSON object member lacks colon")
			}
			valueStart := mcpJSONSkipSpace(data, pos+1)
			valueEnd, _, err := scanMCPJSONValue(data, valueStart)
			if err != nil {
				return 0, nil, err
			}
			object.members = append(object.members, mcpJSONMember{
				key: key, keyStart: keyStart, keyEnd: keyEnd,
				valueStart: valueStart, valueEnd: valueEnd,
			})
			pos = mcpJSONSkipSpace(data, valueEnd)
			if pos >= len(data) {
				return 0, nil, errors.New("unterminated JSON object")
			}
			if data[pos] == '}' {
				object.end = pos + 1
				return object.end, object, nil
			}
			if data[pos] != ',' {
				return 0, nil, errors.New("JSON object member lacks comma")
			}
			pos++
		}
	case '[':
		pos = mcpJSONSkipSpace(data, pos+1)
		if pos < len(data) && data[pos] == ']' {
			return pos + 1, nil, nil
		}
		for {
			var err error
			pos, _, err = scanMCPJSONValue(data, mcpJSONSkipSpace(data, pos))
			if err != nil {
				return 0, nil, err
			}
			pos = mcpJSONSkipSpace(data, pos)
			if pos >= len(data) {
				return 0, nil, errors.New("unterminated JSON array")
			}
			if data[pos] == ']' {
				return pos + 1, nil, nil
			}
			if data[pos] != ',' {
				return 0, nil, errors.New("JSON array member lacks comma")
			}
			pos++
		}
	case '"':
		end, _, err := scanMCPJSONString(data, pos)
		return end, nil, err
	default:
		end := pos
		for end < len(data) && !bytes.ContainsRune([]byte{' ', '\t', '\r', '\n', ',', ']', '}'}, rune(data[end])) {
			end++
		}
		if end == pos {
			return 0, nil, errors.New("invalid JSON value")
		}
		return end, nil, nil
	}
}

func scanMCPJSONString(data []byte, start int) (int, string, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, "", errors.New("JSON object key must be a string")
	}
	for pos := start + 1; pos < len(data); pos++ {
		switch data[pos] {
		case '\\':
			pos++
		case '"':
			end := pos + 1
			var value string
			if err := json.Unmarshal(data[start:end], &value); err != nil {
				return 0, "", err
			}
			return end, value, nil
		}
	}
	return 0, "", errors.New("unterminated JSON string")
}

func mcpJSONSkipSpace(data []byte, pos int) int {
	for pos < len(data) {
		switch data[pos] {
		case ' ', '\t', '\r', '\n':
			pos++
		default:
			return pos
		}
	}
	return pos
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
	return mcpEvaluation{
		configs: cfg.MCP, origins: origins, fingerprints: fingerprints,
		mcpInputs: mcpInputFingerprints(rushDocuments, externalDocuments),
	}, nil
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
	if strings.EqualFold(filepath.Base(path), ".mcp.json") {
		if strings.EqualFold(
			normalizeDiscoveryPath(path),
			normalizeDiscoveryPath(filepath.Join(home.Dir(), ".claude", ".mcp.json")),
		) {
			return homeConfigOwner(), true, nil
		}
		s.workingDirOwnerOnce.Do(func() {
			var enforce bool
			s.workingDirOwner, enforce, s.workingDirOwnerErr = configOwnerForWorkingDir(s.workingDir)
			_ = enforce
		})
		if s.workingDirOwnerErr != nil {
			return 0, false, s.workingDirOwnerErr
		}
		return s.workingDirOwner, s.workingDir != "", nil
	}
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
	s.workingDirOwnerOnce.Do(func() {
		var enforce bool
		s.workingDirOwner, enforce, s.workingDirOwnerErr = configOwnerForWorkingDir(s.workingDir)
		_ = enforce
	})
	if s.workingDirOwnerErr != nil {
		return 0, false, s.workingDirOwnerErr
	}
	return s.workingDirOwner, s.workingDir != "", nil
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
	if result.committedMCPInputs != nil {
		next.mcpInputs = maps.Clone(result.committedMCPInputs)
	}
	next.mcpRevisions = mcpRevisionDiff(cur.mcpRevisions, cur.config, next.config, cur.mcpInputs, next.mcpInputs)
	next.resolverDynamic = configHasDynamicMCPResolution(next.config)
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

func dataFingerprint(path string, data []byte) reloadFileFingerprint {
	digest := sha256.Sum256(data)
	if fingerprint, err := readReloadFingerprint(path); err == nil && fingerprint.exists && fingerprint.digest == digest {
		return fingerprint
	}
	return reloadFileFingerprint{exists: true, size: int64(len(data)), digest: digest, nlink: 1}
}
