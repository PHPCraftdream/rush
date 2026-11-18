package mcpstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	cfgmerge "github.com/PHPCraftdream/rush/internal/config/model/merge"
)

// mcpInputFingerprints is a byte-identical local copy of the root helper
// (internal/config/fingerprints_policy.go:48) needed to fingerprint MCP
// inputs during transaction evaluation; the root keeps the original for the
// load/reload path.
func mcpInputFingerprints(rush, external []stableConfigDocument) map[string][sha256.Size]byte {
	type document struct {
		path      string
		entries   map[string]json.RawMessage
		rawDigest []byte
	}
	documentsByPath := make(map[string]document, len(rush)+len(external))
	names := make(map[string]struct{})
	addDocument := func(sourceDocument stableConfigDocument, entries map[string]json.RawMessage, rawDigest []byte) {
		path := docread.NormalizeReloadPath(sourceDocument.Path)
		if path == "" {
			path = docread.NormalizeDiscoveryPath(sourceDocument.Path)
		}
		if existing, ok := documentsByPath[path]; ok {
			if len(existing.entries) == 0 && len(entries) != 0 {
				documentsByPath[path] = document{path: path, entries: entries, rawDigest: rawDigest}
			}
			return
		}
		documentsByPath[path] = document{path: path, entries: entries, rawDigest: rawDigest}
	}
	for _, sourceDocument := range rush {
		var rawDigest []byte
		if docread.IsKtavConfigPath(sourceDocument.Path) && sourceDocument.Fingerprint.Exists {
			rawDigest = append([]byte(nil), sourceDocument.Fingerprint.Digest[:]...)
		}
		var root struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(sourceDocument.Data, &root) == nil {
			addDocument(sourceDocument, root.MCP, rawDigest)
			for name := range root.MCP {
				names[name] = struct{}{}
			}
		}
	}
	for _, sourceDocument := range external {
		var root struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(sourceDocument.Data, &root) == nil {
			addDocument(sourceDocument, root.MCPServers, nil)
			for name := range root.MCPServers {
				names[name] = struct{}{}
			}
		}
	}
	documents := make([]document, 0, len(documentsByPath))
	for _, document := range documentsByPath {
		documents = append(documents, document)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].path < documents[j].path })
	result := make(map[string][sha256.Size]byte, len(names))
	for name := range names {
		hash := sha256.New()
		for _, document := range documents {
			path := docread.NormalizeReloadPath(document.path)
			if path == "" {
				path = docread.NormalizeDiscoveryPath(document.path)
			}
			hash.Write([]byte(path))
			if raw, ok := document.entries[name]; ok {
				if len(document.rawDigest) > 0 {
					hash.Write(document.rawDigest)
				}
				hash.Write([]byte{1})
				hash.Write(raw)
			} else {
				hash.Write([]byte{0})
			}
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		result[name] = digest
	}
	return result
}

// Sentinel error identity closures (same pattern as the root
// io_forwarders.go): the io packages expose getters and the component binds
// them once.
var (
	errConfigCommitCommitted           = cfgcommit.ErrConfigCommitCommitted()
	errConfigCommitUncertain           = cfgcommit.ErrConfigCommitUncertain()
	errConfigCommitDurabilityUncertain = cfgcommit.ErrConfigCommitDurabilityUncertain()
	errConfigCommitVerification        = fileid.ErrConfigCommitVerification()
	errConfigOwnerMismatch             = fileid.ErrConfigOwnerMismatch()
)

func (s *Store) mutateMCP(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool) (MCPMutationResult, error) {
	return s.mutateMCPWithMode(operation, scope, oldName, newName, value, disabled, false, SettingsAuthorityGuarded)
}

func (s *Store) mutateMCPExact(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool) (MCPMutationResult, error) {
	return s.mutateMCPWithMode(operation, scope, oldName, newName, value, disabled, true, SettingsAuthorityGuarded)
}

func (s *Store) mutatePendingRemoveMCP(scope Scope, name string) (MCPMutationResult, error) {
	return s.mutatePendingRemoveMCPWithAuthority(scope, name, SettingsAuthorityGuarded)
}

