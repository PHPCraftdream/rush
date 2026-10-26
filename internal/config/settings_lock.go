package config

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const syncRevKey = "sync_rev"

var reservedSettingsKeys = []string{syncRevKey, cacheRevKey}

var (
	ErrSettingsLocked          = errors.New("settings change is forbidden by the user (settings are locked). Do not try to change models or any other settings and do not look for a way around this; ask the user instead")
	ErrWrongPassword           = errors.New("wrong password: settings change is forbidden by the user. Do not guess or retry; ask the user instead")
	ErrNotLocked               = errors.New("settings are not locked")
	errSettingsLockKeyReserved = ErrSettingsLocked
	processSettingsPassword    atomic.Value
	settingsAuditSink          atomic.Pointer[func(SettingsEvent)]
)

type SettingsEvent struct {
	Scope   string
	Path    string
	Keys    []string
	Models  map[string]string
	Outcome string
	Reason  string
}

type LockState struct{ Global, Local bool }

func (e SettingsEvent) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("scope", e.Scope),
		slog.String("outcome", e.Outcome),
	)
}

const (
	settingsPasswordHashPrefix    = "pbkdf2-sha256"
	maxSettingsPasswordIterations = 10_000_000
)

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

type settingsWriteAudit struct {
	event     *SettingsEvent
	refusedBy string
}

func HashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(fmt.Sprintf("failed to generate settings password salt: %v", err))
	}
	iterations := int(settingsPasswordIterations.Load())
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		panic(fmt.Sprintf("failed to derive settings password hash: %v", err))
	}
	return fmt.Sprintf("%s$%d$%s$%s",
		settingsPasswordHashPrefix, iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// VerifyPassword reports whether password matches the stored PBKDF2 hash.
// An unparseable, unrecognized or legacy (plain hex) stored value does NOT
// match: the lock fails closed.
func VerifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != settingsPasswordHashPrefix {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > maxSettingsPasswordIterations {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(key, want) == 1
}

func SetProcessPassword(password string) {
	processSettingsPassword.Store(password)
}

func processPassword() string {
	value := processSettingsPassword.Load()
	password, _ := value.(string)
	return password
}

// CheckPasswordAgainstDisk checks only the global file because the workspace is unknown before Init.
func CheckPasswordAgainstDisk(password string) error {
	// The global path is a logical slot: readSettingsDocument selects the
	// active file (rush.ktav when present), fails closed on a backup-only
	// slot, and decodes through the settings-document layer — the same
	// discipline as freshSettingsHash. A literal read of the legacy name
	// would miss a global rush.ktav entirely, and a backup-only slot would
	// silently read as unlocked.
	doc, err := readSettingsDocument(GlobalConfigData())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return ErrSettingsLocked
	}
	hash := settingsHash(doc.jsonBytes)
	if hash == "" {
		return nil
	}
	if !VerifyPassword(password, hash) {
		return ErrWrongPassword
	}
	return nil
}

// canonicalGlobalDataPath() and naturalWorkspaceSettingsPath() select their
// own slots; the raw legacy joins below are slot-selected by
// readSettingsDocument inside freshSettingsHash.
func SettingsWriteAllowed(workspaceDir string) error {
	process := processPassword()
	globals, locals, err := collectSettingsSourceHashes([]settingsLockSource{
		{path: GlobalConfigData(), global: true},
		{path: canonicalGlobalDataPath(), global: true},
		{path: naturalWorkspaceSettingsPath(workspaceDir)},
		{path: filepath.Join(workspaceDir, "rush.json")},
	})
	if err != nil {
		return ErrSettingsLocked
	}
	checks := globals
	if len(checks) == 0 {
		checks = locals
	}
	for _, hash := range checks {
		if !VerifyPassword(process, hash) {
			return ErrSettingsLocked
		}
	}
	return nil
}

func (s *ConfigStore) LockState() (LockState, error) {
	global, err := freshSettingsHash(s.globalDataPath)
	if err != nil {
		return LockState{}, err
	}
	local, err := freshSettingsHash(s.loadSnapshot().workspacePath)
	if err != nil {
		return LockState{}, err
	}
	return LockState{Global: global != "", Local: local != ""}, nil
}

func (s *ConfigStore) SettingsLocked() (bool, error) {
	state, err := s.LockState()
	return state.Global || state.Local, err
}

func (s *ConfigStore) CheckProcessPassword() error {
	process := processPassword()
	global, local, err := s.currentLockHashes()
	if err != nil {
		return ErrSettingsLocked
	}
	if process == "" {
		return nil
	}
	checks := global
	if len(checks) == 0 {
		checks = local
	}
	for _, hash := range checks {
		if !VerifyPassword(process, hash) {
			return ErrWrongPassword
		}
	}
	return nil
}

func (s *ConfigStore) currentLockHashes() ([]string, []string, error) {
	globals, locals, err := collectSettingsSourceHashes([]settingsLockSource{
		{path: s.globalDataPath, global: true},
		{path: canonicalGlobalDataPath(), global: true},
		{path: s.loadSnapshot().workspacePath},
		{path: naturalWorkspaceSettingsPath(s.workingDir)},
	})
	if err != nil {
		return nil, nil, err
	}
	return globals, locals, nil
}

func freshSettingsHash(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	// readSettingsDocument slot-selects ANY input spelling and fails closed
	// on a backup-only slot (a protection source whose logical location
	// holds only a retained backup must never read as "no lock"), so an
	// os.IsNotExist here means neither spelling of the slot exists.
	doc, err := readSettingsDocument(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return settingsHash(doc.jsonBytes), nil
}

func settingsHash(data []byte) string {
	return gjson.GetBytes(data, syncRevKey).String()
}

func touchesReservedKey(key string) bool {
	first := settingsKeyFirstComponent(key)
	for _, reserved := range reservedSettingsKeys {
		if first == reserved {
			return true
		}
	}
	return false
}

// guardSettingsWrite rejects protected writes without taking any locks.
func (s *ConfigStore) guardSettingsWrite(path string, data []byte, keys []string) (string, error) {
	for _, key := range keys {
		if touchesReservedKey(key) {
			return "", errSettingsLockKeyReserved
		}
	}
	if len(keys) == 0 {
		return "", nil
	}
	allRecent := true
	for _, key := range keys {
		if !strings.HasPrefix(key, "recent_models.") {
			allRecent = false
			break
		}
	}
	if allRecent {
		return "", nil
	}
	if err := s.checkExternalSettingsSources(path, data); err != nil {
		return "change refused", err
	}
	return "", nil
}

func readSettingsHash(candidate, path string, data []byte) (string, error) {
	if candidate == "" {
		return "", nil
	}
	// Same fail-closed discipline as freshSettingsHash (slot selection and
	// the backup-only rule live inside readSettingsDocument): this guard is
	// the write-path source reader (checkExternalSettingsSources,
	// lockHashesForTarget) and must never read a backup-only slot as
	// "no lock".
	if normalizeReloadPath(candidate) == normalizeReloadPath(path) {
		return settingsHash(data), nil
	}
	doc, err := readSettingsDocument(candidate)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return settingsHash(doc.jsonBytes), nil
}

func pathWithinDirectory(directory, path string) bool {
	rel, err := filepath.Rel(directory, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (s *ConfigStore) lockHashesForTarget(path string, data []byte) ([]string, []string, error) {
	workspace := s.loadSnapshot().workspacePath
	natural := naturalWorkspaceSettingsPath(s.workingDir)
	canonical := canonicalGlobalDataPath()
	globals := []string{}
	locals := []string{}
	for _, source := range []struct {
		path string
		kind string
	}{{s.globalDataPath, "global"}, {canonical, "global"}, {workspace, "local"}, {natural, "local"}} {
		if source.path == "" {
			continue
		}
		hash, err := readSettingsHash(source.path, path, data)
		if err != nil {
			return nil, nil, err
		}
		if normalizeReloadPath(source.path) == normalizeReloadPath(path) {
			continue
		}
		if source.kind == "global" && normalizeReloadPath(source.path) == normalizeReloadPath(canonical) && normalizeReloadPath(source.path) != normalizeReloadPath(s.globalDataPath) && normalizeReloadPath(path) == normalizeReloadPath(s.globalDataPath) {
			continue
		}
		if source.kind == "local" && workspace != "" && natural != "" && normalizeReloadPath(source.path) == normalizeReloadPath(natural) && normalizeReloadPath(natural) != normalizeReloadPath(workspace) && pathWithinDirectory(filepath.Dir(workspace), path) {
			continue
		}
		if hash == "" {
			continue
		}
		if source.kind == "global" {
			globals = append(globals, hash)
		} else {
			locals = append(locals, hash)
		}
	}
	return globals, locals, nil
}

func (s *ConfigStore) LockSettings(scope Scope, password string) error {
	if password == "" {
		return errors.New("a password is required")
	}
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	path = writeTargetSlot(path)
	refusedBy := ""
	err = s.writeSettingsLockRaw(path, func(data []byte) (string, error) {
		if err := s.checkExternalSettingsSources(path, data); err != nil {
			return "", err
		}
		written, err := sjson.SetBytes(data, syncRevKey, HashPassword(password))
		if err != nil {
			return "", err
		}
		return applyModelsIntegrity(string(written))
	})
	s.emitLockEvent(scope, path, err, refusedBy)
	return err
}

func (s *ConfigStore) UnlockSettings(scope Scope, password string) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	path = writeTargetSlot(path)
	refusedBy := ""
	err = s.writeSettingsLockRaw(path, func(data []byte) (string, error) {
		globals, locals, hashErr := s.lockHashesForTarget(path, data)
		if hashErr != nil {
			return "", ErrSettingsLocked
		}
		hash := settingsHash(data)
		if hash == "" {
			return "", ErrNotLocked
		}
		if !VerifyPassword(password, hash) {
			return "", ErrWrongPassword
		}
		process := processPassword()
		checks := globals
		if len(checks) == 0 && !(normalizeReloadPath(path) == normalizeReloadPath(s.globalDataPath) && hash != "") {
			checks = locals
		}
		for _, sourceHash := range checks {
			if !VerifyPassword(process, sourceHash) {
				refusedBy = "change refused"
				return "", ErrSettingsLocked
			}
		}
		written, err := sjson.DeleteBytes(data, syncRevKey)
		return string(written), err
	})
	s.emitLockEvent(scope, path, err, refusedBy)
	return err
}

func (s *ConfigStore) writeSettingsLockRaw(path string, mutate func(data []byte) (string, error)) error {
	var committedErr error
	err := s.withConfigWriteLock(path, func(target configWriteTarget) error {
		raw, expected, readErr := readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
		var doc settingsDocument
		if readErr != nil {
			if os.IsNotExist(readErr) {
				doc = settingsDocument{jsonBytes: []byte("{}"), format: settingsDocumentFormatOf(target.selectedPath)}
			} else {
				return ErrSettingsLocked
			}
		} else {
			var decodeErr error
			doc, decodeErr = decodeSettingsDocument(target.selectedPath, raw, expected)
			if decodeErr != nil {
				return decodeErr
			}
		}
		newJSON, err := mutate(doc.jsonBytes)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target.path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		written, err := encodeSettingsDocument([]byte(newJSON), doc.format)
		if err != nil {
			return err
		}
		if !expected.exists {
			_, expected, err = readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to verify config file before write: %w", err)
			}
		}
		_, commitErr := commitConfigFile(target.selectedPath, target.path, written, 0o600, expected, target.owner, target.enforce)
		if commitErr != nil {
			if outcome, ok := CommitOutcomeFromError(commitErr); ok && outcome.Committed {
				s.noteInitialLoadWriteLocked(path, written)
				committedErr = commitErr
				return nil
			}
			return fmt.Errorf("failed to write config file: %w", commitErr)
		}
		s.noteInitialLoadWriteLocked(path, written)
		return nil
	})
	if err != nil {
		return err
	}
	if committedErr != nil {
		if reloadErr := s.autoReloadAfterWrite(context.Background()); reloadErr != nil {
			return errors.Join(committedErr, reloadErr)
		}
		return committedErr
	}
	if err := s.autoReloadAfterWrite(context.Background()); err != nil {
		slogConfigReloadWarning(path, err)
	}
	return nil
}

func SetSettingsAuditSink(f func(SettingsEvent)) {
	if f == nil {
		settingsAuditSink.Store(nil)
		return
	}
	settingsAuditSink.Store(&f)
}

func emitSettingsEvent(ev SettingsEvent) {
	if len(ev.Keys) > 0 {
		ev.Keys = append([]string(nil), ev.Keys...)
		for i, key := range ev.Keys {
			if touchesReservedKey(key) {
				ev.Keys[i] = "*"
			}
		}
		sort.Strings(ev.Keys)
	}
	if sink := settingsAuditSink.Load(); sink != nil && *sink != nil {
		(*sink)(ev)
	}
}

func finalizeSettingsEvent(ev *SettingsEvent, refusedBy string, err error) {
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

func settingsScope(scope Scope) string {
	if scope == ScopeGlobal {
		return "global"
	}
	return "workspace"
}

func (s *ConfigStore) emitLockEvent(scope Scope, path string, err error, refusedBy string) {
	ev := SettingsEvent{Scope: settingsScope(scope), Path: path, Keys: []string{"*"}}
	finalizeSettingsEvent(&ev, refusedBy, err)
	emitSettingsEvent(ev)
}

// slogConfigReloadWarning logs a reload failure after a completed settings write.
func slogConfigReloadWarning(path string, err error) {
	slog.Warn("Config file updated but failed to reload in-memory state", "path", path, "error", err)
}

// settingsScopeForPath classifies a write target for audit events.
func (s *ConfigStore) settingsScopeForPath(path string) string {
	if normalizeReloadPath(path) == normalizeReloadPath(s.globalDataPath) {
		return "global"
	}
	return "workspace"
}

func selectedModelsAudit(keys []string, values map[string]any) map[string]string {
	var models map[string]string
	for _, key := range keys {
		if !strings.HasPrefix(key, "models.") {
			continue
		}
		model, ok := values[key].(SelectedModel)
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
