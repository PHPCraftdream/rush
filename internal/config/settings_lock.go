package config

import (
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
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/store/settings"
	"github.com/tidwall/gjson"
)

const syncRevKey = "sync_rev"

var reservedSettingsKeys = []string{syncRevKey, cacheRevKey}

type ErrSettingsLockUnrecognized struct{ Path string }

func (e *ErrSettingsLockUnrecognized) Error() string { return ErrSettingsLocked.Error() }
func (e *ErrSettingsLockUnrecognized) Unwrap() error { return ErrSettingsLocked }

func newUnrecognizedSettingsLock(path string) *ErrSettingsLockUnrecognized {
	absolute, err := filepath.Abs(docread.ReadSlotOf(path))
	if err != nil {
		absolute = filepath.Clean(docread.ReadSlotOf(path))
	}
	return &ErrSettingsLockUnrecognized{Path: absolute}
}

func settingsLockReadError(err error) error {
	var unrecognized *ErrSettingsLockUnrecognized
	if errors.As(err, &unrecognized) {
		return err
	}
	return ErrSettingsLocked
}

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
	// Source identifies which transport performed the write: "" for
	// ordinary writes, "ui" for trusted-UI writes. It is a fixed,
	// secret-free token and never contains credentials.
	Source string
}

type LockState struct{ Global, Local bool }

func (e SettingsEvent) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("scope", e.Scope),
		slog.String("outcome", e.Outcome),
		slog.String("source", e.Source),
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

func parseSettingsPasswordHash(stored string) (int, []byte, []byte, bool) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != settingsPasswordHashPrefix {
		return 0, nil, nil, false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > maxSettingsPasswordIterations {
		return 0, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return 0, nil, nil, false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return 0, nil, nil, false
	}
	return iterations, salt, want, true
}

// VerifyPassword reports whether password matches the stored PBKDF2 hash.
// An unparseable, unrecognized or legacy (plain hex) stored value does NOT
// match: the lock fails closed.
func VerifyPassword(password, stored string) bool {
	iterations, salt, want, ok := parseSettingsPasswordHash(stored)
	if !ok {
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
	// The global path is a logical slot: docread.ReadSettingsDocument selects the
	// active file (rush.ktav when present), fails closed on a backup-only
	// slot, and decodes through the settings-document layer — the same
	// discipline as freshSettingsHash. A literal read of the legacy name
	// would miss a global rush.ktav entirely, and a backup-only slot would
	// silently read as unlocked.
	doc, err := docread.ReadSettingsDocument(GlobalConfigData())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return ErrSettingsLocked
	}
	hash := settingsHash(doc.JSONBytes)
	if hash == "" {
		return nil
	}
	if _, _, _, ok := parseSettingsPasswordHash(hash); !ok {
		return newUnrecognizedSettingsLock(doc.Path)
	}
	if !VerifyPassword(password, hash) {
		return ErrWrongPassword
	}
	return nil
}

// canonicalGlobalDataPath() and naturalWorkspaceSettingsPath() select their
// own slots; the raw legacy joins below are slot-selected by
// docread.ReadSettingsDocument inside freshSettingsHash.
func SettingsWriteAllowed(workspaceDir string) error {
	process := processPassword()
	globals, locals, err := collectSettingsSourceHashes([]settingsLockSource{
		{path: GlobalConfigData(), global: true},
		{path: canonicalGlobalDataPath(), global: true},
		{path: naturalWorkspaceSettingsPath(workspaceDir)},
		{path: filepath.Join(workspaceDir, "rush.json")},
	})
	if err != nil {
		return settingsLockReadError(err)
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
	state, err := s.settings.LockState()
	return LockState{Global: state.Global, Local: state.Local}, err
}

func (s *ConfigStore) SettingsLocked() (bool, error) {
	return s.settings.SettingsLocked()
}

func (s *ConfigStore) CheckProcessPassword() error {
	return s.settings.CheckProcessPassword()
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
	// docread.ReadSettingsDocument slot-selects ANY input spelling and fails closed
	// on a backup-only slot (a protection source whose logical location
	// holds only a retained backup must never read as "no lock"), so an
	// os.IsNotExist here means neither spelling of the slot exists.
	doc, err := docread.ReadSettingsDocument(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if hash := settingsHash(doc.JSONBytes); hash != "" {
		if _, _, _, ok := parseSettingsPasswordHash(hash); !ok {
			return "", newUnrecognizedSettingsLock(doc.Path)
		}
		return hash, nil
	}
	return "", nil
}

func settingsHash(data []byte) string {
	return gjson.GetBytes(data, syncRevKey).String()
}

// guardSettingsWrite rejects protected writes without taking any locks.
// The authority parameter is the per-call trusted-UI exception; ordinary
// writes keep the lock veto.
func (s *ConfigStore) guardSettingsWrite(path string, data []byte, keys []string) (string, error) {
	return s.guardSettingsWriteForAuthority(path, data, keys, SettingsAuthorityGuarded)
}

func (s *ConfigStore) guardSettingsWriteForAuthority(path string, data []byte, keys []string, authority SettingsWriteAuthority) (string, error) {
	return s.settings.GuardWrite(path, data, keys, authority)
}

func readSettingsHash(candidate, path string, data []byte) (string, error) {
	if candidate == "" {
		return "", nil
	}
	// Same fail-closed discipline as freshSettingsHash (slot selection and
	// the backup-only rule live inside docread.ReadSettingsDocument): this guard is
	// the write-path source reader and must never read a backup-only slot as
	// "no lock".
	if docread.NormalizeReloadPath(candidate) == docread.NormalizeReloadPath(path) {
		hash := settingsHash(data)
		if hash != "" {
			if _, _, _, ok := parseSettingsPasswordHash(hash); !ok {
				return "", newUnrecognizedSettingsLock(path)
			}
		}
		return hash, nil
	}
	doc, err := docread.ReadSettingsDocument(candidate)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	hash := settingsHash(doc.JSONBytes)
	if hash != "" {
		if _, _, _, ok := parseSettingsPasswordHash(hash); !ok {
			return "", newUnrecognizedSettingsLock(doc.Path)
		}
	}
	return hash, nil
}

func (s *ConfigStore) LockSettings(scope Scope, password string) error {
	return s.settings.LockSettings(scope, password)
}

func (s *ConfigStore) UnlockSettings(scope Scope, password string) error {
	return s.settings.UnlockSettings(scope, password)
}

// SetSettingsAuditSink bridges the production audit sink to the settings
// component: the store-level API is kept for existing callers while the
// sink itself now lives in internal/config/store/settings.
func SetSettingsAuditSink(f func(SettingsEvent)) {
	if f == nil {
		settings.SetSettingsAuditSink(nil)
		return
	}
	settings.SetSettingsAuditSink(func(ev settings.SettingsEvent) {
		f(SettingsEvent{
			Scope: ev.Scope, Path: ev.Path, Keys: ev.Keys,
			Models: ev.Models, Outcome: ev.Outcome, Reason: ev.Reason,
			Source: ev.Source,
		})
	})
}
