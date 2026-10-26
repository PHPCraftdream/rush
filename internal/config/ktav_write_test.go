// Ktav write-path tests: SetConfigFields / RemoveConfigField / HasConfigField
// on a rush.ktav target now SUCCEED through the settings-document layer
// (decode Ktav -> mutate JSON -> re-encode Ktav -> commit against the raw
// fingerprint). These tests replace the former (D) write-guard tests that
// asserted errKtavWriteNotSupported.
//
// Revert-checks (orchestrator runs the mutants):
//   - TestKtavWriteSetRemoveHasRoundTrip -> making SetConfigFields route
//     around writeTargetSlot (store_write.go) would write rush.json.
//   - TestKtavWriteTypePreservation -> re-typing string values that look like
//     numbers/bools during the Ktav round trip (ktav_doc.go encode path).
//   - TestKtavWritePreservesUnknownFields -> dropping unmapped top-level
//     fields during ConvertJSONToKtav.
//   - TestKtavCorruptKtavAbortsWrite -> swallowing decode errors as "file
//     absent" and recreating the document (ktav_doc.go decodeSettingsDocument).
//   - TestKtavWriteRejectsConcurrentModificationABA -> committing against the
//     JSON-normalized fingerprint instead of the RAW bytes fingerprint.
//   - TestKtavLockUnlockSyncRevRoundTrip -> writing sync_rev without encoding
//     back through the target format (settings_lock.go writeSettingsLockRaw).
//   - TestKtavCreationRespectsAutoMigrateGate / TestKtavActiveSlotNeverCreatesLegacyJSON
//     -> flipping newConfigFileIsKtav or bypassing writeTargetSlot.
//   - TestRejectUnconvertedKtavWriteBoundary -> weakening rejectUnconvertedKtavWrite
//     (ktav_doc.go) at the wave-B (oauth/mcp) seams.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ktavWriteStore isolates global settings paths and returns a store whose
// global scope is path (a rush.ktav in these tests). workingDir stays empty
// so autoReloadAfterWrite is a no-op and the tests observe DISK, not the
// in-memory snapshot.
func ktavWriteStore(t *testing.T, path string) *ConfigStore {
	t.Helper()
	ktavIsolateGlobals(t)
	return newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path})
}

// ktavDecodedJSON reads path and returns its decoded JSON-shaped bytes.
func ktavDecodedJSON(t *testing.T, path string) []byte {
	t.Helper()
	return mustDecodeTestKtav(t, fileBytes(t, path))
}

