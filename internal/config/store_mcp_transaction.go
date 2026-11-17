package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
)

func (s *ConfigStore) mutateMCP(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool) (MCPMutationResult, error) {
	return s.mutateMCPWithMode(operation, scope, oldName, newName, value, disabled, false, SettingsAuthorityGuarded)
}

func (s *ConfigStore) mutateMCPExact(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool) (MCPMutationResult, error) {
	return s.mutateMCPWithMode(operation, scope, oldName, newName, value, disabled, true, SettingsAuthorityGuarded)
}

func (s *ConfigStore) mutatePendingRemoveMCP(scope Scope, name string) (MCPMutationResult, error) {
	return s.mutatePendingRemoveMCPWithAuthority(scope, name, SettingsAuthorityGuarded)
}

func (s *ConfigStore) mutatePendingRemoveMCPWithAuthority(scope Scope, name string, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	if scope != ScopeGlobal {
		return MCPMutationResult{}, fmt.Errorf("pending MCP removal requires global scope: %w", ErrMCPStale)
	}

	path, err := s.configPath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = docread.NormalizeReloadPath(path)
	var result MCPMutationResult
	var audit settingsWriteAudit
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
		writeErr := s.writeMCPFileChanges(files, &audit, authority)
		result.committedFingerprints = committedMCPFingerprints(files)
		after, evalErr := s.evaluateMCPFiles(files)
		if evalErr != nil {
			return evalErr
		}
		result.committedMCPInputs = reconcileMCPInputs(s.loadSnapshot().mcpInputs, after.mcpInputs, result.OldName, result.NewName)
		return writeErr
	})
	if err == nil || mcpCommitWasReconciled(err) {
		result.Generation = s.loadSnapshot().generation + 1
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	auditErr := err
	if mcpCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		finalizeSettingsEvent(audit.event, audit.refusedBy, auditErr)
		emitSettingsEvent(*audit.event)
	}
	if err != nil && !mcpCommitWasReconciled(err) {
		return MCPMutationResult{}, err
	}
	return result, err
}

func (s *ConfigStore) mutateMCPEnableRollback(scope Scope, name string, token MCPMutationResult) (MCPMutationResult, error) {
	return s.mutateMCPEnableRollbackWithAuthority(scope, name, token, SettingsAuthorityGuarded)
}

func (s *ConfigStore) mutateMCPEnableRollbackWithAuthority(scope Scope, name string, token MCPMutationResult, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = docread.NormalizeReloadPath(path)
	var result MCPMutationResult
	var audit settingsWriteAudit
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
		writeErr := s.writeMCPFileChanges(files, &audit, authority)
		after, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		result.NewExists, result.NewConfig, result.NewOrigin = afterValue(after, name)
		result.committedFingerprints = committedMCPFingerprints(files)
		result.committedMCPInputs = reconcileMCPInputs(s.loadSnapshot().mcpInputs, after.mcpInputs, result.OldName, result.NewName)
		return writeErr
	})
	if err == nil || mcpCommitWasReconciled(err) {
		result.Generation = s.loadSnapshot().generation + 1
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	auditErr := err
	if mcpCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		finalizeSettingsEvent(audit.event, audit.refusedBy, auditErr)
		emitSettingsEvent(*audit.event)
	}
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

func mcpEvaluationFingerprint(fingerprints map[string]docread.Fingerprint, path string) (docread.Fingerprint, bool) {
	if fingerprint, ok := fingerprints[path]; ok {
		return fingerprint, true
	}
	discovery := docread.NormalizeDiscoveryPath(path)
	for candidate, fingerprint := range fingerprints {
		if docread.NormalizeDiscoveryPath(candidate) == discovery {
			return fingerprint, true
		}
	}
	return docread.Fingerprint{}, false
}

