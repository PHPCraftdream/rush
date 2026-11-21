// providers show/list command tests running the real subcommand RunE
// through runProvidersCmdInIsolatedApp (isolated config fixture):
// peak-hours rendering in show, its omission without peak hours, and
// the list PEAK column.
package providers

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"

	"github.com/stretchr/testify/assert"
)

const peakFixtureJSON = `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00"}
    },
    "with-peak-message": {
      "name": "With Peak Message",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00", "message": "Ping #ops-oncall first."}
    },
    "no-peak": {
      "name": "No Peak",
      "type": "anthropic",
      "base_url": "https://api.anthropic.com",
      "models": [{"id": "claude-sonnet-4"}]
    }
  }
}`

func TestProvidersShow_PeakHoursRendering(t *testing.T) {
	// Regression: this previously reimplemented the peak-hours line inline
	// and asserted the duplicate against itself. It now runs the real
	// providersShowCmd.RunE and asserts on the actual emitted stdout.
	out, _ := cmdtest.RunProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "with-peak")

	assert.Contains(t, out, "id:          with-peak")
	assert.Contains(t, out, "peak hours:  09:00-18:00 (currently:")
	// The state must be one of the two real branches the command emits.
	assert.True(t, strings.Contains(out, "(currently: in peak)") || strings.Contains(out, "(currently: not in peak)"),
		"expected a real 'currently:' state in output:\n%s", out)
	assert.NotContains(t, out, "peak hours message", "a provider with no configured message must not show the line")
}

func TestProvidersShow_PeakHoursMessageRendering(t *testing.T) {
	// The message is settable from the CLI via --peak-hours-message, but it
	// can also come from the web UI — show must surface it either way, since
	// it's otherwise invisible to a CLI-only operator.
	out, _ := cmdtest.RunProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "with-peak-message")

	assert.Contains(t, out, "id:          with-peak-message")
	assert.Contains(t, out, "peak hours message: Ping #ops-oncall first.")
}

func TestProvidersShow_NoPeakHoursOmitsLine(t *testing.T) {
	// Regression: this previously only asserted p.PeakHours == nil without
	// running the command. It now runs the real providersShowCmd.RunE on a
	// provider without peak hours and asserts the line is absent.
	out, _ := cmdtest.RunProvidersCmdInIsolatedApp(t, providersShowCmd, peakFixtureJSON, "no-peak")

	assert.Contains(t, out, "id:          no-peak")
	assert.NotContains(t, out, "peak hours", "show must omit the peak-hours line when PeakHours is nil")
}

func TestProvidersList_PeakColumn(t *testing.T) {
	// Regression: this previously reimplemented the PEAK column rendering
	// inline. It now runs the real providersListCmd.RunE and asserts on the
	// actual table output.
	out, _ := cmdtest.RunProvidersCmdInIsolatedApp(t, providersListCmd, peakFixtureJSON, "")

	assert.Contains(t, out, "PEAK", "list header must include the PEAK column")
	assert.Contains(t, out, "with-peak", "with-peak row must be present")
	assert.Contains(t, out, "no-peak", "no-peak row must be present")
	// The with-peak row must show the window; the no-peak row must show the
	// em-dash placeholder used by the list command's real rendering.
	assert.Contains(t, out, "09:00-18:00", "with-peak PEAK cell must show the window")
	assert.Contains(t, out, "—", "no-peak PEAK cell must show the placeholder")
}
