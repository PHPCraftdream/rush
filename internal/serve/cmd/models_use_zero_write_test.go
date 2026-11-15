// Zero-write and atomicity tests for `rush models use`: any validation
// failure (invalid effort suffix, unresolvable atom) must leave the config
// file byte-identical, and the fully-valid case must still write all slots.
package cmd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelsUse_InvalidReviewerEffort_ZeroWrites is the core regression test
// for the partial-write bug: a valid smart/fast combined with an invalid
// --reviewer effort suffix must result in NO fields written at all — not
// smart/fast silently persisted while only the reviewer write is rejected.
// Before the fix, RunE wrote large and small to disk (and printed "set
// large = ..." / "set small = ...") BEFORE it ever parsed/validated
// --reviewer, so a typo'd effort like "glm5_3-hihg" left smart/fast durably
// changed even though the overall command reported failure. This asserts the
// raw on-disk bytes are byte-identical to the pre-command state (the seed
// `{}` isolatedModelsEnv wrote), not just rush state's "effective" view,
// which could misleadingly inherit a prior write.
//
// Uses the atom form "glm5_3-hihg" rather than the raw "zai/glm-5.3@hihg"
// string from the live bug report, because raw provider/model resolution
// goes through a.ResolveModel against the (cached) provider catalog, which
// this isolated test harness doesn't reliably resolve for every zai model id
// (see the comment on TestModelsUse_RawZAIEffort_ValidSucceeds above) — an
// environmental quirk, unrelated to the ordering bug under test. The atom
// path exercises the identical validateEffortForModel call without that
// dependency.
func TestModelsUse_InvalidReviewerEffort_ZeroWrites(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm4_6", "glm5_turbo", "--reviewer", "glm5_3-hihg")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "reviewer:")
	assert.Contains(t, runErr.Error(), "not a valid level")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"config file must be byte-identical after a failed `models use` call — zero writes expected, got: %s", after)
}

// TestModelsUse_InvalidReviewerEffort_WorkerAlsoNotWritten is the same
// scenario as above but with a valid --worker present too, confirming the
// batch-write covers ALL provided slots, not just smart/fast: an invalid
// --reviewer must prevent --worker from being written as well.
func TestModelsUse_InvalidReviewerEffort_WorkerAlsoNotWritten(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm4_6", "glm5_turbo",
		"--worker", "glm4_7_flash",
		"--reviewer", "glm5_3-hihg")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "reviewer:")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"worker must not be written either when reviewer validation fails, got: %s", after)
}

// TestModelsUse_InvalidLargePositional_ShortCircuitsBeforeWorkerReviewer
// covers the reverse direction: an invalid large positional arg must fail
// before even attempting to parse/touch worker/reviewer, with the same
// zero-write guarantee. This also guards against a regression where pass 1
// validation order changes and worker/reviewer get parsed (and their errors
// surface) before smart/fast, masking the actual first failure.
func TestModelsUse_InvalidSmartPositional_ShortCircuitsBeforeWorkerReviewer(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"not-a-real-atom-xyz", "glm5_turbo",
		"--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "smart:")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after),
		"an invalid large positional must result in zero writes, got: %s", after)
}

// TestModelsUse_AllFourValid_StillWritesAll is the regression guard for the
// fully-valid four-argument case: restructuring RunE into a validate-then-
// write two-pass flow must not break the working case where every argument
// is valid — all four slots must still end up written.
func TestModelsUse_AllFourValid_StillWritesAll(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm4_6", "glm5_turbo",
		"--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	assert.Contains(t, doc.Models, "smart")
	assert.Contains(t, doc.Models, "fast")
	assert.Contains(t, doc.Models, "worker")
	assert.Contains(t, doc.Models, "reviewer")

	content := string(data)
	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.Contains(t, content, `"glm-4.7-flash"`)
	assert.Contains(t, content, `"glm-5.3"`)
}
