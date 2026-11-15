package message

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/store/db"
)

// CacheSupport records whether a message's cache counters carry information.
//
// Without it, zero is ambiguous: a provider that never reports caching and a
// genuine cache miss both store 0, and an analytics view would render the
// former as a confident "0% cache hit" — a lie. Every read path must branch on
// this rather than dividing blindly.
type CacheSupport string

const (
	// CacheSupportUnknown is the zero value: usage was recorded before this
	// field existed, or by a path that could not classify the provider.
	CacheSupportUnknown CacheSupport = ""
	// CacheSupportNative means the provider reported a real cache breakdown.
	// All four current providers (zai, claude-cli, codex-cli, gemini-cli) do.
	CacheSupportNative CacheSupport = "native"
	// CacheSupportNone means the provider is silent about caching, so the
	// cache counters mean "unknown", NOT "no hits".
	CacheSupportNone CacheSupport = "none"
)

// TokenUsage is the canonical, provider-independent token accounting for a
// single assistant message.
//
// CONVENTION (load-bearing — normalization happens at the provider boundary,
// never here): InputTokens counts ONLY tokens billed as fresh input. Tokens
// served from the prompt cache are in CacheReadTokens; tokens written into the
// cache are in CacheCreationTokens. The three are DISJOINT, so the prompt size
// is their sum.
//
// Providers disagree about this on the wire — claude reports an exclusive
// input count while codex and gemini fold the cache into theirs — which is
// exactly why internal/agent/cliprovider/usage.go exists. Anything reaching
// this type has already been normalized.
type TokenUsage struct {
	InputTokens         int64 `json:"input_tokens,omitempty"`
	OutputTokens        int64 `json:"output_tokens,omitempty"`
	ReasoningTokens     int64 `json:"reasoning_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`

	// TotalTokens is the provider's own figure when it published one, which
	// can EXCEED the itemized fields above: gemini counts thinking tokens its
	// stats block never breaks out. Do not assume it equals the sum.
	TotalTokens int64 `json:"total_tokens,omitempty"`

	// CostUSD is this message's own cost contribution, computed with the same
	// four-tier formula as the session total. Zero for flat-rate models and
	// for every local CLI model (they carry no prices at all).
	CostUSD float64 `json:"cost_usd,omitempty"`

	// Provider and Model record which model actually produced this message. A
	// session can switch models mid-conversation and a sub-agent can run a
	// different model than its parent, so the session's current selection is
	// not a safe substitute when aggregating.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	CacheSupport CacheSupport `json:"cache_support,omitempty"`

	// Estimated is true when the provider omitted usage entirely and the
	// numbers were synthesized from message lengths
	// (internal/agent/usage_fallback.go). Such rows are guesses and must be
	// flagged, not silently mixed into totals.
	Estimated bool `json:"estimated,omitempty"`
}

// PromptTokens is the full prompt size: fresh input plus both cache classes.
// This is the quantity that should be compared against a model's context
// window.
func (u TokenUsage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// CacheHitRatio returns the share of the prompt served from cache, in [0,1].
//
// ok is false when the number would be meaningless — the provider does not
// report caching, or there was no prompt to speak of. Callers MUST render
// "n/a" in that case instead of substituting zero; a fabricated 0% is
// indistinguishable from a real cache miss.
func (u TokenUsage) CacheHitRatio() (ratio float64, ok bool) {
	if u.CacheSupport != CacheSupportNative {
		return 0, false
	}
	prompt := u.PromptTokens()
	if prompt <= 0 {
		return 0, false
	}
	return float64(u.CacheReadTokens) / float64(prompt), true
}

// IsZero reports whether nothing was recorded. Used to avoid writing empty
// usage rows that would then be indistinguishable from a measured zero.
func (u TokenUsage) IsZero() bool {
	return u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.ReasoningTokens == 0 &&
		u.CacheCreationTokens == 0 &&
		u.CacheReadTokens == 0 &&
		u.TotalTokens == 0 &&
		u.CostUSD == 0
}

// Add accumulates another message's usage into this one, for aggregates over a
// session or a group of sessions.
//
// Provider/Model are cleared when they disagree: a sum across two models
// belongs to neither. CacheSupport degrades to CacheSupportNone if ANY
// contributor lacked cache reporting, so a mixed aggregate cannot present a
// cache ratio computed from partial data. Estimated is sticky for the same
// reason — one guess makes the whole total a guess.
func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	sum := TokenUsage{
		InputTokens:         u.InputTokens + other.InputTokens,
		OutputTokens:        u.OutputTokens + other.OutputTokens,
		ReasoningTokens:     u.ReasoningTokens + other.ReasoningTokens,
		CacheCreationTokens: u.CacheCreationTokens + other.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens + other.CacheReadTokens,
		TotalTokens:         u.TotalTokens + other.TotalTokens,
		CostUSD:             u.CostUSD + other.CostUSD,
		Estimated:           u.Estimated || other.Estimated,
	}

	switch {
	case u.IsZero() && u.Provider == "":
		// Adding onto an empty accumulator: adopt the incoming identity.
		sum.Provider, sum.Model = other.Provider, other.Model
		sum.CacheSupport = other.CacheSupport
	case u.Provider == other.Provider && u.Model == other.Model:
		sum.Provider, sum.Model = u.Provider, u.Model
		sum.CacheSupport = mergeCacheSupport(u.CacheSupport, other.CacheSupport)
	default:
		sum.CacheSupport = mergeCacheSupport(u.CacheSupport, other.CacheSupport)
	}
	return sum
}

