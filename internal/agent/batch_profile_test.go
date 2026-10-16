package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchProfileConstructor(t *testing.T) {
	t.Parallel()

	t.Run("zero cap rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewBatchProfile(BatchProfileCaps{IO: 1, CLIDriver: 0, AgentDriver: 1, Default: 1, Global: 3})
		require.ErrorIs(t, err, errBatchCapInvalid)
	})

	t.Run("negative cap rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewBatchProfile(BatchProfileCaps{IO: -1, CLIDriver: 1, AgentDriver: 1, Default: 1, Global: 2})
		require.ErrorIs(t, err, errBatchCapInvalid)
	})

	t.Run("global below sum rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewBatchProfile(BatchProfileCaps{IO: 2, CLIDriver: 1, AgentDriver: 1, Default: 1, Global: 3})
		require.ErrorIs(t, err, errBatchGlobalTooSmall)
	})

	t.Run("global zero rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewBatchProfile(BatchProfileCaps{IO: 1, CLIDriver: 1, AgentDriver: 1, Default: 1, Global: 0})
		require.ErrorIs(t, err, errBatchCapInvalid)
	})

	t.Run("missing default group rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewBatchProfileFromMap(map[string]int{batchGroupIO: 1, batchGroupCLIDriver: 1, batchGroupAgentDriver: 1}, 3)
		require.ErrorIs(t, err, errBatchMissingGroup)
	})

	t.Run("unknown group name rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewBatchProfileFromMap(map[string]int{
			batchGroupIO: 1, batchGroupCLIDriver: 1, batchGroupAgentDriver: 1, batchGroupDefault: 1, "agents": 5,
		}, 9)
		require.ErrorIs(t, err, errBatchUnknownGroupName)
	})

	t.Run("duplicate rank conflict rejected", func(t *testing.T) {
		t.Parallel()
		broken := map[string]int{batchGroupIO: 0, batchGroupCLIDriver: 1, batchGroupAgentDriver: 1, batchGroupDefault: 0}
		err := validateBatchRanks(broken)
		require.ErrorIs(t, err, errBatchRankConflict)
		require.NoError(t, validateBatchRanks(batchGroupRanks))
	})

	t.Run("default profile accepted", func(t *testing.T) {
		t.Parallel()
		p, err := NewBatchProfileDefault()
		require.NoError(t, err)
		require.NotNil(t, p)
	})
}

func TestBatchProfileLimitsRoundTrip(t *testing.T) {
	t.Parallel()
	p, err := NewBatchProfileDefault()
	require.NoError(t, err)
	limits := p.Limits(8, 500)
	require.Equal(t, jobbatch.Limits{
		MaxConcurrent: 53,
		ByGroup:       map[string]int{"io": 50, "cli_driver": 1, "agent_driver": 1, "": 1},
		MaxDepth:      8,
		MaxNodes:      500,
	}, limits)

	// Compile-proof: a real jobbatch constructor consumes the value.
	engine, err := jobbatch.NewEngine(limits)
	require.NoError(t, err)
	require.NotNil(t, engine)
}

func TestBatchProfileLookups(t *testing.T) {
	t.Parallel()
	p, err := NewBatchProfileDefault()
	require.NoError(t, err)

	cap, ok := p.GroupCap(batchGroupIO)
	require.True(t, ok)
	require.Equal(t, 50, cap)

	_, ok = p.GroupCap("agents")
	require.False(t, ok)

	rank, ok := p.GroupRank(batchGroupDefault)
	require.True(t, ok)
	require.Equal(t, 0, rank)

	rank, ok = p.GroupRank(batchGroupAgentDriver)
	require.True(t, ok)
	require.Equal(t, 2, rank)

	_, ok = p.GroupRank("nope")
	require.False(t, ok)

	require.Equal(t, []string{"", "agent_driver", "cli_driver", "io"}, p.Groups())
}

func TestBatchProfileLimitsImmutability(t *testing.T) {
	t.Parallel()
	p, err := NewBatchProfileDefault()
	require.NoError(t, err)
	first := p.Limits(4, 40)
	first.ByGroup[batchGroupIO] = 999
	delete(first.ByGroup, batchGroupDefault)

	second := p.Limits(4, 40)
	require.Equal(t, 50, second.ByGroup[batchGroupIO])
	require.Equal(t, 1, second.ByGroup[batchGroupDefault])
}
