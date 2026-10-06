package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const cacheRevKey = "cache_rev"

// Constant mixed into the cache revision hash; the name is deliberately generic.
const hashSalt = "7c1f4e9a2b8d6350af02c4e8b19d5736"

var ErrModelsEditedDirectly = errors.New("models cannot be changed by editing the settings file directly; they can only be changed with the rush models commands (ask the user if that is refused)")

// settingsKeyFirstComponent extracts the first sjson path component.
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

// canonicalModelsJSON canonicalizes a models value for hashing. Absent, null,
// or empty values are treated as not present.
func canonicalModelsJSON(raw string) (canonical string, present bool, err error) {
	value := gjson.Parse(raw)
	if !value.Exists() || value.Type == gjson.Null {
		return "", false, nil
	}
	if value.IsObject() && len(value.Map()) == 0 {
		return "", false, nil
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return "", false, err
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

// modelsIntegritySignature hashes the canonicalized models value.
func modelsIntegritySignature(canonical string) string {
	digest := sha256.Sum256([]byte("v1|" + hashSalt + "|" + canonical))
	return hex.EncodeToString(digest[:])
}

// applyModelsIntegrity refreshes cache_rev on writes that touch models.
func applyModelsIntegrity(doc string) (string, error) {
	canonical, present, err := canonicalModelsJSON(gjson.Get(doc, "models").Raw)
	if err != nil {
		return "", fmt.Errorf("failed to canonicalize model settings: %w", err)
	}
	if present {
		return sjson.Set(doc, cacheRevKey, modelsIntegritySignature(canonical))
	}
	return sjson.Delete(doc, cacheRevKey)
}

// touchesModelsKey reports whether any key targets the models object.
func touchesModelsKey(keys []string) bool {
	for _, key := range keys {
		if settingsKeyFirstComponent(key) == "models" {
			return true
		}
	}
	return false
}

// anySettingsLockExists reports whether any settings lock file is present.
// A read error counts as "lock exists" and fails closed.
func (s *ConfigStore) anySettingsLockExists() bool {
	sources := []settingsLockSource{
		{path: GlobalConfigData(), global: true},
		{path: canonicalGlobalDataPath(), global: true},
		{path: s.loadSnapshot().workspacePath},
		{path: naturalWorkspaceSettingsPath(s.workingDir)},
		{path: filepath.Join(s.workingDir, "rush.json")},
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
		data, _, err := readStableConfigFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			mismatch = true
			break
		}
		canonical, present, err := canonicalModelsJSON(gjson.GetBytes(data, "models").Raw)
		if err != nil {
			mismatch = true
			break
		}
		if !present {
			continue
		}
		rev := gjson.GetBytes(data, cacheRevKey)
		if !rev.Exists() {
			// Genuinely absent: legacy file. Accepted only when unlocked.
			if s.anySettingsLockExists() {
				mismatch = true
			}
			continue
		}
		// Present but not a valid signature string: a mismatch, locked or not.
		if rev.Type != gjson.String || rev.String() == "" {
			mismatch = true
			break
		}
		if rev.String() != modelsIntegritySignature(canonical) {
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