func (s *Store) mutatePendingRemoveMCPWithAuthority(scope Scope, name string, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	if scope != ScopeGlobal {
		return MCPMutationResult{}, fmt.Errorf("pending MCP removal requires global scope: %w", ErrMCPStale)
	}

	path, err := s.port.ScopePath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = docread.NormalizeReloadPath(path)
	var result MCPMutationResult
	var audit settingsWriteAudit
	err = s.port.PublishMutation(func(pub MutationScope) error {
		lockErr := s.withMCPWriteLocks(func(files *LockedFiles) error {
			before, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return err
			}
			if err := files.validateMutableTopology(); err != nil {
				return err
			}
			if _, exists := before.Configs[name]; exists {
				return fmt.Errorf("%w: %q", ErrMCPTargetExists, name)
			}
			result = MCPMutationResult{
				Operation: "remove", OldName: name, NewName: name,
				OldExists: false, OldConfig: MCPConfig{}, OldOrigin: before.Origins[name],
			}
			// Write the final absent state in one atomic operation. This may create
			// an otherwise empty config file, but it never writes the pending
			// definition, including if the process stops immediately afterward.
			if err := s.PrepareMCPFileMutation(files, path, "remove", name, name, MCPConfig{}, nil); err != nil {
				return err
			}
			if err := s.verifyMCPReadOnlyInputs(before.Fingerprints, path); err != nil {
				return err
			}
			result.committedFingerprints = committedMCPFingerprints(files)
			writeErr := s.writeMCPFileChanges(files, &audit, authority)
			result.committedFingerprints = committedMCPFingerprints(files)
			after, evalErr := s.EvaluateMCPFiles(files)
			if evalErr != nil {
				return evalErr
			}
			result.committedMCPInputs = reconcileMCPInputs(s.currentMCPInputs(), after.MCPInputs, result.OldName, result.NewName)
			return writeErr
		})
		if lockErr == nil || MCPCommitWasReconciled(lockErr) {
			result.Generation = s.currentGeneration() + 1
			s.publishMCPMutationLocked(pub, result)
		}
		return lockErr
	})
	auditErr := err
	if MCPCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		settingsFinalizeEvent(audit.event, audit.refusedBy, auditErr)
		settingsEmitEvent(*audit.event)
	}
	if err != nil && !MCPCommitWasReconciled(err) {
		return MCPMutationResult{}, err
	}
	return result, err
}

func (s *Store) mutateMCPEnableRollback(scope Scope, name string, token MCPMutationResult) (MCPMutationResult, error) {
	return s.mutateMCPEnableRollbackWithAuthority(scope, name, token, SettingsAuthorityGuarded)
}

func (s *Store) mutateMCPEnableRollbackWithAuthority(scope Scope, name string, token MCPMutationResult, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = docread.NormalizeReloadPath(path)
	var result MCPMutationResult
	var audit settingsWriteAudit
	err = s.port.PublishMutation(func(pub MutationScope) error {
		lockErr := s.withMCPWriteLocks(func(files *LockedFiles) error {
			before, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return err
			}
			if !s.mcpEnableRollbackTokenMatches(before, scope, name, token) {
				return ErrMCPMutationStale
			}
			current := before.Configs[name]
			origin := before.Origins[name]
			result = MCPMutationResult{
				Operation: "disable", OldName: name, NewName: name,
				OldExists: true, OldConfig: cloneMCPConfig(current), OldOrigin: origin,
			}
			if err := validateMCPMutation("disable", scope, name, name, true, origin, before.Configs); err != nil {
				return err
			}
			disabled := true
			if err := s.PrepareMCPFileMutation(files, path, "disable", name, name, MCPConfig{}, &disabled); err != nil {
				return err
			}
			if err := s.verifyMCPReadOnlyInputs(before.Fingerprints, path); err != nil {
				return err
			}
			writeErr := s.writeMCPFileChanges(files, &audit, authority)
			after, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return err
			}
			result.NewExists, result.NewConfig, result.NewOrigin = afterValue(after, name)
			result.committedFingerprints = committedMCPFingerprints(files)
			result.committedMCPInputs = reconcileMCPInputs(s.currentMCPInputs(), after.MCPInputs, result.OldName, result.NewName)
			return writeErr
		})
		if lockErr == nil || MCPCommitWasReconciled(lockErr) {
			result.Generation = s.currentGeneration() + 1
			s.publishMCPMutationLocked(pub, result)
		}
		return lockErr
	})
	auditErr := err
	if MCPCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		settingsFinalizeEvent(audit.event, audit.refusedBy, auditErr)
		settingsEmitEvent(*audit.event)
	}
	if err != nil && !MCPCommitWasReconciled(err) {
		return MCPMutationResult{}, err
	}
	return result, err
}

