package config

import (
	"path/filepath"
	"testing"
)

// canonicalGlobalDataPath is the OS-resolved global settings file independent
// of environment overrides. Tests may replace it to use an isolated fixture.
var canonicalGlobalDataPath = func() string {
	if testing.Testing() {
		return ""
	}
	return platformCanonicalGlobalDataPath()
}

func naturalWorkspaceSettingsPath(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	dataDir, _ := defaultDataDirForWorkspace(workingDir)
	return filepath.Join(dataDir, "rush.json")
}

// checkExternalSettingsSources checks locks in settings sources outside the
// target being changed. All sources are read before considering authorization.
func (s *ConfigStore) checkExternalSettingsSources(path string, data []byte) error {
	workspace := s.loadSnapshot().workspacePath
	natural := naturalWorkspaceSettingsPath(s.workingDir)
	globalDataPath := s.globalDataPath
	canonical := canonicalGlobalDataPath()
	processGlobal, err := readSettingsHash(globalDataPath, path, data)
	if err != nil {
		return ErrSettingsLocked
	}
	var canonicalGlobalHash string
	if canonical != "" {
		canonicalGlobalHash, err = readSettingsHash(canonical, path, data)
		if err != nil {
			return ErrSettingsLocked
		}
	}
	processLocal, err := readSettingsHash(workspace, path, data)
	if err != nil {
		return ErrSettingsLocked
	}
	var naturalLocal string
	if natural != "" {
		naturalLocal, err = readSettingsHash(natural, path, data)
		if err != nil {
			return ErrSettingsLocked
		}
	}

	globalHashes := make([]string, 0, 2)
	if processGlobal != "" {
		globalHashes = append(globalHashes, processGlobal)
	}
	if canonicalGlobalHash != "" && (normalizeReloadPath(canonical) == normalizeReloadPath(globalDataPath) || normalizeReloadPath(path) != normalizeReloadPath(globalDataPath)) {
		globalHashes = append(globalHashes, canonicalGlobalHash)
	}
	if len(globalHashes) != 0 {
		password := processPasswordHash()
		for _, hash := range globalHashes {
			if !sameSettingsHash(password, hash) {
				return ErrSettingsLocked
			}
		}
		return nil
	}

	localHashes := make([]string, 0, 2)
	if processLocal != "" {
		localHashes = append(localHashes, processLocal)
	}
	if naturalLocal != "" {
		includeNatural := workspace == "" || normalizeReloadPath(natural) == normalizeReloadPath(workspace)
		if !includeNatural && workspace != "" {
			// A redirected workspace's natural lock applies only outside its directory.
			rel, relErr := filepath.Rel(filepath.Dir(workspace), path)
			if relErr != nil || rel == ".." || filepath.IsAbs(rel) || (len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)) {
				includeNatural = true
			}
		}
		if includeNatural {
			localHashes = append(localHashes, naturalLocal)
		}
	}
	if len(localHashes) == 0 {
		return nil
	}
	password := processPasswordHash()
	for _, hash := range localHashes {
		if !sameSettingsHash(password, hash) {
			return ErrSettingsLocked
		}
	}
	return nil
}
