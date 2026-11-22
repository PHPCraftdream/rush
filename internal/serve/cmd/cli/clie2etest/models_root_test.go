package clie2etest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelsUse_TwoPositionalRegression(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	result := runModelsCLI(t, fixture, false, "models", "use", "glm4_6", "glm5_turbo")
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	content := readModelsFixture(t, fixture)
	assert.Contains(t, content, `"smart"`)
	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"fast"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.NotContains(t, content, `"worker"`)
	assert.NotContains(t, content, `"reviewer"`)
}

func TestModelsUse_FastFlagOnly_LeavesSmartUntouched(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	for _, args := range [][]string{{"models", "use", "glm4_6", "glm5_turbo"}, {"models", "use", "--fast", "glm4_7_flash"}} {
		result := runModelsCLI(t, fixture, false, args...)
		require.Equal(t, 0, result.ExitCode, result.Stderr)
	}
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(readModelsFixture(t, fixture)), &doc))
	assert.Contains(t, string(doc.Models["smart"]), `"glm-4.6"`)
	assert.Contains(t, string(doc.Models["fast"]), `"glm-4.7-flash"`)
	assert.NotContains(t, string(doc.Models["fast"]), `"glm-5-turbo"`)
}

func TestModelsUse_SmartFlagOnly_LeavesFastUntouched(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	for _, args := range [][]string{{"models", "use", "glm4_6", "glm5_turbo"}, {"models", "use", "--smart", "glm4_7_flash"}} {
		result := runModelsCLI(t, fixture, false, args...)
		require.Equal(t, 0, result.ExitCode, result.Stderr)
	}
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(readModelsFixture(t, fixture)), &doc))
	assert.Contains(t, string(doc.Models["smart"]), `"glm-4.7-flash"`)
	assert.NotContains(t, string(doc.Models["smart"]), `"glm-4.6"`)
	assert.Contains(t, string(doc.Models["fast"]), `"glm-5-turbo"`)
}

func TestModelsUse_SmartAndFastFlagsTogether(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	result := runModelsCLI(t, fixture, false, "models", "use", "--smart", "glm4_6", "--fast", "glm5_turbo")
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	content := readModelsFixture(t, fixture)
	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.NotContains(t, content, `"worker"`)
	assert.NotContains(t, content, `"reviewer"`)
}

func TestModelsUse_PositionalAndSmartFlagConflict_Rejected(t *testing.T) {
	result := runModelsCLI(t, newModelsCLIFixture(t), false, "models", "use", "glm4_6", "glm5_turbo", "--smart", "glm4_7_flash")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "cannot combine positional")
}

func TestModelsUse_OnePositionalArg_RejectedAsAmbiguous(t *testing.T) {
	result := runModelsCLI(t, newModelsCLIFixture(t), false, "models", "use", "glm4_6")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "got 1")
}

func TestModelsUse_NoArgsNoFlags_RejectedAsNoOp(t *testing.T) {
	result := runModelsCLI(t, newModelsCLIFixture(t), false, "models", "use")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "nothing to set")
}

func TestModelsUse_WorkerOnlyViaFlags_NoPositionals(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	for _, args := range [][]string{{"models", "use", "glm4_6", "glm5_turbo"}, {"models", "use", "--worker", "glm4_7_flash"}} {
		result := runModelsCLI(t, fixture, false, args...)
		require.Equal(t, 0, result.ExitCode, result.Stderr)
	}
	content := readModelsFixture(t, fixture)
	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.Contains(t, content, `"glm-4.7-flash"`)
}