// TestKtavWriteSetRemoveHasRoundTrip is the rewritten (D) guard test: a write
// to a .ktav target succeeds, lands in the SAME file as valid Ktav, decodes
// back to the expected JSON-shaped content, preserves MCP records and unknown
// future top-level fields, and never creates or modifies the sibling
// rush.json.
func TestKtavWriteSetRemoveHasRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	legacyPath := filepath.Join(dir, "rush.json")
	seed := `{"providers":{"p":{"api_key":"seed-key"}},` +
		`"mcp":{"srv":{"type":"http","url":"http://x.example"}},` +
		`"future_unknown_field":{"nested":[1,2,"three"]}}`
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, seed), 0o600))

	store := ktavWriteStore(t, ktavPath)

	// Set: succeeds (the old guard refused with errKtavWriteNotSupported).
	require.NoError(t, store.SetConfigFields(ScopeGlobal, map[string]any{
		"providers.p.api_key": "rewritten",
		"options.debug":       true,
	}))

	// HasConfigField goes through the decode layer too.
	require.True(t, store.HasConfigField(ScopeGlobal, "providers.p.api_key"))
	require.True(t, store.HasConfigField(ScopeGlobal, "future_unknown_field"))
	require.False(t, store.HasConfigField(ScopeGlobal, "options.missing"))

	doc := string(ktavDecodedJSON(t, ktavPath))
	require.Equal(t, "rewritten", gjson.Get(doc, "providers.p.api_key").String())
	require.True(t, gjson.Get(doc, "options.debug").Bool())

	// The committed bytes must still decode as Ktav (mustDecodeTestKtav
	// requires ktavcodec.DecodeKtav to succeed, which plain JSON does not):
	// the write must land in the SAME file, re-encoded to the target format.
	ktavDecodedJSON(t, ktavPath)

	// Unknown future top-level fields survive byte-semantically.
	var before, after any
	require.NoError(t, json.Unmarshal([]byte(gjson.Get(seed, "future_unknown_field").Raw), &before))
	require.NoError(t, json.Unmarshal([]byte(gjson.Get(doc, "future_unknown_field").Raw), &after))
	require.Equal(t, before, after)

	// MCP records survive.
	mcp := gjson.Get(doc, "mcp.srv")
	require.True(t, mcp.Exists())
	require.Equal(t, "http", gjson.Get(doc, "mcp.srv.type").String())
	require.Equal(t, "http://x.example", gjson.Get(doc, "mcp.srv.url").String())

	// Remove: succeeds and only removes the named key.
	require.NoError(t, store.RemoveConfigField(ScopeGlobal, "providers.p.api_key"))
	require.False(t, store.HasConfigField(ScopeGlobal, "providers.p.api_key"))
	doc = string(ktavDecodedJSON(t, ktavPath))
	require.False(t, gjson.Get(doc, "providers.p.api_key").Exists())
	require.True(t, gjson.Get(doc, "providers.p").Exists(), "removing a leaf must keep its parent object")
	require.True(t, gjson.Get(doc, "options.debug").Exists(), "remove must not lose sibling keys")

	// The legacy sibling is never created or modified by any of this.
	require.NoFileExists(t, legacyPath)
}

