package cmd

// Model resolution and price lookup for `sessions cost`. Both are display
// facts about a session, so they are tested directly rather than through a
// database: the ledger is not what is under test here.

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// stubMessages implements just the one method the resolution needs; the
// embedded interface covers the rest (a nil interface panics only if some
// other method is called, which these tests never do).
type stubMessages struct {
	message.Service
	report message.UsageReport
	err    error
}

func (s stubMessages) UsageBySession(context.Context, string) (message.UsageReport, error) {
	return s.report, s.err
}

func costTestConfig(providers map[string]config.ProviderConfig) *config.Config {
	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	for id, p := range providers {
		p.ID = id
		cfg.Providers.Set(id, p)
	}
	return cfg
}

// TestPriceKnownFor_ResolvesFromProviderThenBareModelID: a table over every
// way a session's (provider, model) can be looked up. The cases are the ones
// operators actually hit: a priced model, a model with all-zero rates, a
// model whose provider the config no longer spells, and a model configured
// nowhere at all.
//
// Without the provider-first lookup a zero-rated model under the wrong
// provider would be reported priced through the fallback; without the
// fallback a model that moved providers would be reported unpriced.
func TestPriceKnownFor_ResolvesFromProviderThenBareModelID(t *testing.T) {
	t.Parallel()

	cfg := costTestConfig(map[string]config.ProviderConfig{
		"zai": {Models: []catwalk.Model{
			{ID: "glm-4.6"},
			{ID: "glm-4.6-air", CostPer1MIn: 0, CostPer1MOut: 0, CostPer1MInCached: 0, CostPer1MOutCached: 0},
		}},
		"zhipu":  {Models: []catwalk.Model{{ID: "glm-4.6", CostPer1MIn: 0.6}}},
		"openai": {Models: []catwalk.Model{{ID: "gpt-5", CostPer1MIn: 1.25}}},
	})

	cases := []struct {
		name string
		ref  costModelRef
		want bool
	}{
		{"priced model under its own provider", costModelRef{Provider: "openai", Model: "gpt-5"}, true},
		{"all four rates zero is no price", costModelRef{Provider: "zai", Model: "glm-4.6-air"}, false},
		{"explicitly zero rates is no price", costModelRef{Provider: "zai", Model: "glm-4.6"}, false},
		{"bare model id falls back across providers", costModelRef{Provider: "renamed-away", Model: "glm-4.6"}, true},
		{"bare model id of a priced model", costModelRef{Provider: "nope", Model: "gpt-5"}, true},
		{"model configured nowhere", costModelRef{Provider: "zai", Model: "not-a-model"}, false},
		{"provider configured nowhere", costModelRef{Provider: "nope", Model: "also-not-a-model"}, false},
		{"empty ref is never priced", costModelRef{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, priceKnownFor(cfg, tc.ref))
		})
	}
}

// TestPriceKnownFor_NilConfigIsNoPrice: a caller with no config at all must
// not be answered with a panic, and must not claim a price either.
func TestPriceKnownFor_NilConfigIsNoPrice(t *testing.T) {
	t.Parallel()

	require.False(t, priceKnownFor(nil, costModelRef{Provider: "openai", Model: "gpt-5"}))
}

// TestPriceKnownFor_ModelIdFallbackIgnoresProviderOrder: the fallback scans
// EVERY provider spelling that model id, so which of them sorts first must
// not decide the answer. A provider that sorts before the priced one must not
// hide the price, and a provider that sorts after it must still be found --
// "last match wins" and "first match wins" are the same bug from either end.
//
// Revert-check: turning the OR into an assignment that lets the LAST matching
// provider win makes the first case false (the unpriced duplicate sorts last)
// while the mirror still passes, which is why both directions are asserted.
func TestPriceKnownFor_ModelIdFallbackIgnoresProviderOrder(t *testing.T) {
	t.Parallel()

	// Priced provider sorts FIRST, unpriced duplicate sorts LAST.
	pricedFirst := costTestConfig(map[string]config.ProviderConfig{
		"aaa": {Models: []catwalk.Model{{ID: "glm-4.6", CostPer1MIn: 0.6}}},
		"zzz": {Models: []catwalk.Model{{ID: "glm-4.6"}}},
	})
	require.True(t, priceKnownFor(pricedFirst, costModelRef{Provider: "renamed-away", Model: "glm-4.6"}),
		"an unpriced duplicate sorting last must not hide a priced one")

	// Mirror: unpriced sorts FIRST, priced sorts LAST.
	unpricedFirst := costTestConfig(map[string]config.ProviderConfig{
		"aaa": {Models: []catwalk.Model{{ID: "glm-4.6"}}},
		"zzz": {Models: []catwalk.Model{{ID: "glm-4.6", CostPer1MIn: 0.6}}},
	})
	require.True(t, priceKnownFor(unpricedFirst, costModelRef{Provider: "renamed-away", Model: "glm-4.6"}),
		"a priced duplicate sorting last must still be found")
}