// mergeCacheSupport takes the weakest of two classifications, so an aggregate
// never claims better cache visibility than its worst contributor.
func mergeCacheSupport(a, b CacheSupport) CacheSupport {
	if a == b {
		return a
	}
	if a == CacheSupportNone || b == CacheSupportNone {
		return CacheSupportNone
	}
	if a == CacheSupportUnknown || b == CacheSupportUnknown {
		return CacheSupportUnknown
	}
	return CacheSupportNative
}

// UsageReport is the analytics view over a session's recorded token usage.
type UsageReport struct {
	// ByModel holds one entry per (provider, model) that actually produced
	// messages in this session. Grouped rather than summed into a single row
	// because cache behaviour is a property of the model — averaging a
	// heavily-cached Claude turn with an uncached Gemini one produces a
	// number that describes neither.
	ByModel []ModelUsage

	// MissingUsage counts assistant messages in this session with no usage
	// recorded — rows written before this feature existed, or turns whose
	// usage write failed. Report it alongside any ratio: a cache-hit figure
	// computed over 3 of 400 messages is not a session statistic.
	MissingUsage int64
}

// DayUsage is one calendar day's totals, for the period view.
type DayUsage struct {
	// Day is a local-time "YYYY-MM-DD" string, matching the format
	// `sessions cost --by day` already prints.
	Day       string
	Usage     TokenUsage
	Messages  int64
	Estimated int64
}

// ModelUsage is one (provider, model) group's totals.
type ModelUsage struct {
	Usage TokenUsage
	// Messages is how many messages contributed to Usage.
	Messages int64
	// Estimated is how many of those had synthesized rather than
	// provider-reported numbers (see TokenUsage.Estimated).
	Estimated int64
}

// Total sums every group. CacheSupport degrades and Provider/Model clear when
// groups disagree, so a mixed total cannot present a cache ratio derived from
// partial visibility — see TokenUsage.Add.
func (r UsageReport) Total() TokenUsage {
	var total TokenUsage
	for _, m := range r.ByModel {
		total = total.Add(m.Usage)
	}
	return total
}

// Messages is the number of messages the report covers.
func (r UsageReport) Messages() int64 {
	var n int64
	for _, m := range r.ByModel {
		n += m.Messages
	}
	return n
}

// Coverage is the share of assistant messages that have usage recorded, in
// [0,1]. ok is false when there are no assistant messages at all, so callers
// render "n/a" rather than a meaningless 0% or a divide-by-zero.
func (r UsageReport) Coverage() (ratio float64, ok bool) {
	covered := r.Messages()
	total := covered + r.MissingUsage
	if total == 0 {
		return 0, false
	}
	return float64(covered) / float64(total), true
}

