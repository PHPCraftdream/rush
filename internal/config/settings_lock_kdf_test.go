package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// revert-check RC-kdf3: reverting VerifyPassword to accept 64-hex legacy
// values (or treating parse failure as a match) makes this test red — the
// legacy sync_rev fixture would be accepted and the writes would succeed.
func TestLegacyHexHashFailsClosedEndToEnd(t *testing.T) {
	legacy := "24a5cfa44bee3072cadb1fea7af9a0ed638d118d63e21d8f5d6979e314b5318e"
	t.Run("direct", func(t *testing.T) {
		require.False(t, VerifyPassword("secret", legacy))
	})
	t.Run("end-to-end", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rush.json")
		fixture := `{"sync_rev":"` + legacy + `"}`
		require.NoError(t, os.WriteFile(path, []byte(fixture), 0o600))
		s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path})
		t.Cleanup(func() { SetProcessPassword("") })
		err := s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "x", Model: "y"})
		require.ErrorIs(t, err, ErrSettingsLocked)
		require.Equal(t, []byte(fixture), fileBytes(t, path), "the refused write must not touch the file")
		err = s.UnlockSettings(ScopeGlobal, "anything")
		var unrecognized *ErrSettingsLockUnrecognized
		require.ErrorAs(t, err, &unrecognized)
		require.ErrorIs(t, err, ErrSettingsLocked)
		require.True(t, filepath.IsAbs(unrecognized.Path))
		require.Equal(t, path, unrecognized.Path)
		require.Equal(t, []byte(fixture), fileBytes(t, path), "the refused unlock must not touch the file")
	})
}

func TestUnrecognizedLocksCarryAbsolutePathAndRemainUntouched(t *testing.T) {
	for name, stored := range map[string]string{
		"legacy":    "24a5cfa44bee3072cadb1fea7af9a0ed638d118d63e21d8f5d6979e314b5318e",
		"malformed": "pbkdf2-sha256$bad$!!!$bad",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "rush.json")
			fixture := `{"sync_rev":"` + stored + `"}`
			require.NoError(t, os.WriteFile(path, []byte(fixture), 0o600))
			s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path})
			SetProcessPassword("")
			t.Cleanup(func() { SetProcessPassword("") })

			err := s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "x", Model: "y"})
			require.ErrorIs(t, err, ErrSettingsLocked)
			var unrecognized *ErrSettingsLockUnrecognized
			require.ErrorAs(t, err, &unrecognized)
			require.True(t, filepath.IsAbs(unrecognized.Path))
			require.Equal(t, []byte(fixture), fileBytes(t, path))

			err = s.UnlockSettings(ScopeGlobal, "anything")
			require.ErrorIs(t, err, ErrSettingsLocked)
			require.ErrorAs(t, err, &unrecognized)
			require.Equal(t, path, unrecognized.Path)
			require.Equal(t, []byte(fixture), fileBytes(t, path))
		})
	}
}

func TestUnrecognizedLockSourceErrorsStayGeneric(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"RUSH_GLOBAL_DATA", "XDG_DATA_HOME"} {
		t.Setenv(key, filepath.Join(root, "data"))
	}
	for _, key := range []string{"RUSH_GLOBAL_CONFIG", "XDG_CONFIG_HOME"} {
		t.Setenv(key, filepath.Join(root, "config"))
	}
	path := filepath.Join(root, "data", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	fixture := `{"sync_rev":"pbkdf2-sha256$bad$!!!$bad"}`
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o600))
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path})
	(*s.WorkingDirForTest()) = filepath.Join(root, "workspace")
	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })

	var unrecognized *ErrSettingsLockUnrecognized
	err := SettingsWriteAllowed((*s.WorkingDirForTest()))
	require.ErrorAs(t, err, &unrecognized)
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, path, unrecognized.Path)

	err = s.CheckProcessPassword()
	require.ErrorAs(t, err, &unrecognized)
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, path, unrecognized.Path)
}

// revert-check RC-kdf4: removing the [1, 10_000_000] iteration bound in
// VerifyPassword makes the 1<<30 case red (a long-running PBKDF2 derivation
// blows the 100ms deadline), and removing any parse check makes its case
// panic or return true.
func TestVerifyPasswordRejectsHostileStoredValuesFast(t *testing.T) {
	t.Parallel()
	for _, stored := range []string{
		"",
		"pbkdf2-sha256",
		"pbkdf2-sha256$0$AAAA$AAAA",
		"pbkdf2-sha256$-1$AAAA$AAAA",
		"pbkdf2-sha256$1073741824$AAAA$AAAA",
		"pbkdf2-sha256$abc$AAAA$AAAA",
		"pbkdf2-sha256$600000$AAAA",
		"pbkdf2-sha256$600000$!!!$AAA",
		"sha256$600000$AAAA$AAAA",
		"pbkdf2-sha256$600000$AAAA$",
	} {
		t.Run(stored, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			require.False(t, VerifyPassword("secret", stored))
			elapsed := time.Since(start)
			// Only parse-rejected cases must be provably fast; a well-formed
			// stored value legitimately runs PBKDF2 at full cost.
			parts := strings.Split(stored, "$")
			parseRejected := len(parts) != 4 || parts[0] != "pbkdf2-sha256" ||
				stored == "pbkdf2-sha256$0$AAAA$AAAA" ||
				stored == "pbkdf2-sha256$-1$AAAA$AAAA" ||
				stored == "pbkdf2-sha256$1073741824$AAAA$AAAA" ||
				stored == "pbkdf2-sha256$abc$AAAA$AAAA"
			if parseRejected {
				require.Less(t, elapsed, 100*time.Millisecond, "parse-rejected stored value must be rejected without running PBKDF2")
			}
		})
	}
}
