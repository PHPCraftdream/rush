// Round-4 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round4.md, W4-APP: R4C-1, R4C-2): same harness as app_run_loop_test.go (real
// App and SQLite, httptest provider).
package app

import (
	"context"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// r4ReviewerModel is the distinct reviewer-model id the loop tests register.
const r4ReviewerModel = "probe-reviewer"

// cancelOnWrite cancels once a diagnostic line containing substr is written:
// a deterministic "Ctrl-C while the loop waits".
type cancelOnWrite struct {
	syncBuffer
	substr string
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if strings.Contains(string(p), w.substr) {
		w.cancel()
	}
	return n, err
}

// r4Models gives the provider the smart model and a distinct reviewer model,
// both with per-million prices so a turn's usage costs money.
func r4Models(t *testing.T, application *App, costPer1M float64) {
	t.Helper()
	cfg := application.config.Config()
	provider, ok := cfg.Providers.Get("openaicompat")
	require.True(t, ok)
	model := func(id string) catwalk.Model {
		return catwalk.Model{ID: id, Name: id, ContextWindow: 200000, DefaultMaxTokens: 1000, CostPer1MIn: costPer1M, CostPer1MOut: costPer1M}
	}
	provider.Models = []catwalk.Model{model("probe"), model(r4ReviewerModel)}
	cfg.Providers.Set("openaicompat", provider)
}