// TestKtavWriteTypePreservation pins that every value written to a .ktav
// target round-trips as the SAME JSON type and content: strings that look
// like numbers, booleans, null, or shell syntax stay strings; real JSON
// numbers/booleans stay numbers/booleans.
func TestKtavWriteTypePreservation(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		value    any
		wantType gjson.Type
		wantRaw  string // expected gjson .Raw in the decoded JSON document
	}{
		{"bool-looking string", "v.boolish", "true", gjson.String, `"true"`},
		{"null-looking string", "v.nullish", "null", gjson.String, `"null"`},
		{"zero string", "v.zero", "0", gjson.String, `"0"`},
		{"leading-zero string", "v.leading", "01", gjson.String, `"01"`},
		{"decimal string", "v.dec", "1.5", gjson.String, `"1.5"`},
		{"empty string", "v.empty", "", gjson.String, `""`},
		{"newline string", "v.nl", "a\nb", gjson.String, `"a\nb"`},
		{"env var string", "v.env", "$VAR", gjson.String, `"$VAR"`},
		{"command substitution", "v.cmd", "$(cmd)", gjson.String, `"$(cmd)"`},
		{"windows path", "v.win", `C:\Users\x\y`, gjson.String, `"C:\\Users\\x\\y"`},
		{"hash-prefix key", `\#\#anchor`, "anchored", gjson.String, `"anchored"`},
		{"real bool", "v.realbool", true, gjson.True, `true`},
		{"real int", "v.int", 42, gjson.Number, `42`},
		{"real float", "v.float", 1.5, gjson.Number, `1.5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ktavPath := filepath.Join(dir, "rush.ktav")
			require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, `{}`), 0o600))
			store := ktavWriteStore(t, ktavPath)

			require.NoError(t, store.SetConfigField(ScopeGlobal, tc.key, tc.value))

			doc := ktavDecodedJSON(t, ktavPath)
			got := gjson.GetBytes(doc, tc.key)
			require.True(t, got.Exists(), "key %q must exist after the round trip", tc.key)
			require.Equal(t, tc.wantType, got.Type, "key %q must keep its JSON type", tc.key)
			require.Equal(t, tc.wantRaw, got.Raw, "key %q must round-trip byte-identically", tc.key)
		})
	}

	t.Run("dotted key via escaped path", func(t *testing.T) {
		dir := t.TempDir()
		ktavPath := filepath.Join(dir, "rush.ktav")
		require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, `{}`), 0o600))
		store := ktavWriteStore(t, ktavPath)

		// Same escaping convention the JSON editor path already uses: a
		// backslash-escaped dot is a LITERAL dot in the key, not a path
		// separator (sjson/gjson escaped-path syntax).
		const key = `opts\.dotted`
		require.NoError(t, store.SetConfigField(ScopeGlobal, key, "literal"))
		doc := ktavDecodedJSON(t, ktavPath)
		require.Equal(t, "literal", gjson.GetBytes(doc, key).String())
		require.True(t, store.HasConfigField(ScopeGlobal, key))
	})
}

// TestKtavWritePreservesUnknownFields pins that a made-up top-level field
// never named by production code survives a SetConfigFields write to a .ktav
// target, byte-semantically, in the decoded JSON.
func TestKtavWritePreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	unknown := `{"top-secret-future":{"a":[1,{"b":"c"}],"z":null},"keep":"me"}`
	seed := `{"providers":{"p":{"api_key":"k"}},"made_up_future_field":` + unknown + `}`
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, seed), 0o600))

	store := ktavWriteStore(t, ktavPath)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))

	var expected, actual any
	require.NoError(t, json.Unmarshal([]byte(unknown), &expected))
	require.NoError(t, json.Unmarshal([]byte(gjson.Get(string(ktavDecodedJSON(t, ktavPath)), "made_up_future_field").Raw), &actual))
	require.Equal(t, expected, actual, "the unknown top-level field must survive byte-semantically")
}

// TestKtavCorruptKtavAbortsWrite pins that an undecodable active .ktav
// document ABORTS the write: SetConfigFields fails, the file bytes are
// unchanged, and neither the error nor the logs echo any seeded document
// values.
func TestKtavCorruptKtavAbortsWrite(t *testing.T) {
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	// These bytes fail DecodeKtav (not a Ktav document at all) and CARRY a
	// secret: the redacted codec diagnostics must never echo it.
	corrupt := []byte("k: " + ktavTestSecret + "\nk: " + ktavTestSecret + "\n")
	require.NoError(t, os.WriteFile(ktavPath, corrupt, 0o600))

	// Sanity: the corruption actually fails the codec's decode path.
	_, decodeErr := decodeConfigDocument(ktavPath, corrupt)
	require.Error(t, decodeErr, "fixture must be undecodable")
	require.NotContains(t, decodeErr.Error(), ktavTestSecret)

	store := ktavWriteStore(t, ktavPath)
	err := store.SetConfigFields(ScopeGlobal, map[string]any{"providers.p.api_key": "new"})
	require.Error(t, err, "a decode failure must abort the write")
	require.NotContains(t, err.Error(), ktavTestSecret)
	require.Equal(t, corrupt, fileBytes(t, ktavPath), "the corrupt file must be left byte-identical")
	require.NoFileExists(t, filepath.Join(dir, "rush.json"))

	require.Error(t, store.RemoveConfigField(ScopeGlobal, "providers.p.api_key"))
	require.Equal(t, corrupt, fileBytes(t, ktavPath))
}

// TestKtavWriteRejectsConcurrentModificationABA is the Ktav mirror of the
// JSON raw-fingerprint ABA check: an external mutation of rush.ktav between
// the read and the commit is refused with the SAME staleness error shape
// (errConfigCommitVerification) as the JSON path, and the external bytes win.
func TestKtavWriteRejectsConcurrentModificationABA(t *testing.T) {
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	original := testKtavBytes(t, `{"providers":{"p":{"api_key":"original"}}}`)
	require.NoError(t, os.WriteFile(ktavPath, original, 0o600))

	// Interpose at the commit boundary: AFTER the store has read the file and
	// built its mutation, a competing writer replaces the bytes. The commit's
	// re-verification of the raw fingerprint must detect the change.
	var interfere sync.Once
	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitCheck
	configTestHooks.beforeCommitCheck = func() {
		interfere.Do(func() {
			external := testKtavBytes(t, `{"providers":{"p":{"api_key":"competing-writer"}}}`)
			require.NoError(t, os.WriteFile(ktavPath, external, 0o600))
		})
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitCheck = previous
		configTestHooks.Unlock()
	})

	store := ktavWriteStore(t, ktavPath)
	err := store.SetConfigFields(ScopeGlobal, map[string]any{"options.debug": true})
	require.ErrorIs(t, err, errConfigCommitVerification,
		"a raw-fingerprint ABA on a .ktav target must be refused exactly like the JSON path")
	require.Equal(t, "competing-writer",
		gjson.Get(string(ktavDecodedJSON(t, ktavPath)), "providers.p.api_key").String(),
		"the competing writer's bytes must win")
	require.False(t, gjson.Get(string(ktavDecodedJSON(t, ktavPath)), "options.debug").Exists(),
		"the stale writer's mutation must not land on top of the competing write")
}

// TestKtavLockUnlockSyncRevRoundTrip mirrors the JSON lock flow on a
// rush.ktav store: LockSettings persists sync_rev (readable through the
// decode layer), the wrong password is refused, the right password clears it,
// and the process-password hash checks read the .ktav source.
func TestKtavLockUnlockSyncRevRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t,
		`{"providers":{"p":{"api_key":"k"}},"mcp":{"srv":{"type":"http","url":"http://x"}}}`), 0o600))
	s := lockStore(t, ktavPath)

	// Fresh hash behavior against a .ktav file.
	hash, err := freshSettingsHash(ktavPath)
	require.NoError(t, err)
	require.Equal(t, "", hash)

	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	lockedDoc := string(ktavDecodedJSON(t, ktavPath))
	require.True(t, VerifyPassword("secret", gjson.Get(lockedDoc, "sync_rev").String()))
	require.True(t, gjson.Get(lockedDoc, "mcp.srv").Exists(), "locking must preserve MCP records")
	hash, err = freshSettingsHash(ktavPath)
	require.NoError(t, err)
	require.True(t, VerifyPassword("secret", hash))
	state, err := s.LockState()
	require.NoError(t, err)
	require.True(t, state.Global)

	// Wrong password is refused and changes nothing.
	before := fileBytes(t, ktavPath)
	require.ErrorIs(t, s.UnlockSettings(ScopeGlobal, "wrong"), ErrWrongPassword)
	require.Equal(t, before, fileBytes(t, ktavPath))

	// The right password clears sync_rev.
	require.NoError(t, s.UnlockSettings(ScopeGlobal, "secret"))
	unlockedDoc := string(ktavDecodedJSON(t, ktavPath))
	require.False(t, gjson.Get(unlockedDoc, "sync_rev").Exists())
	require.True(t, gjson.Get(unlockedDoc, "mcp.srv").Exists())
	hash, err = freshSettingsHash(ktavPath)
	require.NoError(t, err)
	require.Equal(t, "", hash)

	// Process-password hash checks read the .ktav source.
	require.NoError(t, s.LockSettings(ScopeGlobal, "pw"))
	SetProcessPassword("wrong")
	require.ErrorIs(t, s.CheckProcessPassword(), ErrWrongPassword)
	SetProcessPassword("pw")
	require.NoError(t, s.CheckProcessPassword())
}

// TestKtavCreationRespectsAutoMigrateGate pins the CURRENT gate value: with
// NEITHER file present, a gate-on write creates rush.ktav (KTAV-4 made the
// gate default on), and a gate-off write still creates the legacy rush.json.
func TestKtavCreationRespectsAutoMigrateGate(t *testing.T) {
	// This test pins the gate-ON creation behavior (rush.ktav created with
	// neither file present); the package test init() defaults the gate off,
	// so opt in explicitly. NOT parallel: the helper mutates a shared global.
	enableKtavAutoMigrateForTest(t)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "rush.json")
	ktavPath := filepath.Join(dir, "rush.ktav")
	require.True(t, newConfigFileIsKtav(), "the current build defaults to auto-creating rush.ktav (KTAV-4)")

	store := ktavWriteStore(t, jsonPath)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))
	require.FileExists(t, ktavPath, "with neither file present, the Ktav slot is created")
	require.NoFileExists(t, jsonPath)
	require.Equal(t, "true", gjson.Get(string(ktavDecodedJSON(t, ktavPath)), "options.debug").String())

	// Gate off (explicitly): the legacy JSON slot is created instead, and no
	// Ktav file appears. NOT parallel: mutates the shared gate global.
	prev := ktavAutoMigrate
	ktavAutoMigrate = false
	t.Cleanup(func() { ktavAutoMigrate = prev })
	jsonPathOff := filepath.Join(t.TempDir(), "rush.json")
	ktavPathOff := filepath.Join(filepath.Dir(jsonPathOff), "rush.ktav")
	storeOff := ktavWriteStore(t, jsonPathOff)
	require.NoError(t, storeOff.SetConfigField(ScopeGlobal, "options.debug", true))
	require.FileExists(t, jsonPathOff, "with the gate off, the legacy JSON slot is created")
	require.NoFileExists(t, ktavPathOff)
	require.Equal(t, "true", gjson.Get(string(fileBytes(t, jsonPathOff)), "options.debug").String())
}

// TestKtavActiveSlotNeverCreatesLegacyJSON pins writeTargetSlot from the
// write path: when rush.ktav is the slot's selected file, NO write — set or
// remove — ever creates the legacy rush.json.
func TestKtavActiveSlotNeverCreatesLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "rush.json")
	ktavPath := filepath.Join(dir, "rush.ktav")
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, `{"providers":{"p":{"api_key":"k"}}}`), 0o600))

	// The store resolves the LEGACY path (as configPath(ScopeGlobal) does);
	// writeTargetSlot must redirect every mutation to the active Ktav slot.
	store := ktavWriteStore(t, jsonPath)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "providers.p.api_key", "via-ktav"))
	require.NoError(t, store.RemoveConfigField(ScopeGlobal, "providers.p.api_key"))
	require.NoError(t, store.LockSettings(ScopeGlobal, "pw"))

	require.NoFileExists(t, jsonPath, "an active rush.ktav slot must never spawn rush.json")
	doc := string(ktavDecodedJSON(t, ktavPath))
	require.False(t, gjson.Get(doc, "providers.p.api_key").Exists())
	require.True(t, VerifyPassword("pw", gjson.Get(doc, "sync_rev").String()))
}

// TestRejectUnconvertedKtavWriteBoundary pins the wave-B seam helper used by
// store_oauth/store_mcp: a not-yet-converted write boundary must reject a
// .ktav target with errKtavWriteNotSupported (errors.Is) and pass a .json
// target through unchanged.
func TestRejectUnconvertedKtavWriteBoundary(t *testing.T) {
	dir := t.TempDir()
	require.ErrorIs(t, rejectUnconvertedKtavWrite(filepath.Join(dir, "rush.ktav")), errKtavWriteNotSupported)
	require.ErrorIs(t, rejectUnconvertedKtavWrite(filepath.Join(dir, "other.ktav")), errKtavWriteNotSupported)
	require.NoError(t, rejectUnconvertedKtavWrite(filepath.Join(dir, "rush.json")))
	require.NoError(t, rejectUnconvertedKtavWrite(""))

	// The sentinel stays distinct from every public lock error.
	for _, sentinel := range []error{ErrSettingsLocked, ErrWrongPassword, ErrNotLocked} {
		require.NotErrorIs(t, rejectUnconvertedKtavWrite(filepath.Join(dir, "rush.ktav")), sentinel)
	}
	require.True(t, errors.Is(rejectUnconvertedKtavWrite(filepath.Join(dir, "rush.ktav")), errKtavWriteNotSupported))
}
