package cmd

// The model a session's spend is attributed to, and whether a price for it is
// configured at all. Both are display facts, not accounting: the money comes
// from the session ledger, this only decides what the COST column may claim.
//
// The "(unknown)" case is not a bug in the ledger. sessions.smart_model_id is
// written ONLY by an explicit per-session override (Service.UpdateModels --
// the web UI's model picker, the /model switch); a plain session that simply
// inherits the configured default never writes it, so the column is NULL for
// most rows. The model that actually produced the messages is on the messages
// themselves (message.TokenUsage.Provider/Model), so that is the fallback.

import (
	"context"
	"fmt"
	"sort"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// costModelUnknownKey is the printed group key when no model can be
// determined for a session.
const costModelUnknownKey = "(unknown)"

// costNoPriceText replaces "$0.000" for a model with no price configured. A
// zero and a free plan are indistinguishable once both print $0.000, and the
// difference decides whether the operator has to go set prices.
const costNoPriceText = "n/a (no price)"

// costModelRef identifies the model a session's spend is attributed to.
type costModelRef struct {
	Provider string
	Model    string
}

// Known reports whether any model was determined at all.
func (r costModelRef) Known() bool { return r.Model != "" }

// resolveCostModel returns the model a session's spend belongs to: the
// session's own selected model when it has one, otherwise the model that
// actually produced most of its messages. A session that switched models is
// attributed to the dominant one -- the same caveat the help text already
// carries for the whole column.
//
// A message-service failure degrades to an unknown model rather than aborting
// the whole table: this is the second-choice source, and one unreadable
// session must not cost the operator every other row.
func resolveCostModel(ctx context.Context, msgs message.Service, s session.Session) costModelRef {
	if s.SmartModelID != "" {
		return costModelRef{Provider: s.SmartModelProvider, Model: s.SmartModelID}
	}
	if msgs == nil {
		return costModelRef{}
	}
	report, err := msgs.UsageBySession(ctx, s.ID)
	if err != nil {
		return costModelRef{}
	}
	// Ties break on total tokens so the pick is deterministic: a session
	// where two models produced the same number of messages must not flip
	// its row between two runs.
	var best message.ModelUsage
	var found bool
	for _, group := range report.ByModel {
		if group.Usage.Model == "" {
			continue
		}
		if !found || group.Messages > best.Messages ||
			(group.Messages == best.Messages && group.Usage.TotalTokens > best.Usage.TotalTokens) {
			best = group
			found = true
		}
	}
	if !found {
		return costModelRef{}
	}
	return costModelRef{Provider: best.Usage.Provider, Model: best.Usage.Model}
}

// hasAnyPrice reports whether at least one of the four per-million rates is
// configured. All four default to 0, so a model present in the catalog with
// no prices set is exactly the unpriced case.
func hasAnyPrice(m *catwalk.Model) bool {
	return m.CostPer1MIn != 0 || m.CostPer1MOut != 0 ||
		m.CostPer1MInCached != 0 || m.CostPer1MOutCached != 0
}

// priceKnownFor reports whether a price is configured for ref. Lookup is by
// (provider, model) first and by bare model id second: a session can carry a
// provider the config no longer spells, and a model that moved providers
// still has a price.
func priceKnownFor(cfg *config.Config, ref costModelRef) bool {
	if cfg == nil || !ref.Known() {
		return false
	}
	if m := cfg.GetModel(ref.Provider, ref.Model); m != nil {
		return hasAnyPrice(m)
	}
	// Copy() snapshots the map out of the sync wrapper; the ids are sorted so
	// the scan never depends on map iteration order.
	//
	// A model id can be listed under several providers with different rates
	// (the same GLM model under two z.ai account shapes), so the fallback ORs
	// every match instead of taking the first one: a price configured
	// anywhere for this model is a price for this model. Returning on the
	// first match made an unpriced duplicate that sorted first swallow a
	// priced one.
	providers := cfg.Providers.Copy()
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	priced := false
	for _, id := range ids {
		for _, m := range providers[id].Models {
			if m.ID == ref.Model && hasAnyPrice(&m) {
				priced = true
			}
		}
	}
	return priced
}

// costCellText renders the COST column. A session with no price configured
// prints costNoPriceText when its cost is zero; a non-zero cost is real
// evidence and always prints, priced or not.
func costCellText(cost float64, priced bool) string {
	if cost == 0 && !priced {
		return costNoPriceText
	}
	return fmt.Sprintf("$%.3f", cost)
}
