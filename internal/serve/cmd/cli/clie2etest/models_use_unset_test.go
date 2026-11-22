// `rush models unset` tests: clearing worker alone, the "all" positional
// clearing every slot, and clean rejection of unknown slot names.
package clie2etest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelsUnset_ClearsWorkerOnly(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	globalPath := filepath.Join(fixture.globalData, "rush.json")

	result := runModelsCLI(t, fixture, false,
		"models", "use", "glm4_6", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.Equal(t, 0, result.ExitCode, result.Stderr)

	result = runModelsCLI(t, fixture, false, "models", "unset", "worker")
	require.Equal(t, 0, result.ExitCode, result.Stderr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	// "models.worker" (the active override) must be gone; "recent_models"
	// (separate MRU history, untouched by unset) may still list it — assert
	// only against the "models" object so that distinction isn't conflated.
	_, workerStillSet := doc.Models["worker"]
	assert.False(t, workerStillSet, "models.worker should have been removed, got: %s", data)
	assert.Contains(t, doc.Models, "reviewer") // reviewer survives
	assert.Contains(t, doc.Models, "smart")    // large survives
	assert.Contains(t, doc.Models, "fast")     // small survives
}

func TestModelsUnset_AllClearsAllFourSlots(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	globalPath := filepath.Join(fixture.globalData, "rush.json")

	result := runModelsCLI(t, fixture, false,
		"models", "use", "glm4_6", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.Equal(t, 0, result.ExitCode, result.Stderr)

	result = runModelsCLI(t, fixture, false, "models", "unset", "all")
	require.Equal(t, 0, result.ExitCode, result.Stderr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.NotContains(t, content, `"models"`)
}

func TestModelsUnset_UnknownSlotFailsCleanly(t *testing.T) {
	fixture := newModelsCLIFixture(t)

	result := runModelsCLI(t, fixture, false, "models", "unset", "bogus-slot")
	require.NotZero(t, result.ExitCode)
	assert.Contains(t, result.Stderr, "expected smart|fast|worker|reviewer|both|all")
}
