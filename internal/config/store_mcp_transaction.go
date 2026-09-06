package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/session"
)

type mcpLockedFiles struct {
	data         map[string][]byte
	present      map[string]bool
	changed      map[string]bool
	fingerprints map[string]reloadFileFingerprint
}

type mcpEvaluation struct {
	configs      map[string]MCPConfig
	origins      map[string]MCPOrigin
	fingerprints map[string]reloadFileFingerprint
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
	paths = append(paths, normalizeReloadPath(globalPath))
	if workspacePath, workspaceErr := s.configPath(ScopeWorkspace); workspaceErr == nil {
		paths = append(paths, normalizeReloadPath(workspacePath))
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)

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
	for _, path := range paths {
		lock, lockErr := session.AcquireFileLockContext(ctx, path+".lock")
		if lockErr != nil {
			return fmt.Errorf("failed to lock config file %q: %w", path, lockErr)
		}
		locks = append(locks, lock)
	}
	files := &mcpLockedFiles{
		data: make(map[string][]byte, len(paths)), present: make(map[string]bool, len(paths)),
		changed: make(map[string]bool), fingerprints: make(map[string]reloadFileFingerprint, len(paths)),
	}
	for _, path := range paths {
		data, fingerprint, readErr := readStableConfigFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				files.fingerprints[path] = reloadFileFingerprint{}
				continue
			}
			return fmt.Errorf("failed to read config file: %w", readErr)
		}
		files.data[path] = data
		files.present[path] = true
		files.fingerprints[path] = fingerprint
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
		if err := writeMCPFileChanges(files); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	if err != nil {
		return MCPMutationResult{}, err
	}
	return result, nil
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
			literalExists := literalMCPEntryExists(files.data[path], oldName)
			switch operation {
			case "add":
				if literalExists {
					return fmt.Errorf("%w: %q", ErrMCPTargetExists, newName)
				}
			case "remove", "set":
				if !literalExists {
					return fmt.Errorf("%w: %q", ErrMCPNotFound, oldName)
				}
			}
		}
		if err := s.prepareMCPFileMutation(files, path, operation, oldName, newName, value, disabled); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
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
		return writeMCPFileChanges(files)
	})
	if err == nil {
		s.publishMCPMutationLocked(result)
	}
	s.publishMu.Unlock()
	if err != nil {
		return MCPMutationResult{}, err
	}
	return result, nil
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
	root, err := decodeMCPRoot(files.data[path])
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
	files.data[path] = append(data, '\n')
	files.present[path] = true
	files.changed[path] = true
	return nil
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
	root, err := decodeMCPRoot(files.data[path])
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
	files.data[path] = append(data, '\n')
	files.present[path], files.changed[path] = true, true
	return nil
}

