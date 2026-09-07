package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

var (
	ErrMCPNotFound         = errors.New("MCP server not found")
	ErrMCPExternal         = errors.New("MCP server is from .mcp.json and is not writable")
	ErrMCPAmbiguous        = ErrMCPExternal
	ErrMCPUnwritableOrigin = errors.New("MCP server has no writable config scope")
	ErrMCPTargetExists     = errors.New("MCP server target already exists")
	ErrMCPStale            = errors.New("MCP server configuration changed while preparing mutation")
	ErrMCPCommitUncertain  = errors.New("MCP config commit outcome is uncertain")
	ErrMCPMutationStale    = errors.New("MCP mutation result is stale")
)

type mcpCommitUncertainError struct {
	cause error
}

func (e *mcpCommitUncertainError) Error() string {
	return fmt.Sprintf("%s: %v", ErrMCPCommitUncertain, e.cause)
}

func (e *mcpCommitUncertainError) Unwrap() []error {
	return []error{ErrMCPCommitUncertain, e.cause}
}

func mcpCommitWasReconciled(err error) bool {
	outcome, ok := CommitOutcomeFromError(err)
	return ok && outcome.Committed && outcome.Reconciled
}

// MCPMutationResult describes the effective configuration on both sides of a
// durable MCP mutation. NewConfig/NewOrigin may describe a lower-priority
// definition revealed by removing or replacing the old one.
type MCPMutationResult struct {
	Operation string
	OldName   string
	NewName   string
	// Generation identifies the store snapshot published for this mutation.
	// Lifecycle callers must not use a result after a newer snapshot exists.
	Generation            uint64
	OldExists             bool
	NewExists             bool
	OldConfig             MCPConfig
	NewConfig             MCPConfig
	OldOrigin             MCPOrigin
	NewOrigin             MCPOrigin
	FallbackExists        bool
	FallbackConfig        MCPConfig
	FallbackOrigin        MCPOrigin
	committedFingerprints map[string]reloadFileFingerprint
	committedMCPInputs    map[string][32]byte
}

// WithCurrentMCPMutation validates result against a fresh evaluation of the
// locked config files, then runs fn while that evaluation remains pinned.
//
// fn is the runtime publication callback only: it must not mutate config on
// disk or re-enter ConfigStore persistence. It may block on lifecycle locks;
// the shared config sidecar locks stay held until it returns.
func (s *ConfigStore) WithCurrentMCPMutation(result MCPMutationResult, fn func() error) error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		evaluation, err := s.evaluateMCPFiles(files)
		if err != nil {
			return ErrMCPMutationStale
		}
		if !s.mcpMutationResultCurrentLocked(result, evaluation) {
			return ErrMCPMutationStale
		}
		return fn()
	})
}

// WithCurrentMCPAdmission validates an immutable MCP admission snapshot and
// runs fn while the config snapshot and disk inputs remain pinned.
func (s *ConfigStore) WithCurrentMCPAdmission(snapshot MCPAdmissionSnapshot, name string, fn func() error) error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if s.workingDir == "" && s.globalDataPath == "" {
		if !s.mcpAdmissionSnapshotCurrent(snapshot, name) {
			return ErrMCPMutationStale
		}
		return fn()
	}
	return s.withMCPAdmissionLocks(func(files *mcpLockedFiles) error {
		if !s.mcpAdmissionSnapshotCurrent(snapshot, name) {
			return ErrMCPMutationStale
		}
		evaluation, err := s.evaluateMCPFiles(files)
		if err != nil {
			return ErrMCPMutationStale
		}
		input, hasInput := evaluation.mcpInputs[name]
		if hasInput != snapshot.HasMCPInput || hasInput && input != snapshot.MCPInput {
			return ErrMCPMutationStale
		}
		return fn()
	})
}

func (s *ConfigStore) mcpAdmissionSnapshotCurrent(snapshot MCPAdmissionSnapshot, name string) bool {
	current := s.loadSnapshot()
	if current.generation < snapshot.Generation ||
		current.mcpRevisions[name] != snapshot.MCPRevision ||
		current.resolverRevision != snapshot.ResolverRevision ||
		current.config == nil {
		return false
	}
	value, exists := current.config.MCP[name]
	return exists == snapshot.Exists && (!exists || reflect.DeepEqual(value, snapshot.MCPConfig))
}

