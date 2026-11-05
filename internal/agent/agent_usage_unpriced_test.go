package agent

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

func TestModelUnpriced(t *testing.T) {
	t.Parallel()

	used := fantasy.Usage{InputTokens: 10, OutputTokens: 5}
	cost := 0.25
	cases := []struct {
		name     string
		model    Model
		usage    fantasy.Usage
		override *float64
		want     bool
	}{
		{"no price, tokens used", Model{}, used, nil, true},
		{"cache tokens only", Model{}, fantasy.Usage{CacheReadTokens: 1}, nil, true},
		{"no tokens", Model{}, fantasy.Usage{}, nil, false},
		{"input price", Model{CatwalkCfg: catwalk.Model{CostPer1MIn: 1}}, used, nil, false},
		{"output price", Model{CatwalkCfg: catwalk.Model{CostPer1MOut: 1}}, used, nil, false},
		{"cached price", Model{CatwalkCfg: catwalk.Model{CostPer1MInCached: 1}}, used, nil, false},
		{"flat rate", Model{FlatRate: true}, used, nil, false},
		{"provider reported cost", Model{}, used, &cost, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, modelUnpriced(tc.model, tc.usage, tc.override))
		})
	}
}

func TestUpdateSessionUsageWarnsOncePerUnpricedModel(t *testing.T) {
	logs := captureAttachRetryLogs(t)
	a := &sessionAgent{}
	unpriced := Model{ModelCfg: config.SelectedModel{Provider: "unpriced-prov", Model: "unpriced-model"}}
	priced := Model{
		ModelCfg:   config.SelectedModel{Provider: "costed-prov", Model: "costed-model"},
		CatwalkCfg: catwalk.Model{CostPer1MIn: 1, CostPer1MOut: 2},
	}
	usage := fantasy.Usage{InputTokens: 100, OutputTokens: 10}

	for range 3 {
		a.updateSessionUsage(unpriced, &session.Session{}, usage, nil)
		a.updateSessionUsage(priced, &session.Session{}, usage, nil)
	}

	out := logs.String()
	require.Equal(t, 1, strings.Count(out, "Model has no price"), out)
	require.Contains(t, out, "unpriced-prov")
	require.Contains(t, out, "unpriced-model")
	require.NotContains(t, out, "costed-model")
}
