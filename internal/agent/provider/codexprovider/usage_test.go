package codexprovider

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestDecodeResponseUsageUsesDisjointCacheCounters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		wire string
		want fantasy.Usage
	}{
		{
			"cold", `{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}`,
			fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
		},
		{
			"warm", `{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":90},"output_tokens_details":{"reasoning_tokens":4}}}`,
			fantasy.Usage{InputTokens: 10, CacheReadTokens: 90, OutputTokens: 20, ReasoningTokens: 4, TotalTokens: 120},
		},
		{
			"fully cached", `{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":100}}}`,
			fantasy.Usage{CacheReadTokens: 100, OutputTokens: 20, TotalTokens: 120},
		},
		{
			"missing total retains cached input", `{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":90}}}`,
			fantasy.Usage{InputTokens: 10, CacheReadTokens: 90, OutputTokens: 20, TotalTokens: 120},
		},
		{
			"inconsistent counters never make fresh input negative", `{"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":150}}}`,
			fantasy.Usage{CacheReadTokens: 150, OutputTokens: 20, TotalTokens: 120},
		},
		{"empty", `{}`, fantasy.Usage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeResponse(json.RawMessage(tc.wire))
			require.NoError(t, err)
			require.Equal(t, tc.want, got.usage)
		})
	}
}
