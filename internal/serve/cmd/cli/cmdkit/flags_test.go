package cmdkit

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopeFromFlags(t *testing.T) {
	mk := func(global, local bool) *cobra.Command {
		c := &cobra.Command{}
		c.Flags().Bool("global", false, "")
		c.Flags().Bool("local", false, "")
		require.NoError(t, c.ParseFlags(scopeArgs(global, local)))
		return c
	}

	t.Run("default returns supplied default", func(t *testing.T) {
		s, err := ScopeFromFlags(mk(false, false), config.ScopeWorkspace)
		require.NoError(t, err)
		assert.Equal(t, config.ScopeWorkspace, s)
	})
	t.Run("--global wins", func(t *testing.T) {
		s, err := ScopeFromFlags(mk(true, false), config.ScopeWorkspace)
		require.NoError(t, err)
		assert.Equal(t, config.ScopeGlobal, s)
	})
	t.Run("--local wins", func(t *testing.T) {
		s, err := ScopeFromFlags(mk(false, true), config.ScopeGlobal)
		require.NoError(t, err)
		assert.Equal(t, config.ScopeWorkspace, s)
	})
	t.Run("both set is an error", func(t *testing.T) {
		_, err := ScopeFromFlags(mk(true, true), config.ScopeGlobal)
		assert.Error(t, err)
	})
}

func scopeArgs(global, local bool) []string {
	var args []string
	if global {
		args = append(args, "--global")
	}
	if local {
		args = append(args, "--local")
	}
	return args
}