// mutateMCPWithMode is the shared MCP transaction core; authority carries
// the per-call trusted-UI exception and only widens the settings-lock veto.
func (s *ConfigStore) mutateMCPWithMode(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool, exact bool, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = docread.NormalizeReloadPath(path)
	var result MCPMutationResult
	var audit settingsWriteAudit
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
		writeErr := s.writeMCPFileChanges(files, &audit, authority)
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
		result.committedMCPInputs = reconcileMCPInputs(s.loadSnapshot().mcpInputs, after.mcpInputs, result.OldName, result.NewName)
		return writeErr
	})
	if err == nil || mcpCommitWasReconciled(err) {
		result.Generation = s.loadSnapshot().generation + 1
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	auditErr := err
	if mcpCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		finalizeSettingsEvent(audit.event, audit.refusedBy, auditErr)
		emitSettingsEvent(*audit.event)
	}
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

// writeMCPFileChanges commits every changed transaction record; authority
// carries the per-call trusted-UI exception into the settings-lock guard
// and the audit event's Source label.
func (s *ConfigStore) writeMCPFileChanges(files *mcpLockedFiles, audit *settingsWriteAudit, authority SettingsWriteAuthority) error {
	for key, changed := range files.changed {
		if !changed {
			continue
		}
		record := files.records[key]
		if record == nil || record.selectedPath == "" {
			return fmt.Errorf("MCP transaction record %q has no selected destination", key)
		}
		// record.data is the JSON-shaped transaction document. The physical
		// commit bytes are encoded for the target slot's format: Ktav for a
		// rush.ktav target, byte-identical JSON for everything else. The
		// commit expectation stays the ORIGINAL raw fingerprint.
		physicalData, encodeErr := docread.EncodeSettingsDocument(record.data, docread.SettingsDocumentFormatOf(record.selectedPath))
		if encodeErr != nil {
			return fmt.Errorf("failed to encode config file %s: %w", record.selectedPath, encodeErr)
		}
		if !record.expectation.Exists {
			parentPath := filepath.Dir(record.commitPath)
			if currentParent := docread.ConfigDiscoveryFingerprint(filepath.Dir(record.selectedPath)); currentParent != record.expectation.ParentDiscovery {
				return fmt.Errorf("%w: config parent changed", ErrMCPStale)
			}
			if err := os.MkdirAll(parentPath, 0o755); err != nil {
				return err
			}
			record.expectation.ParentDiscovery = docread.ConfigDiscoveryFingerprint(filepath.Dir(record.selectedPath))
		}
		owner, enforce, err := s.mcpOwnerPolicy(record.selectedPath)
		if err != nil {
			return fmt.Errorf("failed to determine config owner: %w", err)
		}
		keys := []string{mcpContainerName(record.commitPath)}
		ev := &SettingsEvent{Scope: s.settingsScopeForPath(record.commitPath), Path: record.commitPath, Keys: keys, Source: settingsAuditSource(authority)}
		refusedBy, guardErr := s.guardSettingsWriteForAuthority(record.commitPath, record.data, keys, authority)
		if guardErr != nil {
			audit.event = ev
			audit.refusedBy = refusedBy
			return guardErr
		}
		audit.event = ev
		committed, commitErr := cfgcommit.CommitConfigFile(record.selectedPath, record.commitPath, physicalData, 0o600, record.expectation, owner, enforce)
		commitReturnedNil := commitErr == nil
		if commitReturnedNil {
			s.diskWriteGeneration++
		} else if outcome, ok := cfgcommit.CommitOutcomeFromError(commitErr); ok && (outcome.Committed || outcome.MaybeCommitted) {
			s.diskWriteGeneration++
		}
		if commitErr != nil {
			if errors.Is(commitErr, errConfigCommitDurabilityUncertain) {
				// Directory fsync failure is never converted to success. The
				// bytes are reconciled into the transaction so its caller can
				// publish a fresh snapshot, but the durability uncertainty is
				// returned after that publication and must not be retried blindly.
				reconciled, ok := s.reconcileMCPCommit(record, physicalData)
				if !ok {
					return &mcpCommitUncertainError{cause: commitErr}
				}
				committed = reconciled
				if fingerprintErr := s.applyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
					cause := commitErr
					if outcome, outcomeOK := cfgcommit.CommitOutcomeFromError(commitErr); outcomeOK {
						cause = cfgcommit.CloneCommitOutcome(outcome, false)
					}
					return &mcpCommitUncertainError{cause: errors.Join(cause, fingerprintErr)}
				}
				record.expectation = committed
				if outcome, outcomeOK := cfgcommit.CommitOutcomeFromError(commitErr); outcomeOK {
					commitErr = cfgcommit.CloneCommitOutcome(outcome, true)
				}
				return &mcpCommitUncertainError{cause: commitErr}
			}
			if errors.Is(commitErr, errConfigCommitUncertain) || errors.Is(commitErr, errConfigCommitCommitted) {
				// A readback/check-hook failure is recoverable when a fresh
				// read proves that the requested bytes are present. Preserve the
				// public outcome and upgrade its reconciliation status so every
				// caller observes one authoritative status source.
				if outcome, outcomeOK := cfgcommit.CommitOutcomeFromError(commitErr); outcomeOK && outcome.Reconciled &&
					!errors.Is(outcome, errConfigCommitDurabilityUncertain) {
					commitErr = nil
				} else {
					reconciled, ok := s.reconcileMCPCommit(record, physicalData)
					if !ok {
						return &mcpCommitUncertainError{cause: commitErr}
					}
					committed = reconciled
					if fingerprintErr := s.applyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
						cause := commitErr
						if outcome, outcomeOK := cfgcommit.CommitOutcomeFromError(commitErr); outcomeOK {
							cause = cfgcommit.CloneCommitOutcome(outcome, false)
						}
						return &mcpCommitUncertainError{cause: errors.Join(cause, fingerprintErr)}
					}
					record.expectation = committed
					if outcome, outcomeOK := cfgcommit.CommitOutcomeFromError(commitErr); outcomeOK {
						commitErr = cfgcommit.CloneCommitOutcome(outcome, true)
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
			cfgcommit.RunConfigAfterMCPCommitHook(record.commitPath)
		}
		if fingerprintErr := s.applyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
			outcome := cfgcommit.NewCommitOutcome(record.commitPath, true, false,
				errConfigCommitCommitted, errConfigCommitUncertain, fingerprintErr)
			return &mcpCommitUncertainError{cause: outcome}
		}
		record.expectation = committed
	}
	return nil
}