func (s *ConfigStore) mcpMutationResultCurrentLocked(result MCPMutationResult, evaluation mcpEvaluation) bool {
	if result.Generation != 0 && s.loadSnapshot().generation != result.Generation {
		return false
	}

	// The result records every exact spelling changed by its commit. Checking
	// these bytes while the sidecars are held closes the release-and-recheck
	// window that allowed another ConfigStore to replace the commit with an
	// ABA-equivalent effective value.
	for path, expected := range result.committedFingerprints {
		actual, ok := mcpEvaluationFingerprint(evaluation.fingerprints, path)
		if !ok || actual != expected {
			return false
		}
	}

	// Preserve the old staleness fence for unrelated tracked inputs too. This
	// also supports compatibility results assembled by lifecycle adapters that
	// predate committedFingerprints.
	snapshot := s.loadSnapshot()
	for path, expected := range snapshot.snapshots {
		if expected.fingerprint == (reloadFileFingerprint{}) {
			continue
		}
		actual, ok := mcpEvaluationFingerprint(evaluation.fingerprints, path)
		if !ok || !reloadFingerprintContentEqual(expected.fingerprint, actual) {
			return false
		}
	}

	current, exists := evaluation.configs[result.NewName]
	if exists != result.NewExists || exists && !reflect.DeepEqual(current, result.NewConfig) {
		return false
	}
	if exists && result.NewOrigin != (MCPOrigin{}) && evaluation.origins[result.NewName] != result.NewOrigin {
		return false
	}
	if result.Operation == "replace" && result.OldName != result.NewName {
		fallback, fallbackExists := evaluation.configs[result.OldName]
		if fallbackExists != result.FallbackExists || fallbackExists && !reflect.DeepEqual(fallback, result.FallbackConfig) {
			return false
		}
		if fallbackExists && result.FallbackOrigin != (MCPOrigin{}) && evaluation.origins[result.OldName] != result.FallbackOrigin {
			return false
		}
	}
	return true
}

func reloadFingerprintContentEqual(expected, actual reloadFileFingerprint) bool {
	if expected.exists != actual.exists || expected.size != actual.size || expected.digest != actual.digest {
		return false
	}
	if !expected.exists {
		return true
	}
	return expected == actual
}

func committedMCPFingerprints(files *mcpLockedFiles) map[string]reloadFileFingerprint {
	result := make(map[string]reloadFileFingerprint)
	for _, record := range files.records {
		if !files.changed[record.key] {
			continue
		}
		for alias := range record.aliases {
			if fingerprint, ok := files.fingerprints[alias]; ok {
				result[alias] = fingerprint
			}
		}
		if fingerprint, ok := files.fingerprints[record.commitPath]; ok {
			result[record.commitPath] = fingerprint
		}
	}
	return result
}

// ResolveMCPWritableScope resolves the effective owner at a fresh disk
// transaction boundary. It never trusts a previously loaded snapshot for a
// scope decision, so a concurrent promotion cannot redirect a write.
func (s *ConfigStore) ResolveMCPWritableScope(name string) (Scope, error) {
	var origin MCPOrigin
	var cfg MCPConfig
	var exists bool
	err := s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		eval, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		cfg, exists = eval.configs[name]
		origin = eval.origins[name]
		return nil
	})
	if err != nil {
		return ScopeGlobal, err
	}
	if !exists {
		return ScopeGlobal, fmt.Errorf("%w: %q", ErrMCPNotFound, name)
	}
	if cfg.Source == MCPSourceExternal || origin.Kind == MCPOriginExternal {
		return ScopeGlobal, fmt.Errorf("%w: %q", ErrMCPExternal, name)
	}
	if !origin.Writable {
		return ScopeGlobal, fmt.Errorf("%w: %q is defined in %s", ErrMCPUnwritableOrigin, name, origin.Path)
	}
	return origin.Scope, nil
}

func (s *ConfigStore) PersistMCPConfig(scope Scope, name string, value MCPConfig) error {
	_, err := s.PersistMCPConfigResult(scope, name, value)
	return err
}

// PersistMCPConfigExact adds a literal server entry to the selected scope.
// Unlike the runtime lifecycle API, it deliberately ignores higher-priority
// definitions in other files; this is the API used by exact-scope CLI edits.
func (s *ConfigStore) PersistMCPConfigExact(scope Scope, name string, value MCPConfig) error {
	_, err := s.mutateMCPExact("add", scope, name, name, value, nil)
	return err
}

func (s *ConfigStore) PersistMCPConfigResult(scope Scope, name string, value MCPConfig) (MCPMutationResult, error) {
	return s.mutateMCP("add", scope, name, name, value, nil)
}

func (s *ConfigStore) PersistRemoveMCPConfig(scope Scope, name string) error {
	_, err := s.PersistRemoveMCPConfigResult(scope, name)
	return err
}