// TestResolveCostModel_PrefersSessionSelection: an explicit per-session
// override wins over what the messages say. Reading the messages first would
// silently re-attribute a session the operator deliberately switched.
func TestResolveCostModel_PrefersSessionSelection(t *testing.T) {
	t.Parallel()

	msgs := stubMessages{report: message.UsageReport{ByModel: []message.ModelUsage{
		{Usage: message.TokenUsage{Provider: "openai", Model: "gpt-5"}, Messages: 9},
	}}}
	ref := resolveCostModel(context.Background(), msgs, session.Session{
		SmartModelProvider: "zai",
		SmartModelID:       "glm-4.6",
	})
	require.True(t, ref.Known())
	require.Equal(t, "zai", ref.Provider)
	require.Equal(t, "glm-4.6", ref.Model)
}

// TestResolveCostModel_FallsBackToMessageModel: with no explicit selection the
// dominant producing model wins. This is the whole fix for the "(unknown)"
// group -- a plain session inherits its default and never writes
// smart_model_id, so the only remaining source is the messages themselves.
func TestResolveCostModel_FallsBackToMessageModel(t *testing.T) {
	t.Parallel()

	msgs := stubMessages{report: message.UsageReport{ByModel: []message.ModelUsage{
		{Usage: message.TokenUsage{Provider: "zai", Model: "glm-4.6"}, Messages: 3},
		{Usage: message.TokenUsage{Provider: "openai", Model: "gpt-5"}, Messages: 9},
	}}}
	ref := resolveCostModel(context.Background(), msgs, session.Session{})
	require.True(t, ref.Known())
	require.Equal(t, "openai", ref.Provider)
	require.Equal(t, "gpt-5", ref.Model)
}

// TestResolveCostModel_TiesBreakOnTokens: two models with the same message
// count must resolve the same way on every run, or the table shuffles rows
// between invocations for no reason.
func TestResolveCostModel_TiesBreakOnTokens(t *testing.T) {
	t.Parallel()

	msgs := stubMessages{report: message.UsageReport{ByModel: []message.ModelUsage{
		{Usage: message.TokenUsage{Provider: "zai", Model: "glm-4.6", TotalTokens: 100}, Messages: 5},
		{Usage: message.TokenUsage{Provider: "openai", Model: "gpt-5", TotalTokens: 900}, Messages: 5},
	}}}
	ref := resolveCostModel(context.Background(), msgs, session.Session{})
	require.Equal(t, "gpt-5", ref.Model)
}

// TestResolveCostModel_IgnoresBlankModelRows: the analytics queries can
// produce a group with no model recorded; attributing spend to "" would land
// it in the (unknown) bucket even though a real model was there.
func TestResolveCostModel_IgnoresBlankModelRows(t *testing.T) {
	t.Parallel()

	msgs := stubMessages{report: message.UsageReport{ByModel: []message.ModelUsage{
		{Usage: message.TokenUsage{Provider: "zai", Model: ""}, Messages: 99},
		{Usage: message.TokenUsage{Provider: "openai", Model: "gpt-5"}, Messages: 1},
	}}}
	ref := resolveCostModel(context.Background(), msgs, session.Session{})
	require.Equal(t, "gpt-5", ref.Model)
}

// TestResolveCostModel_NoMessages: a session with no readable usage has no
// model, and must degrade to unknown rather than to the configured default --
// the default is a guess about which model ran, not a fact.
func TestResolveCostModel_NoMessages(t *testing.T) {
	t.Parallel()

	ref := resolveCostModel(context.Background(), stubMessages{}, session.Session{})
	require.False(t, ref.Known())
}

// TestResolveCostModel_ServiceErrorIsUnknown: one unreadable session must not
// cost the operator every other row, so a messages failure degrades to
// unknown instead of aborting the table.
func TestResolveCostModel_ServiceErrorIsUnknown(t *testing.T) {
	t.Parallel()

	msgs := stubMessages{err: errors.New("database is locked")}
	ref := resolveCostModel(context.Background(), msgs, session.Session{})
	require.False(t, ref.Known())
}

// TestResolveCostModel_NilServiceDegrades: a caller without a message service
// must still get an answer rather than a panic.
func TestResolveCostModel_NilServiceDegrades(t *testing.T) {
	t.Parallel()

	ref := resolveCostModel(context.Background(), nil, session.Session{})
	require.False(t, ref.Known())
}

// TestCostCellText_ZeroIsOnlyADollarWhenPriced: the three cases that decide
// what the COST column may claim. The middle one is the whole point -- a zero
// with no price configured must not read as "free".
func TestCostCellText_ZeroIsOnlyADollarWhenPriced(t *testing.T) {
	t.Parallel()

	require.Equal(t, "$0.000", costCellText(0, true))
	require.Equal(t, costNoPriceText, costCellText(0, false))
	require.Equal(t, "$12.500", costCellText(12.5, false))
}