func (s *ConfigStore) reconcileMCPCommit(record *mcpFileRecord, physicalData []byte) (docread.Fingerprint, bool) {
	owner, enforce, err := s.mcpOwnerPolicy(record.selectedPath)
	if err != nil {
		return docread.Fingerprint{}, false
	}
	cfgcommit.RunConfigBeforeMCPReconcileHook(record.selectedPath, physicalData)
	data, fingerprint, err := docread.ReadStableConfigFileOwned(record.selectedPath, owner, enforce)
	if err != nil || !docread.SameBytesFingerprint(data, sha256.Sum256(physicalData)) {
		return docread.Fingerprint{}, false
	}
	return fingerprint, true
}

func (s *ConfigStore) applyCommittedMCPFingerprint(files *mcpLockedFiles, record *mcpFileRecord, committed docread.Fingerprint) error {
	paths := make(map[string]struct{}, len(record.aliases)+2)
	for alias := range record.aliases {
		paths[docread.NormalizeDiscoveryPath(alias)] = struct{}{}
	}
	paths[docread.NormalizeDiscoveryPath(record.selectedPath)] = struct{}{}
	paths[docread.NormalizeDiscoveryPath(record.commitPath)] = struct{}{}
	delete(paths, "")

	for path := range paths {
		expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
		if ownerErr != nil {
			return fmt.Errorf("%w: failed to determine config owner for %s: %w", errConfigCommitUncertain, path, ownerErr)
		}
		_, fingerprint, readErr := docread.ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
		if readErr != nil && !os.IsNotExist(readErr) {
			return fmt.Errorf("%w: failed to reread committed config spelling %s: %w", errConfigCommitUncertain, path, readErr)
		}
		if fingerprint.Exists != committed.Exists || committed.Exists &&
			(fingerprint.Digest != committed.Digest || committed.Identity.Valid && fingerprint.Identity != committed.Identity) {
			return fmt.Errorf("%w: committed config spelling %s does not match the published file", errConfigCommitUncertain, path)
		}
		files.fingerprints[path] = fingerprint
	}
	return nil
}

