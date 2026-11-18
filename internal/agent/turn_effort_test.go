package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestTurnSmartReasoningEffortUsesResolvedModel(t *testing.T) {
	model := Model{ModelCfg: config.SelectedModel{ReasoningEffort: "high"}}
	require.Equal(t, "high", provauth.TurnSmartReasoningEffort(model))

	// The pinned model configuration is authoritative even when it has no
	// effort; session preferences are intentionally not a fallback here.
	model.ModelCfg.ReasoningEffort = ""
	require.Empty(t, provauth.TurnSmartReasoningEffort(model))
}
