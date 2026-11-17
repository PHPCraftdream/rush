package config

import (
	"context"

	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/settings"
)

// corePort adapts *ConfigStore to the settings.Port interface. It exposes
// exactly the existing ConfigStore primitives (loadSnapshot, configPath,
// withConfigWriteLock, noteInitialLoadWriteLocked, autoReloadAfterWrite,
// modelsIntegrity) without adding any new locking: every method delegates to
// the same code paths the store's own write/reload API uses today.
type corePort struct {
	store *ConfigStore
}

// Compile-time assertion: corePort implements settings.Port.
var _ settings.Port = corePort{}

// ViewSnap samples the store's published snapshot exactly once per callback
// so every view read inside fn sees a single consistent generation.
func (p corePort) ViewSnap(fn func(settings.SnapView) error) error {
	sn := p.store.loadSnapshot()
	return fn(snapView{sn: sn})
}

// snapView exposes a storeSnapshot as a settings.SnapView.
type snapView struct{ sn *storeSnapshot }

func (v snapView) Config() *cfgmodel.Config { return v.sn.config }
func (v snapView) WorkspacePath() string    { return v.sn.workspacePath }
func (v snapView) Generation() uint64       { return v.sn.generation }

func (p corePort) ScopePath(scope cfgmodel.Scope) (string, error) {
	return p.store.configPath(scope)
}

func (p corePort) GlobalDataPath() string { return p.store.globalDataPath }
func (p corePort) WorkingDir() string     { return p.store.workingDir }

// NoteInitialLoadWrite requires diskWriteMu to be held and may be called
// only from inside a WithFileWriteLock callback. It must not call
// PublishMutation or re-enter WithFileWriteLock.
func (p corePort) NoteInitialLoadWrite(path string, data []byte) {
	p.store.noteInitialLoadWriteLocked(path, data)
}

func (p corePort) AutoReload() error {
	return p.store.autoReloadAfterWrite(context.Background())
}

func (p corePort) WithFileWriteLock(path string, fn func(settings.WriteTarget) error) error {
	return p.store.withConfigWriteLock(path, func(target configWriteTarget) error {
		return fn(settings.WriteTarget{
			Path:         target.path,
			SelectedPath: target.selectedPath,
			Owner:        target.owner,
			Enforce:      target.enforce,
		})
	})
}

func (p corePort) ModelsIntegrity() error       { return p.store.modelsIntegrity }
func (p corePort) SetModelsIntegrity(err error) { p.store.modelsIntegrity = err }

// settingsStore returns the store's settings facade, constructing an
// ephemeral one over corePort on demand for stores whose constructor did not
// install one (e.g. zero-value test stores).
func (s *ConfigStore) settingsStore() *settings.Store {
	if s.settings != nil {
		return s.settings
	}
	return settings.NewStore(corePort{store: s})
}
