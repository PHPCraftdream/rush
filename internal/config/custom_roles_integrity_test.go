package config

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCustomRolesIntegrityPreservesModelOnlySignatures(t *testing.T) {
	t.Parallel()
	doc := `{"models":{"smart":{"provider":"p","model":"m"}}}`
	canonical, present, err := canonicalModelsJSON(gjson.Get(doc, "models").Raw)
	require.NoError(t, err)
	require.True(t, present)
	signed, err := applyModelsIntegrity(doc)
	require.NoError(t, err)
	require.Equal(t, modelsIntegritySignature(canonical), gjson.Get(signed, cacheRevKey).String())
	withEmptyRoles, err := sjson.Set(doc, "roles", map[string]any{})
	require.NoError(t, err)
	withEmptyRoles, err = applyModelsIntegrity(withEmptyRoles)
	require.NoError(t, err)
	require.Equal(t, gjson.Get(signed, cacheRevKey).String(), gjson.Get(withEmptyRoles, cacheRevKey).String())
}

func TestCustomRoleIntegrityRejectsUnsignedToolWidening(t *testing.T) {
	t.Parallel()
	signed, err := applyModelsIntegrity(`{"roles":{"reader":{"provider":"p","model":"m","tools":[]}}}`)
	require.NoError(t, err)
	require.NoError(t, modelsIntegrityOf([]byte(signed), true))
	withoutTools, err := sjson.Delete(signed, "roles.reader.tools")
	require.NoError(t, err)
	require.ErrorIs(t, modelsIntegrityOf([]byte(withoutTools), false), ErrModelsEditedDirectly)
	wildcard, err := sjson.Set(signed, "roles.reader.tools", []string{"*"})
	require.NoError(t, err)
	require.ErrorIs(t, modelsIntegrityOf([]byte(wildcard), false), ErrModelsEditedDirectly)
	changedModel, err := sjson.Set(signed, "roles.reader.model", "another-model")
	require.NoError(t, err)
	require.ErrorIs(t, modelsIntegrityOf([]byte(changedModel), false), ErrModelsEditedDirectly)
	require.True(t, touchesModelsKey([]string{"roles.reader.tools"}))
	legacySigned, err := applyModelsIntegrity(`{"models":{"smart":{"provider":"p","model":"m"}}}`)
	require.NoError(t, err)
	unsignedAlias, err := sjson.Set(legacySigned, "Roles.reader", map[string]string{"provider": "p", "model": "m"})
	require.NoError(t, err)
	require.ErrorIs(t, modelsIntegrityOf([]byte(unsignedAlias), false), ErrModelsEditedDirectly,
		"encoding/json aliases cannot introduce roles outside the signed canonical namespace")
}

func TestCustomRoleIntegrityKtavMigrationPreservesValidation(t *testing.T) {
	t.Parallel()
	signed, err := applyModelsIntegrity(`{"roles":{"reader":{"provider":"p","model":"m","provider_options":{"number":1.50},"tools":[]}}}`)
	require.NoError(t, err)
	for _, validSignature := range []bool{true, false} {
		source := signed
		if !validSignature {
			source, err = sjson.Set(source, cacheRevKey, "invalid-signature")
			require.NoError(t, err)
		}
		converted, err := ktavcodec.ConvertJSONToKtav([]byte(source))
		require.NoError(t, err)
		converted, err = applyKtavMigrationModelsIntegrity([]byte(source), converted)
		require.NoError(t, err)
		doc, err := decodeSettingsDocument("rush.ktav", converted, reloadFileFingerprint{})
		require.NoError(t, err)
		err = modelsIntegrityOf(doc.jsonBytes, true)
		if validSignature {
			require.NoError(t, err, "a valid role signature must survive numeric normalization")
		} else {
			require.ErrorIs(t, err, ErrModelsEditedDirectly, "migration must not bless a tampered role")
		}
	}
}
