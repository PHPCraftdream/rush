package config

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

// anySettingsLockExists reports whether any settings lock file is present.
// A read error counts as "lock exists" and fails closed.
func (s *ConfigStore) anySettingsLockExists() bool {
	sources := []settingsLockSource{
		// The process-global data config AND the store's own global path:
		// docread.ReadSettingsDocument selects the active slot for either spelling,
		// so a lock living only in rush.ktav is seen through both.
		{path: GlobalConfigData(), global: true},
		{path: s.globalDataPath, global: true},
		// canonicalGlobalDataPath() and naturalWorkspaceSettingsPath() are
		// slot-selected inside their source helpers; the raw workspace join
		// still needs selection.
		{path: canonicalGlobalDataPath(), global: true},
		{path: s.loadSnapshot().workspacePath},
		{path: naturalWorkspaceSettingsPath(s.workingDir)},
		{path: docread.ReadSlotOf(filepath.Join(s.workingDir, "rush.json"))},
	}
	globals, locals, err := collectSettingsSourceHashes(sources)
	if err != nil {
		return true
	}
	return len(globals)+len(locals) > 0
}

// verifyModelsIntegrityAtLoad performs the one-time startup check. It never
// fails Load and never writes anything; it only records the result.
func (s *ConfigStore) verifyModelsIntegrityAtLoad() {
	if s.modelsIntegrity != nil {
		return
	}
	mismatch := false
	for _, path := range []string{s.globalDataPath, s.loadSnapshot().workspacePath} {
		if path == "" {
			continue
		}
		data, _, err := docread.ReadStableConfigFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			mismatch = true
			break
		}
		doc, err := docread.DecodeSettingsDocument(path, data, docread.Fingerprint{})
		if err != nil {
			// An undecodable document fails closed: it counts as a
			// mismatch, never as an absent models field.
			mismatch = true
			break
		}
		if modelsIntegrityOf(doc.JSONBytes, s.anySettingsLockExists()) != nil {
			mismatch = true
			break
		}
	}
	if mismatch {
		s.modelsIntegrity = ErrModelsEditedDirectly
		slog.Warn("model settings were changed outside rush")
	}
}

// ModelsIntegrity returns the one-time startup check result. It is computed
// only during Load and never recomputed by reload.
func (s *ConfigStore) ModelsIntegrity() error {
	return s.modelsIntegrity
}
