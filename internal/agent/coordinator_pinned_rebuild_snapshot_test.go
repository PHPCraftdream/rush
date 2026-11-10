package agent

// Regression tests for the round-9 F6 residual (2026-09-22 weekly commit
// audit): the sub-agent/summarize pinned-model rebuild used to fetch the
// provider config from one snapshot (currentProviderConfig) and the global
// network/debug defaults from a SECOND, separately-timed snapshot inside
// rebuildPinnedModel, so a reload landing between the two reads built
// buildProvider(cfgB, providerCfgA) — a provider+network pairing that never
// existed in any single config generation. The fix threads ONE snapshot
// from the rebuild caller down to buildProvider (rebuildInputs), the same
// pattern the normal build path's buildModelsFromCfg already uses.
//
// The 2026-09-23 round-11 audit found the same defect class one call site
// over: refreshOAuth2Token resolved its network client from the caller's
// captured providerCfg paired with a fresh live read. The
// TestRefreshOAuth2Token_ test below pins that half to the same
// one-snapshot rule.

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// pinnedRebuildFixture is the two-generation world the atomic-pair test
// plays against: generation 0 pins a clean global network and a buildable
// probe provider; generation 1 (published via the same copy-on-write
// runtime path a real reload uses) flips BOTH halves — the provider config
// becomes a different FlatRate generation, and the live generation's global
// network becomes a DoH URL that deterministically fails HTTP client
// construction (the established "malformed doh url" fixture from
// coordinator_providers_network_test.go).
type pinnedRebuildFixture struct {
	coord       *coordinator
	pinnedCfg   *config.Config
	pinnedGen   uint64
	pinnedProv  config.ProviderConfig
	pinnedModel Model
}

