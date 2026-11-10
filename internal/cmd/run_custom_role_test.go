package cmd

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRunRoleParserAcceptsBuiltinsAndExactCustomNames(t *testing.T) {
	for _, role := range []string{"smart", "fast", "worker", "reviewer"} {
		parsed, err := validateRunRole(role)
		require.NoError(t, err)
		require.NotEmpty(t, parsed)
	}
	for _, role := range []string{"Research Analyst", "café", "role.v2"} {
		parsed, err := validateRunRole(role)
		require.NoError(t, err)
		require.Empty(t, parsed, "custom profile names must not become ModelRole")
		require.Equal(t, role, requestedRunRole(role))
	}
	for _, role := range []string{"", " smart", "smart ", "Smart", "bad/name"} {
		_, err := validateRunRole(role)
		require.Error(t, err, "invalid role %q", role)
	}
}

func TestFoldRoleModelCustomRoleDoesNotRequireBuiltinSlot(t *testing.T) {
	role := "Research Analyst"
	if !customRunRole(role) || requestedRunRole(role) != role {
		t.Fatalf("custom role parser lost exact role name: %q", role)
	}
	if customRunRole(string(config.SelectedModelTypeSmart)) {
		t.Fatal("smart must remain a built-in role")
	}
}
