package agent

// Regression coverage for the operator's peak-hours message
// (config.PeakHoursWindow.Message) along the refusal chain: config window →
// checkPeakHours / checkLivePeakHours → *PeakHoursError → guidance text.
// The mid-turn watcher leg was removed in task #56: the gate is pre-flight
// only, and a window opening mid-turn never aborts a running stream.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

const regressionPeakMessage = "OPERATOR-NOTE: quota exhausted, resume after midnight"

func TestCheckPeakHours_CarriesOperatorMessage(t *testing.T) {
	w := peakWindowAroundNow()
	w.Message = regressionPeakMessage

	err := checkPeakHours(config.ProviderConfig{ID: "zai", PeakHours: &config.ProviderPeakHours{Legacy: w}})
	var pe *PeakHoursError
	require.True(t, errors.As(err, &pe), "refusal must be a *PeakHoursError")
	require.Equal(t, regressionPeakMessage, pe.Message, "the window's message must reach the error")
	require.Contains(t, PeakHoursGuidance(err), regressionPeakMessage, "guidance must append the operator message")
}

// Mid-turn source: the live check re-reads config edited on disk while the
// agent runs (what `rush providers set --local ...` does to a running
// session) and must pick up the message too, not just the window.
func TestCheckLivePeakHours_CarriesMessageFromEditedConfig(t *testing.T) {
	// Legacy-JSON fixture test: it rewrites rush.json at its literal path
	// after config.Init and expects the live reload to see it, so the gate
	// must be off (after migration the active file is rush.ktav).
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "rush.json")
	base := `{"options":{"disable_default_providers":true},"providers":{"custom":{"api_key":"k","base_url":"https://api.example.test/v1","models":[{"id":"model"}]%s}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(base, "")), 0o600))

	store, err := config.Init(workDir, workDir, false)
	require.NoError(t, err)
	coord := &coordinator{cfg: store}
	require.NoError(t, coord.checkLivePeakHours("custom"))

	w := peakWindowAroundNow()
	peak := fmt.Sprintf(`,"peak_hours":{"start":%q,"end":%q,"message":%q}`, w.Start, w.End, regressionPeakMessage)
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(base, peak)), 0o600))

	err = coord.checkLivePeakHours("custom")
	var pe *PeakHoursError
	require.True(t, errors.As(err, &pe), "mid-session window must refuse with *PeakHoursError, got %v", err)
	require.Equal(t, regressionPeakMessage, pe.Message)
}