func TestModelsUse_WorkerAndReviewerFlags(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	result := runModelsCLI(t, fixture, false, "models", "use", "glm4_6", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	content := readModelsFixture(t, fixture)
	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.Contains(t, content, `"glm-4.7-flash"`)
	assert.Contains(t, content, `"glm-5.3"`)
}

func TestModelsUse_WorkerViaShortCode(t *testing.T) {
	// Verify the short-code/atom resolution path (o3x, h1l, ...) also
	// applies to the new --worker/--reviewer flags, not just smart/fast.
	fixture := newModelsCLIFixture(t)
	result := runModelsCLI(t, fixture, false, "models", "use", "glm4_6", "glm5_turbo", "--worker", "fl")
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	content := readModelsFixture(t, fixture)

	// "fl" is the generated top Fable code, effort low.
	assert.Contains(t, content, `"local-cli"`)
	entry, ok := modelatoms.LookupShortCode("fl")
	require.True(t, ok)
	assert.Contains(t, content, `"`+entry.Slug+`"`)
	assert.Contains(t, content, `"low"`)
}

func TestModelsUse_UnknownWorkerAtomFailsCleanly(t *testing.T) {
	result := runModelsCLI(t, newModelsCLIFixture(t), false, "models", "use", "glm4_6", "glm5_turbo", "--worker", "not-a-real-atom-xyz")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "worker:")
}

func checkModelsStateLocalRestoreUsesWorkspaceConfig(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	fixturePath := fixture.localConfig
	seed := `{"providers":{"zai":{"api_key":"test-zai-key"}},"models":{"smart":{"provider":"zai","model":"glm-4.6","reasoning_effort":"on"}}}`
	require.NoError(t, os.WriteFile(fixturePath, []byte(seed), 0o600))
	globalBefore, err := os.ReadFile(filepath.Join(fixture.globalData, "rush.json"))
	require.NoError(t, err)

	changed := runModelsCLI(t, fixture, false, "models", "use", "--local", "--fast", "glm4_7_flash")
	require.Equal(t, 0, changed.ExitCode, changed.Stderr)
	state := runModelsCLI(t, fixture, false, "models", "state", "--json")
	require.Equal(t, 0, state.ExitCode, state.Stderr)
	var payload struct {
		Global map[string]json.RawMessage `json:"global"`
		Local  map[string]json.RawMessage `json:"local"`
	}
	require.NoError(t, json.Unmarshal([]byte(state.Stdout), &payload))
	assert.Contains(t, string(payload.Local["fast"]), `"glm-4.7-flash"`)
	assert.Contains(t, string(payload.Local["smart"]), `"glm-4.6"`)
	assert.Empty(t, payload.Global["fast"], "the global fast model must remain unset")
	localAfter, err := os.ReadFile(fixturePath)
	require.NoError(t, err)
	assert.Contains(t, string(localAfter), `"glm-4.7-flash"`)
	globalAfter, err := os.ReadFile(filepath.Join(fixture.globalData, "rush.json"))
	require.NoError(t, err)
	assert.Equal(t, string(globalBefore), string(globalAfter), "--local must not write global settings")
}

func TestModelsUse_InvalidReviewerEffort_ZeroWrites(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	path := filepath.Join(fixture.globalData, "rush.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	result := runModelsCLI(t, fixture, false, "models", "use", "glm4_6", "glm5_turbo", "--reviewer", "glm5_3-hihg")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "reviewer:")
	assert.Contains(t, result.Stderr, "not a valid level")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "failed models use must leave global config byte-identical")
}

func TestModelsUse_InvalidReviewerEffort_WorkerAlsoNotWritten(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	path := filepath.Join(fixture.globalData, "rush.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	result := runModelsCLI(t, fixture, false, "models", "use", "glm4_6", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3-hihg")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "reviewer:")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "failed reviewer validation must not write worker either")
}

func TestModelsUse_InvalidSmartPositional_ShortCircuitsBeforeWorkerReviewer(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	path := filepath.Join(fixture.globalData, "rush.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	result := runModelsCLI(t, fixture, false, "models", "use", "not-a-real-atom-xyz", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "smart:")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "invalid smart positional must leave global config byte-identical")
}

func TestModelsUse_AllFourValid_StillWritesAll(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	result := runModelsCLI(t, fixture, false, "models", "use", "glm4_6", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(readModelsFixture(t, fixture)), &doc))
	assert.Contains(t, doc.Models, "smart")
	assert.Contains(t, doc.Models, "fast")
	assert.Contains(t, doc.Models, "worker")
	assert.Contains(t, doc.Models, "reviewer")
	content := string(mustJSON(t, doc.Models))
	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.Contains(t, content, `"glm-4.7-flash"`)
	assert.Contains(t, content, `"glm-5.3"`)
}

