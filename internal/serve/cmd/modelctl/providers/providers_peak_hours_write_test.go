package providers

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const policySeedJSON = `{
  "providers": {
    "polprov": {
      "name": "Policy", "type": "openai", "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1", "models": [{"id": "gpt-4o"}],
      "peak_hours": {"windows": [{"days": "mon-fri", "start": "14:00", "end": "18:00"}, {"start": "20:00", "end": "23:00"}], "message": "weekly-msg"}
    }
  }
}`

func TestLegacyPeakWindowEditPreservesWeeklyMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   string
		message string
	}{
		{"keep message", "", "weekly-msg"},
		{"replace message", " --peak-hours-message replacement", "replacement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := config.SetKtavAutoMigrateForTest(false)
			t.Cleanup(restore)
			const until = "2026-11-09T18:00:00Z"
			seed := strings.Replace(policySeedJSON, `"peak_hours": {`, `"peak_hours": {"timezone":"UTC+08:00","paused_until":"`+until+`",`, 1)
			_, path, _, err := cmdtest.RunProvidersCmdInIsolatedAppFull(t, providersSetCmd, seed, "polprov --peak-hours 09:00-17:00"+tc.flags, "")
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var entry config.ProviderPeakHours
			require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(data, "providers.polprov.peak_hours").Raw), &entry))
			require.NotNil(t, entry.Policy)
			require.Nil(t, entry.Legacy)
			require.Equal(t, "UTC+08:00", entry.Policy.Timezone)
			require.Equal(t, until, entry.Policy.PausedUntil)
			require.Equal(t, tc.message, entry.Policy.Message)
			require.Equal(t, []config.PeakHoursWindowSpec{{Start: "09:00", End: "17:00"}}, entry.Policy.Windows)
		})
	}
}
