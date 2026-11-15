package cliprovider

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/catalog"
	"github.com/stretchr/testify/require"
)

func TestAvailableCLIModelsProjectsAvailableSpecs(t *testing.T) {
	orig := AvailableFunc
	AvailableFunc = func() []CLISpec {
		return []CLISpec{{ModelID: "cli-x", ModelName: "X", ContextWindow: 123}}
	}
	t.Cleanup(func() { AvailableFunc = orig })

	models := AvailableCLIModels()
	require.Len(t, models, 1)
	require.Equal(t, catalog.LocalCLIModel{ID: "cli-x", Name: "X", ContextWindow: 123}, models[0])
	require.Equal(t, catalog.LocalCLIProviderID, ProviderID)
	require.Equal(t, catalog.LocalCLIProviderType, ProviderType)
}
