package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPeakRequestGateRejectsEveryNewModelCallBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(fantasy.LanguageModel) error
	}{
		{"generate", func(m fantasy.LanguageModel) error { _, err := m.Generate(t.Context(), fantasy.Call{}); return err }},
		{"stream", func(m fantasy.LanguageModel) error { _, err := m.Stream(t.Context(), fantasy.Call{}); return err }},
		{"generate object", func(m fantasy.LanguageModel) error {
			_, err := m.GenerateObject(t.Context(), fantasy.ObjectCall{})
			return err
		}},
		{"stream object", func(m fantasy.LanguageModel) error {
			_, err := m.StreamObject(t.Context(), fantasy.ObjectCall{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &hbFakeModel{provider: "peak-probe", model: "fixture"}
			provider := &peakHoursRequestProvider{Provider: &hbFakeProvider{m: inner}, check: func(context.Context) error {
				return errProviderPeakHours
			}}
			model, err := provider.LanguageModel(t.Context(), "fixture")
			require.NoError(t, err)
			require.ErrorIs(t, tc.run(model), errProviderPeakHours)
			require.Zero(t, inner.genCalls.Load())
			require.Zero(t, inner.streamCalls.Load())
			require.Zero(t, inner.objCalls.Load())
		})
	}
}

func TestPeakRequestGateDoesNotCancelAnAlreadyStartedStream(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var blocked atomic.Bool
	inner := &hbFakeModel{provider: "peak-probe", model: "fixture", parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "before"},
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "after"},
	}}
	provider := &peakHoursRequestProvider{Provider: &hbFakeProvider{m: inner}, check: func(context.Context) error {
		if blocked.Load() {
			return errProviderPeakHours
		}
		return nil
	}}
	model, err := provider.LanguageModel(ctx, "fixture")
	require.NoError(t, err)
	stream, err := model.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	var text string
	stream(func(part fantasy.StreamPart) bool {
		text += part.Delta
		blocked.Store(true)
		return true
	})
	require.Equal(t, "beforeafter", text)
	require.NoError(t, ctx.Err(), "entering peak hours must not cancel the admitted stream")
	_, err = model.Stream(ctx, fantasy.Call{})
	require.ErrorIs(t, err, errProviderPeakHours)
	require.EqualValues(t, 1, inner.streamCalls.Load(), "the next request must not reach upstream")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("peak admission: blocked=%v upstream_streams=%d text=%q", blocked.Load(), inner.streamCalls.Load(), text)
		}
	})
}

func TestPeakRequestGatePreservesExplicitAndLegacyBypassPrecedence(t *testing.T) {
	coord := &coordinator{}
	inner := &hbFakeModel{provider: "peak-probe", model: "fixture"}
	provider := coord.withPeakHoursRequestGate(&hbFakeProvider{m: inner}, config.ProviderConfig{
		PeakHours: &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Timezone: "UTC", Windows: []config.PeakHoursWindowSpec{{Start: "00:00", End: "24:00"}},
		}},
	})
	model, err := provider.LanguageModel(t.Context(), "fixture")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.ErrorIs(t, err, errProviderPeakHours)
	allowed := WithCallOptions(t.Context(), &CallOptions{AllowPeakHours: true})
	_, err = model.Generate(allowed, fantasy.Call{})
	require.NoError(t, err)
	legacy := context.WithValue(t.Context(), legacyPeakHoursBypassKey{}, true)
	_, err = model.Generate(legacy, fantasy.Call{})
	require.NoError(t, err)
	denied := WithCallOptions(legacy, &CallOptions{AllowPeakHours: false})
	_, err = model.Generate(denied, fantasy.Call{})
	require.ErrorIs(t, err, errProviderPeakHours)
	require.EqualValues(t, 2, inner.genCalls.Load())
}

func TestBuiltProviderInstallsRequestGateBeforeHTTP(t *testing.T) {
	hbIsolate(t)
	coord, _ := newControlCoordinator(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	provider, err := coord.buildProviderWithLiteralCredentials(coord.cfg.Config(), config.ProviderConfig{
		Type: "openai", APIKey: "fixture", BaseURL: server.URL,
		PeakHours: &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Timezone: "UTC", Windows: []config.PeakHoursWindowSpec{{Start: "00:00", End: "24:00"}},
		}},
	}, config.SelectedModel{Model: "fixture"}, false)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "fixture")
	require.NoError(t, err)
	_, err = model.Stream(t.Context(), fantasy.Call{})
	require.ErrorIs(t, err, errProviderPeakHours)
	require.Zero(t, requests.Load(), "the production provider factory must install the gate before network admission")
}