func (s *service) SetUsage(ctx context.Context, id string, usage TokenUsage) error {
	// Refuse to write an all-zero row: it would be stored as a measured zero
	// and become indistinguishable from a real reading, quietly inflating
	// coverage while contributing nothing.
	if usage.IsZero() {
		return nil
	}

	estimated := int64(0)
	if usage.Estimated {
		estimated = 1
	}

	err := s.q.UpdateMessageUsage(ctx, db.UpdateMessageUsageParams{
		ID:                  id,
		InputTokens:         nullInt64(usage.InputTokens),
		OutputTokens:        nullInt64(usage.OutputTokens),
		ReasoningTokens:     nullInt64(usage.ReasoningTokens),
		CacheCreationTokens: nullInt64(usage.CacheCreationTokens),
		CacheReadTokens:     nullInt64(usage.CacheReadTokens),
		// total_tokens is the column the analytics queries use to tell
		// "recorded" from "never written", so it must always be non-NULL on a
		// row we did record — even if the provider reported no total and the
		// derived value happens to be 0.
		TotalTokens:    sql.NullInt64{Int64: usage.TotalTokens, Valid: true},
		CostUsd:        sql.NullFloat64{Float64: usage.CostUSD, Valid: true},
		UsageProvider:  nullString(usage.Provider),
		UsageModel:     nullString(usage.Model),
		CacheSupport:   nullString(string(usage.CacheSupport)),
		UsageEstimated: sql.NullInt64{Int64: estimated, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("record usage for message %s: %w", id, err)
	}
	return nil
}

// UsageByModelInRange aggregates recorded usage across ALL sessions whose
// messages fall in [since, until], grouped by the model that produced them.
//
// since/until are Unix seconds; pass 0 and math.MaxInt64 for "everything".
func (s *service) UsageByModelInRange(ctx context.Context, since, until int64) (UsageReport, error) {
	rows, err := s.qRead.SumMessageUsageByModelInRange(ctx, db.SumMessageUsageByModelInRangeParams{
		CreatedAt:   since,
		CreatedAt_2: until,
	})
	if err != nil {
		return UsageReport{}, fmt.Errorf("aggregate usage by model: %w", err)
	}

	report := UsageReport{ByModel: make([]ModelUsage, 0, len(rows))}
	for _, r := range rows {
		report.ByModel = append(report.ByModel, ModelUsage{
			Messages:  r.Recorded,
			Estimated: r.Estimated,
			Usage: TokenUsage{
				InputTokens:         r.InputTokens,
				OutputTokens:        r.OutputTokens,
				ReasoningTokens:     r.ReasoningTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
				TotalTokens:         r.TotalTokens,
				CostUSD:             r.CostUsd,
				Provider:            r.Provider,
				Model:               r.Model,
				CacheSupport:        CacheSupport(r.CacheSupport),
				Estimated:           r.Estimated > 0,
			},
		})
	}

	missing, err := s.qRead.CountMessagesMissingUsageInRange(ctx, db.CountMessagesMissingUsageInRangeParams{
		CreatedAt:   since,
		CreatedAt_2: until,
	})
	if err != nil {
		return UsageReport{}, fmt.Errorf("count messages missing usage: %w", err)
	}
	report.MissingUsage = missing

	return report, nil
}

// UsageByDayInRange is UsageByModelInRange bucketed by local calendar day.
//
// The per-day rows carry no Provider/Model: a day can span several models, and
// labelling the bucket with one of them would be a lie. CacheSupport is left
// unset for the same reason, which makes CacheHitRatio decline to answer —
// deliberately, since a day's blended cache ratio across providers with
// different cache visibility is not a meaningful number. Use the by-model
// view when the ratio is what matters.
func (s *service) UsageByDayInRange(ctx context.Context, since, until int64) ([]DayUsage, error) {
	rows, err := s.qRead.SumMessageUsageByDayInRange(ctx, db.SumMessageUsageByDayInRangeParams{
		CreatedAt:   since,
		CreatedAt_2: until,
	})
	if err != nil {
		return nil, fmt.Errorf("aggregate usage by day: %w", err)
	}

	out := make([]DayUsage, 0, len(rows))
	for _, r := range rows {
		out = append(out, DayUsage{
			Day:       r.Day,
			Messages:  r.Recorded,
			Estimated: r.Estimated,
			Usage: TokenUsage{
				InputTokens:         r.InputTokens,
				OutputTokens:        r.OutputTokens,
				ReasoningTokens:     r.ReasoningTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
				TotalTokens:         r.TotalTokens,
				CostUSD:             r.CostUsd,
				Estimated:           r.Estimated > 0,
			},
		})
	}
	return out, nil
}

func (s *service) UsageBySession(ctx context.Context, sessionID string) (UsageReport, error) {
	rows, err := s.qRead.SumMessageUsageBySession(ctx, sessionID)
	if err != nil {
		return UsageReport{}, fmt.Errorf("aggregate usage for session %s: %w", sessionID, err)
	}

	report := UsageReport{ByModel: make([]ModelUsage, 0, len(rows))}
	for _, r := range rows {
		report.ByModel = append(report.ByModel, ModelUsage{
			Messages:  r.Recorded,
			Estimated: r.Estimated,
			Usage: TokenUsage{
				InputTokens:         r.InputTokens,
				OutputTokens:        r.OutputTokens,
				ReasoningTokens:     r.ReasoningTokens,
				CacheCreationTokens: r.CacheCreationTokens,
				CacheReadTokens:     r.CacheReadTokens,
				TotalTokens:         r.TotalTokens,
				CostUSD:             r.CostUsd,
				Provider:            r.Provider,
				Model:               r.Model,
				CacheSupport:        CacheSupport(r.CacheSupport),
				Estimated:           r.Estimated > 0,
			},
		})
	}

	missing, err := s.qRead.CountMessagesMissingUsage(ctx, sessionID)
	if err != nil {
		return UsageReport{}, fmt.Errorf("count messages missing usage for session %s: %w", sessionID, err)
	}
	report.MissingUsage = missing

	return report, nil
}

// nullInt64 stores 0 as a real 0 rather than NULL: within a row we did record,
// zero is a measurement ("no cache hits this turn"), and collapsing it to NULL
// would make it read as "never recorded".
func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: true}
}

func nullString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}
