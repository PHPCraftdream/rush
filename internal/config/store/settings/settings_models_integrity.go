package settings

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
)

// AnyLockExists reports whether any settings lock file is present.
// A read error counts as "lock exists" and fails closed.
func (s *Store) AnyLockExists() bool {
	sources := []settingsLockSource{
		{path: datadir.GlobalConfigData(), global: true},
		{path: s.port.GlobalDataPath(), global: true},
		{path: canonicalGlobalDataPath(), global: true},
		{path: s.workspacePath()},
		{path: naturalWorkspaceSettingsPath(s.port.WorkingDir())},
		{path: docread.ReadSlotOf(filepath.Join(s.port.WorkingDir(), "rush.json"))},
	}
	globals, locals, err := collectSettingsSourceHashes(sources)
	if err != nil {
		return true
	}
	return len(globals)+len(locals) > 0
}

// VerifyModelsIntegrityAtLoad performs the one-time startup check. It never
// fails Load and never writes anything; it only records the result.
func (s *Store) VerifyModelsIntegrityAtLoad() {
	if s.port.ModelsIntegrity() != nil {
		return
	}
	mismatch := false
	for _, path := range []string{s.port.GlobalDataPath(), s.workspacePath()} {
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
		if cfgmodel.ModelsIntegrityOf(doc.JSONBytes, s.AnyLockExists()) != nil {
			mismatch = true
			break
		}
	}
	if mismatch {
		s.port.SetModelsIntegrity(cfgmodel.ErrModelsEditedDirectly)
		slog.Warn("model settings were changed outside rush")
	}
}

// ModelsIntegrity returns the one-time startup check result. It is computed
// only during Load and never recomputed by reload.
func (s *Store) ModelsIntegrity() error { return s.port.ModelsIntegrity() }