func (s *ConfigStore) evaluateMCPFiles(files *mcpLockedFiles) (mcpEvaluation, error) {
	paths := s.orderedMCPPaths()
	externalPaths := uniqueMCPPaths(docread.McpJSONCandidatePaths(s.workingDir))
	input := make([][]byte, 0, len(paths))
	fingerprints := make(map[string]docread.Fingerprint, len(paths)+len(externalPaths))
	origins := make(map[string]MCPOrigin)
	rushDocuments := make([]docread.StableConfigDocument, 0, len(paths))
	seenRushRecords := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		data, present, err := s.mcpPathData(files, path)
		if err != nil {
			return mcpEvaluation{}, err
		}
		fingerprint := files.mcpFingerprint(path)
		fingerprints[path] = fingerprint
		rushDocuments = append(rushDocuments, docread.StableConfigDocument{
			Path: path, Data: data, Fingerprint: fingerprint, Present: present,
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
	externalDocuments := make([]docread.StableConfigDocument, 0, len(externalPaths))
	seenExternalRecords := make(map[string]struct{}, len(externalPaths))
	for _, path := range externalPaths {
		data, present, err := s.mcpPathData(files, path)
		if err != nil {
			return mcpEvaluation{}, err
		}
		fingerprint := files.mcpFingerprint(path)
		fingerprints[path] = fingerprint
		document := docread.StableConfigDocument{Path: path, Data: data, Fingerprint: fingerprint, Present: present}
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
		external, err := docread.LoadMCPJSONBytes(data)
		if err != nil {
			return mcpEvaluation{}, err
		}
		for name, ext := range external {
			if _, defined := rushDefined[name]; defined {
				continue
			}
			ext.Disabled = externalOverlayValue(rushDocuments, name, ext.Disabled)
			cfg.MCP[name] = ext
			origin := MCPOrigin{Kind: MCPOriginExternal, Path: docread.NormalizeReloadPath(path), Scope: ScopeGlobal, Writable: false}
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
		if eligible := docread.EligibleConfigCandidate(path, owner); eligible != "" {
			paths = append(paths, eligible)
		}
	}
	if systemPath := s.systemConfigPathValue(); systemPath != "" {
		appendSafe(systemPath, systemConfigOwner())
	}
	// One logical slot is one document: the selected spelling, exactly as
	// docread.LookupConfigCandidates and the loader resolve it. The fixed legacy
	// spelling would add the rush.json kept beside a migrated rush.ktav as a
	// second document and make every input fingerprint differ from the
	// loaded snapshot's.
	appendSafe(ActiveConfigFileName(GlobalConfig()), homeConfigOwner())
	appendSafe(s.globalDataPath, homeConfigOwner())
	// docread.LookupConfigCandidates applies the same global/project owner policy;
	// retain its discovery spelling so mcpPathData can bind the alias to the
	// opened file identity.
	paths = append(paths, docread.LookupConfigCandidates(s.workingDir)...)
	if workspacePath := s.configPathOrEmpty(ScopeWorkspace); workspacePath != "" {
		if owner, enforce, err := docread.ConfigOwnerForWorkingDir(s.workingDir); err == nil && (!enforce || docread.EligibleConfigCandidate(workspacePath, owner) != "") {
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
	key := docread.NormalizeDiscoveryPath(path)
	if record := files.mcpRecord(key); record != nil {
		fingerprint, ok := files.fingerprints[key]
		if !ok {
			fingerprint = docread.RebindReloadFingerprintPath(path, record.expectation)
			files.fingerprints[key] = fingerprint
		}
		files.data[key] = record.data
		files.present[key] = fingerprint.Exists
		return record.data, fingerprint.Exists, nil
	}
	expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
	if ownerErr != nil {
		return nil, false, ownerErr
	}
	data, fingerprint, err := docread.ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
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
	// Rush documents may be Ktav on disk: decode to JSON-shaped bytes for
	// the semantic work while the fingerprint stays bound to the RAW bytes.
	// A decode failure is an error, never "no servers".
	data, decodeErr := docread.DecodeConfigDocument(path, data)
	if decodeErr != nil {
		return nil, false, decodeErr
	}
	record := files.bindMCPPath(path, data, fingerprint.Exists, fingerprint)
	return record.data, fingerprint.Exists, nil
}

func (s *ConfigStore) mcpOwnerPolicy(path string) (int, bool, error) {
	if strings.EqualFold(filepath.Base(path), ".mcp.json") {
		if strings.EqualFold(
			docread.NormalizeDiscoveryPath(path),
			docread.NormalizeDiscoveryPath(filepath.Join(home.Dir(), ".claude", ".mcp.json")),
		) {
			return homeConfigOwner(), true, nil
		}
		s.workingDirOwnerOnce.Do(func() {
			var enforce bool
			s.workingDirOwner, enforce, s.workingDirOwnerErr = docread.ConfigOwnerForWorkingDir(s.workingDir)
			_ = enforce
		})
		if s.workingDirOwnerErr != nil {
			return 0, false, s.workingDirOwnerErr
		}
		return s.workingDirOwner, s.workingDir != "", nil
	}
	canonical := docread.NormalizeReloadPath(path)
	if systemPath := s.systemConfigPathValue(); systemPath != "" && canonical == docread.NormalizeReloadPath(systemPath) {
		return systemConfigOwner(), true, nil
	}
	if docread.MatchesLogicalSlot(canonical, GlobalConfig()) || docread.MatchesLogicalSlot(canonical, s.globalDataPath) {
		return homeConfigOwner(), true, nil
	}
	if canonical == docread.NormalizeReloadPath(filepath.Join(home.Dir(), ".claude", ".mcp.json")) {
		return homeConfigOwner(), true, nil
	}
	s.workingDirOwnerOnce.Do(func() {
		var enforce bool
		s.workingDirOwner, enforce, s.workingDirOwnerErr = docread.ConfigOwnerForWorkingDir(s.workingDir)
		_ = enforce
	})
	if s.workingDirOwnerErr != nil {
		return 0, false, s.workingDirOwnerErr
	}
	return s.workingDirOwner, s.workingDir != "", nil
}

func entryOrigins(origins map[string]MCPOrigin, path, workspacePath, globalPath, systemPath string, data []byte) {
	path = docread.NormalizeReloadPath(path)
	workspacePath = docread.NormalizeReloadPath(workspacePath)
	globalPath = docread.NormalizeReloadPath(globalPath)
	systemPath = docread.NormalizeReloadPath(systemPath)
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if json.Unmarshal(data, &root) != nil {
		return
	}
	for name, raw := range root.MCP {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || docread.IsMCPDisabledOnlyEntry(entry, true) {
			continue
		}
		origin := MCPOrigin{Kind: MCPOriginProject, Path: path, Scope: ScopeGlobal, Writable: false}
		// docread.MatchesLogicalSlot treats the legacy JSON spelling and its Ktav
		// sibling as one logical slot: a mid-session migration renames
		// rush.json to rush.ktav while globalDataPath still anchors the
		// legacy name, and the slot must stay writable through either
		// spelling.
		switch {
		case docread.MatchesLogicalSlot(path, workspacePath):
			origin.Kind, origin.Scope, origin.Writable = MCPOriginWorkspace, ScopeWorkspace, true
		case docread.MatchesLogicalSlot(path, globalPath):
			origin.Kind, origin.Scope, origin.Writable = MCPOriginGlobal, ScopeGlobal, true
		case docread.MatchesLogicalSlot(path, systemPath):
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

func externalOverlayValue(documents []docread.StableConfigDocument, name string, fallback bool) bool {
	for _, document := range documents {
		if !document.Present {
			continue
		}
		entry, ok := docread.McpEntryFromJSON(document.Data, name)
		if !docread.IsMCPDisabledOnlyEntry(entry, ok) {
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

func (s *ConfigStore) verifyMCPReadOnlyInputs(expected map[string]docread.Fingerprint, selected string) error {
	for path, fingerprint := range expected {
		if docread.NormalizeReloadPath(path) == docread.NormalizeReloadPath(selected) {
			continue
		}
		expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
		if ownerErr != nil {
			return fmt.Errorf("%w: %s", ErrMCPStale, path)
		}
		_, actual, err := docread.ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
		missing := os.IsNotExist(err)
		if missing {
			// A missing candidate still carries its discovery-chain fingerprint.
			err = nil
		}
		if missing && configEquivalentMissingMCPInput(path, selected, fingerprint, actual) {
			continue
		}
		if err != nil || actual != fingerprint {
			return fmt.Errorf("%w: %s", ErrMCPStale, path)
		}
	}
	return nil
}

func configEquivalentMissingMCPInput(path, selected string, expected, actual docread.Fingerprint) bool {
	if expected.Exists || actual.Exists || !expected.ParentIdentity.Valid ||
		expected.ParentIdentity != actual.ParentIdentity ||
		expected.ParentDiscovery != actual.ParentDiscovery {
		return false
	}
	if filepath.Dir(path) != filepath.Dir(selected) ||
		!cfgcommit.ConfigEquivalentLeafNames(filepath.Dir(path), filepath.Base(path), filepath.Base(selected)) {
		return false
	}
	return true
}

func reconcileMCPInputs(current, committed map[string][32]byte, names ...string) map[string][32]byte {
	result := maps.Clone(current)
	if result == nil {
		result = make(map[string][32]byte)
	}
	for _, name := range names {
		if input, exists := committed[name]; exists {
			result[name] = input
		} else {
			delete(result, name)
		}
	}
	return result
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
		for path, fallback := range result.committedFingerprints {
			for key, snapshot := range next.snapshots {
				if docread.NormalizeReloadPath(key) != docread.NormalizeReloadPath(path) {
					continue
				}
				fingerprint := fallback
				if current, err := docread.ReadReloadFingerprint(key); err == nil {
					fingerprint = current
				}
				updateMCPCommittedSnapshot(&snapshot, key, fingerprint)
				next.snapshots[key] = snapshot
			}
		}
	}
	s.publishLocked(next)
}

func updateMCPCommittedSnapshot(snapshot *fileSnapshot, key string, fingerprint docread.Fingerprint) {
	*snapshot = docread.NewFileSnapshot(key, fingerprint)
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
		identity := docread.NormalizeDiscoveryPath(path)
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		result = append(result, path)
	}
	return result
}

func dataFingerprint(path string, data []byte) docread.Fingerprint {
	digest := sha256.Sum256(data)
	if fingerprint, err := docread.ReadReloadFingerprint(path); err == nil && fingerprint.Exists && fingerprint.Digest == digest {
		return fingerprint
	}
	return docread.Fingerprint{Exists: true, Size: int64(len(data)), Digest: digest, Nlink: 1}
}
