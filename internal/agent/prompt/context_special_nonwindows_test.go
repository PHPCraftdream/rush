//go:build !windows

package prompt

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBuild_ContextAbsoluteSpecialFileIsRejectedBeforeRead(t *testing.T) {
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{
		GlobalContextPaths: []string{"/dev/zero"},
	}}, t.TempDir())
	p, err := NewPrompt("special-context", "{{range .GlobalContextFiles}}{{.Content}}{{end}}")
	require.NoError(t, err)
	got, err := p.Build(context.Background(), "", "", store, store.Config(), false)
	require.NoError(t, err)
	require.Empty(t, got)
}
