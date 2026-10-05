package server

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/stretchr/testify/require"
)

func TestConfigWireZAIModelPickerUsesFinalizedContextWindow(t *testing.T) {
	models := discover.ApplyModelFacts("zai", []catwalk.Model{{ID: "glm-5.3-flash", ContextWindow: 0}}, nil)
	infos := visibleModelInfos("zai", models, nil)
	require.Len(t, infos, 2)
	var flash *ModelInfoWire
	for i := range infos {
		if infos[i].ID == "glm-5.3-flash" {
			flash = &infos[i]
			break
		}
	}
	require.NotNil(t, flash)
	require.Equal(t, int64(1000000), flash.ContextWindow)
}
