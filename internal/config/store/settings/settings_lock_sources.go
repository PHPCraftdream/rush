package settings

import (
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
)

// canonicalGlobalDataPath is the OS-resolved global settings file independent
// of environment overrides. Tests may replace it to use an isolated fixture.
var canonicalGlobalDataPath = func() string {
	// The canonical lock protects a specific config file. Under
	// --global-dir the isolated instance never writes the machine's real
	// global data file, so the real lock must not apply to it — and the
	// isolated lock must not apply to the real instance. Route the
	// canonical source into the isolated root instead of the platform path.
	if root, ok := datadir.GlobalDir(); ok {
		return docread.ReadSlotOf(filepath.Join(root, "data", cfgmodel.AppName+".json"))
	}
	if testing.Testing() {
		return ""
	}
	return platformCanonicalGlobalDataPath()
}

type settingsLockSource struct {
	path   string
	global bool
}

// collectSettingsSourceHashes reads every configured lock source and preserves
// every non-empty hash so no source can silently override another.
func collectSettingsSourceHashes(sources []settingsLockSource) ([]string, []string, error) {
	globals := make([]string, 0, len(sources))
	locals := make([]string, 0, len(sources))
	for _, source := range sources {
		if source.path == "" {
			continue
		}
		hash, err := freshSettingsHash(source.path)
		if err != nil {
			return nil, nil, err
		}
		if hash == "" {
			continue
		}
		if source.global {
			globals = append(globals, hash)
		} else {
			locals = append(locals, hash)
		}
	}
	return globals, locals, nil
}

// NaturalWorkspaceSettingsPath resolves the workspace's natural settings slot.
func NaturalWorkspaceSettingsPath(workingDir string) string { return naturalWorkspaceSettingsPath(workingDir) }

func naturalWorkspaceSettingsPath(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	dataDir, _ := datadir.DefaultDataDirForWorkspace(workingDir)
	// The natural workspace settings file is a LOGICAL slot: select the
	// active sibling (rush.ktav when present) so a lock living only in the
	// Ktav document is visible. docread.ReadSlotOf passes an already-selected .ktav
	// path through unchanged.
	return docread.ReadSlotOf(filepath.Join(dataDir, "rush.json"))
}

// checkExternalSettingsSources checks locks in settings sources outside the
// target being changed. All sources are read before considering authorization.
func (s *Store) checkExternalSettingsSources(path string, data []byte) error {
	workspace := s.workspacePath()
	natural := naturalWorkspaceSettingsPath(s.port.WorkingDir())
	globalDataPath := s.port.GlobalDataPath()
	canonical := canonicalGlobalDataPath()
	processGlobal, err := readSettingsHash(globalDataPath, path, data)
	if err != nil {
		return settingsLockReadError(err)
	}
	var canonicalGlobalHash string
	if canonical != "" {
		canonicalGlobalHash, err = readSettingsHash(canonical, path, data)
		if err != nil {
			return settingsLockReadError(err)
		}
	}
	processLocal, err := readSettingsHash(workspace, path, data)
	if err != nil {
		return settingsLockReadError(err)
	}
	var naturalLocal string
	if natural != "" {
		naturalLocal, err = readSettingsHash(natural, path, data)
		if err != nil {
			return settingsLockReadError(err)
		}
	}

	globalHashes := make([]string, 0, 2)
	if processGlobal != "" {
		globalHashes = append(globalHashes, processGlobal)
	}
	if canonicalGlobalHash != "" && (docread.NormalizeReloadPath(canonical) == docread.NormalizeReloadPath(globalDataPath) || docread.NormalizeReloadPath(path) != docread.NormalizeReloadPath(globalDataPath)) {
		globalHashes = append(globalHashes, canonicalGlobalHash)
	}
	if len(globalHashes) != 0 {
		password := processPassword()
		for _, hash := range globalHashes {
			if !VerifyPassword(password, hash) {
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
		includeNatural := workspace == "" || docread.NormalizeReloadPath(natural) == docread.NormalizeReloadPath(workspace)
		if !includeNatural && workspace != "" {
			// A redirected workspace's natural lock applies only outside its directory.
			// Both sides canonical: the target arrives resolved, so an aliased
			// workspace dir (macOS /var → /private/var, Windows 8.3) must too.
			rel, relErr := filepath.Rel(filepath.Dir(docread.NormalizeReloadPath(workspace)), docread.NormalizeReloadPath(path))
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
	password := processPassword()
	for _, hash := range localHashes {
		if !VerifyPassword(password, hash) {
			return ErrSettingsLocked
		}
	}
	return nil
}
