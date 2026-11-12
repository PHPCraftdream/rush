package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStandaloneSummarizer_CLIWorkerRefusesWithoutFallback(t *testing.T) {
	probe := &standaloneSummaryProbe{}
	server := standaloneSummaryServer(t, probe)
	defer server.Close()
	ctx, sessions, messages, sessionID, seeded, dataDir := newStandaloneSummaryFixture(t)

	store := standaloneSummaryConfig(server.URL, true, dataDir)
	store.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: "local-cli", Model: "worker-model"}
	store.Config().Providers.Set("local-cli", config.ProviderConfig{ID: "local-cli", Type: cliprovider.ProviderType})

	var err error
	require.NotPanics(t, func() {
		err = NewStandaloneSummarizer(store, sessions, messages).SummarizeWithRole(ctx, sessionID, config.SelectedModelTypeWorker)
	})
	require.ErrorContains(t, err, "does not support CLI")
	require.Zero(t, probe.calls.Load(), "unexpected provider requests: %d (model %q)", probe.calls.Load(), probe.model())
	standaloneRequireHistory(t, ctx, sessions, messages, sessionID, seeded)
}

func TestStandaloneSummarizer_EmptyDataDirectoryRefusesWithoutNetwork(t *testing.T) {
	probe := &standaloneSummaryProbe{}
	server := standaloneSummaryServer(t, probe)
	defer server.Close()
	ctx, sessions, messages, sessionID, seeded, dataDir := newStandaloneSummaryFixture(t)

	store := standaloneSummaryConfig(server.URL, true, dataDir)
	store.Config().Options.DataDirectory = ""

	err := NewStandaloneSummarizer(store, sessions, messages).SummarizeWithRole(ctx, sessionID, config.SelectedModelTypeWorker)
	require.ErrorContains(t, err, "session locking")
	require.Zero(t, probe.calls.Load(), "unexpected provider requests: %d (model %q)", probe.calls.Load(), probe.model())
	standaloneRequireHistory(t, ctx, sessions, messages, sessionID, seeded)
}
