package settings

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

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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
	doc, err := docread.ReadSettingsDocument(datadir.GlobalConfigData())
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
		{path: datadir.GlobalConfigData(), global: true},
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

func (s *Store) LockState() (LockState, error) {
	global, err := freshSettingsHash(s.port.GlobalDataPath())
	if err != nil {
		return LockState{}, err
	}
	local, err := freshSettingsHash(s.workspacePath())
	if err != nil {
		return LockState{}, err
	}
	return LockState{Global: global != "", Local: local != ""}, nil
}

func (s *Store) SettingsLocked() (bool, error) {
	state, err := s.LockState()
	return state.Global || state.Local, err
}

func (s *Store) CheckProcessPassword() error {
	process := processPassword()
	global, local, err := s.currentLockHashes()
	if err != nil {
		return settingsLockReadError(err)
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

func (s *Store) currentLockHashes() ([]string, []string, error) {
	globals, locals, err := collectSettingsSourceHashes([]settingsLockSource{
		{path: s.port.GlobalDataPath(), global: true},
		{path: canonicalGlobalDataPath(), global: true},
		{path: s.workspacePath()},
		{path: naturalWorkspaceSettingsPath(s.port.WorkingDir())},
	})
	if err != nil {
		return nil, nil, err
	}
	return globals, locals, nil
}

// FreshSettingsHash reads the sync_rev hash from the slot at path (either
// spelling); "" when absent, fail-closed error on unrecognized hashes.
func FreshSettingsHash(path string) (string, error) { return freshSettingsHash(path) }

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
func (s *Store) GuardWrite(path string, data []byte, keys []string, authority cfgmodel.SettingsWriteAuthority) (string, error) {
	return s.guardSettingsWriteForAuthority(path, data, keys, authority)
}

func (s *Store) guardSettingsWrite(path string, data []byte, keys []string) (string, error) {
	return s.guardSettingsWriteForAuthority(path, data, keys, cfgmodel.SettingsAuthorityGuarded)
}

// guardSettingsWriteForAuthority is guardSettingsWrite parameterised on the
// caller's write authority. The reserved-key and recent-models rules apply
// to every authority; the settings-lock veto is the only check the
// trusted-UI authority lifts, and it lifts it for this one call only.
func (s *Store) guardSettingsWriteForAuthority(path string, data []byte, keys []string, authority cfgmodel.SettingsWriteAuthority) (string, error) {
	for _, key := range keys {
		if TouchesReservedKey(key) {
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
	if authority == cfgmodel.SettingsAuthorityTrustedUI {
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
	// the backup-only rule live inside docread.ReadSettingsDocument): this guard is
	// the write-path source reader (checkExternalSettingsSources,
	// lockHashesForTarget) and must never read a backup-only slot as
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

func pathWithinDirectory(directory, path string) bool {
	rel, err := filepath.Rel(directory, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (s *Store) lockHashesForTarget(path string, data []byte) ([]string, []string, error) {
	workspace := s.workspacePath()
	natural := naturalWorkspaceSettingsPath(s.port.WorkingDir())
	canonical := canonicalGlobalDataPath()
	globals := []string{}
	locals := []string{}
	for _, source := range []struct {
		path string
		kind string
	}{{s.port.GlobalDataPath(), "global"}, {canonical, "global"}, {workspace, "local"}, {natural, "local"}} {
		if source.path == "" {
			continue
		}
		hash, err := readSettingsHash(source.path, path, data)
		if err != nil {
			return nil, nil, err
		}
		if docread.NormalizeReloadPath(source.path) == docread.NormalizeReloadPath(path) {
			continue
		}
		if source.kind == "global" && docread.NormalizeReloadPath(source.path) == docread.NormalizeReloadPath(canonical) && docread.NormalizeReloadPath(source.path) != docread.NormalizeReloadPath(s.port.GlobalDataPath()) && docread.NormalizeReloadPath(path) == docread.NormalizeReloadPath(s.port.GlobalDataPath()) {
			continue
		}
		if source.kind == "local" && workspace != "" && natural != "" && docread.NormalizeReloadPath(source.path) == docread.NormalizeReloadPath(natural) && docread.NormalizeReloadPath(natural) != docread.NormalizeReloadPath(workspace) && pathWithinDirectory(filepath.Dir(workspace), path) {
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

func (s *Store) LockSettings(scope Scope, password string) error {
	if password == "" {
		return errors.New("a password is required")
	}
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return err
	}
	path = slots.WriteTargetSlot(path)
	refusedBy := ""
	err = s.writeSettingsLockRaw(path, func(data []byte) (string, error) {
		if err := s.checkExternalSettingsSources(path, data); err != nil {
			return "", err
		}
		written, err := sjson.SetBytes(data, syncRevKey, HashPassword(password))
		if err != nil {
			return "", err
		}
		return cfgmodel.ApplyModelsIntegrity(string(written))
	})
	s.emitLockEvent(scope, path, err, refusedBy)
	return err
}

func (s *Store) UnlockSettings(scope Scope, password string) error {
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return err
	}
	path = slots.WriteTargetSlot(path)
	refusedBy := ""
	err = s.writeSettingsLockRaw(path, func(data []byte) (string, error) {
		globals, locals, hashErr := s.lockHashesForTarget(path, data)
		if hashErr != nil {
			return "", settingsLockReadError(hashErr)
		}
		hash := settingsHash(data)
		if hash == "" {
			return "", ErrNotLocked
		}
		if _, _, _, ok := parseSettingsPasswordHash(hash); !ok {
			return "", newUnrecognizedSettingsLock(path)
		}
		if !VerifyPassword(password, hash) {
			return "", ErrWrongPassword
		}
		process := processPassword()
		checks := globals
		if len(checks) == 0 && !(docread.NormalizeReloadPath(path) == docread.NormalizeReloadPath(s.port.GlobalDataPath()) && hash != "") {
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

func (s *Store) writeSettingsLockRaw(path string, mutate func(data []byte) (string, error)) error {
	var committedErr error
	err := s.port.WithFileWriteLock(path, func(target WriteTarget) error {
		raw, expected, readErr := docread.ReadStableConfigFileOwned(target.SelectedPath, target.Owner, target.Enforce)
		var doc docread.SettingsDocument
		if readErr != nil {
			if os.IsNotExist(readErr) {
				doc = docread.SettingsDocument{JSONBytes: []byte("{}"), Format: docread.SettingsDocumentFormatOf(target.SelectedPath)}
			} else {
				return ErrSettingsLocked
			}
		} else {
			var decodeErr error
			doc, decodeErr = docread.DecodeSettingsDocument(target.SelectedPath, raw, expected)
			if decodeErr != nil {
				return decodeErr
			}
		}
		newJSON, err := mutate(doc.JSONBytes)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target.Path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		written, err := docread.EncodeSettingsDocument([]byte(newJSON), doc.Format)
		if err != nil {
			return err
		}
		if !expected.Exists {
			_, expected, err = docread.ReadStableConfigFileOwned(target.SelectedPath, target.Owner, target.Enforce)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to verify config file before write: %w", err)
			}
		}
		_, commitErr := cfgcommit.CommitConfigFile(target.SelectedPath, target.Path, written, 0o600, expected, target.Owner, target.Enforce)
		if commitErr != nil {
			if outcome, ok := cfgcommit.CommitOutcomeFromError(commitErr); ok && outcome.Committed {
				s.port.NoteInitialLoadWrite(path, written)
				committedErr = commitErr
				return nil
			}
			return fmt.Errorf("failed to write config file: %w", commitErr)
		}
		s.port.NoteInitialLoadWrite(path, written)
		return nil
	})
	if err != nil {
		return err
	}
	if committedErr != nil {
		if reloadErr := s.port.AutoReload(); reloadErr != nil {
			return errors.Join(committedErr, reloadErr)
		}
		return committedErr
	}
	if err := s.port.AutoReload(); err != nil {
		slogConfigReloadWarning(path, err)
	}
	return nil
}

func (s *Store) emitLockEvent(scope Scope, path string, err error, refusedBy string) {
	ev := SettingsEvent{Scope: SettingsScope(scope), Path: path, Keys: []string{"*"}}
	FinalizeSettingsEvent(&ev, refusedBy, err)
	EmitSettingsEvent(ev)
}

func (s *Store) ScopeForPath(path string) string {
	if docread.NormalizeReloadPath(path) == docread.NormalizeReloadPath(s.port.GlobalDataPath()) {
		return "global"
	}
	return "workspace"
}

// slogConfigReloadWarning logs a reload failure after a completed settings write.
func slogConfigReloadWarning(path string, err error) {
	slog.Warn("Config file updated but failed to reload in-memory state", "path", path, "error", err)
}
