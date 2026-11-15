package discover

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverZAIEffortDocsUsesExplicitAPITiersOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`### Core Parameters
* Available values: ` + "`max`" + ` (default), ` + "`high`" + `, ` + "`low`" + `
* In the API request:
  * For GLM-5.3 and GLM-5.3-FLASH, only ` + "`max`" + `, ` + "`high`" + ` and ` + "`low`" + ` are supported.
  * For GLM-5.2, the supported options are ` + "`max`" + `, ` + "`xhigh`" + `, ` + "`high`" + `, ` + "`medium`" + `, ` + "`low`" + `, ` + "`minimal`" + `, and ` + "`none`" + `.
* In the Coding Plan request:
  * For GLM-5.3 and GLM-5.3-FLASH, ` + "`none`" + `, ` + "`minimal`" + ` and ` + "`low`" + ` are mapped to ` + "`low`" + `.
`))
	}))
	defer server.Close()

	models, err := discoverZAIEffortDocs(t.Context(), server.Client(), server.URL)
	require.NoError(t, err)
	require.Len(t, models, 3)
	levels := make(map[string][]string, len(models))
	for _, model := range models {
		levels[model.ID] = model.ReasoningLevels
	}
	require.Equal(t, []string{"low", "high", "max"}, levels["glm-5.3"])
	require.Equal(t, []string{"low", "high", "max"}, levels["glm-5.3-flash"])
	require.Equal(t, []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, levels["glm-5.2"])
	require.NotContains(t, levels, "glm-5.1")
}

func TestDiscoverZAIEffortDocsFailsClosedWhenProseChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Some GLM-5.3 models support reasoning. No per-model ladder is published."))
	}))
	defer server.Close()
	_, err := discoverZAIEffortDocs(t.Context(), server.Client(), server.URL)
	require.ErrorContains(t, err, "no explicit per-model effort levels")
}
