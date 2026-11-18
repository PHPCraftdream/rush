package mcpstore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/store/settings"
)

// The commit half of an MCP transaction: per-record settings guard, atomic
// commit, uncertainty classification, and post-commit fingerprint
// reconciliation (split out of store_mcp_transaction.go in X2 to keep every
// file under 900 lines).

// writeMCPFileChanges commits every changed transaction record; authority
// carries the per-call trusted-UI exception into the settings-lock guard and the audit event's Source label.
func (s *Store) writeMCPFileChanges(files *LockedFiles, audit *settingsWriteAudit, authority SettingsWriteAuthority) error {
	for key, changed := range files.Changed {
		if !changed {
			continue
		}
		record := files.Records[key]
		if record == nil || record.SelectedPath == "" {
			return fmt.Errorf("MCP transaction record %q has no selected destination", key)
		}
		// record.Data is the JSON-shaped transaction document. The physical
		// commit bytes are encoded for the target slot's format: Ktav for a
		// rush.ktav target, byte-identical JSON for everything else. The
		// commit expectation stays the ORIGINAL raw fingerprint.
		physicalData, encodeErr := docread.EncodeSettingsDocument(record.Data, docread.SettingsDocumentFormatOf(record.SelectedPath))
		if encodeErr != nil {
			return fmt.Errorf("failed to encode config file %s: %w", record.SelectedPath, encodeErr)
		}
		if !record.Expectation.Exists {
			parentPath := filepath.Dir(record.CommitPath)
			if currentParent := docread.ConfigDiscoveryFingerprint(filepath.Dir(record.SelectedPath)); currentParent != record.Expectation.ParentDiscovery {
				return fmt.Errorf("%w: config parent changed", ErrMCPStale)
			}
			if err := os.MkdirAll(parentPath, 0o755); err != nil {
				return err
			}
			record.Expectation.ParentDiscovery = docread.ConfigDiscoveryFingerprint(filepath.Dir(record.SelectedPath))
		}
		owner, enforce, err := s.mcpOwnerPolicy(record.SelectedPath)
		if err != nil {
			return fmt.Errorf("failed to determine config owner: %w", err)
		}
		keys := []string{MCPContainerName(record.CommitPath)}
		ev := &settings.SettingsEvent{Scope: s.port.SettingsGuard().ScopeForPath(record.CommitPath), Path: record.CommitPath, Keys: keys, Source: settings.SettingsAuditSource(authority)}
		refusedBy, guardErr := s.port.SettingsGuard().GuardWrite(record.CommitPath, record.Data, keys, authority)
		if guardErr != nil {
			audit.event = ev
			audit.refusedBy = refusedBy
			return guardErr
		}
		audit.event = ev
		committed, commitErr := cfgcommit.CommitConfigFile(record.SelectedPath, record.CommitPath, physicalData, 0o600, record.Expectation, owner, enforce)
		commitReturnedNil := commitErr == nil
		if commitReturnedNil {
			s.port.CommitDiskWrite()
		} else if outcome, ok := cfgcommit.CommitOutcomeFromError(commitErr); ok && (outcome.Committed || outcome.MaybeCommitted) {
			s.port.CommitDiskWrite()
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
				if fingerprintErr := s.ApplyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
					cause := commitErr
					if outcome, outcomeOK := cfgcommit.CommitOutcomeFromError(commitErr); outcomeOK {
						cause = cfgcommit.CloneCommitOutcome(outcome, false)
					}
					return &mcpCommitUncertainError{cause: errors.Join(cause, fingerprintErr)}
				}
				record.Expectation = committed
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
					if fingerprintErr := s.ApplyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
						cause := commitErr
						if outcome, outcomeOK := cfgcommit.CommitOutcomeFromError(commitErr); outcomeOK {
							cause = cfgcommit.CloneCommitOutcome(outcome, false)
						}
						return &mcpCommitUncertainError{cause: errors.Join(cause, fingerprintErr)}
					}
					record.Expectation = committed
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
			cfgcommit.RunConfigAfterMCPCommitHook(record.CommitPath)
		}
		if fingerprintErr := s.ApplyCommittedMCPFingerprint(files, record, committed); fingerprintErr != nil {
			outcome := cfgcommit.NewCommitOutcome(record.CommitPath, true, false,
				errConfigCommitCommitted, errConfigCommitUncertain, fingerprintErr)
			return &mcpCommitUncertainError{cause: outcome}
		}
		record.Expectation = committed
	}
	return nil
}

func (s *Store) reconcileMCPCommit(record *FileRecord, physicalData []byte) (docread.Fingerprint, bool) {
	owner, enforce, err := s.mcpOwnerPolicy(record.SelectedPath)
	if err != nil {
		return docread.Fingerprint{}, false
	}
	cfgcommit.RunConfigBeforeMCPReconcileHook(record.SelectedPath, physicalData)
	data, fingerprint, err := docread.ReadStableConfigFileOwned(record.SelectedPath, owner, enforce)
	if err != nil || !docread.SameBytesFingerprint(data, sha256.Sum256(physicalData)) {
		return docread.Fingerprint{}, false
	}
	return fingerprint, true
}

// ApplyCommittedMCPFingerprint is the exported form used by the root
// white-box test forwarder (applyCommittedMCPFingerprint).
func (s *Store) ApplyCommittedMCPFingerprint(files *LockedFiles, record *FileRecord, committed docread.Fingerprint) error {
	paths := make(map[string]struct{}, len(record.Aliases)+2)
	for alias := range record.Aliases {
		paths[docread.NormalizeDiscoveryPath(alias)] = struct{}{}
	}
	paths[docread.NormalizeDiscoveryPath(record.SelectedPath)] = struct{}{}
	paths[docread.NormalizeDiscoveryPath(record.CommitPath)] = struct{}{}
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
		files.Fingerprints[path] = fingerprint
	}
	return nil
}