func newPinnedRebuildFixture(t *testing.T) pinnedRebuildFixture {
	t.Helper()
	store := config.NewLibraryStore(&config.Config{
		Options:   &config.Options{Debug: false},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, t.TempDir())
	pinnedProv := config.ProviderConfig{
		ID:       "probe-provider",
		Type:     openaicompat.Name,
		APIKey:   "key-gen-0",
		BaseURL:  "http://probe.invalid/v1",
		FlatRate: true,
		Models:   []catwalk.Model{{ID: "probe-model"}},
	}
	store.Config().Providers.Set("probe-provider", pinnedProv)

	f := pinnedRebuildFixture{
		coord:       &coordinator{cfg: store},
		pinnedProv:  pinnedProv,
		pinnedModel: Model{ModelCfg: config.SelectedModel{Provider: "probe-provider", Model: "probe-model"}},
	}
	f.pinnedCfg, f.pinnedGen = store.Snapshot()

	// Publish generation 1 through the runtime copy-on-write path, then
	// diverge ONLY the live generation's global network in place.
	// SetProviderRuntimeConfig clones the snapshot's *Config struct but
	// shares the *Options pointer, so replacing the live generation's
	// Options field with a modified copy leaves the pinned generation's
	// network untouched — exactly the before/after-reload world a mid-call
	// reload produces. A direct write into the SHARED *Options would mutate
	// the pinned snapshot too and make this fixture vacuous.
	store.SetProviderRuntimeConfig("probe-provider", config.ProviderConfig{
		ID:       "probe-provider",
		Type:     openaicompat.Name,
		APIKey:   "key-gen-1",
		BaseURL:  "http://probe.invalid/v1",
		FlatRate: false,
		Models:   []catwalk.Model{{ID: "probe-model"}},
	})
	live := store.Config()
	diverged := *live.Options
	diverged.Network = &config.NetworkConfig{DoHURL: "https://[::1"}
	live.Options = &diverged

	require.Greater(t, store.Generation(), f.pinnedGen, "SetProviderRuntimeConfig must publish a new generation")
	require.Nil(t, f.pinnedCfg.Options.Network, "the pinned generation must keep the clean global network")
	require.NotNil(t, store.Config().Options.Network, "the live generation must carry the malformed global network")
	return f
}

// TestRebuildPinnedModel_UsesCallerPinnedSnapshotForNetwork proves
// rebuildPinnedModel builds the provider client strictly from the cfg
// snapshot its caller handed it together with providerCfg — never from a
// fresh live read. This is the sub-agent/summarize rebuild path's half of
// the round-9 F6 residual fix: both halves of the provider+network pair
// must come from ONE config generation.
//
// REVERT CHECK: restore the pre-fix body of rebuildPinnedModel —
//
//	cfg, _ := c.cfg.Snapshot()
//
// (ignoring the cfg parameter, taking its own later live snapshot instead)
// — and both subtests fail: the live generation's malformed DoH URL makes
// buildProvider's network client construction fail with a wrapped "build
// network client" error, even though the pinned generation the caller
// passed is perfectly buildable. With the fix in place the pinned snapshot
// is the only cfg consulted, so both subtests succeed and the returned
// model carries the PINNED generation's FlatRate (true), never generation
// 1's (false).
func TestRebuildPinnedModel_UsesCallerPinnedSnapshotForNetwork(t *testing.T) {
	t.Parallel()

	t.Run("sub-agent flavor", func(t *testing.T) {
		t.Parallel()
		f := newPinnedRebuildFixture(t)
		got, err := f.coord.rebuildPinnedModel(t.Context(), f.pinnedCfg, f.pinnedModel, f.pinnedProv, true)
		require.NoError(t, err)
		require.NotNil(t, got.Model)
		require.True(t, got.FlatRate, "FlatRate must come from the pinned providerCfg, not the live generation")
	})

	t.Run("summarize flavor", func(t *testing.T) {
		t.Parallel()
		f := newPinnedRebuildFixture(t)
		got, err := f.coord.rebuildPinnedModel(t.Context(), f.pinnedCfg, f.pinnedModel, f.pinnedProv, false)
		require.NoError(t, err)
		require.NotNil(t, got.Model)
		require.True(t, got.FlatRate, "FlatRate must come from the pinned providerCfg, not the live generation")
	})
}

// TestSummarize_ProactiveRefreshRebuildCarriesFreshProviderPair walks the
// caller-level wiring the fix touched: Summarize's rebuildSnapshot closure
// must fetch the fresh provider config and the cfg handed to the client
// build from ONE snapshot (rebuildInputs), and hand BOTH to
// rebuildPinnedModel together. After the simulated reload (published inside
// the OAuth refresh hook, exactly where a real token refresh plus config
// change would land), the rebuilt snapshot's model and providerCfg must
// both carry the NEW generation's provider identity — a stale outer
// providerCfg wired against the fresh cfg would show up here as the old
// FlatRate or the old API key.
func TestSummarize_ProactiveRefreshRebuildCarriesFreshProviderPair(t *testing.T) {
	// No t.Parallel(): newWorkerToolTestCoordinator's config isolation
	// calls t.Setenv, which Go forbids in a parallel test.
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	expired := &oauth.Token{AccessToken: "old-token", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	c6ConfigureProvider(t, coord, "smart-provider", "summary-model", "old-key", expired)
	smartProviderCfg, ok := coord.cfg.Config().Providers.Get("smart-provider")
	require.True(t, ok)
	smartProviderCfg.FlatRate = true
	coord.cfg.SetProviderRuntimeConfig("smart-provider", smartProviderCfg)

	session, err := env.sessions.Create(t.Context(), "proactive refresh fresh pair")
	require.NoError(t, err)
	snapshot, err := coord.buildSummarizeSnapshot(t.Context(), session.ID)
	require.NoError(t, err)
	require.True(t, snapshot.model.FlatRate, "precondition: the initial snapshot carries generation 0's FlatRate=true pairing")

	coord.refreshOAuth2TokenFn = func(_ context.Context, _ config.ProviderConfig) error {
		fresh := config.ProviderConfig{
			ID:         "smart-provider",
			Type:       openai.Name,
			APIKey:     "new-key",
			FlatRate:   false,
			OAuthToken: &oauth.Token{AccessToken: "new-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Models:     []catwalk.Model{{ID: "summary-model", DefaultMaxTokens: 321}},
		}
		coord.cfg.SetProviderRuntimeConfig("smart-provider", fresh)
		return nil
	}
	var gotSnap *SummarizeSnapshot
	agent := &mockSessionAgent{model: snapshot.model}
	agent.summarizeFunc = func(_ context.Context, _ string, got *SummarizeSnapshot) error {
		gotSnap = got
		return nil
	}
	coord.currentAgent = agent

	require.NoError(t, coord.Summarize(t.Context(), session.ID, snapshot))
	require.NotNil(t, gotSnap)
	require.False(t, gotSnap.model.FlatRate, "the rebuilt model must carry the fresh generation's FlatRate")
	require.Equal(t, "new-key", gotSnap.providerCfg.APIKey, "the rebuilt snapshot's providerCfg must be the fresh generation's, not the stale outer one")
	require.Equal(t, "smart-provider", gotSnap.providerCfg.ID)
}

// TestRunSubAgent_401RebuildCarriesFreshProviderPair does the same for the
// sub-agent caller: runSubAgent's rebuildCall (the 401 unauthorized-retry
// path) must resolve the fresh provider config and the client-build cfg
// from one snapshot and hand both to rebuildPinnedModel. The generation
// bump is published from inside the first (failing) run — the same seam
// TestRunSubAgent_401RebuildsPinnedSharedClient uses — and the rebuilt
// pinned model must carry the NEW generation's FlatRate while keeping the
// PINNED model config (provider and model ids), the property a mixed
// cross-generation rebuild would violate.
func TestRunSubAgent_401RebuildCarriesFreshProviderPair(t *testing.T) {
	// No t.Parallel(): newWorkerToolTestCoordinator's config isolation
	// calls t.Setenv, which Go forbids in a parallel test.
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	var calls []SessionAgentCall
	preflightRefreshes, retryRefreshes := 0, 0
	// Preflight rebuilds even an unexpired token's client. Keep it valid and
	// distinguish it from the refresh after the first request returns 401.
	coord.refreshOAuth2TokenFn = func(context.Context, config.ProviderConfig) error {
		if len(calls) == 0 {
			preflightRefreshes++
		} else {
			retryRefreshes++
		}
		return nil
	}
	c6ConfigureProvider(t, coord, "smart-provider", "pinned-model", "old-key",
		&oauth.Token{AccessToken: "old-token", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	smartProviderCfg, ok := coord.cfg.Config().Providers.Get("smart-provider")
	require.True(t, ok)
	smartProviderCfg.FlatRate = true
	smartProviderCfg.ProviderOptions = map[string]any{"user": "gen0"}
	coord.cfg.SetProviderRuntimeConfig("smart-provider", smartProviderCfg)
	oldModel := c6BuildModel(t, coord, true)
	require.True(t, oldModel.FlatRate, "precondition: the initial model carries generation 0's FlatRate=true pairing")
	parent, err := env.sessions.Create(t.Context(), "401 rebuild fresh pair")
	require.NoError(t, err)

	agent := &mockSessionAgent{model: oldModel}
	agent.runFunc = func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls = append(calls, call)
		if len(calls) == 1 {
			// Reload during the first request, after the preflight rebuild.
			coord.cfg.SetProviderRuntimeConfig("smart-provider", config.ProviderConfig{
				ID:              "smart-provider",
				Type:            openai.Name,
				APIKey:          "new-key",
				FlatRate:        false,
				Models:          []catwalk.Model{{ID: "pinned-model", DefaultMaxTokens: 321}},
				ProviderOptions: map[string]any{"user": "gen1"},
			})
			return nil, c6Unauthorized()
		}
		return agentResultWithText("fresh client succeeded"), nil
	}
	coord.currentAgent = agent

	response, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parent.ID,
		AgentMessageID: "message",
		ToolCallID:     "tool",
		Prompt:         "rebuild with the fresh generation pair",
		SessionTitle:   "401 rebuild fresh pair child",
	})
	require.NoError(t, err)
	require.False(t, response.IsError, "response content: %+v", response.Content)
	require.Len(t, calls, 2)
	require.Equal(t, 1, preflightRefreshes)
	require.Equal(t, 1, retryRefreshes)
	require.NotNil(t, calls[0].SmartModel)
	require.True(t, calls[0].SmartModel.FlatRate, "preflight must retain the initial provider generation")
	require.Equal(t, "gen0", providerOptionsUser(t, calls[0]))
	require.NotNil(t, calls[1].SmartModel)
	require.NotSame(t, calls[0].SmartModel.Model, calls[1].SmartModel.Model, "401 retry must rebuild the pinned client")
	require.False(t, calls[1].SmartModel.FlatRate, "the rebuilt pinned model must carry the fresh generation's FlatRate, not the stale one")
	require.Equal(t, "gen1", providerOptionsUser(t, calls[1]), "provider options must advance with the rebuilt model, not retain the stale provider config")
	require.Equal(t, "smart-provider", calls[1].SmartModel.ModelCfg.Provider, "the pinned model config must survive the rebuild")
	require.Equal(t, "pinned-model", calls[1].SmartModel.ModelCfg.Model)
}

// authRefreshTwoGenFixture is the two-generation world for the OAuth
// refresh path's half of the F6 defect: generation 0 pins a provider with
// NO per-provider network policy and clean globals; generation 1
// (published via the same SetProviderRuntimeConfig copy-on-write path a
// real reload uses) replaces that provider's entry with one whose
// per-provider DoH URL is the deterministic "malformed doh url" fixture
// from coordinator_providers_network_test.go. The globals never change, so
// a "build network client" failure can only come from the FRESH
// generation's provider entry — never from the caller's stale copy or the
// (identical in both generations) global network.
type authRefreshTwoGenFixture struct {
	coord     *coordinator
	pinnedCfg *config.Config
	staleProv config.ProviderConfig
}

func newAuthRefreshTwoGenFixture(t *testing.T) authRefreshTwoGenFixture {
	t.Helper()
	store := config.NewLibraryStore(&config.Config{
		Options:   &config.Options{Debug: false},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, t.TempDir())
	staleProv := config.ProviderConfig{
		ID:      "probe-provider",
		Type:    openaicompat.Name,
		APIKey:  "key-gen-0",
		BaseURL: "http://probe.invalid/v1",
		Models:  []catwalk.Model{{ID: "probe-model"}},
	}
	store.Config().Providers.Set("probe-provider", staleProv)
	f := authRefreshTwoGenFixture{
		coord:     &coordinator{cfg: store},
		staleProv: staleProv,
	}
	f.pinnedCfg, _ = store.Snapshot()

	store.SetProviderRuntimeConfig("probe-provider", config.ProviderConfig{
		ID:      "probe-provider",
		Type:    openaicompat.Name,
		APIKey:  "key-gen-1",
		BaseURL: "http://probe.invalid/v1",
		Network: &config.NetworkConfig{DoHURL: "https://[::1"},
		Models:  []catwalk.Model{{ID: "probe-model"}},
	})
	return f
}

// TestRefreshOAuth2Token_UsesFreshSameSnapshotProviderPair pins the
// auth-path half of the round-11 F6 finding: refreshOAuth2Token resolved
// its network client from the CALLER's earlier providerCfg paired with a
// fresh live c.cfg.Config() read — the same two-generation mix
// rebuildPinnedModel was fixed against. A reload that lands between the
// caller's fetch and the refresh must not leave the token exchange riding
// generation A's provider network policy against generation B's globals;
// the refresh must consult ONE generation for both halves.
//
// REVERT CHECK: restore
//
//	httpClient, clientErr := c.resolveProviderHTTPClient(c.cfg.Config(), providerCfg)
//
// and the test fails: the caller's generation-0 providerCfg (no
// per-provider network) pairs with the unchanged clean globals, the client
// resolves to (nil, nil), and the refresh sails past the network gate to
// fail later inside RefreshOAuthTokenWithClient with "does not have an
// OAuth token" — the fresh generation's malformed DoH policy is never
// consulted. With the fix, the fresh snapshot's provider entry (malformed
// DoH) plus that same snapshot's globals fail client construction before
// any store access, and the wrapped "build network client" error is the
// observable proof the pair came from one generation.
func TestRefreshOAuth2Token_UsesFreshSameSnapshotProviderPair(t *testing.T) {
	t.Parallel()

	f := newAuthRefreshTwoGenFixture(t)

	// Precondition: the caller's stale pair is perfectly buildable — the
	// generation-0 provider carries no network policy and neither do the
	// generation-0 globals — so the failure asserted below can only come
	// from the fresh generation's provider entry.
	client, err := f.coord.resolveProviderHTTPClient(f.pinnedCfg, f.staleProv)
	require.NoError(t, err)
	require.Nil(t, client)

	err = f.coord.refreshOAuth2Token(t.Context(), f.staleProv)
	require.Error(t, err)
	require.Contains(t, err.Error(), "build network client")
	require.Contains(t, err.Error(), "probe-provider")
}
