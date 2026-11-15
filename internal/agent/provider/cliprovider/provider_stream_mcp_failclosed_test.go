package cliprovider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestStartStreamMCPPermissionSetupFailsClosedWithoutSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*CLISpec)
	}{
		{"qwen", func(spec *CLISpec) { spec.QwenMCPIntegration = true }},
		{"gemini", func(spec *CLISpec) { spec.GeminiMCPIntegration = true }},
		{"codex", func(spec *CLISpec) { spec.CodexMCPIntegration = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHomeDir(t)
			wd := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(wd, ".rush"), 0o755))
			tmpDir := t.TempDir()
			t.Setenv("TMPDIR", tmpDir)
			t.Setenv("TMP", tmpDir)
			t.Setenv("TEMP", tmpDir)
			started := false
			const modelID = "test-model"
			spec := CLISpec{ModelID: modelID, Binary: "unused", NoPTY: true, BuildArgs: func(bool) []string { return nil }}
			tc.set(&spec)
			model := &cliModel{
				spec: spec, perms: &recordingMCPPermissionService{}, workingDir: wd,
				streamProcess: func(bool, []string) procHandle {
					started = true
					return procHandle{stdout: strings.NewReader(""), kill: func() {}, wait: func() (string, error) { return "", nil }}
				},
			}
			_, err := model.StartStream(t.Context(), fantasy.Call{})
			require.ErrorContains(t, err, "configured permission MCP server is unavailable for "+modelID)
			require.ErrorContains(t, err, "MCP server requires a session ID when permissions are enabled")
			require.False(t, started, "CLI process must not start when permissions cannot be routed through MCP")
			for _, pattern := range []string{"rush-qwen-mcp-*", "rush-gemini-mcp-*"} {
				matches, err := filepath.Glob(filepath.Join(tmpDir, pattern))
				require.NoError(t, err)
				require.Empty(t, matches, "unexpected MCP artifact matching %s", pattern)
			}
		})
	}
}

func TestStartStreamMCPIntegrationWithoutPermissionsOrBridgeStarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*CLISpec)
	}{
		{"qwen", func(spec *CLISpec) { spec.QwenMCPIntegration = true }},
		{"gemini", func(spec *CLISpec) { spec.GeminiMCPIntegration = true }},
		{"codex", func(spec *CLISpec) { spec.CodexMCPIntegration = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHomeDir(t)
			wd := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(wd, ".rush"), 0o755))
			tmpDir := t.TempDir()
			t.Setenv("TMPDIR", tmpDir)
			t.Setenv("TMP", tmpDir)
			t.Setenv("TEMP", tmpDir)
			started := false
			spec := CLISpec{Binary: "unused", NoPTY: true, BuildArgs: func(bool) []string { return nil }}
			tc.set(&spec)
			model := &cliModel{
				spec: spec, workingDir: wd,
				streamProcess: func(bool, []string) procHandle {
					started = true
					return procHandle{stdout: strings.NewReader(""), kill: func() {}, wait: func() (string, error) { return "", nil }}
				},
			}
			stream, err := model.StartStream(t.Context(), fantasy.Call{})
			require.NoError(t, err)
			require.True(t, started, "CLI process should start when MCP permissions and bridge are not configured")
			stream.Stream()(func(fantasy.StreamPart) bool { return true })
		})
	}
}