func (s *Store) mcpEnableRollbackTokenMatches(before Evaluation, scope Scope, name string, token MCPMutationResult) bool {
	if token.NewName != name || !token.NewExists || token.Operation != "add" && token.Operation != "disable" {
		return false
	}
	if token.Generation != 0 && s.currentGeneration() != token.Generation {
		return false
	}
	current, exists := before.Configs[name]
	if !exists || !reflect.DeepEqual(current, token.NewConfig) || before.Origins[name] != token.NewOrigin {
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
		actual, ok := mcpEvaluationFingerprint(before.Fingerprints, expectedPath)
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
func (s *Store) mutateMCPWithMode(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool, exact bool, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return MCPMutationResult{}, err
	}
	path = docread.NormalizeReloadPath(path)
	var result MCPMutationResult
	var audit settingsWriteAudit
	err = s.port.PublishMutation(func(pub MutationScope) error {
		lockErr := s.withMCPWriteLocks(func(files *LockedFiles) error {
			before, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return err
			}
			if err := files.validateMutableTopology(); err != nil {
				return err
			}
			old, oldOK := before.Configs[oldName]
			oldOrigin := before.Origins[oldName]
			result = MCPMutationResult{
				Operation: operation, OldName: oldName, NewName: newName,
				OldExists: oldOK, OldConfig: cloneMCPConfig(old), OldOrigin: oldOrigin,
			}
			if err := validateMCPMutation(operation, scope, oldName, newName, oldOK, oldOrigin, before.Configs); err != nil && !exact {
				// A zero-working-directory test store has no discoverable project
				// pipeline. Its in-memory config is the only origin available.
				if s.port.WorkingDir() != "" || operation == "add" {
					return err
				}
			}
			if exact {
				literalExists := literalMCPEntryExists(files.MCPData(path), oldName)
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
						return validateMCPMutation(operation, scope, oldName, newName, oldOK, oldOrigin, before.Configs)
					}
				}
			}
			if err := s.PrepareMCPFileMutation(files, path, operation, oldName, newName, value, disabled); err != nil {
				return err
			}
			if err := s.verifyMCPReadOnlyInputs(before.Fingerprints, path); err != nil {
				return err
			}
			writeErr := s.writeMCPFileChanges(files, &audit, authority)
			after, err := s.EvaluateMCPFiles(files)
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
			result.committedMCPInputs = reconcileMCPInputs(s.currentMCPInputs(), after.MCPInputs, result.OldName, result.NewName)
			return writeErr
		})
		if lockErr == nil || MCPCommitWasReconciled(lockErr) {
			result.Generation = s.currentGeneration() + 1
			s.publishMCPMutationLocked(pub, result)
		}
		return lockErr
	})
	auditErr := err
	if MCPCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		settingsFinalizeEvent(audit.event, audit.refusedBy, auditErr)
		settingsEmitEvent(*audit.event)
	}
	if err != nil && !MCPCommitWasReconciled(err) {
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

// PrepareMCPFileMutation is the exported form used by the root white-box
// test forwarder (prepareMCPFileMutation).
func (s *Store) PrepareMCPFileMutation(files *LockedFiles, path, operation, oldName, newName string, value MCPConfig, disabled *bool) error {
	data, err := editMCPDocumentWithKind(files.MCPData(path), mcpDocumentRush, operation, oldName, newName, value, disabled, nil)
	if err != nil {
		return err
	}
	return files.SetMCPData(path, data)
}

func (s *Store) EvaluateMCPFiles(files *LockedFiles) (Evaluation, error) {
	paths := s.orderedMCPPaths()
	externalPaths := uniqueMCPPaths(docread.McpJSONCandidatePaths(s.port.WorkingDir()))
	input := make([][]byte, 0, len(paths))
	fingerprints := make(map[string]docread.Fingerprint, len(paths)+len(externalPaths))
	origins := make(map[string]MCPOrigin)
	rushDocuments := make([]docread.StableConfigDocument, 0, len(paths))
	seenRushRecords := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		data, present, err := s.MCPPathData(files, path)
		if err != nil {
			return Evaluation{}, err
		}
		fingerprint := files.MCPFingerprint(path)
		fingerprints[path] = fingerprint
		rushDocuments = append(rushDocuments, docread.StableConfigDocument{
			Path: path, Data: data, Fingerprint: fingerprint, Present: present,
		})
		if present {
			record := files.MCPRecord(path)
			duplicate := record != nil
			if duplicate {
				_, duplicate = seenRushRecords[record.Key]
				seenRushRecords[record.Key] = struct{}{}
			}
			if len(data) > 0 {
				if !json.Valid(data) {
					return Evaluation{}, fmt.Errorf("invalid JSON in config file %s", path)
				}
				if err := cfgmerge.ValidateMCPDisabledOverlays(data); err != nil {
					return Evaluation{}, fmt.Errorf("invalid MCP configuration in config file %s: %w", path, err)
				}
				if !duplicate {
					input = append(input, data)
				}
				EntryOrigins(origins, path, s.workspacePathValue(), s.port.GlobalDataPath(), s.systemConfigPathValue(), data)
			}
		}
	}
	cfg, err := cfgmerge.LoadFromBytes(input)
	if err != nil {
		return Evaluation{}, err
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
		data, present, err := s.MCPPathData(files, path)
		if err != nil {
			return Evaluation{}, err
		}
		fingerprint := files.MCPFingerprint(path)
		fingerprints[path] = fingerprint
		document := docread.StableConfigDocument{Path: path, Data: data, Fingerprint: fingerprint, Present: present}
		externalDocuments = append(externalDocuments, document)
		if !present {
			continue
		}
		if record := files.MCPRecord(path); record != nil {
			if _, seen := seenExternalRecords[record.Key]; seen {
				continue
			}
			seenExternalRecords[record.Key] = struct{}{}
		}
		external, err := docread.LoadMCPJSONBytes(data)
		if err != nil {
			return Evaluation{}, err
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
	return Evaluation{
		Configs: cfg.MCP, Origins: origins, Fingerprints: fingerprints,
		MCPInputs: mcpInputFingerprints(rushDocuments, externalDocuments),
	}, nil
}

// OrderedMCPPaths is the exported form used by the root white-box test
// forwarders (orderedMCPPaths).
func (s *Store) OrderedMCPPaths() []string { return s.orderedMCPPaths() }

func (s *Store) orderedMCPPaths() []string {
	paths := make([]string, 0, 8)
	appendSafe := func(path string, owner int) {
		if eligible := docread.EligibleConfigCandidate(path, owner); eligible != "" {
			paths = append(paths, eligible)
		}
	}
	if systemPath := s.systemConfigPathValue(); systemPath != "" {
		appendSafe(systemPath, fileid.SystemConfigOwner())
	}
	// One logical slot is one document: the selected spelling, exactly as
	// docread.LookupConfigCandidates and the loader resolve it. The fixed legacy
	// spelling would add the rush.json kept beside a migrated rush.ktav as a
	// second document and make every input fingerprint differ from the loaded snapshot's.
	appendSafe(slots.ActiveConfigFileName(datadir.GlobalConfig()), fileid.HomeConfigOwner())
	appendSafe(s.port.GlobalDataPath(), fileid.HomeConfigOwner())
	// docread.LookupConfigCandidates applies the same global/project owner policy;
	// retain its discovery spelling so mcpPathData can bind the alias to the
	// opened file identity.
	paths = append(paths, docread.LookupConfigCandidates(s.port.WorkingDir())...)
	workspacePath, _ := s.port.ScopePath(ScopeWorkspace)
	if workspacePath != "" {
		if owner, enforce, err := docread.ConfigOwnerForWorkingDir(s.port.WorkingDir()); err == nil && (!enforce || docread.EligibleConfigCandidate(workspacePath, owner) != "") {
			paths = append(paths, workspacePath)
		}
	}
	return uniqueMCPPaths(paths)
}

// MCPPathData is the exported form used by the root white-box test
// forwarder (mcpPathData).
func (s *Store) MCPPathData(files *LockedFiles, path string) ([]byte, bool, error) {
	key := docread.NormalizeDiscoveryPath(path)
	if record := files.MCPRecord(key); record != nil {
		fingerprint, ok := files.Fingerprints[key]
		if !ok {
			fingerprint = docread.RebindReloadFingerprintPath(path, record.Expectation)
			files.Fingerprints[key] = fingerprint
		}
		files.Data[key] = record.Data
		files.Present[key] = fingerprint.Exists
		return record.Data, fingerprint.Exists, nil
	}
	expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
	if ownerErr != nil {
		return nil, false, ownerErr
	}
	data, fingerprint, err := docread.ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
	if err != nil {
		if os.IsNotExist(err) {
			files.BindMCPPath(path, nil, false, fingerprint)
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
	record := files.BindMCPPath(path, data, fingerprint.Exists, fingerprint)
	return record.Data, fingerprint.Exists, nil
}

// EntryOrigins is the exported form used by the root white-box test
// forwarder (entryOrigins).
func EntryOrigins(origins map[string]MCPOrigin, path, workspacePath, globalPath, systemPath string, data []byte) {
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

func afterValue(eval Evaluation, name string) (bool, MCPConfig, MCPOrigin) {
	cfg, ok := eval.Configs[name]
	return ok, cloneMCPConfig(cfg), eval.Origins[name]
}

func (s *Store) verifyMCPReadOnlyInputs(expected map[string]docread.Fingerprint, selected string) error {
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

func (s *Store) publishMCPMutationLocked(scope MutationScope, result MCPMutationResult) {
	cur := scope.Current()
	if cur.Config() == nil {
		return
	}
	next := mutationNext()
	cfg := *cur.Config()
	cfg.MCP = maps.Clone(cur.Config().MCP)
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
	next.Config = &cfg
	if result.committedMCPInputs != nil {
		next.MCPInputs = maps.Clone(result.committedMCPInputs)
	}
	next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), cur.Config(), next.Config, cur.MCPInputs(), next.MCPInputs)
	next.ResolverDynamic = configHasDynamicMCPResolution(next.Config)
	if len(result.committedFingerprints) > 0 {
		if cur.Snapshots() == nil {
			next.Snapshots = make(map[string]fileSnapshot)
		} else {
			next.Snapshots = maps.Clone(cur.Snapshots())
		}
		for path, fallback := range result.committedFingerprints {
			for key, snapshot := range next.Snapshots {
				if docread.NormalizeReloadPath(key) != docread.NormalizeReloadPath(path) {
					continue
				}
				fingerprint := fallback
				if current, err := docread.ReadReloadFingerprint(key); err == nil {
					fingerprint = current
				}
				updateMCPCommittedSnapshot(&snapshot, key, fingerprint)
				next.Snapshots[key] = snapshot
			}
		}
	}
	scope.Publish(next)
}

func updateMCPCommittedSnapshot(snapshot *fileSnapshot, key string, fingerprint docread.Fingerprint) {
	*snapshot = docread.NewFileSnapshot(key, fingerprint)
}

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

// DataFingerprint is the exported form used by the root write path
// (store_write.go dataFingerprint forwarder).
func DataFingerprint(path string, data []byte) docread.Fingerprint {
	if fingerprint, err := docread.ReadReloadFingerprint(path); err == nil && fingerprint.Exists && fingerprint.Digest == sha256.Sum256(data) {
		return fingerprint
	}
	return docread.Fingerprint{Exists: true, Size: int64(len(data)), Digest: sha256.Sum256(data), Nlink: 1}
}