func TestModelsIntegrityCmd_SessionsListAborts(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	seedTamperedModelsCLI(t, fixture)
	result := runModelsCLI(t, fixture, false, "sessions", "list")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, config.ErrModelsEditedDirectly.Error())
}

func TestModelsIntegrityCmd_ModelsStateWarns(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	seedTamperedModelsCLI(t, fixture)
	warning := "warning: the model settings were changed outside the rush models commands"

	text := runModelsCLI(t, fixture, true, "models", "state")
	require.Equal(t, 0, text.ExitCode, text.Stderr)
	assert.Contains(t, text.Stderr, warning)
	assert.Contains(t, text.Stdout, "EFFECTIVE")

	jsonResult := runModelsCLI(t, fixture, true, "models", "state", "--json")
	require.Equal(t, 0, jsonResult.ExitCode, jsonResult.Stderr)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonResult.Stdout), &payload))
	require.Equal(t, true, payload["edited_directly"])

	clean := `{"providers":{"zai":{"api_key":"test-zai-key"}}}`
	ktavPath := filepath.Join(fixture.globalData, "rush.ktav")
	cleanDoc, err := ktavcodec.ConvertJSONToKtav([]byte(clean))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ktavPath, cleanDoc, 0o600))
	cleanText := runModelsCLI(t, fixture, true, "models", "state")
	require.Equal(t, 0, cleanText.ExitCode, cleanText.Stderr)
	assert.NotContains(t, cleanText.Stderr, warning)
	cleanJSON := runModelsCLI(t, fixture, true, "models", "state", "--json")
	require.Equal(t, 0, cleanJSON.ExitCode, cleanJSON.Stderr)
	assert.NotContains(t, strings.TrimSpace(cleanJSON.Stdout), `"edited_directly"`)
	var cleanPayload map[string]any
	require.NoError(t, json.Unmarshal([]byte(cleanJSON.Stdout), &cleanPayload))
	assert.NotContains(t, cleanPayload, "edited_directly")
}

func TestModelsIntegrityCmd_ModelsUseRepairs(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	globalPath := seedTamperedModelsCLI(t, fixture)
	use := runModelsCLI(t, fixture, false, "models", "use", "--smart", "glm4_6", "--fast", "glm5_turbo")
	require.Equal(t, 0, use.ExitCode, use.Stderr)
	assert.Contains(t, use.Stderr, "warning: the model settings were changed outside the rush models commands")
	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var doc struct {
		CacheRev string `json:"cache_rev"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	require.NotEmpty(t, doc.CacheRev)
	assert.NotEqual(t, "0000000000000000000000000000000000000000000000000000000000000000", doc.CacheRev)
	result := runModelsCLI(t, fixture, false, "sessions", "list")
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	state := runModelsCLI(t, fixture, false, "models", "state")
	require.Equal(t, 0, state.ExitCode, state.Stderr)
	assert.NotContains(t, state.Stderr, "warning: the model settings were changed outside the rush models commands")
}

func TestModelsIntegrityCmd_LockedMissingKeyAborts(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	seedLockedModelsMissingRevCLI(t, fixture)
	aborts := runModelsCLI(t, fixture, true, "sessions", "list")
	require.NotZero(t, aborts.ExitCode)
	assert.Contains(t, aborts.Stderr, config.ErrModelsEditedDirectly.Error())
	lite := runModelsCLI(t, fixture, true, "models", "state")
	require.Equal(t, 0, lite.ExitCode, lite.Stderr)
}

func readModelsFixture(t *testing.T, fixture modelsCLIFixture) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.globalData, "rush.json"))
	require.NoError(t, err)
	return string(data)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}
