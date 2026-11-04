package app

import (
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// A reload rebuilds the config from disk (the disk poller, or the auto-reload
// after a config write). The shared fixture's provider must live on disk, or
// the reload drops it ("No providers configured after reload"), SetupAgents is
// skipped and every batch bind fails with "coder tool configuration
// unavailable" -- the flake this pins.
func TestAdmissionRaceAppSurvivesConfigReload(t *testing.T) {
	application, _ := newAdmissionRaceApp(t, func(http.ResponseWriter, *http.Request) {})

	require.NoError(t, application.config.ReloadFromDisk(t.Context()))

	cfg := application.config.Config()
	_, hasProvider := cfg.Providers.Get("openaicompat")
	require.True(t, hasProvider, "the fixture provider must survive a reload from disk")
	_, hasCoder := cfg.Agents[config.AgentCoder]
	require.True(t, hasCoder, "the coder agent must survive a reload from disk")
}
