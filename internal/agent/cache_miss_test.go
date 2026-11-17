package agent

import (
	"strconv"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func obs(model, prefix, tools string, digests ...string) *cacheRequestObs {
	return &cacheRequestObs{prefix: prefix, tools: tools, model: model, digests: digests, at: time.Now()}
}

func TestClassifyCacheMiss(t *testing.T) {
	base := obs("p/m", "pre", "tools", "a", "b", "c")
	tests := []struct {
		name       string
		prev, cur  *cacheRequestObs
		wantCause  string
		wantCommon int
	}{
		{"first request", nil, obs("p/m", "pre", "tools", "a"), cacheMissFirstRequest, 0},
		{"history only grew", base, obs("p/m", "pre", "tools", "a", "b", "c", "d"), cacheMissProviderSide, 3},
		{"model changed", base, obs("p/other", "pre", "tools", "a", "b", "c", "d"), cacheMissModelChanged, 3},
		{"system prefix changed", base, obs("p/m", "pre2", "tools", "a", "b", "c", "d"), cacheMissPrefixChanged, 3},
		{"tools changed", base, obs("p/m", "pre", "tools2", "a", "b", "c", "d"), cacheMissToolsChanged, 3},
		{"history rewritten in the middle", base, obs("p/m", "pre", "tools", "a", "x", "c", "d"), cacheMissHistoryChanged, 1},
		{"history truncated", base, obs("p/m", "pre", "tools", "a", "b"), cacheMissHistoryChanged, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cause, common := classifyCacheMiss(tc.prev, tc.cur)
			require.Equal(t, tc.wantCause, cause)
			require.Equal(t, tc.wantCommon, common)
		})
	}
}

// The per-message digests must move only where the message moved, otherwise the
// divergence index in the log line is meaningless.
func TestPreparedCacheDiagnostic_MessageDigestsLocateTheChange(t *testing.T) {
	msgs := func(texts ...string) []fantasy.Message {
		out := make([]fantasy.Message, len(texts))
		for i, s := range texts {
			role := fantasy.MessageRoleUser
			if i == 0 {
				role = fantasy.MessageRoleSystem
			}
			out[i] = fantasy.Message{Role: role, Content: []fantasy.MessagePart{fantasy.TextPart{Text: s}}}
		}
		return out
	}
	a := preparedCacheDiagnostic(msgs("sys", "one", "two", "three"), nil)
	b := preparedCacheDiagnostic(msgs("sys", "one", "TWO", "three"), nil)
	require.Len(t, a.MessageDigests, 4)
	require.Equal(t, 2, commonPrefixLen(a.MessageDigests, b.MessageDigests))
	grown := preparedCacheDiagnostic(msgs("sys", "one", "two", "three", "four"), nil)
	require.Equal(t, 4, commonPrefixLen(a.MessageDigests, grown.MessageDigests))
}

func TestSwapCacheObs_BoundedAndReturnsPrevious(t *testing.T) {
	cacheObs.mu.Lock()
	cacheObs.last = map[string]*cacheRequestObs{}
	cacheObs.mu.Unlock()
	first := obs("p/m", "p", "t", "a")
	require.Nil(t, swapCacheObs("s", first))
	require.Same(t, first, swapCacheObs("s", obs("p/m", "p", "t", "a", "b")))
	for i := 0; i < cacheObsMaxSessions+40; i++ {
		swapCacheObs("k"+strconv.Itoa(i), obs("p/m", "p", "t"))
	}
	cacheObs.mu.Lock()
	n := len(cacheObs.last)
	cacheObs.mu.Unlock()
	require.LessOrEqual(t, n, cacheObsMaxSessions+1)
}