// PersistRemoveMCPConfigExact removes only the literal entry in the selected
// scope, even when another scope shadows it in the effective configuration.
func (s *ConfigStore) PersistRemoveMCPConfigExact(scope Scope, name string) error {
	_, err := s.mutateMCPExact("remove", scope, name, name, MCPConfig{}, nil)
	return err
}

func (s *ConfigStore) PersistRemoveMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.mutateMCP("remove", scope, name, name, MCPConfig{}, nil)
}

// PersistRemovePendingMCPConfigResult conditionally completes a pending MCP
// add without ever writing its in-memory definition. It succeeds only while
// the server is absent from the durable configuration; an existing durable
// definition is a collision and is left untouched.
func (s *ConfigStore) PersistRemovePendingMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.mutatePendingRemoveMCP(scope, name)
}

// PersistRemovePendingMCPConfig is the error-only form of
// PersistRemovePendingMCPConfigResult.
func (s *ConfigStore) PersistRemovePendingMCPConfig(scope Scope, name string) error {
	_, err := s.PersistRemovePendingMCPConfigResult(scope, name)
	return err
}

func (s *ConfigStore) PersistMCPDisabledOverride(scope Scope, name string, disabled bool) error {
	_, err := s.PersistMCPDisabledOverrideResult(scope, name, disabled)
	return err
}

func (s *ConfigStore) PersistMCPDisabledOverrideResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mutateMCP("disable", scope, name, name, MCPConfig{}, &disabled)
}

// PersistMCPEnableRollbackResult conditionally disables the exact definition
// published by an Enable mutation. It is safe for both ordinary definitions
// and pending global adds.
func (s *ConfigStore) PersistMCPEnableRollbackResult(scope Scope, name string, token MCPMutationResult) (MCPMutationResult, error) {
	return s.mutateMCPEnableRollback(scope, name, token)
}

// PersistMCPDisabledOverrideExact changes only the selected scope. It may
// update a shadowed literal entry and may create a workspace overlay for an
// external server, but it never redirects the write to another scope.
func (s *ConfigStore) PersistMCPDisabledOverrideExact(scope Scope, name string, disabled bool) error {
	_, err := s.PersistMCPDisabledOverrideExactResult(scope, name, disabled)
	return err
}

func (s *ConfigStore) PersistMCPDisabledOverrideExactResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mutateMCPExact("disable", scope, name, name, MCPConfig{}, &disabled)
}

// PersistMCPDisabledOverrideAtScope changes only the selected scope.
func (s *ConfigStore) PersistMCPDisabledOverrideAtScope(scope Scope, name string, disabled bool) error {
	return s.PersistMCPDisabledOverrideExact(scope, name, disabled)
}

// PersistMCPDisabledOverrideInScope changes only the selected scope.
func (s *ConfigStore) PersistMCPDisabledOverrideInScope(scope Scope, name string, disabled bool) error {
	return s.PersistMCPDisabledOverrideExact(scope, name, disabled)
}

func (s *ConfigStore) PersistReplaceMCP(oldName, newName string, value MCPConfig) error {
	_, err := s.PersistReplaceMCPResult(ScopeGlobal, oldName, newName, value)
	return err
}

func (s *ConfigStore) PersistReplaceMCPResult(scope Scope, oldName, newName string, value MCPConfig) (MCPMutationResult, error) {
	return s.mutateMCP("replace", scope, oldName, newName, value, nil)
}

func (s *ConfigStore) PersistReplaceMCPInScope(scope Scope, oldName, newName string, value MCPConfig) error {
	_, err := s.PersistReplaceMCPResult(scope, oldName, newName, value)
	return err
}

func (s *ConfigStore) PersistReplaceMCPAtScope(scope Scope, oldName, newName string, value MCPConfig) error {
	return s.PersistReplaceMCPInScope(scope, oldName, newName, value)
}

// PersistMCPFields is the low-level literal-key field editor retained for
// non-lifecycle callers. It still takes the same ordered two-file transaction
// locks as lifecycle mutations.
func (s *ConfigStore) PersistMCPFields(scope Scope, name string, fields map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	path = normalizeReloadPath(path)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var effective MCPConfig
	var effectiveExists bool
	var committed map[string]reloadFileFingerprint
	var committedInputs map[string][32]byte
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		before, evalErr := s.evaluateMCPFiles(files)
		if evalErr != nil {
			return evalErr
		}
		if err := files.validateMutableTopology(); err != nil {
			return err
		}
		_, oldOK := before.configs[name]
		if err := validateMCPMutation("set", scope, name, name, oldOK, before.origins[name], before.configs); err != nil {
			return err
		}
		if err := updateMCPFile(files, path, name, func(entry map[string]any) error {
			for _, key := range keys {
				entry[key] = fields[key]
			}
			return nil
		}); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
		after, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		effectiveExists, effective, _ = afterValue(after, name)
		committedInputs = after.mcpInputs
		committed = committedMCPFingerprints(files)
		writeErr := s.writeMCPFileChanges(files)
		committed = committedMCPFingerprints(files)
		if writeErr != nil {
			return writeErr
		}
		return nil
	})
	if err == nil || mcpCommitWasReconciled(err) {
		// The selected file may be shadowed, so publish the effective value
		// only after the exact file mutation has committed. A later reload
		// remains authoritative for all other fields.
		s.publishMCPValueAndStalenessLocked(name, effective, effectiveExists, committed, committedInputs)
	}
	s.publishMu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// PersistMCPFieldsExact updates only the literal entry in the selected file.