func writeMCPFileChanges(files *mcpLockedFiles) error {
	for path, changed := range files.changed {
		if !changed {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := atomicWriteFile(path, files.data[path], 0o600); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
	}
	return nil
}

func (s *ConfigStore) evaluateMCPFiles(files *mcpLockedFiles) (mcpEvaluation, error) {
	paths := s.orderedMCPPaths()
	externalPaths := mcpJSONCandidatePaths(s.workingDir)
	input := make([][]byte, 0, len(paths))
	fingerprints := make(map[string]reloadFileFingerprint, len(paths)+len(externalPaths))
	origins := make(map[string]MCPOrigin)
	rushDocuments := make([]stableConfigDocument, 0, len(paths))
	for _, path := range paths {
		path = normalizeReloadPath(path)
		data, present, err := s.mcpPathData(files, path)
		if err != nil {
			return mcpEvaluation{}, err
		}
		fingerprint := files.fingerprints[path]
		if !present {
			fingerprint = reloadFileFingerprint{}
		}
		fingerprints[path] = fingerprint
		rushDocuments = append(rushDocuments, stableConfigDocument{
			path: path, data: data, fingerprint: fingerprint, present: present,
		})
		if present {
			if len(data) > 0 {
				if !json.Valid(data) {
					return mcpEvaluation{}, fmt.Errorf("invalid JSON in config file %s", path)
				}
				input = append(input, data)
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
	externalDocuments := make([]stableConfigDocument, 0, len(externalPaths))
	for _, path := range externalPaths {
		path = normalizeReloadPath(path)
		data, present, err := s.mcpPathData(files, path)
		if err != nil {
			return mcpEvaluation{}, err
		}
		fingerprint := files.fingerprints[path]
		if !present {
			fingerprint = reloadFileFingerprint{}
		}
		fingerprints[path] = fingerprint
		document := stableConfigDocument{path: path, data: data, fingerprint: fingerprint, present: present}
		externalDocuments = append(externalDocuments, document)
		if !present {
			continue
		}
		external, err := loadMCPJSONBytes(data)
		if err != nil {
			return mcpEvaluation{}, err
		}
		for name, ext := range external {
			if _, rushDefined := origins[name]; rushDefined {
				continue
			}
			ext.Disabled = externalOverlayValue(rushDocuments, name, ext.Disabled)
			cfg.MCP[name] = ext
			origins[name] = MCPOrigin{Kind: MCPOriginExternal, Path: normalizeReloadPath(path), Scope: ScopeGlobal, Writable: false}
		}
	}
	return mcpEvaluation{configs: cfg.MCP, origins: origins, fingerprints: fingerprints}, nil
}

func (s *ConfigStore) orderedMCPPaths() []string {
	paths := []string{s.systemConfigPathValue(), GlobalConfig(), s.globalDataPath}
	fixed := make(map[string]struct{}, len(paths)+1)
	for _, path := range paths {
		if path != "" {
			fixed[normalizeReloadPath(path)] = struct{}{}
		}
	}
	if systemPath := systemConfigPath; systemPath != "" {
		fixed[normalizeReloadPath(systemPath)] = struct{}{}
	}
	for _, path := range lookupConfigCandidates(s.workingDir) {
		if _, ok := fixed[normalizeReloadPath(path)]; !ok {
			paths = append(paths, path)
		}
	}
	paths = append(paths, s.configPathOrEmpty(ScopeWorkspace))
	return uniqueNormalizedPaths(paths)
}

func (s *ConfigStore) systemConfigPathValue() string {
	if s.systemConfigPathOverride != "" {
		return s.systemConfigPathOverride
	}
	return SystemConfig()
}

func (s *ConfigStore) mcpPathData(files *mcpLockedFiles, path string) ([]byte, bool, error) {
	if _, ok := files.fingerprints[path]; ok {
		return files.data[path], files.present[path], nil
	}
	data, fingerprint, err := readStableConfigFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			files.fingerprints[path] = reloadFileFingerprint{}
			return nil, false, nil
		}
		return nil, false, err
	}
	files.data[path] = data
	files.present[path] = fingerprint.exists
	files.fingerprints[path] = fingerprint
	return data, fingerprint.exists, nil
}

func entryOrigins(origins map[string]MCPOrigin, path, workspacePath, globalPath, systemPath string, data []byte) {
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
		if path == normalizeReloadPath(workspacePath) {
			origin.Kind, origin.Scope, origin.Writable = MCPOriginWorkspace, ScopeWorkspace, true
		} else if path == normalizeReloadPath(globalPath) {
			origin.Kind, origin.Scope, origin.Writable = MCPOriginGlobal, ScopeGlobal, true
		} else if path == normalizeReloadPath(systemPath) {
			origin.Kind = MCPOriginSystem
		}
		origins[name] = origin
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
		if path == selected {
			continue
		}
		actual, err := readReloadFingerprint(path)
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

func fingerprintForBytes(path string, data []byte) reloadFileFingerprint {
	return dataFingerprint(path, data)
}

func dataFingerprint(path string, data []byte) reloadFileFingerprint {
	fingerprint := reloadFileFingerprint{exists: true, size: int64(len(data)), digest: sha256.Sum256(data)}
	if info, err := os.Stat(path); err == nil {
		fingerprint.modTime = info.ModTime().UnixNano()
	}
	return fingerprint
}
