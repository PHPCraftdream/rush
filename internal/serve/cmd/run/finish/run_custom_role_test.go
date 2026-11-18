package runfinish

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRunRoleParserAcceptsBuiltinsAndExactCustomNames(t *testing.T) {
	for _, role := range []string{"smart", "fast", "worker", "reviewer"} {
		parsed, err := ValidateRunRole(role)
		require.NoError(t, err)
		require.NotEmpty(t, parsed)
	}
	for _, role := range []string{"Research Analyst", "café", "role.v2"} {
		parsed, err := ValidateRunRole(role)
		require.NoError(t, err)
		require.Empty(t, parsed, "custom profile names must not become ModelRole")
		require.Equal(t, role, RequestedRunRole(role))
	}
	for _, role := range []string{"", " smart", "smart ", "Smart", "bad/name"} {
		_, err := ValidateRunRole(role)
		require.Error(t, err, "invalid role %q", role)
	}
}

func TestFoldRoleModelCustomRoleDoesNotRequireBuiltinSlot(t *testing.T) {
	role := "Research Analyst"
	if !customRunRole(role) || RequestedRunRole(role) != role {
		t.Fatalf("custom role parser lost exact role name: %q", role)
	}
	if customRunRole(string(config.SelectedModelTypeSmart)) {
		t.Fatal("smart must remain a built-in role")
	}
}
