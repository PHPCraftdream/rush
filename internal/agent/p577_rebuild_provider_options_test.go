package agent

// Regression test for task #577/P1-2: RebuildSessionAgentCall used to take
// ONE atomic snapshot for the smart/fast model rebuild, then read provider
// options from a SECOND, separately-timed c.cfg.Config() call. A reload
// (SetProviderRuntimeConfig -- exactly what a credential refresh does)
// landing between those two reads could hand back a model built from
// generation N together with provider options/credentials from generation
// N+1. RebuildSessionAgentCall exists specifically for the DURABLE RECOVERY
// path: replaying a queued call is only correct if it reproduces exactly
// what was queued, which requires every field to come from one generation.
//
// REVERT CHECK: in RebuildSessionAgentCall (coordinator_interrupt.go),
// change "smartProviderCfg, _ := cfg.Providers.Get(...)" back to
// "smartProviderCfg, _ := c.cfg.Config().Providers.Get(...)" and
// TestRebuildSessionAgentCall_ProviderOptionsNeverTearFromModelGeneration
// fails under concurrent reload -- verified below, see that test's own doc.

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
)

// providerOptionsUser extracts the "user" probe field RebuildSessionAgentCall
// attached to the rebuilt call's ProviderOptions. getProviderOptions merges
// config.ProviderConfig.ProviderOptions straight through openai.ParseOptions
// into openai.ProviderOptions.User, with no other contributing source (unlike,
// say, a context-file path, which can also be satisfied by the filesystem
// regardless of which generation named it -- see p554_pinned_config_test.go's
// snapshotConfig doc for why that filesystem-fallback trap matters when
// picking a probe field). Returns "" if the probe wasn't set yet (only
// possible on the very first iteration of a concurrent test, before the
// writer goroutine's first publish).
func providerOptionsUser(t *testing.T, call SessionAgentCall) string {
	t.Helper()
	data, ok := call.ProviderOptions[openai.Name]
	if !ok {
		return ""
	}
	opts, ok := data.(*openai.ProviderOptions)
	require.True(t, ok, "openai-keyed ProviderOptions entry must be *openai.ProviderOptions")
	if opts.User == nil {
		return ""
	}
	return *opts.User
}

// registerSmartProviderGen publishes generation gen of smart-provider: the
// catwalk model's ContextWindow AND ProviderConfig.ProviderOptions.user both
// encode gen, so gen0 and gen1 are distinguishable from two INDEPENDENT
// downstream fields:
//
//   - smart.CatwalkCfg.ContextWindow comes from buildModelsFromCfg, which
//     already receives the PINNED cfg RebuildSessionAgentCall captured at
//     entry (line ~437) -- this field was never the bug.
//   - providerOptionsUser(call) comes from the smartProviderCfg lookup this
//     task fixes (line ~474) -- BEFORE the fix, this was a live
//     c.cfg.Config().Providers.Get() call, independent of the pinned cfg
//     buildModelsFromCfg used.
//
// A torn read shows up as these two fields disagreeing about which
// generation produced them -- ContextWindow from one generation,
// ProviderOptions.User from another -- which could never happen if both
// come from the same pinned snapshot.
func registerSmartProviderGen(cfg *config.ConfigStore, gen int) {
	contextWindow := int64(100_000)
	user := "gen0"
	if gen == 1 {
		contextWindow = 200_000
		user = "gen1"
	}
	cfg.SetProviderRuntimeConfig("smart-provider", config.ProviderConfig{
		ID:   "smart-provider",
		Type: openai.Name,
		Models: []catwalk.Model{
			{ID: "smart-model", ContextWindow: contextWindow},
		},
		ProviderOptions: map[string]any{"user": user},
	})
}

// TestRebuildSessionAgentCall_ProviderOptionsMatchModelGeneration is the
// simple, non-concurrent half of the regression coverage: a fresh
// RebuildSessionAgentCall call must observe whatever generation is live at
// the time it's invoked, for both the model and its provider options
// together -- establishing the baseline the concurrent test below builds on.
func TestRebuildSessionAgentCall_ProviderOptionsMatchModelGeneration(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	registerSmartProviderGen(coord.cfg, 0)

	data := session.SessionAgentCallData{
		SessionID: "probe-session",
		Prompt:    "hello",
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err)
	require.NotNil(t, call.SmartModel)
	require.Equal(t, int64(100_000), call.SmartModel.CatwalkCfg.ContextWindow)
	require.Equal(t, "gen0", providerOptionsUser(t, call),
		"provider options must come from the same generation RebuildSessionAgentCall built the model from")

	registerSmartProviderGen(coord.cfg, 1)

	call2, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err)
	require.Equal(t, int64(200_000), call2.SmartModel.CatwalkCfg.ContextWindow)
	require.Equal(t, "gen1", providerOptionsUser(t, call2),
		"a fresh RebuildSessionAgentCall after a reload must observe the new generation's provider options")
}

// TestRebuildSessionAgentCall_ProviderOptionsNeverTearFromModelGeneration is
// the generation-straddling version: while RebuildSessionAgentCall is
// repeatedly invoked, a concurrent goroutine keeps toggling smart-provider
// between generation 0 and generation 1 via SetProviderRuntimeConfig --
// exactly what a concurrent credential refresh does.
//
// Before the fix, ContextWindow (from the pinned cfg) and User (from the
// live re-read) could each independently land on either generation's value
// for the SAME call, producing four possible (ContextWindow, User) pairs
// instead of the two legitimate ones -- that mismatch is what this test
// catches. With the fix, both come from the one pinned cfg, so only the two
// legitimate pairs are structurally possible no matter how the toggle
// interleaves -- this is a structural guarantee once the fix is in place,
// not a probabilistic catch, so a bounded iteration count under real
// concurrency is sufficient once the race is given enough chances to fire.
func TestRebuildSessionAgentCall_ProviderOptionsNeverTearFromModelGeneration(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	registerSmartProviderGen(coord.cfg, 0)

	stop := make(chan struct{})
	toggleDone := make(chan struct{})
	go func() {
		defer close(toggleDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			registerSmartProviderGen(coord.cfg, i%2)
			i++
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-toggleDone
	})

	data := session.SessionAgentCallData{
		SessionID: "probe-session",
		Prompt:    "hello",
	}

	for i := 0; i < 2000; i++ {
		call, err := coord.RebuildSessionAgentCall(t.Context(), data)
		require.NoError(t, err)
		require.NotNil(t, call.SmartModel)

		user := providerOptionsUser(t, call)
		if user == "" {
			// Raced the writer goroutine's very first publish; no
			// provider configured yet for this call. Not a torn read.
			continue
		}

		switch call.SmartModel.CatwalkCfg.ContextWindow {
		case 100_000:
			require.Equal(t, "gen0", user,
				"torn generation read: model built from gen0 (ContextWindow=100000) but provider options came from gen1")
		case 200_000:
			require.Equal(t, "gen1", user,
				"torn generation read: model built from gen1 (ContextWindow=200000) but provider options came from gen0")
		default:
			t.Fatalf("unexpected ContextWindow %d (neither known generation)", call.SmartModel.CatwalkCfg.ContextWindow)
		}
	}
}
