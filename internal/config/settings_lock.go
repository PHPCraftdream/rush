package config

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const syncRevKey = "sync_rev"

var (
	ErrSettingsLocked           = errors.New("settings change is forbidden by the user (settings are locked). Do not try to change models or any other settings and do not look for a way around this; ask the user instead")
	ErrWrongPassword            = errors.New("wrong password: settings change is forbidden by the user. Do not guess or retry; ask the user instead")
	ErrNotLocked                = errors.New("settings are not locked")
	errSettingsLockKeyReserved  = ErrSettingsLocked
	processSettingsPasswordHash atomic.Value
	settingsAuditSink           atomic.Pointer[func(SettingsEvent)]
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

type settingsWriteAudit struct {
	event     *SettingsEvent
	refusedBy string
}

func HashPassword(password string) string {
	digest := sha256.Sum256([]byte(password))
	for range 9 {
		digest = sha256.Sum256(digest[:])
	}
	return hex.EncodeToString(digest[:])
}

func SetProcessPassword(password string) {
	hash := ""
	if password != "" {
		hash = HashPassword(password)
	}
	processSettingsPasswordHash.Store(hash)
}

func processPasswordHash() string {
	value := processSettingsPasswordHash.Load()
	hash, _ := value.(string)
	return hash
}

// CheckPasswordAgainstDisk checks only the global file because the workspace is unknown before Init.
func CheckPasswordAgainstDisk(password string) error {
	data, _, err := readStableConfigFile(GlobalConfigData())
	if err != nil {
		return nil
	}
	hash := settingsHash(data)
	if hash == "" {
		return nil
	}
	if !sameSettingsHash(HashPassword(password), hash) {
		return ErrWrongPassword
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
	process := processPasswordHash()
	if process == "" {
		return nil
	}
	global, local := s.currentLockHashes()
	if global == "" && local == "" {
		return nil
	}
	if !authorizedSettingsPassword(process, global, local) {
		return ErrWrongPassword
	}
	return nil
}

func (s *ConfigStore) currentLockHashes() (string, string) {
	global, _ := freshSettingsHash(s.globalDataPath)
	local, _ := freshSettingsHash(s.loadSnapshot().workspacePath)
	return global, local
}

func freshSettingsHash(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, _, err := readStableConfigFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return settingsHash(data), nil
}

func settingsHash(data []byte) string {
	return gjson.GetBytes(data, syncRevKey).String()
}
func sameSettingsHash(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func authorizedSettingsPassword(process, global, local string) bool {
	if global != "" {
		return sameSettingsHash(process, global)
	}
	return local != "" && sameSettingsHash(process, local)
}

// guardSettingsWrite rejects protected writes without taking any locks.
func (s *ConfigStore) guardSettingsWrite(path string, data []byte, keys []string) (string, error) {
	for _, key := range keys {
		if key == syncRevKey || strings.HasPrefix(key, syncRevKey+".") {
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
	workspace := s.loadSnapshot().workspacePath
	globalHash := readSettingsHashBestEffort(s.globalDataPath, path, data)
	localHash := readSettingsHashBestEffort(workspace, path, data)
	if globalHash == "" && localHash == "" {
		return "", nil
	}
	if authorizedSettingsPassword(processPasswordHash(), globalHash, localHash) || allRecent {
		return "", nil
	}
	return "change refused", ErrSettingsLocked
}

func readSettingsHashBestEffort(candidate, path string, data []byte) string {
	if candidate == "" {
		return ""
	}
	if normalizeReloadPath(candidate) == normalizeReloadPath(path) {
		return settingsHash(data)
	}
	contents, _, err := readStableConfigFile(candidate)
	if err != nil {
		return ""
	}
	return settingsHash(contents)
}

func (s *ConfigStore) LockSettings(scope Scope, password string) error {
	if password == "" {
		return errors.New("a password is required")
	}
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	refusedBy := ""
	err = s.writeSettingsLockRaw(path, func(data []byte) (string, error) {
		globalHash := readSettingsHashBestEffort(s.globalDataPath, path, data)
		localHash := readSettingsHashBestEffort(s.loadSnapshot().workspacePath, path, data)
		if globalHash != "" || localHash != "" {
			if !authorizedSettingsPassword(processPasswordHash(), globalHash, localHash) {
				return "", ErrSettingsLocked
			}
		}
		written, err := sjson.SetBytes(data, syncRevKey, HashPassword(password))
		return string(written), err
	})
	s.emitLockEvent(scope, path, err, refusedBy)
	return err
}

func (s *ConfigStore) UnlockSettings(scope Scope, password string) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	refusedBy := ""
	err = s.writeSettingsLockRaw(path, func(data []byte) (string, error) {
		hash := settingsHash(data)
		if hash == "" {
			return "", ErrNotLocked
		}
		if !sameSettingsHash(HashPassword(password), hash) {
			return "", ErrWrongPassword
		}
		globalHash := readSettingsHashBestEffort(s.globalDataPath, path, data)
		if scope == ScopeWorkspace && globalHash != "" && !sameSettingsHash(processPasswordHash(), globalHash) {
			refusedBy = "change refused"
			return "", ErrSettingsLocked
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
		data, expected, err := readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
		if err != nil {
			if os.IsNotExist(err) {
				data = []byte("{}")
			} else {
				return fmt.Errorf("failed to read config file: %w", err)
			}
		}
		newValue, err := mutate(data)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target.path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		written := []byte(newValue)
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
			if key == syncRevKey || strings.HasPrefix(key, syncRevKey+".") {
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