// It does not resolve or validate the effective owner in another scope.
func (s *ConfigStore) PersistMCPFieldsExact(scope Scope, name string, fields map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	path = normalizeReloadPath(path)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var effective MCPConfig
	var effectiveExists bool
	var committed map[string]reloadFileFingerprint
	var committedInputs map[string][32]byte
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		before, evalErr := s.evaluateMCPFiles(files)
		if evalErr != nil {
			return evalErr
		}
		if err := files.validateMutableTopology(); err != nil {
			return err
		}
		if !literalMCPEntryExists(files.mcpData(path), name) {
			return fmt.Errorf("%w: %q", ErrMCPNotFound, name)
		}
		if err := updateMCPFile(files, path, name, func(entry map[string]any) error {
			for _, key := range keys {
				entry[key] = fields[key]
			}
			return nil
		}); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
		after, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		effectiveExists, effective, _ = afterValue(after, name)
		committedInputs = after.mcpInputs
		committed = committedMCPFingerprints(files)
		writeErr := s.writeMCPFileChanges(files)
		committed = committedMCPFingerprints(files)
		if writeErr != nil {
			return writeErr
		}
		return nil
	})
	if err == nil || mcpCommitWasReconciled(err) {
		s.publishMCPValueAndStalenessLocked(name, effective, effectiveExists, committed, committedInputs)
	}
	s.publishMu.Unlock()
	return err
}

// AtScope aliases make the exact-file intent explicit to callers that prefer
// the older lifecycle method naming convention.
func (s *ConfigStore) PersistMCPConfigAtScope(scope Scope, name string, value MCPConfig) error {
	return s.PersistMCPConfigExact(scope, name, value)
}

func (s *ConfigStore) PersistRemoveMCPConfigAtScope(scope Scope, name string) error {
	return s.PersistRemoveMCPConfigExact(scope, name)
}

func (s *ConfigStore) PersistMCPFieldsAtScope(scope Scope, name string, fields map[string]any) error {
	return s.PersistMCPFieldsExact(scope, name, fields)
}

func (s *ConfigStore) PersistMCPConfigInScope(scope Scope, name string, value MCPConfig) error {
	return s.PersistMCPConfigExact(scope, name, value)
}

func (s *ConfigStore) PersistRemoveMCPConfigInScope(scope Scope, name string) error {
	return s.PersistRemoveMCPConfigExact(scope, name)
}

func (s *ConfigStore) PersistMCPFieldsInScope(scope Scope, name string, fields map[string]any) error {
	return s.PersistMCPFieldsExact(scope, name, fields)
}

func (s *ConfigStore) publishMCPConfigLocked(oldName, newName string) {
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
	if oldName != newName {
		delete(cfg.MCP, oldName)
	}
	next.config = &cfg
	next.mcpRevisions = mcpRevisionDiff(cur.mcpRevisions, cur.config, next.config, cur.mcpInputs, next.mcpInputs)
	s.publishLocked(next)
}

func (s *ConfigStore) publishMCPValueAndStalenessLocked(name string, value MCPConfig, exists bool, committed map[string]reloadFileFingerprint, committedInputs map[string][32]byte) {
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
	if exists {
		cfg.MCP[name] = cloneMCPConfig(value)
	} else {
		delete(cfg.MCP, name)
	}
	next.config = &cfg
	if committedInputs != nil {
		next.mcpInputs = maps.Clone(committedInputs)
	}
	next.mcpRevisions = mcpRevisionDiff(cur.mcpRevisions, cur.config, next.config, cur.mcpInputs, next.mcpInputs)
	if len(committed) > 0 {
		if next.snapshots == nil {
			next.snapshots = make(map[string]fileSnapshot)
		} else {
			next.snapshots = maps.Clone(next.snapshots)
		}
		for path, fingerprint := range committed {
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
