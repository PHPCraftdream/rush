package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// TestReadAllModelsAtScope_ReadsKtavSlot pins that the per-scope models
// reader goes through the format-aware document layer: with only a rush.ktav
// in the slot (the active spelling under KTAV-4), the reader must decode the
// Ktav document and return the models, not fail with a JSON parse error.
// Uses the white-box store harness so no Load/Init (and hence no provider
// validation) runs; the ktav gate is left at its default ON value and is not
// flipped, so no parallel-hostile global mutation happens either.
func TestReadAllModelsAtScope_ReadsKtavSlot(t *testing.T) {
	ktavLightIsolateEnv(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	global := filepath.Join(dir, "rush.json")
	jsonDoc := []byte(`{"models":{` +
		`"smart":{"provider":"p","model":"m"},` +
		`"fast":{"provider":"p2","model":"m2"}}}`)
	ktavDoc, err := ktavcodec.ConvertJSONToKtav(jsonDoc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ktavSiblingOf(global), ktavDoc, 0o644))

	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: global})
	all, err := s.ReadAllModelsAtScope(ScopeGlobal)
	require.NoError(t, err)
	require.NotNil(t, all[SelectedModelTypeSmart])
	require.Equal(t, "p", all[SelectedModelTypeSmart].Provider)
	require.Equal(t, "m", all[SelectedModelTypeSmart].Model)
	require.NotNil(t, all[SelectedModelTypeFast])
	require.Equal(t, "p2", all[SelectedModelTypeFast].Provider)
	require.Equal(t, "m2", all[SelectedModelTypeFast].Model)
	require.Nil(t, all[SelectedModelTypeWorker])
	require.Nil(t, all[SelectedModelTypeReviewer])
}

// TestReadAllModelsAtScope_MissingSlotIsEmpty pins the os.IsNotExist
// semantics: a slot with no document at all reads back as an empty map, not
// an error.
func TestReadAllModelsAtScope_MissingSlotIsEmpty(t *testing.T) {
	ktavLightIsolateEnv(t)
	global := filepath.Join(t.TempDir(), "rush.json")
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: global})
	all, err := s.ReadAllModelsAtScope(ScopeGlobal)
	require.NoError(t, err)
	require.Empty(t, all)
}

// TestReadAllModelsAtScope_InvalidKtavFailsClosed pins the fail-closed
// behavior: a definitively invalid Ktav document (duplicated top-level key)
// surfaces as an error, never as an empty model set.
func TestReadAllModelsAtScope_InvalidKtavFailsClosed(t *testing.T) {
	ktavLightIsolateEnv(t)
	global := filepath.Join(t.TempDir(), "rush.json")
	require.NoError(t, os.WriteFile(ktavSiblingOf(global), []byte("k: a\nk: b\n"), 0o644))
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: global})
	_, err := s.ReadAllModelsAtScope(ScopeGlobal)
	require.Error(t, err)
}
