package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelsCacheClearRemovesSelectedCatalogOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "rush")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	zai := filepath.Join(dir, "model-catalog-zai.json")
	stepfun := filepath.Join(dir, "model-catalog-stepfun.json")
	for _, path := range []string{zai, stepfun} {
		require.NoError(t, os.WriteFile(path, []byte(`{"models":[]}`), 0o600))
	}
	var output bytes.Buffer
	modelsCacheClearCmd.SetOut(&output)
	t.Cleanup(func() { modelsCacheClearCmd.SetOut(nil) })
	require.NoError(t, modelsCacheClearCmd.RunE(modelsCacheClearCmd, []string{"zai"}))
	_, err := os.Stat(zai)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(stepfun)
	require.NoError(t, err)
	require.Contains(t, output.String(), "Restart the WebUI server")
}
