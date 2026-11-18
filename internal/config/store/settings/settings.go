package settings

import (
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
)

// Scope and SettingsWriteAuthority are configuration-model contracts.
type (
	Scope                  = cfgmodel.Scope
	SettingsWriteAuthority = cfgmodel.SettingsWriteAuthority
)

type SnapView interface {
	Config() *cfgmodel.Config
	WorkspacePath() string
	Generation() uint64
}

type WriteTarget struct {
	Path, SelectedPath string
	Owner              int
	Enforce            bool
}

type Port interface {
	ViewSnap(fn func(SnapView) error) error
	ScopePath(cfgmodel.Scope) (string, error)
	GlobalDataPath() string
	WorkingDir() string
	// NoteInitialLoadWrite requires diskWriteMu to be held and may be called only from a WithFileWriteLock callback.
	// It must not call PublishMutation or re-enter WithFileWriteLock.
	NoteInitialLoadWrite(path string, data []byte)
	AutoReload() error
	WithFileWriteLock(path string, fn func(WriteTarget) error) error
	ModelsIntegrity() error
	SetModelsIntegrity(error)
}

type Store struct{ port Port }

func NewStore(p Port) *Store { return &Store{port: p} }

func (s *Store) workspacePath() string {
	var path string
	_ = s.port.ViewSnap(func(view SnapView) error {
		path = view.WorkspacePath()
		return nil
	})
	return path
}

const (
	syncRevKey                    = "sync_rev"
	settingsPasswordHashPrefix    = "pbkdf2-sha256"
	maxSettingsPasswordIterations = 10_000_000
)

var (
	ErrSettingsLocked          = errors.New("settings change is forbidden by the user (settings are locked). Do not try to change models or any other settings and do not look for a way around this; ask the user instead")
	ErrWrongPassword           = errors.New("wrong password: settings change is forbidden by the user. Do not guess or retry; ask the user instead")
	ErrNotLocked               = errors.New("settings are not locked")
	errSettingsLockKeyReserved = ErrSettingsLocked
	processSettingsPassword    atomic.Value
	settingsAuditSink          atomic.Pointer[func(SettingsEvent)]
	reservedSettingsKeys       = []string{syncRevKey, cfgmodel.CacheRevKey}
)

func ErrSettingsLockKeyReserved() error { return errSettingsLockKeyReserved }

var settingsPasswordIterations atomic.Int64

func init() { settingsPasswordIterations.Store(600_000) }

// SetSettingsPasswordIterationsForTest is a test seam ONLY: it sets the
// PBKDF2 iteration count used by HashPassword and returns a func restoring
// the previous value.
func SetSettingsPasswordIterationsForTest(n int) (restore func()) {
	prev := settingsPasswordIterations.Load()
	settingsPasswordIterations.Store(int64(n))
	return func() { settingsPasswordIterations.Store(prev) }
}

type SettingsEvent struct {
	Scope   string
	Path    string
	Keys    []string
	Models  map[string]string
	Outcome string
	Reason  string
	// Source identifies which transport performed the write: "" for
	// ordinary writes, "ui" for trusted-UI writes. It is a fixed,
	// secret-free token and never contains credentials.
	Source string
}

type LockState struct{ Global, Local bool }

type ErrSettingsLockUnrecognized struct{ Path string }

func (e *ErrSettingsLockUnrecognized) Error() string { return ErrSettingsLocked.Error() }
func (e *ErrSettingsLockUnrecognized) Unwrap() error { return ErrSettingsLocked }

type settingsWriteAudit struct {
	event     *SettingsEvent
	refusedBy string
}

func (e SettingsEvent) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("scope", e.Scope),
		slog.String("outcome", e.Outcome),
		slog.String("source", e.Source),
	)
}

func SetSettingsAuditSink(f func(SettingsEvent)) {
	if f == nil {
		settingsAuditSink.Store(nil)
		return
	}
	settingsAuditSink.Store(&f)
}

func SettingsScope(scope cfgmodel.Scope) string {
	if scope == cfgmodel.ScopeGlobal {
		return "global"
	}
	return "workspace"
}

func SettingsAuditSource(authority cfgmodel.SettingsWriteAuthority) string {
	if authority == cfgmodel.SettingsAuthorityTrustedUI {
		return "ui"
	}
	return ""
}

func SelectedModelsAudit(keys []string, values map[string]any) map[string]string {
	var models map[string]string
	for _, key := range keys {
		if !strings.HasPrefix(key, "models.") {
			continue
		}
		model, ok := values[key].(cfgmodel.SelectedModel)
		if !ok {
			continue
		}
		if models == nil {
			models = make(map[string]string)
		}
		value := model.Provider + "/" + model.Model
		if model.ReasoningEffort != "" {
			value += " effort=" + model.ReasoningEffort
		}
		models[key] = value
	}
	return models
}

func FinalizeSettingsEvent(ev *SettingsEvent, refusedBy string, err error) {
	if err == nil {
		ev.Outcome = "applied"
		return
	}
	if errors.Is(err, ErrSettingsLocked) {
		ev.Outcome, ev.Reason = "blocked", "change refused"
		return
	}
	ev.Outcome = "failed"
	if errors.Is(err, errSettingsLockKeyReserved) || errors.Is(err, ErrNotLocked) || errors.Is(err, ErrWrongPassword) {
		ev.Outcome = "blocked"
	}
	ev.Reason = "operation failed"
}

func EmitSettingsEvent(ev SettingsEvent) {
	if len(ev.Keys) > 0 {
		ev.Keys = append([]string(nil), ev.Keys...)
		for i, key := range ev.Keys {
			if TouchesReservedKey(key) {
				ev.Keys[i] = "*"
			}
		}
		sort.Strings(ev.Keys)
	}
	if sink := settingsAuditSink.Load(); sink != nil && *sink != nil {
		(*sink)(ev)
	}
}

func TouchesReservedKey(key string) bool {
	first := settingsKeyFirstComponent(key)
	for _, reserved := range reservedSettingsKeys {
		if first == reserved {
			return true
		}
	}
	return false
}

// settingsKeyFirstComponent matches sjson's first path component handling.
func settingsKeyFirstComponent(key string) string {
	first := strings.TrimPrefix(key, ":")
	var component strings.Builder
	for i := 0; i < len(first); i++ {
		switch first[i] {
		case '.':
			return component.String()
		case '\\':
			i++
			if i < len(first) {
				component.WriteByte(first[i])
			}
		default:
			component.WriteByte(first[i])
		}
	}
	return component.String()
}
